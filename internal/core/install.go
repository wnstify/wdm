package core

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

const (
	installHostMemoryReserveBytes  = uint64(1024 * 1024 * 1024)
	installComposeFilename         = "docker-compose.yml"
	installComposeOverrideFilename = "docker-compose.override.yml"
	installEnvFilename             = ".env"
	installEnvUserFilename         = ".env.user"
	installLockFilename            = ".wdm.lock"
	installComposeFileMode         = os.FileMode(0o644)
)

// installRollbackTimeout bounds the pre-manifest Docker rollback so it
// cannot stall indefinitely. It is generous enough for a multi-service
// `compose down` graceful stop plus the project-labeled volume and
// network removals, yet finite. The rollback context is derived with
// [context.WithoutCancel] so a canceled install (Ctrl-C mid-deploy) still
// reclaims its resources instead of orphaning them; this timeout is the
// only deadline that then applies.
const installRollbackTimeout = 60 * time.Second

type installPlan struct {
	app                  catalog.App
	stackPath            string
	composeProject       string
	catalogChannel       string
	catalogVersion       string
	selectedDomain       string
	placeholders         []render.Placeholder
	resolvedValues       map[string]string
	generatedFields      []string
	generatedValues      []string
	reusedSecretValues   []string
	localPorts           []types.PortBinding
	rendered             render.RenderedStack
	guidance             *types.PostInstallGuidance
	recommendedResources *state.RecommendedResources

	// shownCredentials holds argon2id one-time plaintexts surfaced to the
	// operator exactly once on the finish screen. These are deliberately
	// kept OUT of resolvedValues, generatedValues, and the .env: only the
	// $$-escaped PHC hash is persisted and redacted, never the plaintext.
	shownCredentials []types.GeneratedCredential

	// probePort verifies a single planned host binding is free. Carried on
	// the plan (sourced from the Engine) so both the planning probe and the
	// pre-deploy re-check go through one injectable seam. Default is
	// [checkPortAvailable]; tests replace it so a real net.Listen on a
	// catalog-fixed public port (which a localhost-port rewrite cannot make
	// ephemeral) does not flake on a busy host.
	probePort func(context.Context, types.PortBinding) error

	// portOverrides remaps conflicting single loopback host ports
	// (oldHostPort → newHostPort) during port planning, before the probe
	// (ADR 0004). Sourced from [types.InstallRequest.PortOverrides].
	portOverrides map[int]int

	// frozen marks the end of the plan's producing region (issue #120). A
	// plan is mutated only while it is built — placeholders, ports,
	// resources, secrets, and the render output — after which [freeze] flips
	// this and every later phase (validate/write/deploy/verify) is expected
	// to consume the plan read-only. The flag is a boundary marker, not an
	// enforced lock: it does not block field writes, it fail-closes a
	// producer that re-runs over an already-built plan. The producing region
	// binds the redactor over the completed generated-secret set and renders
	// before freezing, so the load-bearing order (all secrets generated ->
	// redactor bound -> render -> freeze) holds by construction.
	frozen bool
}

// freeze closes the plan's producing region. It is the single transition
// from "being built" to "read-only"; calling it twice is a producer-logic
// bug and fails closed rather than silently re-freezing, so a future edit
// that runs a producer over an already-frozen plan is caught at the seam.
func (p *installPlan) freeze() error {
	if p.frozen {
		return types.NewError(
			types.ErrCodeGeneric,
			"stack plan was already produced",
			"this is a wdm bug: report it with the failing operation",
		)
	}
	p.frozen = true
	return nil
}

type timezoneLookupDeps struct {
	LookupEnv    func(string) (string, bool)
	ReadFile     func(string) ([]byte, error)
	ReadLink     func(string) (string, error)
	LoadLocation func(string) (*time.Location, error)
}

var defaultTimezoneLookupDeps = timezoneLookupDeps{
	LookupEnv:    os.LookupEnv,
	ReadFile:     os.ReadFile,
	ReadLink:     os.Readlink,
	LoadLocation: time.LoadLocation,
}

// Install creates and starts a new managed stack (PRD §17). It plans
// catalog selection, stack path, placeholder values, localhost ports,
// and resource limits; generates install secrets; renders stack
// artifacts and post-install guidance; validates the rendered Compose
// config via docker compose config against a private tempdir copy before
// any stack file is exposed (PRD §13); writes docker-compose.yml, .env,
// and additional_files under the per-stack flock; asks the Confirmer to
// authorize deployment with the ports, volumes, and networks it will
// touch (PRD §17 step 11); pre-creates catalog-declared Docker networks;
// re-checks localhost port availability immediately before deployment;
// deploys via `docker compose up -d`; captures image digests
// opportunistically; persists the .wdm.lock manifest through the held
// per-stack flock fd; verifies post-deploy status by Compose project and
// wdm labels (PRD §18); and returns the structured
// [types.InstallResult] with Compose project, ports, status, and
// Pangolin guidance (PRD §16).
// The per-stack flock spans file write → confirm → networks → deploy
// → manifest write → release. A fault
// after stack files are written and before the manifest fsync is
// durable removes only this install's Docker resources — a safe
// `docker compose down` without -v, the project-labeled named volumes,
// and the networks this install newly created in reverse order — then
// removes the partial fresh-install files (compose, env, additional
// files, the empty .wdm.lock, and the created stack directory) per
// protocol step 4's fresh-install clause. The Docker rollback removes
// nothing it did not create: a pre-existing network is never removed,
// and a confirmation decline or a pre-network failure runs the
// file-only cleanup with no Docker call.
// After the manifest fsync the operation is durable:
// status-verification problems mark the result needs-attention rather
// than failing the install, and no later failure removes files. The
// protocol step 7 backup-retention pass is skipped — a no-op for fresh
// installs, which refuse existing stack paths so no .wdm-backups can
// exist.
// A nil confirmer refuses to proceed past the confirmation step per
// the pkg/engine contract; a decline surfaces
// [types.ErrCodeUserCanceled] and falls through to the fresh-install
// sad-path cleanup before any network creation or deployment.
func (e *Engine) Install(ctx context.Context, req types.InstallRequest, onProgress types.ProgressFn, confirmer types.Confirmer) (*types.InstallResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	// Start with structural-only redaction; once secrets are minted in
	// renderInstall the logger is rebound to also scrub those literals
	// (PRD §24 defense-in-depth). The op-start line and every failure
	// point below log NON-secret facts only — never a secret value.
	lg := e.newOpLogger(e.installLogger(nil), "install")
	lg.start(ctx, req.AppID)

	handle, err := e.acquireRuntimeLock(ctx, "install")
	if err != nil {
		lg.failure(ctx, req.AppID, "", "acquire_runtime_lock", err)
		return nil, err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless
	lg.step(ctx, "runtime lock acquired")

	probe := e.detectHostResources
	if probe == nil {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"host resource probe is required",
			"construct the engine with a host resource probe",
		)
	}
	host, err := probe()
	if err != nil {
		lg.failure(ctx, req.AppID, "", "detect_host_resources", err)
		return nil, err
	}
	lg.step(ctx, "host resources detected",
		slog.Int("cpu_cores", host.CPUCores),
		slog.Uint64("total_memory_bytes", host.TotalMemoryBytes),
	)

	plan, err := e.planInstall(ctx, req, host, onProgress, defaultTimezoneLookupDeps)
	if err != nil {
		lg.failure(ctx, req.AppID, "", "plan_install", err)
		return nil, err
	}
	lg.step(ctx, "install planned",
		slog.String("app", plan.app.AppID),
		slog.String("stack_path", plan.stackPath),
		slog.String("compose_project", plan.composeProject),
	)
	if err := e.renderInstall(ctx, plan, onProgress); err != nil {
		lg.failure(ctx, plan.app.AppID, plan.stackPath, "render_install", err)
		return nil, err
	}
	// Rebind to a secret-aware logger now that generated values exist, so
	// any later line is scrubbed of those literals too (PRD §24 rule 2).
	// Register every generated secret: the persisted generatedValues plus the
	// argon2id one-time plaintexts, which live only in shownCredentials.
	// generated_secret_fields logs the placeholder NAMES, never values.
	lg = e.newOpLogger(e.installLogger(installRedactionSecrets(plan)), "install")
	lg.step(ctx, "stack rendered",
		slog.Any("generated_secret_fields", plan.generatedFields),
	)
	dockerClient, err := e.installDockerClient(plan)
	if err != nil {
		lg.failure(ctx, plan.app.AppID, plan.stackPath, "build_docker_client", err)
		return nil, err
	}
	lg.debug(ctx, "compose config validate",
		slog.String("command", "docker compose config --quiet"),
	)
	if err := validateInstallComposeConfig(ctx, dockerClient, plan, onProgress); err != nil {
		lg.failure(ctx, plan.app.AppID, plan.stackPath, "validate_compose_config", err)
		return nil, err
	}
	lg.step(ctx, "compose config validated")

	if req.Force {
		if err := e.recoverOrphanedStack(ctx, dockerClient, plan.stackPath, plan.composeProject); err != nil {
			lg.failure(ctx, plan.app.AppID, plan.stackPath, "recover_orphaned_stack", err)
			return nil, err
		}
	}

	stackHandle, err := writeInstallFiles(ctx, plan, onProgress)
	if err != nil {
		lg.failure(ctx, plan.app.AppID, plan.stackPath, "write_install_files", err)
		return nil, err
	}
	lg.step(ctx, "stack files written")
	composeProject, err := installComposeProject(plan)
	if err != nil {
		lg.failure(ctx, plan.app.AppID, plan.stackPath, "resolve_compose_project", err)
		return nil, err
	}
	cleanup := &freshInstallDockerCleanup{client: dockerClient, project: composeProject}
	// Seed an empty .env.user (create-if-missing, 0600) after the stack
	// files exist but before `compose up`, so the env_file overlay
	// resolves on deploy. A fault here unwinds the fresh install.
	if _, err := ensureUserEnvFile(plan.stackPath); err != nil {
		lg.failure(ctx, plan.app.AppID, plan.stackPath, "ensure_user_env", err)
		return nil, failFreshInstall(ctx, err, plan, stackHandle, cleanup)
	}
	lg.step(ctx, "user env seeded")
	lg.debug(ctx, "deploy stack",
		slog.String("command", "docker compose up -d"),
		slog.String("compose_project", plan.composeProject),
	)
	if err := e.commitInstall(ctx, dockerClient, plan, stackHandle, confirmer, cleanup, onProgress); err != nil {
		// Pre-commit fault: the manifest is not durable yet, so the
		// protocol step 7 sad path removes the partial fresh-install
		// files while the per-stack flock is held, after rolling back
		// only the Docker resources this install created.
		lg.failure(ctx, plan.app.AppID, plan.stackPath, "commit_install", err)
		return nil, failFreshInstall(ctx, err, plan, stackHandle, cleanup)
	}
	lg.step(ctx, "stack deployed and manifest committed")

	status, err := verifyInstallStatus(ctx, dockerClient, plan, onProgress)
	if err != nil {
		// Post-commit cancellation: the manifest is durable, files
		// stay in place, only the flock is released.
		lg.failure(ctx, plan.app.AppID, plan.stackPath, "verify_install_status", err)
		if releaseErr := stackHandle.Release(); releaseErr != nil {
			return nil, errors.Join(err, releaseErr)
		}
		return nil, err
	}
	if err := stackHandle.Release(); err != nil {
		lg.failure(ctx, plan.app.AppID, plan.stackPath, "release_stack_lock", err)
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"stack lock could not be released",
			"check stack directory permissions and retry",
			err,
		)
	}
	lg.success(ctx, plan.app.AppID, plan.stackPath)
	return buildInstallResult(plan, status), nil
}

// installAdditionalFileKind and installConfigArtifactKind name the two
// catalog-declared artifact kinds the dest tracker guards, so its
// diagnostics report which kind a collision belongs to.
const (
	installAdditionalFileKind = "additional file"
	installConfigArtifactKind = "config artifact"
)

func serviceKey(service string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToUpper(service) {
		isAllowed := (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if isAllowed {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func usageValidationError(message, hint string, cause error) error {
	if cause == nil {
		return types.NewError(types.ErrCodeUsageValidation, message, hint)
	}
	return types.WrapError(types.ErrCodeUsageValidation, message, hint, cause)
}

func catalogVerificationError(message, hint string, cause error) error {
	if cause == nil {
		return types.NewError(types.ErrCodeVerificationFailed, message, hint)
	}
	return types.WrapError(types.ErrCodeVerificationFailed, message, hint, cause)
}

func genericError(message, hint string, cause error) error {
	if cause == nil {
		return types.NewError(types.ErrCodeGeneric, message, hint)
	}
	return types.WrapError(types.ErrCodeGeneric, message, hint, cause)
}
