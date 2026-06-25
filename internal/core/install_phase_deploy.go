package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// commitInstall runs protocol steps 4 (Confirmer clause) through 6 for
// a fresh install: confirmation, catalog network pre-creation, the
// second port check, `docker compose up -d`,
// opportunistic image-digest capture, and the .wdm.lock manifest write
// through the held flock fd. A nil return means the manifest fsync
// succeeded — the commit point after which no failure removes files.
func (e *Engine) commitInstall(
	ctx context.Context,
	client docker.Client,
	plan *installPlan,
	stackHandle *state.StackLockHandle,
	confirmer types.Confirmer,
	cleanup *freshInstallDockerCleanup,
	onProgress types.ProgressFn,
) error {
	if err := confirmInstallDeployment(ctx, confirmer, plan, onProgress); err != nil {
		return err
	}
	if err := ensureInstallNetworks(ctx, client, plan, cleanup, onProgress); err != nil {
		return err
	}
	if err := plan.recheckPorts(ctx); err != nil {
		return err
	}
	// Arm the Docker rollback before `docker compose up`: from here a
	// fault may have created containers or volumes.
	cleanup.deployAttempted = true
	if err := deployInstallStack(ctx, client, plan, onProgress); err != nil {
		return err
	}
	pins, err := captureInstallImagePins(ctx, client, plan)
	if err != nil {
		return err
	}
	return e.writeInstallLockManifest(ctx, plan, stackHandle, pins, onProgress)
}

// installDockerClient builds the Docker client for one install
// operation. The factory receives a fresh active redactor carrying the
// plan's full redaction set (generated secrets, argon2id one-time
// plaintexts, and sensitive --set values) via installRedactionSecrets, so
// any Docker stderr surfaced by compose validation or network pre-creation
// is scrubbed before it can reach errors or logs — the same source the
// install logger uses.
func (e *Engine) installDockerClient(plan *installPlan) (docker.Client, error) {
	return e.buildDockerClient(security.NewActiveRedactor(installRedactionSecrets(plan)))
}

// buildDockerClient constructs one operation's Docker client through
// the engine's injectable factory with the supplied redactor wired in,
// so Docker stderr is scrubbed before it can reach errors or logs. Nil
// factories and nil returned clients fail closed before any Docker
// work. Install passes a redactor carrying its generated secret
// literals; the read-only Status path passes a structural active
// redactor (no literals exist in memory there, and the
// env/JSON/Bearer/URL patterns still scrub anything secret-shaped).
func (e *Engine) buildDockerClient(redactor security.Redactor) (docker.Client, error) {
	factory := e.newDockerClient
	if factory == nil {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"docker client factory is required",
			"construct the engine with a docker client factory",
		)
	}
	client, err := factory(redactor)
	if err != nil {
		return nil, fmt.Errorf("core: constructing docker client: %w", err)
	}
	if client == nil {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"docker client factory returned no client",
			"construct the engine with a docker client factory that returns a client",
		)
	}
	return client, nil
}

// freshInstallDockerCleanup tracks the Docker resources a single
// pre-manifest fresh install may have created so [failFreshInstall] can
// remove exactly those — and nothing else — when the install fails
// before the .wdm.lock manifest is durable (PRD §18). It is armed
// only once a resource can exist: deployAttempted is set immediately
// before `docker compose up`, and createdNetworks holds the names this
// install newly created (in creation order). When nothing was armed —
// the confirmation was declined, or network pre-creation failed before
// creating any network — the rollback never touches Docker state.
type freshInstallDockerCleanup struct {
	client          docker.Client
	project         docker.ComposeProject
	createdNetworks []string
	deployAttempted bool
}

// armed reports whether any Docker resource may exist for this install,
// so [failFreshInstall] must run the Docker rollback rather than the
// file-only sad path.
func (c *freshInstallDockerCleanup) armed() bool {
	if c == nil {
		return false
	}
	return c.deployAttempted || len(c.createdNetworks) > 0
}

// runDocker removes only the Docker resources this install created,
// fail-closed in PRD §19 / §12 terms: the safe `docker compose down`
// (never -v), then the project-labeled named volumes, then the networks
// this install created in REVERSE creation order. It removes containers
// and volumes only when a deploy was actually attempted, and removes a
// network only if EnsureNetworkReport reported it as newly created — a
// pre-existing or external network is never removed. Every primitive
// fault is collected and joined so the inconsistency stays visible.
func (c *freshInstallDockerCleanup) runDocker(ctx context.Context) error {
	var faults []error
	if c.deployAttempted {
		if err := docker.ComposeDown(ctx, c.client, c.project); err != nil {
			faults = append(faults, err)
		}
		volumes, err := docker.ListProjectNamedVolumes(ctx, c.client, c.project.ProjectName)
		if err != nil {
			faults = append(faults, err)
		}
		for _, volume := range volumes {
			if err := docker.RemoveNamedVolume(ctx, c.client, volume); err != nil {
				faults = append(faults, err)
			}
		}
	}
	for i := len(c.createdNetworks) - 1; i >= 0; i-- {
		if err := docker.RemoveNetwork(ctx, c.client, c.createdNetworks[i]); err != nil {
			faults = append(faults, err)
		}
	}
	if len(faults) > 0 {
		return errors.Join(faults...)
	}
	return nil
}

// failFreshInstall is the protocol step 7 sad path for fresh installs:
// when armed and pre-manifest it first removes only the Docker resources
// this install created — safe compose down, project-labeled
// volumes, and its own networks in reverse order — under a
// cancellation-detached, bounded context so a canceled install still
// reclaims its resources, then removes the partial files this operation
// wrote while the per-stack flock is held, then releases the flock. When
// nothing was armed (confirmation declined or network creation failed
// before creating any network) it runs the file cleanup and lock release
// only, never touching Docker state. On
// clean cleanup the original fault returns UNCHANGED so docker-layer
// errors and typed codes stay authoritative; a cleanup failure joins a
// typed error naming the leftover stack path so the inconsistency is
// visible (PRD §18 fail-closed on uncertain state).
func failFreshInstall(ctx context.Context, fault error, plan *installPlan, handle *state.StackLockHandle, cleanup *freshInstallDockerCleanup) error {
	var dockerErr error
	if cleanup.armed() {
		// Detach the rollback from the install context so a canceled
		// install (Ctrl-C mid-deploy) — the most common failure mode —
		// still removes its own containers, volumes, and networks rather
		// than short-circuiting the moment the docker client sees the
		// canceled ctx. context.WithoutCancel preserves context values
		// (logger and the like); the bound is the only deadline left.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), installRollbackTimeout)
		defer cancel()
		if err := cleanup.runDocker(cleanupCtx); err != nil {
			dockerErr = installDockerCleanupError(cleanup.project.ProjectName, plan.stackPath, err)
		}
	}
	cleanupErr := cleanupFreshInstallArtifacts(plan)
	var releaseErr error
	if handle != nil {
		releaseErr = handle.Release()
	}
	if dockerErr == nil && cleanupErr == nil && releaseErr == nil {
		return fault
	}
	return errors.Join(fault, dockerErr, cleanupErr, releaseErr)
}

// installDockerCleanupError wraps a failed pre-manifest Docker rollback so
// the inconsistency is visible and actionable: it names the compose
// project and stack path so an operator can inspect the leftover
// containers, volumes, or networks (PRD §18 fail-closed on uncertain
// state). The cause chain is preserved so the original docker-layer error
// stays discoverable with errors.Is.
func installDockerCleanupError(composeProject, stackPath string, cause error) error {
	return types.WrapError(
		types.ErrCodeGeneric,
		fmt.Sprintf(
			"install rollback could not remove all docker resources for project %s at stack %s",
			composeProject,
			stackPath,
		),
		"inspect the named compose project and stack, then remove leftover containers, volumes, or networks manually",
		cause,
	)
}

// cleanupFreshInstallArtifacts removes exactly the artifacts a fresh
// install writes: the rendered files, the seeded .env.user, the .wdm.lock
// created at flock acquisition, the nested additional-file parent
// directories, and the created stack directory itself. Every file removal is contained to
// the stack root via [security.EnsureWithinRoot]; directories use
// [os.Remove] (which refuses non-empty directories) so user-dropped
// content can never be deleted, only reported as a leftover. Missing
// artifacts are skipped — cleanup may run after a fault at any point in
// the write sequence.
func cleanupFreshInstallArtifacts(plan *installPlan) error {
	stackRoot := filepath.Clean(plan.stackPath)
	writes, err := installFileWrites(plan)
	if err != nil {
		return installCleanupError(stackRoot, err)
	}

	var faults []error
	seenDirs := map[string]struct{}{}
	var relDirs []string
	for _, write := range writes {
		if err := removeFreshInstallFile(stackRoot, write.path); err != nil {
			faults = append(faults, err)
		}
		rel, relErr := filepath.Rel(stackRoot, filepath.Clean(write.path))
		if relErr != nil {
			continue
		}
		for dir := filepath.Dir(rel); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
			if _, ok := seenDirs[dir]; ok {
				break
			}
			seenDirs[dir] = struct{}{}
			relDirs = append(relDirs, dir)
		}
	}
	if err := removeFreshInstallFile(stackRoot, filepath.Join(stackRoot, installLockFilename)); err != nil {
		faults = append(faults, err)
	}
	// The seeded .env.user is not in installFileWrites; remove it too so
	// the sad-path stack-dir removal does not trip on a leftover file.
	if err := removeFreshInstallFile(stackRoot, filepath.Join(stackRoot, installEnvUserFilename)); err != nil {
		faults = append(faults, err)
	}

	// Deepest directories first so emptied parents become removable.
	sort.Slice(relDirs, func(i, j int) bool {
		depthI := strings.Count(relDirs[i], string(filepath.Separator))
		depthJ := strings.Count(relDirs[j], string(filepath.Separator))
		if depthI != depthJ {
			return depthI > depthJ
		}
		return relDirs[i] > relDirs[j]
	})
	for _, dir := range relDirs {
		if err := removeFreshInstallDir(filepath.Join(stackRoot, dir)); err != nil {
			faults = append(faults, err)
		}
	}
	if err := removeFreshInstallDir(stackRoot); err != nil {
		faults = append(faults, err)
	}

	if len(faults) > 0 {
		return installCleanupError(stackRoot, errors.Join(faults...))
	}
	return nil
}

func removeFreshInstallFile(stackRoot, path string) error {
	if err := security.EnsureWithinRoot(stackRoot, filepath.Clean(path)); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func removeFreshInstallDir(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func installCleanupError(stackPath string, cause error) error {
	return types.WrapError(
		types.ErrCodeGeneric,
		"install cleanup could not remove all partial files",
		fmt.Sprintf("inspect %s and remove leftover install files manually", stackPath),
		cause,
	)
}

// confirmInstallDeployment asks the Confirmer to authorize deployment
// immediately before protocol step 5, after all rendered bytes are
// exposed and validated (PRD §17 step 11). A nil
// confirmer refuses to proceed past the confirmation step per the
// pkg/engine contract; a decline maps to [types.ErrCodeUserCanceled]
// and stops the install before any network creation or deployment.
func confirmInstallDeployment(
	ctx context.Context,
	confirmer types.Confirmer,
	plan *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required before deployment",
			"pass a confirmer that can authorize docker compose deployment",
		)
	}
	if onProgress != nil {
		onProgress(types.StepInstallConfirm, 45, "confirming deployment")
	}

	confirmed, err := confirmer.Confirm(ctx, installConfirmation(plan))
	if err != nil {
		return fmt.Errorf("core.install: confirming deployment: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"install canceled before deployment",
			"re-run the install and confirm the deployment prompt",
		)
	}
	return nil
}

// installConfirmation assembles the consequence payload shown before
// deployment: the stack identity
// plus the localhost ports that will bind, the volumes the deployment
// will create, and the Docker networks it will ensure. When the plan binds
// any public port (catalog port.public:true → 0.0.0.0), a delimited PUBLIC
// PORT WARNING block names each public port and protocol so the operator
// confirms reachable-from-any-network exposure deliberately (PRD §11.1(e)).
// Plans with no public port carry no warning noise. When the app declares an
// enabled docker-socket-proxy, a delimited DOCKER SOCKET ACCESS WARNING block
// states whether the app can read or read-and-control the Docker host through
// the proxy so socket access is never confirmed silently (PRD §12.1). The
// payload carries no secret values.
func installConfirmation(plan *installPlan) types.Confirmation {
	lines := []string{
		"app: " + plan.app.AppID,
		"stack path: " + plan.stackPath,
		"compose project: " + plan.composeProject,
	}
	for _, binding := range plan.localPorts {
		lines = append(lines, fmt.Sprintf(
			"binds %s:%d/%s for service %s",
			binding.HostIP,
			binding.HostPort,
			binding.Protocol,
			binding.Service,
		))
	}
	for _, mount := range plan.rendered.VolumeMounts {
		lines = append(lines, "creates volume "+mount)
	}
	for _, network := range plan.app.Networks {
		line := "ensures docker network " + network.Name
		if network.Internal {
			line += " (internal)"
		}
		lines = append(lines, line)
	}
	lines = append(lines, publicPortWarningLines(plan.localPorts)...)
	lines = append(lines, socketProxyWarningLines(plan.app)...)
	return types.Confirmation{
		Kind:    "install_deploy",
		Title:   "deploy " + plan.app.AppID,
		Message: strings.Join(lines, "\n"),
	}
}

// publicPortWarningLines builds the delimited PUBLIC PORT WARNING block for
// every public bind in the plan (PRD §11.1(e)). The block fires uniformly for
// every public port — data ports included — so no public exposure is ever
// confirmed silently. It returns no lines when nothing binds publicly.
func publicPortWarningLines(bindings []types.PortBinding) []string {
	var warnings []string
	for _, binding := range bindings {
		if binding.HostIP != "0.0.0.0" {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s:%d/%s (service %s) is reachable from any network interface",
			binding.HostIP,
			binding.HostPort,
			binding.Protocol,
			binding.Service,
		))
	}
	if len(warnings) == 0 {
		return nil
	}
	block := []string{"", "PUBLIC PORT WARNING"}
	block = append(block, warnings...)
	return block
}

// containerPrivilegeDisclosureLines builds the delimited ELEVATED PRIVILEGE
// block surfaced on the install finish screen (PRD §12.2). It names each
// hardened service's re-added capabilities and sysctls (and privileged, which
// is always false) so no elevation above the cap_drop:ALL baseline ships
// silently. Services declaring no elevation are skipped, and the whole block is
// omitted when no service declares any — the four curated apps carry no
// ServiceHardening, so their finish screens stay unchanged.
func containerPrivilegeDisclosureLines(app catalog.App) []string {
	var lines []string
	for _, hardening := range app.ServiceHardening {
		var details []string
		if hardening.Capabilities != nil && len(hardening.Capabilities.Add) > 0 {
			caps := append([]string(nil), hardening.Capabilities.Add...)
			sort.Strings(caps)
			details = append(details, "capabilities "+strings.Join(caps, ", "))
		}
		if len(hardening.Sysctls) > 0 {
			sysctls := make([]string, 0, len(hardening.Sysctls))
			for _, sysctl := range hardening.Sysctls {
				sysctls = append(sysctls, sysctl.Name)
			}
			sort.Strings(sysctls)
			details = append(details, "sysctls "+strings.Join(sysctls, ", "))
		}
		if hardening.Privileged {
			details = append(details, "privileged")
		}
		if len(details) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf(
			"service %s: %s",
			hardening.Service,
			strings.Join(details, "; "),
		))
	}
	if len(lines) == 0 {
		return nil
	}
	sort.Strings(lines)
	block := []string{"", "ELEVATED PRIVILEGE"}
	block = append(block, lines...)
	return block
}

// ensureInstallNetworks pre-creates every catalog-declared Docker network
// before any compose subcommand runs (protocol step 4).
// [docker.EnsureNetworkReport] is idempotent: an existing network with a
// matching internal flag is skipped, and internal-flag drift surfaces
// unchanged as the locked [types.ErrCodeUsageValidation] mismatch error.
// Each network this call newly creates is recorded on
// cleanup.createdNetworks in creation order so a later pre-manifest
// failure can remove exactly those — and never a pre-existing or external
// network — in reverse order (PRD §9).
func ensureInstallNetworks(
	ctx context.Context,
	client docker.Client,
	plan *installPlan,
	cleanup *freshInstallDockerCleanup,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(plan.app.Networks) == 0 {
		return nil
	}
	if onProgress != nil {
		onProgress(types.StepInstallNetworkCreate, 55, "ensuring docker networks")
	}

	for _, network := range plan.app.Networks {
		spec := docker.NetworkSpec{
			Name:     network.Name,
			Internal: network.Internal,
			// Stamp the PRD §10 ownership labels on networks wdm creates. Only
			// newly-created networks are labeled; one reached through the
			// EnsureNetworkReport "already exists" path is NOT re-labeled.
			AppID: plan.app.AppID,
		}
		// A catalog IPAM block pins the subnet (and optional gateway) for
		// fixed addressing (PRD §9); nil IPAM leaves Docker's default bridge
		// addressing unchanged.
		if network.IPAM != nil {
			spec.Subnet = network.IPAM.Subnet
			spec.Gateway = network.IPAM.Gateway
		}
		created, err := docker.EnsureNetworkReport(ctx, client, spec)
		if err != nil {
			return err
		}
		// The update path passes a nil cleanup: it has its own snapshot
		// rollback and never removes networks here.
		if created && cleanup != nil {
			cleanup.createdNetworks = append(cleanup.createdNetworks, spec.Name)
		}
	}
	return nil
}

// deployInstallStack runs `docker compose up -d` against the written
// stack files (protocol step 5). Install never runs an explicit pull —
// `up -d` pulls missing images, and reserves the
// pull+force-recreate pair for the update path. Client errors
// propagate unchanged so the internal/docker error-code mapping stays
// authoritative.
func deployInstallStack(
	ctx context.Context,
	client docker.Client,
	plan *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepInstallDeploy, 65, "deploying docker compose stack")
	}

	project, err := installComposeProject(plan)
	if err != nil {
		return err
	}
	return docker.ComposeUp(ctx, client, project, docker.ComposeUpOptions{})
}

func installComposeProject(plan *installPlan) (docker.ComposeProject, error) {
	composePath, err := security.SafeJoin(plan.stackPath, installComposeFilename)
	if err != nil {
		return docker.ComposeProject{}, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	envPath, err := security.SafeJoin(plan.stackPath, installEnvFilename)
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
		ProjectName: plan.composeProject,
	}, nil
}

// captureInstallImagePins resolves the catalog image pins into the
// .wdm.lock shape, capturing each image's sha256 digest
// opportunistically after deployment.
// [docker.InspectImageDigest] returns an empty digest for ordinary
// absence or inspect failure — never a fault — while context
// cancellation and invalid references propagate.
func captureInstallImagePins(
	ctx context.Context,
	client docker.Client,
	plan *installPlan,
) ([]state.ImagePin, error) {
	if len(plan.app.ImagePins) == 0 {
		return nil, nil
	}
	pins := make([]state.ImagePin, 0, len(plan.app.ImagePins))
	for _, pin := range plan.app.ImagePins {
		digest, err := docker.InspectImageDigest(ctx, client, pin.Image+":"+pin.Tag)
		if err != nil {
			return nil, err
		}
		pins = append(pins, state.ImagePin{
			Service: pin.Service,
			Image:   pin.Image,
			Tag:     pin.Tag,
			Digest:  digest,
		})
	}
	return pins, nil
}

// writeInstallLockManifest persists the full.wdm.lock manifest
// through the held per-stack flock fd (protocol step 6 — the commit
// point, PRD §30). [state.StackLockHandle.Write] uses the
// in-place truncate/seek/write/fsync pattern; tmp+rename is forbidden
// for lock files because rename would detach the flocked inode.
// BackupHistory stays empty: protocol step 3 is a no-op for fresh
// installs, so there is no snapshot path to append.
func (e *Engine) writeInstallLockManifest(
	ctx context.Context,
	plan *installPlan,
	stackHandle *state.StackLockHandle,
	pins []state.ImagePin,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepInstallLockUpdate, 80, "updating stack lock manifest")
	}

	localPorts := make([]int, 0, len(plan.localPorts))
	for _, binding := range plan.localPorts {
		localPorts = append(localPorts, binding.HostPort)
	}
	now := time.Now().UTC()
	lock := state.StackLock{
		SchemaVersion:     1,
		AppID:             plan.app.AppID,
		TemplateName:      plan.app.TemplateName,
		TemplateVersion:   plan.app.TemplateVersion,
		CatalogChannel:    plan.catalogChannel,
		CatalogVersion:    plan.catalogVersion,
		StackPath:         plan.stackPath,
		SelectedDomain:    plan.selectedDomain,
		LocalPorts:        localPorts,
		ComposeProject:    plan.composeProject,
		ImagePins:         pins,
		CompletedServices: append([]string(nil), plan.app.CompletedServices...),
		GeneratedFields:   append([]string(nil), plan.generatedFields...),
		LastSuccessfulOperation: &types.Operation{
			Kind:       "install",
			At:         now,
			WDMVersion: e.version,
		},
		RecommendedResources: plan.recommendedResources,
	}
	if err := stackHandle.Write(lock); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack lock manifest could not be written",
			"check stack directory permissions and retry",
			err,
		)
	}
	return nil
}

// socketProxyWarningLines builds the delimited DOCKER SOCKET ACCESS WARNING block
// appended to the install confirmation (PRD §12.1): the operator must confirm
// what the app can do with the Docker host before install proceeds. It branches
// on whether allowed_api includes the POST write/control switch and returns no
// lines when the app declares no enabled socket proxy.
func socketProxyWarningLines(app catalog.App) []string {
	proxy := app.SocketProxy
	if proxy == nil || !proxy.Enabled {
		return nil
	}
	var detail string
	if socketProxyAllowsControl(proxy.AllowedAPI) {
		detail = fmt.Sprintf(
			"%s can READ AND CONTROL the Docker host (create, start, stop, and remove containers) through the %s socket proxy",
			app.AppID,
			proxy.Service,
		)
	} else {
		detail = fmt.Sprintf(
			"%s can READ Docker host state (containers, images, networks, volumes) through the %s socket proxy",
			app.AppID,
			proxy.Service,
		)
	}
	return []string{"", "DOCKER SOCKET ACCESS WARNING", detail}
}
