package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// Logs streams structured container log lines for one managed stack through
// onLine via `docker compose logs` (PRD §24). Streaming
// stops when the upstream closes (finite tail / no follow), when ctx is
// canceled, or when the command fails.
// Managed-only ordering (PRD §10): the stack must resolve to a directory
// whose .wdm.lock manifest names req.AppID BEFORE any Docker command runs.
// Unmanaged directories and uninstalled apps surface
// [types.ErrCodeUsageValidation] refusals without touching Docker, and
// requested services not recorded in the manifest's image pins are refused
// the same way. Container identity is then resolved by inspecting the
// manifest's Compose project: only containers carrying wdm.managed=true plus
// wdm.app=<app> contribute to the container-name → service map, and streamed
// lines whose container prefix is not in that map are dropped, so wdm never
// relays log content from containers it does not manage.
// Read-only discipline (PRD §26): Logs acquires neither the global
// runtime.lock nor a blocking per-stack flock and writes nothing; a stack
// mid-Install/Update surfaces a [types.ErrCodeRuntimeLockHeld] refusal
// through the shared non-blocking manifest read, like Status.
// Redaction (PRD §12, §24): every streamed line passes through the docker
// client's active redactor before parsing, so secret-shaped content (env
// assignments, JSON fields, bearer tokens, URL credentials) is scrubbed
// before it can reach the callback, terminal, or JSON output.
// Callback contract: onLine is required — streaming without a receiver is a
// usage error, refused before any Docker call. The engine blocks on the
// callback, applying back-pressure to the upstream reader ([types.LogLineFn]
// notes that implementations should return quickly). Context cancellation
// propagates as a typed [types.ErrCodeUserCanceled] error from the docker
// layer — for follow-mode streams that is the ordinary teardown path.
func (e *Engine) Logs(ctx context.Context, req types.LogsRequest, onLine types.LogLineFn) error {
	if e.isClosed() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("core.Logs: %w", err)
	}
	if onLine == nil {
		return usageValidationError(
			"log line callback is required",
			"pass a non-nil log line callback to receive streamed lines",
			nil,
		)
	}
	if req.AppID == "" {
		return usageValidationError(
			"app id is required",
			"pass the app id of an installed stack",
			nil,
		)
	}
	if req.Tail < 0 {
		return usageValidationError(
			"log tail must be zero or positive",
			"pass 0 to stream all history or a positive line count",
			nil,
		)
	}

	stackPath, lock, err := e.resolveManagedStack(ctx, req.AppID)
	if err != nil {
		return err
	}
	services, err := validateRequestedLogServices(req.Services, lock)
	if err != nil {
		return err
	}

	// Seed the redactor with the stack's .env VALUES so a bare secret literal
	// echoed into a log line is scrubbed, mirroring ValidateConfig. Same
	// fail-closed policy: an absent .env degrades to structural-only redaction;
	// a present-but-rejected .env propagates rather than streaming with weaker
	// redaction (PRD §28).
	redactor, err := validateConfigRedactor(stackPath)
	if err != nil {
		return err
	}
	client, err := e.buildDockerClient(redactor)
	if err != nil {
		return err
	}

	containers, err := docker.InspectProjectContainers(ctx, client, lock.ComposeProject)
	if err != nil {
		return err
	}
	managed := managedContainerServices(req.AppID, containers)

	project, err := logsComposeProject(stackPath, lock.ComposeProject)
	if err != nil {
		return err
	}

	opts := docker.ComposeLogsOptions{
		Follow:   req.Follow,
		Tail:     req.Tail,
		Services: services,
	}
	return docker.ComposeLogs(ctx, client, project, opts, func(entry docker.ComposeLogEntry) {
		service, ok := managed[entry.ContainerName]
		if !ok {
			return
		}
		onLine(types.LogLine{
			Timestamp:      entry.Timestamp,
			AppID:          req.AppID,
			ComposeProject: lock.ComposeProject,
			ContainerName:  entry.ContainerName,
			Service:        service,
			Stream:         entry.Stream,
			Text:           entry.Text,
		})
	})
}

// validateRequestedLogServices checks an optional service restriction against
// the manifest's image-pin service set before any Docker call: a requested
// service the manifest does not record is refused with a typed
// usage-validation error naming the known set (managed-only, PRD §10).
// Manifests without image pins skip the membership check — the docker layer's
// strict service-name shape validation still guards the argv. Duplicates are
// dropped while preserving request order.
func validateRequestedLogServices(requested []string, lock *state.StackLock) ([]string, error) {
	if len(requested) == 0 {
		return nil, nil
	}

	expected := expectedStatusServices(lock)
	known := make(map[string]struct{}, len(expected))
	for _, service := range expected {
		known[service] = struct{}{}
	}

	seen := make(map[string]struct{}, len(requested))
	services := make([]string, 0, len(requested))
	for _, service := range requested {
		if service == "" {
			return nil, usageValidationError(
				"service name is empty",
				"pass non-empty Compose service names",
				nil,
			)
		}
		if len(known) > 0 {
			if _, ok := known[service]; !ok {
				return nil, usageValidationError(
					"service is not part of this app",
					fmt.Sprintf("known services: %s", strings.Join(expected, ", ")),
					fmt.Errorf("service %q is not recorded in the stack manifest", service),
				)
			}
		}
		if _, ok := seen[service]; ok {
			continue
		}
		seen[service] = struct{}{}
		services = append(services, service)
	}
	return services, nil
}

// managedContainerServices indexes Compose service names by container name
// for the containers carrying the wdm management labels (PRD §10) — the same
// label test Status applies. Container names key the map because `docker
// compose logs` prefixes each line with the container name, which the curated
// templates override via container_name, making the service label the only
// authoritative mapping. Containers without a service label cannot be
// attributed and are skipped.
func managedContainerServices(appID string, containers []docker.ContainerInfo) map[string]string {
	managed := make(map[string]string, len(containers))
	for _, container := range containers {
		if container.Labels["wdm.managed"] != "true" || container.Labels["wdm.app"] != appID {
			continue
		}
		if container.Name == "" || container.Service == "" {
			continue
		}
		managed[container.Name] = container.Service
	}
	return managed
}

// logsComposeProject builds the validated Compose project triple for the
// stack's existing files, applying installComposeProject's SafeJoin
// discipline so the docker layer only ever sees contained absolute paths.
func logsComposeProject(stackPath, composeProjectName string) (docker.ComposeProject, error) {
	composePath, err := security.SafeJoin(stackPath, installComposeFilename)
	if err != nil {
		return docker.ComposeProject{}, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	envPath, err := security.SafeJoin(stackPath, installEnvFilename)
	if err != nil {
		return docker.ComposeProject{}, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	return docker.ComposeProject{
		ComposeFile: composePath,
		EnvFile:     envPath,
		ProjectName: composeProjectName,
	}, nil
}
