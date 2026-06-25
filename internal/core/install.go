package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/internal/system"
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

func (e *Engine) planInstall(
	ctx context.Context,
	req types.InstallRequest,
	host system.HostResources,
	onProgress types.ProgressFn,
	tzDeps timezoneLookupDeps,
) (*installPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.AppID == "" {
		return nil, usageValidationError(
			"app id is required",
			"choose an app from the catalog",
			nil,
		)
	}
	if onProgress != nil {
		onProgress(types.StepInstallPlanning, 5, "planning install")
	}

	cat, err := e.loadInstallCatalog(ctx)
	if err != nil {
		return nil, err
	}
	app, err := selectCatalogApp(cat, req.AppID)
	if err != nil {
		return nil, err
	}

	stackPath, err := e.planInstallStackPath(req, app)
	if err != nil {
		return nil, err
	}
	probePort := e.probePort
	if probePort == nil {
		probePort = checkPortAvailable
	}
	plan := &installPlan{
		app:            app,
		stackPath:      stackPath,
		composeProject: "wdm-" + app.AppID,
		catalogChannel: e.settings.CatalogChannel,
		catalogVersion: cat.GeneratedAt.UTC().Format(time.RFC3339),
		resolvedValues: map[string]string{},
		localPorts:     []types.PortBinding{},
		probePort:      probePort,
	}

	if err := plan.planPlaceholders(req, e.settings.Timezone, tzDeps); err != nil {
		return nil, err
	}
	if err := plan.planPorts(ctx); err != nil {
		return nil, err
	}
	if err := plan.planResources(req, host, onProgress); err != nil {
		return nil, err
	}
	return plan, nil
}

func (e *Engine) loadInstallCatalog(ctx context.Context) (*catalog.Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	channel := e.settings.CatalogChannel
	if channel == "" || !fs.ValidPath(channel) || strings.Contains(channel, "/") || channel == "." {
		return nil, usageValidationError(
			"catalog channel is invalid",
			"set catalog_channel to stable in config.toml",
			fmt.Errorf("invalid catalog channel %q", channel),
		)
	}

	catalogPath := path.Join(channel, "catalog.yaml")
	raw, err := fs.ReadFile(e.installCatalogFS(), catalogPath)
	if err != nil {
		return nil, catalogVerificationError(
			"catalog could not be read",
			"install the stable catalog before running apps install",
			err,
		)
	}
	cat, err := catalog.LoadCatalogBytes(ctx, raw)
	if err != nil {
		return nil, catalogVerificationError(
			"catalog could not be verified",
			"refresh the catalog and retry",
			err,
		)
	}
	return cat, nil
}

func (e *Engine) installCatalogFS() fs.FS {
	if e.catalog != nil {
		return e.catalog
	}
	return os.DirFS(filepath.Join(e.dataDir, "catalogs"))
}

func (e *Engine) renderInstall(
	ctx context.Context,
	plan *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepInstallRender, 25, "rendering install")
	}
	// Generate-then-bind is one inseparable step: the redactor is built from
	// the generated-secret set the same call mints, so it cannot be bound
	// before generation completes (issue #120 ordering invariant).
	redactor, err := plan.generateSecretsAndBindRedactor(e.generateSecret, e.generateArgon2idCredential)
	if err != nil {
		return err
	}

	input, err := e.installRenderInput(ctx, plan)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return redactedVerificationError(
			redactor,
			"install templates could not be loaded",
			"refresh the catalog and retry",
			err,
		)
	}
	envStack, err := render.RenderEnv(input)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"env template could not be rendered",
			"refresh the catalog and retry",
			err,
		)
	}
	composeStack, err := render.RenderLabels(input)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"compose template could not be rendered",
			"refresh the catalog and retry",
			err,
		)
	}

	plan.rendered = render.RenderedStack{
		ComposeBytes:    composeStack.ComposeBytes,
		EnvBytes:        envStack.EnvBytes,
		AdditionalFiles: composeStack.AdditionalFiles,
		ConfigArtifacts: composeStack.ConfigArtifacts,
		ServiceLabels:   composeStack.ServiceLabels,
		VolumeMounts:    composeStack.VolumeMounts,
	}

	guidance, err := buildInstallGuidance(plan)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"install guidance could not be rendered",
			"refresh the catalog and retry",
			err,
		)
	}
	plan.guidance = guidance
	if err := verifyImagePinsMatchTemplate(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifyPublicBindsMatchCatalog(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifyContainerPrivilegeMatchCatalog(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifySocketPolicyMatchCatalog(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifyHostModuleMountMatchCatalog(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifyNetworkIPAMMatchCatalog(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifyCompletedServicesMatchCatalog(plan.app, plan.rendered.ServiceLabels); err != nil {
		return err
	}
	// Generated secrets plus sensitive --set values are both forbidden from
	// non-secret artifacts; sensitive values fail closed against inline
	// rendering exactly as on the update and reconfigure paths. argon2id
	// one-time plaintexts are deliberately excluded here (they never enter
	// generatedValues), keeping the leak-check scope unchanged for every app.
	leakSecrets := append(slices.Clone(plan.generatedValues), sensitiveSetValues(plan)...)
	if err := verifyRenderedNonSecretArtifacts(redactor, leakSecrets, plan.rendered, guidance); err != nil {
		return err
	}
	// End of the producing region: secrets are minted, the redactor is
	// bound, the stack is rendered and leak-checked. Freeze so every later
	// install phase consumes a read-only plan (issue #120).
	return plan.freeze()
}

// buildInstallGuidance assembles the post-install guidance from the
// catalog's structured fields. The local
// target URL comes from local_target_url_template rendered against
// the resolved placeholder map, falling back to the first declared
// port as http://127.0.0.1:<port> per the confirmation rules. Pangolin guidance
// is omitted when the catalog entry carries no guidance content, so
// JSON consumers see the field dropped, not empty.
func buildInstallGuidance(plan *installPlan) (*types.PostInstallGuidance, error) {
	localTargetURL, err := renderInstallLocalTargetURL(plan)
	if err != nil {
		return nil, err
	}

	firstRunNotes := append([]string(nil), plan.app.FirstRunNotes...)
	firstRunNotes = append(firstRunNotes, containerPrivilegeDisclosureLines(plan.app)...)

	guidance := &types.PostInstallGuidance{
		LocalTargetURL: localTargetURL,
		FirstRunNotes:  firstRunNotes,
	}
	// One-time credential plaintexts ride on the guidance struct for the
	// human finish screen only. They are deliberately NOT fed to
	// guidanceText() (the non-secret leak check), so the plaintext is never
	// inspected against the rendered artifacts, and the json:"-" tag keeps
	// them out of every JSON envelope (PRD §24).
	if len(plan.shownCredentials) > 0 {
		guidance.GeneratedCredentials = append([]types.GeneratedCredential(nil), plan.shownCredentials...)
	}
	pangolin := plan.app.PangolinGuidance
	if pangolin.TargetURL != "" || pangolin.RecommendedSubdomain != "" || len(pangolin.Notes) > 0 {
		guidance.Pangolin = &types.PangolinGuidance{
			TargetURL:            pangolin.TargetURL,
			RecommendedSubdomain: pangolin.RecommendedSubdomain,
			Notes:                append([]string(nil), pangolin.Notes...),
		}
	}
	return guidance, nil
}

func renderInstallLocalTargetURL(plan *installPlan) (string, error) {
	if plan.app.LocalTargetURLTemplate == "" {
		if len(plan.localPorts) == 0 {
			return "", nil
		}
		return fmt.Sprintf("http://127.0.0.1:%d", plan.localPorts[0].HostPort), nil
	}

	tmpl, err := template.New("local-target-url").
		Option("missingkey=error").
		Parse(plan.app.LocalTargetURLTemplate)
	if err != nil {
		return "", fmt.Errorf("parse local_target_url_template: %w", err)
	}
	var rendered strings.Builder
	if err := tmpl.Execute(&rendered, plan.resolvedValues); err != nil {
		return "", fmt.Errorf("render local_target_url_template: %w", err)
	}
	return rendered.String(), nil
}

// guidanceText flattens the guidance strings for the non-secret
// artifact verification pass. Guidance reaches terminal output, JSON
// envelopes, and logs, so a generated secret must never appear in it.
func guidanceText(guidance *types.PostInstallGuidance) []byte {
	if guidance == nil {
		return nil
	}
	parts := []string{guidance.LocalTargetURL}
	parts = append(parts, guidance.FirstRunNotes...)
	if guidance.Pangolin != nil {
		parts = append(parts, guidance.Pangolin.TargetURL, guidance.Pangolin.RecommendedSubdomain)
		parts = append(parts, guidance.Pangolin.Notes...)
	}
	return []byte(strings.Join(parts, "\n"))
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

// validateInstallComposeConfig runs `docker compose config --quiet`
// against a private 0o700 tempdir copy of the complete rendered artifact
// set (PRD §13), so validation happens
// before any byte is exposed under the stack path. The secret-bearing
// copies keep secret-file mode through the same atomic write path as the
// real stack write; the workspace is removed best-effort on return.
// Client errors propagate unchanged so the internal/docker error-code
// mapping stays authoritative.
func validateInstallComposeConfig(
	ctx context.Context,
	client docker.Client,
	plan *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepInstallComposeValidate, 30, "validating compose config")
	}
	return validateRenderedComposeConfig(ctx, client, &plan.rendered)
}

// validateRenderedComposeConfig runs `docker compose config --quiet`
// against a private 0o700 tempdir copy of the COMPLETE deployed artifact
// set — docker-compose.yml, .env, every additional_file, and every
// config_generation artifact (PRD §13). Staging the full set (not just
// compose + env) keeps the validated layout faithful to what deploys: an
// `env_file:` pointing at a rendered config artifact, for example,
// resolves against the temp project dir exactly as it will against the
// stack. The same [renderedArtifactWrites] enumerator backs both this
// staging and the real install writer, so the two cannot drift.
// Both the install pre-exposure validation and the update post-rewrite
// validation (PRD §20 step 9) share it: each validates the in-memory
// rendered bytes hermetically rather than the live stack files, so the
// secret-bearing copies never outlive the call. Secret-bearing copies
// keep secret-file mode through the same atomic write path as the real
// stack write; the workspace is removed best-effort on return. Client
// errors propagate unchanged so the internal/docker error-code mapping
// stays authoritative.
func validateRenderedComposeConfig(
	ctx context.Context,
	client docker.Client,
	rendered *render.RenderedStack,
) error {
	if rendered == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"rendered stack is required",
			"render the stack before validating compose config",
		)
	}

	tempDir, err := os.MkdirTemp("", "wdm-compose-validate-*")
	if err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"compose validation workspace could not be created",
			"check temp directory permissions and retry",
			err,
		)
	}
	defer os.RemoveAll(tempDir) //nolint:errcheck // best-effort cleanup; the 0o700 workspace and any 0o600 copies stay private regardless

	// Stage exactly what the real writer deploys: compose + .env first,
	// then the rendered additional_files and config_artifacts rooted at
	// the temp dir via the shared enumerator. The whole set is removed
	// with the workspace on return.
	composePath := filepath.Join(tempDir, installComposeFilename)
	staged := []installFileWrite{
		{path: composePath, data: rendered.ComposeBytes, mode: installComposeFileMode},
		{path: filepath.Join(tempDir, installEnvFilename), data: rendered.EnvBytes, mode: security.SecretFileMode},
		// Stage an empty .env.user so a template's env_file: [.env.user]
		// resolves during `compose config`. It is user-owned, not a
		// rendered artifact, so it is seeded on disk at install (T2) and
		// staged empty here for pre-write validation only.
		{path: filepath.Join(tempDir, installEnvUserFilename), data: []byte{}, mode: security.SecretFileMode},
	}
	artifactWrites, err := renderedArtifactWrites(rendered, tempDir)
	if err != nil {
		return err
	}
	staged = append(staged, artifactWrites...)

	for _, write := range staged {
		// Create nested parents (e.g. init-scripts/) at 0o700 inside the
		// 0o700 workspace before the atomic write, so a secret-mode copy's
		// parent is never group/world-writable.
		if err := os.MkdirAll(filepath.Dir(write.path), 0o700); err != nil {
			return types.WrapError(
				types.ErrCodeGeneric,
				"compose validation copy could not be written",
				"check temp directory permissions and retry",
				err,
			)
		}
		if err := state.WriteFileAtomic(write.path, write.data, write.mode); err != nil {
			return types.WrapError(
				types.ErrCodeGeneric,
				"compose validation copy could not be written",
				"check temp directory permissions and retry",
				err,
			)
		}
	}
	return docker.ValidateComposeConfig(ctx, client, tempDir, composePath)
}

type installFileWrite struct {
	path string
	data []byte
	mode os.FileMode
}

// writeInstallFiles refuses existing managed or unmanaged stack
// paths, creates the fresh stack directory, acquires the per-stack
// .wdm.lock flock, and writes the rendered artifacts atomically. On
// success it returns the HELD lock handle so the caller keeps the
// flock across confirm → networks → deploy → manifest write → release
// A fault after the stack directory is
// created triggers the fresh-install sad-path cleanup before the error
// returns.
func writeInstallFiles(ctx context.Context, plan *installPlan, onProgress types.ProgressFn) (*state.StackLockHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	writes, err := installFileWrites(plan)
	if err != nil {
		return nil, err
	}

	stackExists, err := installStackPathExists(plan.stackPath)
	if err != nil {
		return nil, err
	}
	if stackExists {
		handle, err := acquireExistingInstallStackLock(ctx, plan.stackPath)
		if err != nil {
			return nil, err
		}
		defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

		if handle.Lock() != nil {
			return nil, managedStackExistsError(plan.stackPath)
		}
		return nil, stackPathExistsError(plan.stackPath)
	}

	if err := createInstallStackDir(plan.stackPath); err != nil {
		return nil, err
	}

	handle, err := acquireInstallStackLock(ctx, plan.stackPath)
	if err != nil {
		return nil, failFreshInstall(ctx, err, plan, nil, nil)
	}
	if handle.Lock() != nil {
		// A foreign manifest appeared between mkdir and flock. Refuse
		// WITHOUT cleanup: removing another actor's manifest would
		// destroy state wdm does not own.
		if releaseErr := handle.Release(); releaseErr != nil {
			return nil, errors.Join(managedStackExistsError(plan.stackPath), releaseErr)
		}
		return nil, managedStackExistsError(plan.stackPath)
	}

	if onProgress != nil {
		onProgress(types.StepInstallWriteFiles, 35, "writing install files")
	}
	for _, write := range writes {
		if err := validateInstallWritePath(plan.stackPath, write.path); err != nil {
			return nil, failFreshInstall(ctx, usageValidationError(
				"install file path is unsafe",
				"remove symlinks from the stack path and retry",
				err,
			), plan, handle, nil)
		}
		if err := state.WriteFileAtomic(write.path, write.data, write.mode); err != nil {
			return nil, failFreshInstall(ctx, types.WrapError(
				types.ErrCodeGeneric,
				"install files could not be written",
				"check stack directory permissions and retry",
				err,
			), plan, handle, nil)
		}
	}
	return handle, nil
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

// verifyInstallStatus inspects the deployed containers by Compose
// project and wdm labels and fuses the install-time PRD §18
// conditions (missing container, unexpected exit, restart loop,
// unhealthy, port mismatch) into a [types.AppStatus]. The pass runs
// AFTER the protocol step 6 commit point, so a failed inspection never
// fails the install: it marks the result needs-attention with the
// status_check_failed reason instead. Context cancellation propagates
// — the durable manifest stays in place either way.
func verifyInstallStatus(
	ctx context.Context,
	client docker.Client,
	plan *installPlan,
	onProgress types.ProgressFn,
) (*types.AppStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepInstallStatus, 90, "verifying deployed stack status")
	}

	now := time.Now().UTC()
	status := &types.AppStatus{
		AppID:          plan.app.AppID,
		ComposeProject: plan.composeProject,
		StackPath:      plan.stackPath,
		LocalPorts:     append([]types.PortBinding(nil), plan.localPorts...),
		UpdatedAt:      &now,
	}

	containers, err := docker.InspectProjectContainers(ctx, client, plan.composeProject)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		status.State = statusStateNeedsAttention
		status.NeedsAttention = true
		status.AttentionReasons = []string{statusReasonStatusCheckFailed}
		status.Message = "post-install status verification failed; run apps status for details"
		return status, nil
	}

	fuseInstallStatus(plan, containers, status)
	return status, nil
}

// fuseInstallStatus matches the expected rendered services against
// the inspected containers through the shared
// [fuseManagedServiceStatuses] fusion (PRD §10, §18; row
// 33), then layers the install-specific port check on top: the plan
// carries full bindings, so a planned port counts as published only
// when both the protocol and the host port match.
func fuseInstallStatus(plan *installPlan, containers []docker.ContainerInfo, status *types.AppStatus) {
	services := make([]string, 0, len(plan.rendered.ServiceLabels))
	for service := range plan.rendered.ServiceLabels {
		services = append(services, service)
	}
	sort.Strings(services)

	managed, reasons := fuseManagedServiceStatuses(plan.app.AppID, services, completedServiceSet(plan.app.CompletedServices), containers, status)

	published := map[string]struct{}{}
	for _, container := range managed {
		for _, port := range container.Ports {
			published[fmt.Sprintf("%s/%d", port.Protocol, port.HostPort)] = struct{}{}
		}
	}
	for _, binding := range plan.localPorts {
		if _, ok := published[fmt.Sprintf("%s/%d", binding.Protocol, binding.HostPort)]; !ok {
			reasons[statusReasonPortMismatch] = struct{}{}
		}
	}

	finalizeStatus(
		status,
		reasons,
		"all managed services are running",
		"post-install verification found issues; run apps status for details",
	)
}

func publishedPortBindings(container docker.ContainerInfo) []types.PortBinding {
	if len(container.Ports) == 0 {
		return nil
	}
	bindings := make([]types.PortBinding, 0, len(container.Ports))
	for _, port := range container.Ports {
		bindings = append(bindings, types.PortBinding{
			Service:       container.Service,
			HostIP:        port.HostIP,
			HostPort:      port.HostPort,
			ContainerPort: port.ContainerPort,
			Protocol:      port.Protocol,
		})
	}
	return bindings
}

// buildInstallResult assembles the structured install result (PRD §17
// steps 13-14, §32): Compose project, started services, local ports,
// the post-install guidance built at render time, and the fused
// post-deploy status snapshot.
func buildInstallResult(plan *installPlan, status *types.AppStatus) *types.InstallResult {
	var started []string
	for _, service := range status.Services {
		if service.State == statusStateRunning {
			started = append(started, service.Service)
		}
	}
	sort.Strings(started)

	return &types.InstallResult{
		AppID:               plan.app.AppID,
		StackPath:           plan.stackPath,
		ComposeProject:      plan.composeProject,
		StartedServices:     started,
		LocalPorts:          append([]types.PortBinding(nil), plan.localPorts...),
		PostInstallGuidance: plan.guidance,
		Status:              status,
	}
}

func installFileWrites(plan *installPlan) ([]installFileWrite, error) {
	if plan == nil {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"install plan is required",
			"construct an install plan before writing files",
		)
	}

	composePath, err := security.SafeJoin(plan.stackPath, installComposeFilename)
	if err != nil {
		return nil, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	envPath, err := security.SafeJoin(plan.stackPath, installEnvFilename)
	if err != nil {
		return nil, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}

	writes := []installFileWrite{
		{
			path: composePath,
			data: plan.rendered.ComposeBytes,
			mode: installComposeFileMode,
		},
		{
			path: envPath,
			data: plan.rendered.EnvBytes,
			mode: security.SecretFileMode,
		},
	}

	artifactWrites, err := renderedArtifactWrites(&plan.rendered, plan.stackPath)
	if err != nil {
		return nil, err
	}
	writes = append(writes, artifactWrites...)
	return writes, nil
}

// ensureUserEnvFile seeds an empty user-owned .env.user (0600) inside
// stackPath only when it is absent, and returns its resolved path. The
// file is user-editable env injected into every service via the
// template env_file: directive; wdm creates it but NEVER regenerates or
// truncates it, so install, edit, and rewire all share this primitive
// while `wdm update` leaves the user's content untouched.
// security.CreateSecretFile's O_EXCL makes the create idempotent: an
// already-present file surfaces fs.ErrExist, which is treated as "kept
// as-is" rather than an error. The returned file is empty and closed.
func ensureUserEnvFile(stackPath string) (string, error) {
	path, err := security.SafeJoin(stackPath, installEnvUserFilename)
	if err != nil {
		return "", usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	f, err := security.CreateSecretFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return path, nil
		}
		return "", err
	}
	if closeErr := f.Close(); closeErr != nil {
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"user env file could not be finalized",
			"check stack directory permissions and retry",
			closeErr,
		)
	}
	return path, nil
}

// renderedArtifactWrites enumerates the rendered additional_files and
// config_artifacts as concrete file writes rooted at root, in the stable
// order additional_files-then-config_artifacts. It is the single source
// of truth for which sidecar artifacts a stack deploys: the real install
// writer roots it at the stack path, and the pre-deploy compose-config
// validation roots it at its hermetic temp project dir, so the validated
// file set can never drift from the deployed file set.
// One [installAdditionalDestTracker] spans both kinds so a config
// artifact cannot collide with an additional file, a reserved file, its
// temp path, or another artifact of either kind; the kind threads into
// every diagnostic so a rejected config artifact is reported accurately.
// Every Dest is path-safe-joined against root and its mode parsed at the
// filesystem boundary, exactly as the deploy writer requires.
func renderedArtifactWrites(rendered *render.RenderedStack, root string) ([]installFileWrite, error) {
	if rendered == nil {
		return nil, types.NewError(
			types.ErrCodeUsageValidation,
			"rendered stack is required",
			"render the stack before enumerating artifact writes",
		)
	}

	cleanRoot := filepath.Clean(root)
	tracker := newInstallAdditionalDestTracker()
	writes := make([]installFileWrite, 0, len(rendered.AdditionalFiles)+len(rendered.ConfigArtifacts))

	add := func(kind, unsafeMsg, reservedMsg, modeMsg string, file render.RenderedFile) error {
		path, err := security.SafeJoin(root, file.Dest)
		if err != nil {
			return catalogVerificationError(unsafeMsg, "refresh the catalog and retry", err)
		}
		relPath, err := filepath.Rel(cleanRoot, filepath.Clean(path))
		if err != nil {
			return catalogVerificationError(unsafeMsg, "refresh the catalog and retry", err)
		}
		if err := tracker.add(kind, file.Dest, relPath); err != nil {
			return catalogVerificationError(reservedMsg, "refresh the catalog and retry", err)
		}
		mode, err := parseRenderedFileMode(file.Mode)
		if err != nil {
			return catalogVerificationError(modeMsg, "refresh the catalog and retry", err)
		}
		writes = append(writes, installFileWrite{
			path: path,
			data: file.Bytes,
			mode: mode,
		})
		return nil
	}

	for _, file := range rendered.AdditionalFiles {
		if err := add(
			installAdditionalFileKind,
			"catalog additional file destination is unsafe",
			"catalog additional file destination is reserved or duplicated",
			"catalog additional file mode is invalid",
			file,
		); err != nil {
			return nil, err
		}
	}
	for _, artifact := range rendered.ConfigArtifacts {
		if err := add(
			installConfigArtifactKind,
			"catalog config artifact destination is unsafe",
			"catalog config artifact destination is reserved or duplicated",
			"catalog config artifact mode is invalid",
			artifact,
		); err != nil {
			return nil, err
		}
	}
	return writes, nil
}

// installAdditionalFileKind and installConfigArtifactKind name the two
// catalog-declared artifact kinds the dest tracker guards, so its
// diagnostics report which kind a collision belongs to.
const (
	installAdditionalFileKind = "additional file"
	installConfigArtifactKind = "config artifact"
)

type installAdditionalDestTracker struct {
	final map[string]string
	temp  map[string]string
}

func newInstallAdditionalDestTracker() *installAdditionalDestTracker {
	return &installAdditionalDestTracker{
		final: map[string]string{
			installComposeFilename:         installComposeFilename,
			installEnvFilename:             installEnvFilename,
			installEnvUserFilename:         installEnvUserFilename,
			installComposeOverrideFilename: installComposeOverrideFilename,
			installLockFilename:            installLockFilename,
			state.BackupDirName:            state.BackupDirName,
		},
		temp: map[string]string{
			installComposeFilename + ".tmp": installComposeFilename,
			installEnvFilename + ".tmp":     installEnvFilename,
			installLockFilename + ".tmp":    installLockFilename,
		},
	}
}

// add records a stack-relative destination for an artifact of the named
// kind ("additional file" or "config artifact") and refuses any
// collision with a reserved file, its temp path, or an already-recorded
// artifact of either kind. The kind threads into every diagnostic so a
// rejected config artifact is never misreported as an additional file;
// it does not affect which paths conflict, so both kinds share one
// reserved-name set and dest namespace.
func (t *installAdditionalDestTracker) add(kind string, rawDest string, relPath string) error {
	cleaned := filepath.Clean(relPath)
	if cleaned == "." {
		return fmt.Errorf("%s %q targets the stack root", kind, rawDest)
	}
	if existing, owner, ok := t.findConflict(cleaned, t.final); ok {
		return fmt.Errorf(
			"%s %q targets %q that conflicts with %s at %q",
			kind,
			rawDest,
			cleaned,
			owner,
			existing,
		)
	}
	if existing, owner, ok := t.findConflict(cleaned, t.temp); ok {
		return fmt.Errorf(
			"%s %q targets %q that conflicts with temp path for %s at %q",
			kind,
			rawDest,
			cleaned,
			owner,
			existing,
		)
	}

	tempPath := cleaned + ".tmp"
	if existing, owner, ok := t.findConflict(tempPath, t.final); ok {
		return fmt.Errorf(
			"%s %q would use temp path %q that conflicts with %s at %q",
			kind,
			rawDest,
			tempPath,
			owner,
			existing,
		)
	}
	if existing, owner, ok := t.findConflict(tempPath, t.temp); ok {
		return fmt.Errorf(
			"%s %q would use temp path %q that conflicts with temp path for %s at %q",
			kind,
			rawDest,
			tempPath,
			owner,
			existing,
		)
	}

	owner := fmt.Sprintf("%s %q", kind, rawDest)
	t.final[cleaned] = owner
	t.temp[tempPath] = owner
	return nil
}

func (t *installAdditionalDestTracker) findConflict(candidate string, paths map[string]string) (string, string, bool) {
	for existing, owner := range paths {
		if installPathsConflict(candidate, existing) {
			return existing, owner, true
		}
	}
	return "", "", false
}

func installPathsConflict(a, b string) bool {
	return installPathHasRoot(a, b) || installPathHasRoot(b, a)
}

func installPathHasRoot(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func installStackPathExists(stackPath string) (bool, error) {
	if err := security.RejectUnsafeRoot(stackPath); err != nil {
		return false, stackPathUnsafeError(err)
	}
	if err := validateInstallPathAncestors(stackPath); err != nil {
		return false, stackPathUnsafeError(err)
	}

	info, err := os.Lstat(stackPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, types.WrapError(
			types.ErrCodeGeneric,
			"stack directory could not be inspected",
			"check stack directory permissions and retry",
			err,
		)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, stackPathUnsafeError(fmt.Errorf("stack path %q is a symlink", stackPath))
	}
	if !info.IsDir() {
		return false, usageValidationError(
			"stack path is not a directory",
			"choose an empty directory path for the stack",
			fmt.Errorf("stack path %q is not a directory", stackPath),
		)
	}
	return true, nil
}

func createInstallStackDir(stackPath string) error {
	if err := security.RejectUnsafeRoot(stackPath); err != nil {
		return stackPathUnsafeError(err)
	}
	if err := validateInstallPathAncestors(stackPath); err != nil {
		return stackPathUnsafeError(err)
	}

	parent := filepath.Dir(stackPath)
	if err := os.MkdirAll(parent, state.GeneratedDirMode); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack parent directories could not be created",
			"check stack directory permissions and retry",
			err,
		)
	}
	if err := validateInstallPathAncestors(stackPath); err != nil {
		return stackPathUnsafeError(err)
	}
	if err := os.Mkdir(stackPath, state.GeneratedDirMode); err != nil {
		if errors.Is(err, os.ErrExist) {
			return stackPathExistsError(stackPath)
		}
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack directory could not be created",
			"check stack directory permissions and retry",
			err,
		)
	}
	if err := os.Chmod(stackPath, state.GeneratedDirMode); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack directory mode could not be enforced",
			"check stack directory permissions and retry",
			err,
		)
	}
	if err := state.SyncDirectory(parent); err != nil {
		return types.WrapError(
			types.ErrCodeGeneric,
			"stack directory entry could not be synced",
			"check stack directory permissions and retry",
			err,
		)
	}
	return nil
}

func acquireExistingInstallStackLock(ctx context.Context, stackPath string) (*state.StackLockHandle, error) {
	lockPath := filepath.Join(stackPath, ".wdm.lock")
	info, err := os.Lstat(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, stackPathExistsError(stackPath)
	}
	if err != nil {
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"stack lock could not be inspected",
			"check stack directory permissions and retry",
			err,
		)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, stackPathUnsafeError(fmt.Errorf("stack lock %q is a symlink", lockPath))
	}
	if !info.Mode().IsRegular() {
		return nil, usageValidationError(
			"stack lock is not a regular file",
			"choose a different stack path or remove the existing stack first",
			fmt.Errorf("stack lock %q is not a regular file", lockPath),
		)
	}
	return acquireInstallStackLock(ctx, stackPath)
}

func acquireInstallStackLock(ctx context.Context, stackPath string) (*state.StackLockHandle, error) {
	handle, err := state.AcquireStackLock(ctx, filepath.Join(stackPath, ".wdm.lock"))
	if err != nil {
		return nil, fmt.Errorf("core.install: acquiring stack lock: %w", err)
	}
	return handle, nil
}

func validateInstallWritePath(stackPath, targetPath string) error {
	if err := security.EnsureWithinRoot(filepath.Clean(stackPath), filepath.Clean(targetPath)); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Clean(stackPath), filepath.Clean(targetPath))
	if err != nil {
		return fmt.Errorf("calculating relative path for %q: %w", targetPath, err)
	}
	return validateInstallRelativePathAncestors(stackPath, rel)
}

func validateInstallRelativePathAncestors(stackPath, relativePath string) error {
	parentPath := filepath.Dir(filepath.Clean(relativePath))
	if parentPath == "." {
		return nil
	}

	currentPath := filepath.Clean(stackPath)
	for _, component := range strings.Split(parentPath, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}

		currentPath = filepath.Join(currentPath, component)
		info, err := os.Lstat(currentPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stating install file path component %q: %w", currentPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("install file path component %q is a symlink", currentPath)
		}
		if !info.IsDir() {
			return fmt.Errorf("install file path component %q is not a directory", currentPath)
		}
	}
	return nil
}

func validateInstallPathAncestors(path string) error {
	parentPath := filepath.Dir(filepath.Clean(path))
	if parentPath == "." || parentPath == string(filepath.Separator) {
		return nil
	}

	currentPath := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(parentPath, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}

		currentPath = filepath.Join(currentPath, component)
		info, err := os.Lstat(currentPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stating stack path component %q: %w", currentPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("stack path component %q is a symlink", currentPath)
		}
		if !info.IsDir() {
			return fmt.Errorf("stack path component %q is not a directory", currentPath)
		}
	}
	return nil
}

func stackPathUnsafeError(cause error) error {
	return usageValidationError(
		"stack path is unsafe",
		"choose a stack path under your home directory without symlinked path components",
		cause,
	)
}

func stackPathExistsError(stackPath string) error {
	return usageValidationError(
		"stack path already exists",
		"choose an empty stack path or remove the existing directory first",
		fmt.Errorf("stack path %q already exists", stackPath),
	)
}

func managedStackExistsError(stackPath string) error {
	return usageValidationError(
		"stack is already managed",
		"choose a different stack path or remove the existing stack first",
		fmt.Errorf("stack %q already has a lock manifest", stackPath),
	)
}

func parseRenderedFileMode(mode string) (os.FileMode, error) {
	parsed, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal: %w", mode, err)
	}
	if parsed > 0o777 {
		return 0, fmt.Errorf("mode %q is outside permission bits", mode)
	}
	return os.FileMode(parsed), nil
}

// generatedCredentialNote is the verbatim line shown beside every
// one-time credential on the finish screen. The plaintext cannot be
// re-derived from the persisted PHC hash, so the operator must record it
// at install time.
const generatedCredentialNote = "Store this now — it cannot be recovered."

// generateSecretsAndBindRedactor mints the install's secrets and returns a
// redactor bound to the resulting generated-secret set in one step, so the
// redactor is never constructed before generation completes (issue #120).
// generateInstallSecrets stays the single atomic generation step; this only
// orders generation and binding inseparably.
func (p *installPlan) generateSecretsAndBindRedactor(
	generate func(security.Encoding) (string, error),
	generateArgon2id func() (plaintext, phc string, err error),
) (security.Redactor, error) {
	if err := p.generateInstallSecrets(generate, generateArgon2id); err != nil {
		return nil, err
	}
	return security.NewActiveRedactor(p.generatedValues), nil
}

func (p *installPlan) generateInstallSecrets(
	generate func(security.Encoding) (string, error),
	generateArgon2id func() (plaintext, phc string, err error),
) error {
	if generate == nil || generateArgon2id == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"secret generator is required",
			"construct the engine with a secret generator",
		)
	}
	for _, ph := range p.app.Placeholders {
		if render.Type(ph.Type) != render.TypeSecret {
			continue
		}
		encoding := security.Encoding(ph.Encoding)
		switch encoding {
		case security.EncodingBase64URL, security.EncodingBase64Std, security.EncodingHex:
			value, err := generate(encoding)
			if err != nil {
				return err
			}
			p.resolvedValues[ph.Name] = value
			p.generatedValues = append(p.generatedValues, value)
		case security.EncodingArgon2id:
			if err := p.generateArgon2idSecret(ph, generateArgon2id); err != nil {
				return err
			}
		default:
			return catalogVerificationError(
				"catalog contains an invalid secret encoding",
				"refresh the catalog and retry",
				fmt.Errorf("placeholder %q has encoding %q", ph.Name, ph.Encoding),
			)
		}
	}
	return nil
}

// generateArgon2idSecret mints a one-time argon2id credential for ph and
// binds it across the three sinks with the right value in each:
//   - the .env (and the redaction set) get the $$-escaped PHC hash, so
//     Compose's --env-file interpolation collapses each $$ back to a single
//     $ and the container sees the canonical PHC (see [installEnvEscapeDollar]);
//   - the one-time plaintext goes ONLY to [installPlan.shownCredentials],
//     never to resolvedValues, generatedValues, or any persisted artifact.
//
// The escaping is scoped to this encoding alone — base64url/hex values
// never contain `$` and must pass through the value-agnostic renderer
// untouched, so it lives here at bind time, not in [render.RenderEnv].
func (p *installPlan) generateArgon2idSecret(
	ph catalog.Placeholder,
	generateArgon2id func() (plaintext, phc string, err error),
) error {
	plaintext, phc, err := generateArgon2id()
	if err != nil {
		return err
	}
	escapedPHC := installEnvEscapeDollar(phc)
	p.resolvedValues[ph.Name] = escapedPHC
	p.generatedValues = append(p.generatedValues, escapedPHC)
	p.shownCredentials = append(p.shownCredentials, types.GeneratedCredential{
		Label: strings.TrimSpace(p.app.Name + " " + ph.Name),
		Value: plaintext,
		Note:  generatedCredentialNote,
	})
	return nil
}

// installRedactionSecrets is the full secret-aware redaction set for an
// install's logger: the persisted generated values plus every argon2id
// one-time plaintext (which lives only in shownCredentials, never in
// generatedValues). Clones rather than mutating generatedValues so the
// plan's persisted secret set stays unchanged (PRD §24 rule 2).
func installRedactionSecrets(plan *installPlan) []string {
	secrets := make([]string, 0, len(plan.generatedValues)+len(plan.shownCredentials))
	secrets = append(secrets, plan.generatedValues...)
	for _, cred := range plan.shownCredentials {
		secrets = append(secrets, cred.Value)
	}
	return append(secrets, sensitiveSetValues(plan)...)
}

// sensitiveSetValues returns the resolved plaintext of user-supplied
// placeholders flagged Sensitive (type:string via --set). wdm never
// generates these, so they are not secret-typed; they are collected
// separately for value-redaction and non-secret leak-checking, matching
// the reused-secret treatment on the update and reconfigure paths.
func sensitiveSetValues(plan *installPlan) []string {
	var vals []string
	for _, ph := range plan.app.Placeholders {
		if ph.Sensitive {
			if v := plan.resolvedValues[ph.Name]; v != "" {
				vals = append(vals, v)
			}
		}
	}
	return vals
}

// installEnvEscapeDollar doubles every `$` so a value survives Docker
// Compose's --env-file variable interpolation verbatim. Compose expands
// `$VAR` / `${VAR}` references read from the env-file and treats `$$` as a
// literal `$`; an un-escaped argon2id PHC (`$argon2id$v=19$m=...`) would
// otherwise have its segments interpreted as variable references and
// corrupted before the container sees them.
func installEnvEscapeDollar(value string) string {
	return strings.ReplaceAll(value, "$", "$$")
}

func (e *Engine) installRenderInput(ctx context.Context, plan *installPlan) (render.Input, error) {
	if err := ctx.Err(); err != nil {
		return render.Input{}, err
	}
	catalogFS := e.installCatalogFS()
	composeTemplate, err := readCatalogTemplate(catalogFS, plan.app.ComposeTemplate)
	if err != nil {
		return render.Input{}, err
	}
	envTemplate, err := readCatalogTemplate(catalogFS, plan.app.EnvTemplate)
	if err != nil {
		return render.Input{}, err
	}
	additionalFiles, err := readAdditionalFileTemplates(catalogFS, plan.app)
	if err != nil {
		return render.Input{}, err
	}
	configGeneration, err := readConfigGenerationTemplates(catalogFS, plan.app)
	if err != nil {
		return render.Input{}, err
	}

	values := make(map[string]string, len(plan.resolvedValues))
	for key, value := range plan.resolvedValues {
		values[key] = value
	}
	placeholders := append([]render.Placeholder(nil), plan.placeholders...)
	return render.Input{
		EnvTemplate:      string(envTemplate),
		ComposeTemplate:  string(composeTemplate),
		Placeholders:     placeholders,
		Values:           values,
		AppID:            plan.app.AppID,
		AdditionalFiles:  additionalFiles,
		ConfigGeneration: configGeneration,
	}, nil
}

func readCatalogTemplate(catalogFS fs.FS, name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, catalogVerificationError(
			"catalog template path is invalid",
			"refresh the catalog and retry",
			fmt.Errorf("template path %q is invalid", name),
		)
	}
	raw, err := fs.ReadFile(catalogFS, name)
	if err != nil {
		return nil, fmt.Errorf("read template %q: %w", name, err)
	}
	return raw, nil
}

func readAdditionalFileTemplates(catalogFS fs.FS, app catalog.App) ([]render.AdditionalFile, error) {
	if len(app.AdditionalFiles) == 0 {
		return nil, nil
	}
	templateDir := path.Dir(app.ComposeTemplate)
	files := make([]render.AdditionalFile, 0, len(app.AdditionalFiles))
	for _, file := range app.AdditionalFiles {
		if !fs.ValidPath(file.Src) {
			return nil, catalogVerificationError(
				"catalog additional file source path is invalid",
				"refresh the catalog and retry",
				fmt.Errorf("additional file source path %q is invalid", file.Src),
			)
		}
		raw, err := readCatalogTemplate(catalogFS, path.Join(templateDir, file.Src))
		if err != nil {
			return nil, err
		}
		files = append(files, render.AdditionalFile{
			Dest:     file.Dest,
			Mode:     file.Mode,
			Mount:    file.Mount,
			Template: string(raw),
		})
	}
	return files, nil
}

// readConfigGenerationTemplates reads each catalog config_generation
// template off the catalog FS and projects it into the render-local
// [render.ConfigArtifact] shape so [render.RenderLabels] can render the
// artifact, verify its declared mount, and traversal-check its dest. The
// template path is resolved relative to the app's template directory,
// mirroring readAdditionalFileTemplates; both kinds are catalog-declared
// templates rendered in memory before any disk write (PRD §17).
func readConfigGenerationTemplates(catalogFS fs.FS, app catalog.App) ([]render.ConfigArtifact, error) {
	if len(app.ConfigGeneration) == 0 {
		return nil, nil
	}
	templateDir := path.Dir(app.ComposeTemplate)
	artifacts := make([]render.ConfigArtifact, 0, len(app.ConfigGeneration))
	for _, artifact := range app.ConfigGeneration {
		if !fs.ValidPath(artifact.Template) {
			return nil, catalogVerificationError(
				"catalog config artifact source path is invalid",
				"refresh the catalog and retry",
				fmt.Errorf("config artifact source path %q is invalid", artifact.Template),
			)
		}
		raw, err := readCatalogTemplate(catalogFS, path.Join(templateDir, artifact.Template))
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, render.ConfigArtifact{
			Dest:     artifact.Dest,
			Mode:     artifact.Mode,
			Mount:    artifact.Mount,
			Template: string(raw),
		})
	}
	return artifacts, nil
}

func verifyRenderedNonSecretArtifacts(
	redactor security.Redactor,
	secrets []string,
	stack render.RenderedStack,
	guidance *types.PostInstallGuidance,
) error {
	artifacts := []struct {
		name  string
		bytes []byte
	}{
		{name: "docker-compose.yml", bytes: stack.ComposeBytes},
		{name: "post-install guidance", bytes: guidanceText(guidance)},
	}
	for _, file := range stack.AdditionalFiles {
		if file.Mode == "0600" {
			continue
		}
		artifacts = append(artifacts, struct {
			name  string
			bytes []byte
		}{
			name:  file.Dest,
			bytes: file.Bytes,
		})
	}
	// Config artifacts share the additional_files convention: 0600 is the
	// secret-bearing mode and is excluded, but any non-0600 config artifact
	// is a non-secret sink and must be refused if it carries a generated or
	// reused secret (PRD §17, §24).
	for _, artifact := range stack.ConfigArtifacts {
		if artifact.Mode == "0600" {
			continue
		}
		artifacts = append(artifacts, struct {
			name  string
			bytes []byte
		}{
			name:  artifact.Dest,
			bytes: artifact.Bytes,
		})
	}

	for _, artifact := range artifacts {
		for _, secret := range secrets {
			if secret == "" || !bytes.Contains(artifact.bytes, []byte(secret)) {
				continue
			}
			return redactedVerificationError(
				redactor,
				"rendered non-secret artifact contains a generated secret",
				"fix the catalog template so generated secrets stay in .env or 0600-only files",
				fmt.Errorf("%s contains generated secret %q", artifact.name, secret),
			)
		}
	}
	return nil
}

// composeImageProjection is the minimal slice of a rendered
// docker-compose.yml needed to read each service's image: literal.
// Only the image field is decoded; yaml.v3 ignores every other service
// key because the struct declares no field for it.
type composeImageProjection struct {
	Services map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"services"`
}

// verifyImagePinsMatchTemplate enforces that every catalog image pin
// names the exact image[:tag] the rendered Compose service deploys
// (PRD §9, §22). The catalog pins drive wdm's update diffing
// (diffUpdateServicePins compares lock pins against catalog pins); the
// template's literal image: line drives what `docker compose up`
// actually pulls. When the two drift — a maintainer bumps the template
// image without the pin, or vice versa — wdm would report "updates"
// that do not match what deploys, a silent correctness lie. This check
// makes that drift structurally impossible by refusing install and
// update before any Docker contact.
// It parses the RENDERED compose bytes
// ([render.RenderedStack.ComposeBytes]) rather than the raw template
// text: the rendered output is genuine YAML already emitted by
// [render.RenderLabels], so the parse is robust against any
// text/template actions the template carried and inspects the exact
// image references Compose will see. The compared reference is
// `image[:tag]` built from the catalog pin via [updateImageRef] —
// byte-identical to the update path's old → new diff surface — so the
// install-time and update-time views of "what is pinned" cannot
// diverge either.
// Both the install render path ([Engine.renderInstall]) and the update
// re-render path (rewriteUpdateStack) call this alongside
// [verifyRenderedNonSecretArtifacts], so a drifted catalog is refused
// on both arcs. Failures wrap [types.ErrCodeVerificationFailed] (the
// catalog-integrity class, matching the surrounding render errors)
// through the operation redactor and name the app, service, pinned
// image, and rendered template image. Catalog metadata carries no
// secrets, but the cause is redacted defensively for parity with the
// sibling render-stage errors.
func verifyImagePinsMatchTemplate(redactor security.Redactor, app catalog.App, composeBytes []byte) error {
	var projection composeImageProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for image-pin verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	seen := map[string]struct{}{}
	for _, pin := range app.ImagePins {
		if pin.Service == "" {
			continue
		}
		if _, ok := seen[pin.Service]; ok {
			continue
		}
		seen[pin.Service] = struct{}{}

		pinnedRef := updateImageRef(pin.Image, pin.Tag)
		service, ok := projection.Services[pin.Service]
		if !ok {
			return redactedVerificationError(
				redactor,
				"catalog image pin names a service absent from the rendered compose",
				"align the catalog image_pins service names with the compose template",
				fmt.Errorf(
					"app %q pins image %q for service %q but the rendered compose declares no such service",
					app.AppID,
					pinnedRef,
					pin.Service,
				),
			)
		}
		if service.Image != pinnedRef {
			return redactedVerificationError(
				redactor,
				"catalog image pin does not match the rendered compose image",
				"align the catalog image_pins tag with the compose template image line (or vice versa)",
				fmt.Errorf(
					"app %q service %q: catalog pins image %q but the compose template deploys %q",
					app.AppID,
					pin.Service,
					pinnedRef,
					service.Image,
				),
			)
		}
	}
	return nil
}

// composePortsProjection is the minimal slice of a rendered
// docker-compose.yml needed to read each service's published host-port
// bindings and network mode. Only services.<name>.ports and
// services.<name>.network_mode are decoded; yaml.v3 ignores every other key
// because the struct declares no field for it.
type composePortsProjection struct {
	Services map[string]composePortsService `yaml:"services"`
}

// composePortsService carries one service's rendered ports and network mode.
// network_mode is read so the public-bind scan can refuse host networking,
// which exposes every container port outside the services.<name>.ports list
// and would otherwise be invisible to the scan.
type composePortsService struct {
	Ports       []composePortEntry `yaml:"ports"`
	NetworkMode string             `yaml:"network_mode"`
}

// composePortEntry is one rendered Compose ports: entry. Compose accepts
// both the short string form ("127.0.0.1:3008:3001", "6881:6881/udp") and
// the long mapping form ({target, published, host_ip, protocol}); a custom
// UnmarshalYAML normalizes both into the same fields so the public-bind scan
// reads one shape.
type composePortEntry struct {
	hostIP    string
	published string
	protocol  string
	raw       string
}

type composePortLong struct {
	HostIP    string `yaml:"host_ip"`
	Published string `yaml:"published"`
	Target    string `yaml:"target"`
	Protocol  string `yaml:"protocol"`
}

// UnmarshalYAML accepts either a scalar short form or a mapping long form.
// The short form is parsed lexically rather than expanded: only the host IP,
// the published host port (or range), and the protocol matter to the
// public-bind classification.
func (e *composePortEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		e.raw = node.Value
		e.hostIP, e.published, e.protocol = parseShortPort(node.Value)
		return nil
	case yaml.MappingNode:
		var long composePortLong
		if err := node.Decode(&long); err != nil {
			return err
		}
		published := long.Published
		if published == "" {
			published = long.Target
		}
		e.hostIP = long.HostIP
		e.published = published
		e.protocol = long.Protocol
		e.raw = fmt.Sprintf("%s:%s/%s", long.HostIP, published, long.Protocol)
		return nil
	default:
		return fmt.Errorf("unexpected compose port node kind %d", node.Kind)
	}
}

// parseShortPort splits a Compose short-form port string into host IP,
// published host port (or range), and protocol. Short form is
// "[host_ip:][host:]container[/protocol]"; the host IP, when present, is the
// leading segment that is not purely numeric/range. A bare "6881:6881" has
// no host IP, so Docker would bind all interfaces — that is why an empty host
// IP classifies as public downstream.
func parseShortPort(value string) (hostIP, published, protocol string) {
	spec, proto, hasProto := strings.Cut(value, "/")
	if hasProto {
		protocol = proto
	}
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 3:
		// host_ip:host:container
		return parts[0], parts[1], protocol
	case 2:
		// host:container (no host IP, binds all interfaces)
		return "", parts[0], protocol
	default:
		// container-only (Docker assigns the host port, all interfaces)
		return "", spec, protocol
	}
}

// verifyPublicBindsMatchCatalog enforces PRD §11.1(a)(b): the set of PUBLIC
// host-port binds in the rendered compose must exactly equal the set of
// port.public:true declarations in the signed catalog, matched by (protocol,
// host port / host range). A published port is local IFF its host IP is a
// loopback address; 0.0.0.0, an empty/missing host IP (Docker defaults to all
// interfaces), or any non-loopback IP is PUBLIC and requires a backing public
// declaration. This makes two failures structurally impossible before any
// Docker contact: an unsigned/tampered template introducing a public bind
// with no catalog backing (§11.1(b)), and a public declaration that renders
// as 127.0.0.1 so the warning would lie. It also refuses any service that
// renders network_mode: host, which publishes every container port on the
// host outside the scanned ports list and would otherwise hide exposure from
// the scan — host networking is never permitted in the curated set. Both
// render paths ([Engine.renderInstall] and rewriteUpdateStack) call it
// alongside [verifyImagePinsMatchTemplate]. Failures wrap
// [types.ErrCodeVerificationFailed] through the operation redactor;
// catalog/template metadata carries no secrets but is redacted defensively
// for parity with the sibling render errors.
func verifyPublicBindsMatchCatalog(redactor security.Redactor, app catalog.App, composeBytes []byte) error {
	var projection composePortsProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for public-bind verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	// Host networking publishes every container port on the host directly,
	// bypassing the services.<name>.ports list the public-bind scan reads, so
	// the scan can never see that exposure. It is never permitted in the
	// curated set; refuse any service that renders it (fail closed).
	for _, name := range sortedServiceNames(projection) {
		if strings.EqualFold(strings.TrimSpace(projection.Services[name].NetworkMode), "host") {
			return redactedVerificationError(
				redactor,
				"rendered compose runs a service on the host network",
				"remove network_mode: host; bind only the declared ports on 127.0.0.1 (or a declared public port)",
				fmt.Errorf(
					"app %q service %q renders network_mode: host, which exposes every container port outside the scanned ports list",
					app.AppID,
					name,
				),
			)
		}
	}

	declared, err := declaredPublicBinds(app)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"catalog public port declaration is invalid",
			"refresh the catalog and retry",
			err,
		)
	}

	rendered, err := renderedPublicBinds(app, projection)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose public bind could not be classified",
			"refresh the catalog and retry",
			err,
		)
	}

	// A public bind in the rendered compose with no backing public:true
	// declaration is an unsigned/tampered template introducing exposure.
	for _, key := range sortedBindKeys(rendered) {
		if _, ok := declared[key]; !ok {
			return redactedVerificationError(
				redactor,
				"rendered compose binds a public port the catalog does not declare",
				"a public bind must be declared public:true in the signed catalog",
				fmt.Errorf(
					"app %q service %q binds public port %s with no catalog public declaration",
					app.AppID,
					rendered[key],
					key,
				),
			)
		}
	}
	// A public:true declaration that did not render as a public bind means
	// the template drifted to 127.0.0.1; the warning would otherwise lie.
	for _, key := range sortedBindKeys(declared) {
		if _, ok := rendered[key]; !ok {
			return redactedVerificationError(
				redactor,
				"catalog declares a public port the rendered compose does not bind publicly",
				"align the compose template bind interface with the catalog public declaration",
				fmt.Errorf(
					"app %q service %q declares public port %s but the rendered compose does not bind it on all interfaces",
					app.AppID,
					declared[key],
					key,
				),
			)
		}
	}
	return nil
}

// declaredPublicBinds maps each catalog public:true port to its service,
// keyed by "<protocol>/<host>" (single ports) or "<protocol>/<lo>-<hi>"
// (ranges). The same protocol/range normalization is applied on the rendered
// side so the two sets compare like for like.
func declaredPublicBinds(app catalog.App) (map[string]string, error) {
	declared := map[string]string{}
	for _, port := range app.Ports {
		if !port.Public {
			continue
		}
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		if port.HostRange != "" {
			lo, hi, err := parsePortRange(port.Service, port.HostRange)
			if err != nil {
				return nil, err
			}
			declared[fmt.Sprintf("%s/%d-%d", protocol, lo, hi)] = port.Service
			continue
		}
		declared[fmt.Sprintf("%s/%d", protocol, port.Host)] = port.Service
	}
	return declared, nil
}

// renderedPublicBinds maps each PUBLIC published bind in the rendered compose
// to its service, keyed the same way as declaredPublicBinds. Loopback-bound
// ports are local and excluded; everything else is public per the fail-closed
// classification.
func renderedPublicBinds(
	app catalog.App,
	projection composePortsProjection,
) (map[string]string, error) {
	rendered := map[string]string{}
	for _, service := range sortedServiceNames(projection) {
		for _, entry := range projection.Services[service].Ports {
			if isLoopbackBind(entry.hostIP) {
				continue
			}
			protocol := entry.protocol
			if protocol == "" {
				protocol = "tcp"
			}
			key, err := publicBindKey(protocol, entry)
			if err != nil {
				return nil, fmt.Errorf("app %q service %q: %w", app.AppID, service, err)
			}
			rendered[key] = service
		}
	}
	return rendered, nil
}

// publicBindKey normalizes a rendered published value (a single port or a
// "lo-hi" range) into the comparison key shared with the catalog side.
func publicBindKey(protocol string, entry composePortEntry) (string, error) {
	if entry.published == "" {
		return "", fmt.Errorf("public bind %q has no published host port", entry.raw)
	}
	if lo, hi, ok := strings.Cut(entry.published, "-"); ok {
		loPort, loErr := strconv.Atoi(lo)
		hiPort, hiErr := strconv.Atoi(hi)
		if loErr != nil || hiErr != nil {
			return "", fmt.Errorf("public bind %q has a non-numeric port range", entry.raw)
		}
		return fmt.Sprintf("%s/%d-%d", protocol, loPort, hiPort), nil
	}
	port, err := strconv.Atoi(entry.published)
	if err != nil {
		return "", fmt.Errorf("public bind %q has a non-numeric host port", entry.raw)
	}
	return fmt.Sprintf("%s/%d", protocol, port), nil
}

// isLoopbackBind reports whether a Compose host_ip binds a loopback address
// (127.0.0.0/8 or ::1). An empty host IP is NOT loopback: Docker defaults a
// portless or IP-less binding to all interfaces, so it must classify as
// public (fail closed).
func isLoopbackBind(hostIP string) bool {
	if hostIP == "" {
		return false
	}
	ip := net.ParseIP(hostIP)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func sortedServiceNames(projection composePortsProjection) []string {
	names := make([]string, 0, len(projection.Services))
	for name := range projection.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedBindKeys(binds map[string]string) []string {
	keys := make([]string, 0, len(binds))
	for key := range binds {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// allowedCapabilities is the closed Linux-capability allow-list from PRD §12.2.
// SYS_MODULE is intentionally excluded: a kernel-module need is met by a host
// modprobe prerequisite plus a /lib/modules:ro mount, not the capability. Keys
// are the bare, upper-case capability names (no CAP_ prefix); the scan
// normalizes rendered values before matching.
var allowedCapabilities = map[string]struct{}{
	"NET_BIND_SERVICE": {},
	"CHOWN":            {},
	"SETUID":           {},
	"SETGID":           {},
	"DAC_OVERRIDE":     {},
	"FOWNER":           {},
	"NET_ADMIN":        {},
	"NET_RAW":          {},
}

// allowedSysctls is the closed sysctl allow-list from PRD §12.2.
var allowedSysctls = map[string]struct{}{
	"net.ipv4.ip_forward":              {},
	"net.ipv4.conf.all.src_valid_mark": {},
}

// composePrivilegeProjection is the minimal slice of a rendered
// docker-compose.yml needed to read each service's container-privilege posture.
// Only the cap_add/cap_drop/sysctls/devices/privileged keys are decoded; yaml.v3
// ignores every other key because the struct declares no field for it.
type composePrivilegeProjection struct {
	Services map[string]composePrivilegeService `yaml:"services"`
}

// composePrivilegeService carries one service's rendered privilege keys.
// Sysctls is a custom type because Compose accepts both a mapping form
// ({name: value}) and a sequence form ([name=value]); UnmarshalYAML normalizes
// both into the same name→value shape so the scan reads one form.
type composePrivilegeService struct {
	CapAdd     []string       `yaml:"cap_add"`
	CapDrop    []string       `yaml:"cap_drop"`
	Sysctls    composeSysctls `yaml:"sysctls"`
	Devices    []string       `yaml:"devices"`
	Privileged bool           `yaml:"privileged"`
}

// composeSysctls is the rendered sysctls block normalized to name→value.
type composeSysctls struct {
	entries map[string]string
}

// UnmarshalYAML accepts Compose's two sysctls forms: a mapping {name: value}
// and a sequence of "name=value" strings. Any other node kind, or a sequence
// entry without an '=', fails closed so the scan refuses an unclassifiable
// declaration rather than silently passing it.
func (s *composeSysctls) UnmarshalYAML(node *yaml.Node) error {
	s.entries = map[string]string{}
	switch node.Kind {
	case yaml.MappingNode:
		var mapping map[string]string
		if err := node.Decode(&mapping); err != nil {
			return err
		}
		for name, value := range mapping {
			s.entries[name] = value
		}
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := node.Decode(&items); err != nil {
			return err
		}
		for _, item := range items {
			name, value, ok := strings.Cut(item, "=")
			if !ok {
				return fmt.Errorf("sysctl entry %q is not in name=value form", item)
			}
			s.entries[name] = value
		}
		return nil
	default:
		return fmt.Errorf("unexpected compose sysctls node kind %d", node.Kind)
	}
}

// normalizeCapability returns the bare, upper-case capability name used for
// allow-list matching. Compose accepts both "NET_ADMIN" and "CAP_NET_ADMIN"
// and is case-insensitive, so a leading CAP_ prefix is stripped and the name
// upper-cased before comparison.
func normalizeCapability(name string) string {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.TrimPrefix(upper, "CAP_")
}

// verifyContainerPrivilegeMatchCatalog enforces the PRD §12.2 closed
// container-privilege allow-list against the rendered compose, mirroring the
// public-bind scan ([verifyPublicBindsMatchCatalog]). It runs three checks and
// fails closed on any YAML it cannot classify:
//
//   - (A) the catalog's per-service ServiceHardening declarations must stay
//     inside the allow-list (caps, sysctls, empty device set, privileged=false);
//   - (B) every rendered service — declaring or not — must keep cap_add inside
//     the allow-list, keep sysctls inside the allow-list, declare no devices,
//     run unprivileged, and (when it adds any capability) carry cap_drop:ALL as
//     the baseline; and a service the catalog does NOT declare must add zero
//     capabilities and set zero sysctls — any elevation a non-declaring service
//     renders is unbacked by the signed catalog and is refused; and
//   - (C) every service that HAS a ServiceHardening entry must render exactly
//     the declared capability and sysctl sets (defense-in-depth parity for the
//     hardened apps). A non-declaring service's zero-elevation posture is the
//     baseline (enforced by (B)), so it is not parity-checked. This keeps the
//     four cap-using curated apps green via parity and the zero-cap apps green
//     via the baseline.
//
// Both render paths ([Engine.renderInstall] and rewriteUpdateStack) call it
// alongside [verifyPublicBindsMatchCatalog]. Catalog-declaration refusals (A)
// name only allow-list metadata (capability/sysctl names are not secrets) and
// use [catalogVerificationError]; every refusal derived from the rendered
// compose, and any parse failure, is routed through [redactedVerificationError]
// so rendered content can never leak. All failures map to
// [types.ErrCodeVerificationFailed].
func verifyContainerPrivilegeMatchCatalog(
	redactor security.Redactor,
	app catalog.App,
	composeBytes []byte,
) error {
	if err := verifyCatalogPrivilegeDeclarations(app); err != nil {
		return err
	}

	var projection composePrivilegeProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for container-privilege verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	declaredServices := declaredHardeningServices(app)
	if err := verifyRenderedPrivilegeBounds(redactor, app, projection, declaredServices); err != nil {
		return err
	}
	return verifyRenderedPrivilegeParity(redactor, app, projection)
}

// declaredHardeningServices is the set of Compose service names the catalog
// declares a ServiceHardening entry for. A service outside this set must render
// the zero-elevation baseline (no cap_add, no sysctls); a service inside it is
// governed by the parity check.
func declaredHardeningServices(app catalog.App) map[string]struct{} {
	declared := make(map[string]struct{}, len(app.ServiceHardening))
	for _, hardening := range app.ServiceHardening {
		declared[hardening.Service] = struct{}{}
	}
	return declared
}

// verifyCatalogPrivilegeDeclarations enforces check (A): every
// ServiceHardening entry must stay inside the closed allow-list. It reads only
// catalog metadata, so refusals are non-redacting.
func verifyCatalogPrivilegeDeclarations(app catalog.App) error {
	for _, hardening := range app.ServiceHardening {
		if hardening.Capabilities != nil {
			for _, capability := range hardening.Capabilities.Add {
				if _, ok := allowedCapabilities[normalizeCapability(capability)]; !ok {
					return catalogVerificationError(
						"catalog declares a capability outside the allow-list",
						"declare only PRD §12.2 allow-list capabilities, or drop the capability",
						fmt.Errorf(
							"app %q service %q declares capability %q outside the allow-list",
							app.AppID,
							hardening.Service,
							capability,
						),
					)
				}
			}
		}
		for _, sysctl := range hardening.Sysctls {
			if _, ok := allowedSysctls[sysctl.Name]; !ok {
				return catalogVerificationError(
					"catalog declares a sysctl outside the allow-list",
					"declare only PRD §12.2 allow-list sysctls, or drop the sysctl",
					fmt.Errorf(
						"app %q service %q declares sysctl %q outside the allow-list",
						app.AppID,
						hardening.Service,
						sysctl.Name,
					),
				)
			}
		}
		if len(hardening.Devices) > 0 {
			return catalogVerificationError(
				"catalog declares a device map but the device allow-list is empty",
				"remove the device declaration",
				fmt.Errorf(
					"app %q service %q declares %d device(s) against an empty allow-list",
					app.AppID,
					hardening.Service,
					len(hardening.Devices),
				),
			)
		}
		if hardening.Privileged {
			return catalogVerificationError(
				"catalog declares a privileged service absent a recorded amendment",
				"keep privileged false; a privileged declaration requires a recorded PRD amendment",
				fmt.Errorf(
					"app %q service %q declares privileged:true",
					app.AppID,
					hardening.Service,
				),
			)
		}
	}
	return nil
}

// verifyRenderedPrivilegeBounds enforces check (B): the universal allow-list
// bounds that apply to every rendered service whether or not the catalog
// declares hardening for it, plus the requirement that a service the catalog
// does NOT declare renders the zero-elevation baseline (no cap_add, no
// sysctls). declaredServices is the set of service names with a catalog
// ServiceHardening entry; a declaring service's caps/sysctls are governed by
// the parity check instead. Refusals reference rendered compose content, so
// they are redacted.
func verifyRenderedPrivilegeBounds(
	redactor security.Redactor,
	app catalog.App,
	projection composePrivilegeProjection,
	declaredServices map[string]struct{},
) error {
	for _, name := range sortedPrivilegeServiceNames(projection) {
		service := projection.Services[name]
		if _, declared := declaredServices[name]; !declared {
			if len(service.CapAdd) > 0 {
				return redactedVerificationError(
					redactor,
					"rendered compose adds a capability the catalog does not declare",
					"declare the capability in catalog service_hardening or remove it from the compose template",
					fmt.Errorf(
						"app %q service %q adds capabilities but the catalog declares no service_hardening for it",
						app.AppID,
						name,
					),
				)
			}
			if len(service.Sysctls.entries) > 0 {
				return redactedVerificationError(
					redactor,
					"rendered compose sets a sysctl the catalog does not declare",
					"declare the sysctl in catalog service_hardening or remove it from the compose template",
					fmt.Errorf(
						"app %q service %q sets sysctls but the catalog declares no service_hardening for it",
						app.AppID,
						name,
					),
				)
			}
		}
		for _, capability := range service.CapAdd {
			if _, ok := allowedCapabilities[normalizeCapability(capability)]; !ok {
				return redactedVerificationError(
					redactor,
					"rendered compose adds a capability outside the allow-list",
					"a re-added capability must be in the PRD §12.2 allow-list",
					fmt.Errorf(
						"app %q service %q adds capability %q outside the allow-list",
						app.AppID,
						name,
						capability,
					),
				)
			}
		}
		for sysctlName := range service.Sysctls.entries {
			if _, ok := allowedSysctls[sysctlName]; !ok {
				return redactedVerificationError(
					redactor,
					"rendered compose sets a sysctl outside the allow-list",
					"a sysctl must be in the PRD §12.2 allow-list",
					fmt.Errorf(
						"app %q service %q sets sysctl %q outside the allow-list",
						app.AppID,
						name,
						sysctlName,
					),
				)
			}
		}
		if len(service.Devices) > 0 {
			return redactedVerificationError(
				redactor,
				"rendered compose declares a device map but the device allow-list is empty",
				"remove the device mapping from the compose template",
				fmt.Errorf(
					"app %q service %q declares %d device(s) against an empty allow-list",
					app.AppID,
					name,
					len(service.Devices),
				),
			)
		}
		if service.Privileged {
			return redactedVerificationError(
				redactor,
				"rendered compose runs a service privileged",
				"remove privileged:true from the compose template",
				fmt.Errorf(
					"app %q service %q renders privileged:true",
					app.AppID,
					name,
				),
			)
		}
		if len(service.CapAdd) > 0 && !capDropContainsAll(service.CapDrop) {
			return redactedVerificationError(
				redactor,
				"rendered compose adds capabilities without the cap_drop:ALL baseline",
				"keep cap_drop: [ALL] as the baseline and re-add only the declared capabilities",
				fmt.Errorf(
					"app %q service %q adds capabilities but does not drop ALL",
					app.AppID,
					name,
				),
			)
		}
	}
	return nil
}

// verifyRenderedPrivilegeParity enforces check (C): every service the catalog
// hardens must render exactly the declared capability and sysctl sets and the
// declared privileged flag. Refusals reference rendered compose content, so
// they are redacted.
func verifyRenderedPrivilegeParity(
	redactor security.Redactor,
	app catalog.App,
	projection composePrivilegeProjection,
) error {
	for _, hardening := range app.ServiceHardening {
		service, ok := projection.Services[hardening.Service]
		if !ok {
			return redactedVerificationError(
				redactor,
				"catalog hardens a service absent from the rendered compose",
				"align the catalog service_hardening service names with the compose template",
				fmt.Errorf(
					"app %q hardens service %q but the rendered compose declares no such service",
					app.AppID,
					hardening.Service,
				),
			)
		}

		declaredCaps := map[string]struct{}{}
		if hardening.Capabilities != nil {
			for _, capability := range hardening.Capabilities.Add {
				declaredCaps[normalizeCapability(capability)] = struct{}{}
			}
		}
		renderedCaps := map[string]struct{}{}
		for _, capability := range service.CapAdd {
			renderedCaps[normalizeCapability(capability)] = struct{}{}
		}
		if !stringSetsEqual(declaredCaps, renderedCaps) {
			return redactedVerificationError(
				redactor,
				"rendered compose capability set does not match the catalog declaration",
				"align the compose template cap_add with the catalog service_hardening capabilities",
				fmt.Errorf(
					"app %q service %q: catalog declares capabilities %s but the rendered compose adds %s",
					app.AppID,
					hardening.Service,
					sortedSetValues(declaredCaps),
					sortedSetValues(renderedCaps),
				),
			)
		}

		declaredSysctls := map[string]string{}
		for _, sysctl := range hardening.Sysctls {
			declaredSysctls[sysctl.Name] = sysctl.Value
		}
		if !stringMapsEqual(declaredSysctls, service.Sysctls.entries) {
			return redactedVerificationError(
				redactor,
				"rendered compose sysctl set does not match the catalog declaration",
				"align the compose template sysctls with the catalog service_hardening sysctls",
				fmt.Errorf(
					"app %q service %q: catalog declares sysctls %s but the rendered compose sets %s",
					app.AppID,
					hardening.Service,
					sortedSysctlPairs(declaredSysctls),
					sortedSysctlPairs(service.Sysctls.entries),
				),
			)
		}

		if service.Privileged != hardening.Privileged {
			return redactedVerificationError(
				redactor,
				"rendered compose privileged flag does not match the catalog declaration",
				"align the compose template privileged flag with the catalog declaration",
				fmt.Errorf(
					"app %q service %q: catalog declares privileged %t but the rendered compose renders %t",
					app.AppID,
					hardening.Service,
					hardening.Privileged,
					service.Privileged,
				),
			)
		}
	}
	return nil
}

// capDropContainsAll reports whether a rendered cap_drop list drops every
// capability ("ALL", case-insensitive).
func capDropContainsAll(capDrop []string) bool {
	for _, dropped := range capDrop {
		if strings.EqualFold(strings.TrimSpace(dropped), "ALL") {
			return true
		}
	}
	return false
}

func sortedPrivilegeServiceNames(projection composePrivilegeProjection) []string {
	names := make([]string, 0, len(projection.Services))
	for name := range projection.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func stringSetsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if _, ok := b[key]; !ok {
			return false
		}
	}
	return true
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

func sortedSetValues(set map[string]struct{}) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func sortedSysctlPairs(pairs map[string]string) []string {
	formatted := make([]string, 0, len(pairs))
	for name, value := range pairs {
		formatted = append(formatted, name+"="+value)
	}
	sort.Strings(formatted)
	return formatted
}

// allowedSocketProxyPermissions is the closed set of docker-socket-proxy
// permission flags wdm recognizes (PRD §12.1, schema socket_proxy_permission).
// The read-scoped flags are the baseline; POST is the write/control switch.
var allowedSocketProxyPermissions = map[string]struct{}{
	"CONTAINERS": {}, "IMAGES": {}, "NETWORKS": {}, "VOLUMES": {},
	"INFO": {}, "EVENTS": {}, "PING": {}, "VERSION": {}, "POST": {},
}

// composeSocketProjection is the minimal slice of a rendered docker-compose.yml
// needed to find direct Docker-socket bind mounts. Only services[].volumes is
// decoded; yaml.v3 ignores every other key.
type composeSocketProjection struct {
	Services map[string]composeSocketService `yaml:"services"`
}

type composeSocketService struct {
	Volumes []composeVolume `yaml:"volumes"`
}

// composeVolume captures the host-side source of one Compose volume entry.
// Compose accepts a short string form ("source:target[:mode]") and a long
// mapping form ({source, target, ...}); UnmarshalYAML normalizes both to the
// source string and fails closed on any other node kind so an unclassifiable
// volume is refused rather than silently passed.
type composeVolume struct {
	source string
}

func (v *composeVolume) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var raw string
		if err := node.Decode(&raw); err != nil {
			return err
		}
		source, _, _ := strings.Cut(raw, ":")
		v.source = source
		return nil
	case yaml.MappingNode:
		var mapping struct {
			Source string `yaml:"source"`
		}
		if err := node.Decode(&mapping); err != nil {
			return err
		}
		v.source = mapping.Source
		return nil
	default:
		return fmt.Errorf("unexpected compose volume node kind %d", node.Kind)
	}
}

// normalizeSocketPermission upper-cases and trims a declared permission for
// allow-list matching (the schema enum is upper-case).
func normalizeSocketPermission(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}

// socketProxyAllowsControl reports whether the declared allowed_api includes the
// POST write/control switch (read-and-control vs read-only).
func socketProxyAllowsControl(allowedAPI []string) bool {
	for _, perm := range allowedAPI {
		if normalizeSocketPermission(perm) == "POST" {
			return true
		}
	}
	return false
}

// isDockerSocketSource reports whether a Compose volume host-side source binds
// the Docker socket. It matches the path basename so /var/run/docker.sock,
// /run/docker.sock, and a bare docker.sock all match, while a named volume
// (dockersock) or an unrelated file (docker.sock.conf) do not.
func isDockerSocketSource(source string) bool {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return false
	}
	return path.Base(path.Clean(trimmed)) == "docker.sock"
}

func sortedSocketServiceNames(projection composeSocketProjection) []string {
	names := make([]string, 0, len(projection.Services))
	for name := range projection.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// verifySocketPolicyMatchCatalog enforces PRD §12.1 against the rendered
// compose, mirroring the container-privilege scan. It runs two checks and fails
// closed on any YAML it cannot classify:
//
//   - (A) when the catalog declares a socket_proxy, every allowed_api flag must
//     be in the recognized closed set, the declared network must reference a
//     networks[] entry with internal:true (so wdm creates it --internal), and
//     the proxy service must be image-pinned; and
//   - (B) no rendered service may bind the Docker socket directly, EXCEPT the
//     declared, enabled docker-socket-proxy sidecar, which legitimately mounts
//     it. Every other direct docker.sock bind is a hard failure; and
//   - (C) the declared, enabled proxy sidecar may attach only to internal
//     networks the catalog declares. Check (A) proves the declared
//     socket_proxy.network is internal, but a tampered template could also
//     attach the sidecar to a non-internal (front/egress) network and make the
//     Docker API reachable off-host. Check (C) closes that gap against the
//     rendered compose.
//
// Both render paths (renderInstall and rewriteUpdateStack) call it. Catalog
// declaration refusals (A) name only catalog metadata (permission/network/
// service names are not secrets) and use catalogVerificationError; every refusal
// derived from the rendered compose, and any parse failure, is routed through
// redactedVerificationError. All failures map to types.ErrCodeVerificationFailed.
func verifySocketPolicyMatchCatalog(
	redactor security.Redactor,
	app catalog.App,
	composeBytes []byte,
) error {
	if err := verifyCatalogSocketDeclaration(app); err != nil {
		return err
	}

	var projection composeSocketProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for socket-policy verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}
	if err := verifyRenderedNoDirectSocketMount(redactor, app, projection); err != nil {
		return err
	}
	return verifyRenderedProxyNetworkInternal(redactor, app, composeBytes)
}

// verifyCatalogSocketDeclaration enforces check (A). It reads only catalog
// metadata, so refusals are non-redacting.
func verifyCatalogSocketDeclaration(app catalog.App) error {
	proxy := app.SocketProxy
	if proxy == nil {
		return nil
	}
	for _, perm := range proxy.AllowedAPI {
		if _, ok := allowedSocketProxyPermissions[normalizeSocketPermission(perm)]; !ok {
			return catalogVerificationError(
				"catalog declares a socket-proxy permission outside the allow-list",
				"declare only recognized docker-socket-proxy permissions",
				fmt.Errorf(
					"app %q socket proxy declares permission %q outside the allow-list",
					app.AppID,
					perm,
				),
			)
		}
	}

	networkInternal, networkFound := false, false
	for _, network := range app.Networks {
		if network.Name == proxy.Network {
			networkFound = true
			networkInternal = network.Internal
			break
		}
	}
	if !networkFound {
		return catalogVerificationError(
			"catalog socket proxy references an undeclared network",
			"point socket_proxy.network at a declared internal network",
			fmt.Errorf(
				"app %q socket proxy references network %q absent from the networks declaration",
				app.AppID,
				proxy.Network,
			),
		)
	}
	if !networkInternal {
		return catalogVerificationError(
			"catalog socket proxy network is not internal",
			"mark the socket-proxy network internal:true so the proxy is never reachable off-host",
			fmt.Errorf(
				"app %q socket proxy network %q is not internal",
				app.AppID,
				proxy.Network,
			),
		)
	}

	for _, pin := range app.ImagePins {
		if pin.Service == proxy.Service {
			return nil
		}
	}
	return catalogVerificationError(
		"catalog socket proxy service is not image-pinned",
		"add an image_pins entry for the socket-proxy service",
		fmt.Errorf(
			"app %q socket proxy service %q has no image pin",
			app.AppID,
			proxy.Service,
		),
	)
}

// verifyRenderedNoDirectSocketMount enforces check (B): no rendered service
// binds the Docker socket directly except the declared, enabled proxy sidecar.
// The exemption requires a non-empty proxy service name and an explicit flag, so
// it never collides with the zero value: absent a real proxy, every socket bind
// — including one on an empty-named service — is refused (fail closed). Refusals
// reference rendered compose content, so they are redacted.
func verifyRenderedNoDirectSocketMount(
	redactor security.Redactor,
	app catalog.App,
	projection composeSocketProjection,
) error {
	proxyService, hasProxyExemption := "", false
	if app.SocketProxy != nil && app.SocketProxy.Enabled && app.SocketProxy.Service != "" {
		proxyService, hasProxyExemption = app.SocketProxy.Service, true
	}
	for _, name := range sortedSocketServiceNames(projection) {
		if hasProxyExemption && name == proxyService {
			continue
		}
		for _, volume := range projection.Services[name].Volumes {
			if isDockerSocketSource(volume.source) {
				return redactedVerificationError(
					redactor,
					"rendered compose binds the Docker socket directly into a container",
					"route Docker API access through a declared docker-socket-proxy sidecar; never bind docker.sock directly",
					fmt.Errorf(
						"app %q service %q binds the Docker socket directly",
						app.AppID,
						name,
					),
				)
			}
		}
	}
	return nil
}

// verifyRenderedProxyNetworkInternal enforces check (C): the declared, enabled
// docker-socket-proxy sidecar may attach only to internal networks the catalog
// declares. It runs solely when a real enabled proxy is declared (a non-nil
// SocketProxy with Enabled and a non-empty Service), the same gating as the
// check-(B) exemption, so it never acts on the zero value; absent that it is a
// no-op, which keeps it silent for the curated apps (none declares socket_proxy).
//
// Network-naming convention: wdm templates declare top-level networks as
// external:true, and wdm pre-creates each one with `docker network create`
// (passing --internal when [catalog.Network.Internal] is set) under the exact
// declared name. Because the networks are external, a service's networks list
// references the compose-local key with no project prefix, so each rendered
// network name equals a catalog [catalog.Network.Name]. Check (C) maps the
// proxy's rendered attachments to app.Networks by that name.
//
// It refuses if the proxy is attached to any network that is non-internal or
// absent from the catalog, and it fails closed on absence: a proxy service with
// no networks block joins the project's non-internal default network and would
// be reachable, and a proxy service entirely absent from the rendered services
// is equally a refusal. Refusals derive from the rendered compose, so they route
// through [redactedVerificationError]; any YAML that cannot be classified fails
// closed the same way. All failures map to [types.ErrCodeVerificationFailed].
func verifyRenderedProxyNetworkInternal(
	redactor security.Redactor,
	app catalog.App,
	composeBytes []byte,
) error {
	if app.SocketProxy == nil || !app.SocketProxy.Enabled || app.SocketProxy.Service == "" {
		return nil
	}
	proxyService := app.SocketProxy.Service

	var projection composeIPAMProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for socket-proxy network verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	service, ok := projection.Services[proxyService]
	if !ok {
		return redactedVerificationError(
			redactor,
			"socket-proxy service is absent from the rendered compose",
			"declare the socket-proxy service in the compose template",
			fmt.Errorf(
				"app %q socket proxy service %q is absent from the rendered compose",
				app.AppID,
				proxyService,
			),
		)
	}

	attached := service.Networks.names()
	if len(attached) == 0 {
		return redactedVerificationError(
			redactor,
			"socket-proxy service attaches to no network in the rendered compose",
			"attach the socket-proxy service to the declared internal network so it never joins the default network",
			fmt.Errorf(
				"app %q socket proxy service %q declares no networks and would join the non-internal default network",
				app.AppID,
				proxyService,
			),
		)
	}

	internalNetworks := internalNetworkNames(app)
	for _, network := range attached {
		if _, ok := internalNetworks[network]; !ok {
			return redactedVerificationError(
				redactor,
				"socket-proxy service attaches to a network that is not a declared internal network",
				"attach the socket-proxy service only to catalog networks marked internal:true",
				fmt.Errorf(
					"app %q socket proxy service %q attaches to network %q that is not a catalog internal network",
					app.AppID,
					proxyService,
					network,
				),
			)
		}
	}
	return nil
}

// internalNetworkNames returns the set of catalog network names marked
// internal:true, the allow-list check (C) admits the socket-proxy sidecar to.
func internalNetworkNames(app catalog.App) map[string]struct{} {
	names := make(map[string]struct{}, len(app.Networks))
	for _, network := range app.Networks {
		if network.Internal {
			names[network.Name] = struct{}{}
		}
	}
	return names
}

// hostModulePath is the host kernel-module tree a service needing a host-loaded
// kernel module mounts read-only (PRD §9, §12.2). It pairs with a host-side
// modprobe prerequisite and replaces the excluded SYS_MODULE capability; the
// mount is the sole shape catalog service_hardening host_module_mount permits.
const hostModulePath = "/lib/modules"

// composeModuleProjection is the minimal slice of a rendered docker-compose.yml
// needed to find host /lib/modules bind mounts. Only services[].volumes is
// decoded; yaml.v3 ignores every other key.
type composeModuleProjection struct {
	Services map[string]composeModuleService `yaml:"services"`
}

type composeModuleService struct {
	Volumes []composeModuleVolume `yaml:"volumes"`
}

// composeModuleVolume captures the source, target, and read-only posture of one
// Compose volume entry, reusing [composeVolume]'s normalization of both the
// short "source:target[:mode]" string form and the long {source, target,
// read_only} mapping form. It fails closed on any other node kind so an
// unclassifiable volume is refused rather than silently passed. read_only is
// true when the short form carries the ":ro" mode or the long form sets
// read_only:true.
type composeModuleVolume struct {
	source   string
	target   string
	readOnly bool
}

func (v *composeModuleVolume) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var raw string
		if err := node.Decode(&raw); err != nil {
			return err
		}
		source, rest, hasRest := strings.Cut(raw, ":")
		v.source = source
		if hasRest {
			target, mode, hasMode := strings.Cut(rest, ":")
			v.target = target
			v.readOnly = hasMode && shortVolumeModeIsReadOnly(mode)
		}
		return nil
	case yaml.MappingNode:
		var mapping struct {
			Source   string `yaml:"source"`
			Target   string `yaml:"target"`
			ReadOnly bool   `yaml:"read_only"`
		}
		if err := node.Decode(&mapping); err != nil {
			return err
		}
		v.source = mapping.Source
		v.target = mapping.Target
		v.readOnly = mapping.ReadOnly
		return nil
	default:
		return fmt.Errorf("unexpected compose volume node kind %d", node.Kind)
	}
}

// shortVolumeModeIsReadOnly reports whether a Compose short-form mode field
// (the segment after the second ':') marks the bind read-only. Compose accepts
// a comma-separated mode list (e.g. "ro,z"), so any "ro" element qualifies.
func shortVolumeModeIsReadOnly(mode string) bool {
	for _, part := range strings.Split(mode, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "ro") {
			return true
		}
	}
	return false
}

// isHostModuleSource reports whether a Compose volume host-side source binds the
// host kernel-module tree. It matches the cleaned path so /lib/modules and a
// trailing-slash variant both match, while a named volume (libmodules) or an
// unrelated path (/lib/modules.bak) does not.
func isHostModuleSource(source string) bool {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return false
	}
	return path.Clean(trimmed) == hostModulePath
}

func sortedModuleServiceNames(projection composeModuleProjection) []string {
	names := make([]string, 0, len(projection.Services))
	for name := range projection.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// verifyHostModuleMountMatchCatalog enforces the PRD §9/§12.2 host /lib/modules
// mount policy against the rendered compose, mirroring the container-privilege
// scan ([verifyContainerPrivilegeMatchCatalog]). SYS_MODULE is excluded from the
// allow-list, so a kernel-module need is met by a host modprobe prerequisite
// plus a read-only /lib/modules mount declared via service_hardening
// host_module_mount. It runs three checks and fails closed on any YAML it cannot
// classify:
//
//   - (UNIVERSAL bound) no rendered service may bind the host /lib/modules path
//     unless its catalog ServiceHardening declares host_module_mount:true — a
//     /lib/modules mount on a non-declaring service is unbacked by the signed
//     catalog and is refused;
//   - (PARITY, presence) every service the catalog declares host_module_mount
//     for MUST render a /lib/modules host mount; declared-but-absent is refused;
//     and
//   - (PARITY, shape) a declaring service's mount must bind host /lib/modules to
//     container /lib/modules read-only; a read-write mount, a different container
//     target, or a missing target is refused.
//
// Both render paths ([Engine.renderInstall] and rewriteUpdateStack) call it
// after [verifyContainerPrivilegeMatchCatalog] and [verifySocketPolicyMatchCatalog].
// The catalog declares only a boolean flag (no metadata to validate), so every
// refusal derives from the rendered compose; refusals name the service from
// catalog metadata (not a secret) but route through [redactedVerificationError]
// for parity with the F4/F5 siblings, as does any parse failure. All failures
// map to [types.ErrCodeVerificationFailed].
func verifyHostModuleMountMatchCatalog(
	redactor security.Redactor,
	app catalog.App,
	composeBytes []byte,
) error {
	var projection composeModuleProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for host-module-mount verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	declaredServices := declaredModuleMountServices(app)
	if err := verifyRenderedModuleMountBounds(redactor, app, projection, declaredServices); err != nil {
		return err
	}
	return verifyRenderedModuleMountParity(redactor, app, projection, declaredServices)
}

// declaredModuleMountServices is the set of Compose service names the catalog
// declares host_module_mount:true for. A service outside this set must render no
// /lib/modules mount; a service inside it is governed by the parity check.
func declaredModuleMountServices(app catalog.App) map[string]struct{} {
	declared := make(map[string]struct{})
	for _, hardening := range app.ServiceHardening {
		if hardening.HostModuleMount {
			declared[hardening.Service] = struct{}{}
		}
	}
	return declared
}

// verifyRenderedModuleMountBounds enforces the universal bound: no rendered
// service may bind host /lib/modules unless the catalog declares
// host_module_mount:true for it. declaredServices is the set of service names
// with that declaration; a declaring service's mount shape is governed by the
// parity check instead. Refusals reference rendered compose content, so they are
// redacted.
func verifyRenderedModuleMountBounds(
	redactor security.Redactor,
	app catalog.App,
	projection composeModuleProjection,
	declaredServices map[string]struct{},
) error {
	for _, name := range sortedModuleServiceNames(projection) {
		if _, declared := declaredServices[name]; declared {
			continue
		}
		for _, volume := range projection.Services[name].Volumes {
			if isHostModuleSource(volume.source) {
				return redactedVerificationError(
					redactor,
					"rendered compose mounts the host module tree into an undeclared service",
					"declare host_module_mount in catalog service_hardening or remove the /lib/modules mount from the compose template",
					fmt.Errorf(
						"app %q service %q binds host /lib/modules but the catalog declares no host_module_mount for it",
						app.AppID,
						name,
					),
				)
			}
		}
	}
	return nil
}

// verifyRenderedModuleMountParity enforces that every service the catalog
// declares host_module_mount:true for renders exactly one read-only host
// /lib/modules → /lib/modules mount. A declared-but-absent mount, a read-write
// mount, or a mount to a different container target is refused. Refusals
// reference rendered compose content, so they are redacted.
func verifyRenderedModuleMountParity(
	redactor security.Redactor,
	app catalog.App,
	projection composeModuleProjection,
	declaredServices map[string]struct{},
) error {
	for _, name := range sortedSetValues(declaredServices) {
		service, ok := projection.Services[name]
		if !ok {
			return redactedVerificationError(
				redactor,
				"catalog declares a host module mount for a service absent from the rendered compose",
				"align the catalog service_hardening service names with the compose template",
				fmt.Errorf(
					"app %q declares host_module_mount for service %q but the rendered compose declares no such service",
					app.AppID,
					name,
				),
			)
		}

		var moduleMount *composeModuleVolume
		for i := range service.Volumes {
			if isHostModuleSource(service.Volumes[i].source) {
				moduleMount = &service.Volumes[i]
				break
			}
		}
		if moduleMount == nil {
			return redactedVerificationError(
				redactor,
				"catalog declares a host module mount the rendered compose does not bind",
				"add a read-only /lib/modules:/lib/modules mount to the declaring service or drop the catalog host_module_mount declaration",
				fmt.Errorf(
					"app %q service %q declares host_module_mount but the rendered compose binds no /lib/modules mount",
					app.AppID,
					name,
				),
			)
		}
		if path.Clean(strings.TrimSpace(moduleMount.target)) != hostModulePath {
			return redactedVerificationError(
				redactor,
				"rendered compose host module mount targets the wrong container path",
				"mount host /lib/modules at container /lib/modules read-only",
				fmt.Errorf(
					"app %q service %q binds host /lib/modules to container target %q, not %q",
					app.AppID,
					name,
					moduleMount.target,
					hostModulePath,
				),
			)
		}
		if !moduleMount.readOnly {
			return redactedVerificationError(
				redactor,
				"rendered compose host module mount is not read-only",
				"mount host /lib/modules read-only (append :ro or set read_only:true)",
				fmt.Errorf(
					"app %q service %q binds host /lib/modules read-write; the mount must be read-only",
					app.AppID,
					name,
				),
			)
		}
	}
	return nil
}

// composeIPAMProjection is the minimal slice of a rendered docker-compose.yml
// needed to read each service's per-network static IPv4 attachment. Only
// services[].networks is decoded; yaml.v3 ignores every other key.
type composeIPAMProjection struct {
	Services map[string]composeIPAMService `yaml:"services"`
}

type composeIPAMService struct {
	Networks composeServiceNetworks `yaml:"networks"`
}

// composeServiceNetworks normalizes Compose's two service-networks forms — a
// sequence of bare network names (no static address) and a mapping of network
// name to attachment options ({ipv4_address: ...}) — into one network→address
// shape. A static IPv4 is expressible only via the mapping form, so the sequence
// form records every attached network with an empty address. Any other node
// kind fails closed.
type composeServiceNetworks struct {
	ipv4ByNetwork map[string]string
}

func (n *composeServiceNetworks) UnmarshalYAML(node *yaml.Node) error {
	n.ipv4ByNetwork = map[string]string{}
	switch node.Kind {
	case yaml.SequenceNode:
		var names []string
		if err := node.Decode(&names); err != nil {
			return err
		}
		for _, name := range names {
			n.ipv4ByNetwork[name] = ""
		}
		return nil
	case yaml.MappingNode:
		var mapping map[string]composeNetworkAttachment
		if err := node.Decode(&mapping); err != nil {
			return err
		}
		for name, attachment := range mapping {
			n.ipv4ByNetwork[name] = attachment.IPv4Address
		}
		return nil
	default:
		return fmt.Errorf("unexpected compose service networks node kind %d", node.Kind)
	}
}

// names returns the network names the service attaches to, sorted for
// deterministic iteration. The set spans both the sequence and mapping forms
// normalized by UnmarshalYAML; an empty slice means the service declares no
// networks block. It is a freshly built slice, so callers cannot mutate the
// projection through it.
func (n *composeServiceNetworks) names() []string {
	return sortedStringKeys(n.ipv4ByNetwork)
}

// composeNetworkAttachment carries one service-to-network attachment's static
// address. A null mapping value (e.g. "front:" with no options) decodes to the
// zero value, which is the no-static-address case.
type composeNetworkAttachment struct {
	IPv4Address string `yaml:"ipv4_address"`
}

// verifyNetworkIPAMMatchCatalog enforces the PRD §9 static-addressing policy
// against the rendered compose, mirroring the container-privilege scan
// ([verifyContainerPrivilegeMatchCatalog]). Templates author ipv4_address
// literally; the catalog declares the IPAM; internal/core verifies parity and
// bounds. It runs three checks and fails closed on any YAML it cannot classify:
//
//   - (CATALOG validation) for each network with IPAM the subnet must be a valid
//     IPv4 CIDR, a set gateway must be an IPv4 within the subnet, and each
//     declared address must be an IPv4 within the subnet naming a real compose
//     service. These refusals read only catalog metadata, so they are
//     non-redacting [catalogVerificationError]s;
//   - (PARITY) every catalog-declared per-service address must equal the address
//     the rendered compose pins for that service on that network; a declared
//     address missing or different in the rendered compose is refused; and
//   - (UNIVERSAL bound) no rendered service may pin an ipv4_address the catalog
//     IPAM does not declare — a tampered template cannot fix an unintended static
//     IP.
//
// Both render paths ([Engine.renderInstall] and rewriteUpdateStack) call it
// after [verifyHostModuleMountMatchCatalog]. Every refusal derived from the
// rendered compose, and any parse failure, is routed through
// [redactedVerificationError]; catalog/template metadata carries no secrets but
// is redacted defensively for parity with the sibling render errors. All
// failures map to [types.ErrCodeVerificationFailed].
func verifyNetworkIPAMMatchCatalog(
	redactor security.Redactor,
	app catalog.App,
	composeBytes []byte,
) error {
	declared, err := validateCatalogIPAMDeclarations(app)
	if err != nil {
		return err
	}

	var projection composeIPAMProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return redactedVerificationError(
			redactor,
			"rendered compose could not be parsed for network IPAM verification",
			"refresh the catalog and retry",
			fmt.Errorf("parse rendered compose for app %q: %w", app.AppID, err),
		)
	}

	if err := verifyRenderedIPAMParity(redactor, app, projection, declared); err != nil {
		return err
	}
	return verifyRenderedIPAMBounds(redactor, app, projection, declared)
}

// ipamAddressKey identifies one declared static assignment by network and
// service, the key the parity and universal-bound checks compare on.
type ipamAddressKey struct {
	network string
	service string
}

// validateCatalogIPAMDeclarations enforces the catalog-validation check and
// returns the declared per-(network,service) static addresses, normalized to
// their canonical netip string form so the rendered comparison is exact. It
// reads only catalog metadata, so refusals are non-redacting.
func validateCatalogIPAMDeclarations(app catalog.App) (map[ipamAddressKey]string, error) {
	declared := map[ipamAddressKey]string{}
	for _, network := range app.Networks {
		if network.IPAM == nil {
			continue
		}
		prefix, err := netip.ParsePrefix(network.IPAM.Subnet)
		if err != nil || !prefix.Addr().Is4() {
			return nil, catalogVerificationError(
				"catalog declares an invalid IPAM subnet",
				"declare an IPv4 CIDR such as 10.0.0.0/24",
				fmt.Errorf(
					"app %q network %q declares subnet %q that is not a valid IPv4 CIDR",
					app.AppID,
					network.Name,
					network.IPAM.Subnet,
				),
			)
		}
		canonicalSubnet := prefix.Masked()

		if network.IPAM.Gateway != "" {
			gateway, err := netip.ParseAddr(network.IPAM.Gateway)
			if err != nil || !gateway.Is4() || !canonicalSubnet.Contains(gateway) {
				return nil, catalogVerificationError(
					"catalog declares an IPAM gateway outside the subnet",
					"declare a gateway that is a valid IPv4 within the subnet",
					fmt.Errorf(
						"app %q network %q declares gateway %q outside subnet %q",
						app.AppID,
						network.Name,
						network.IPAM.Gateway,
						network.IPAM.Subnet,
					),
				)
			}
		}

		for _, address := range network.IPAM.Addresses {
			addr, err := netip.ParseAddr(address.IPv4Address)
			if err != nil || !addr.Is4() || !canonicalSubnet.Contains(addr) {
				return nil, catalogVerificationError(
					"catalog declares an IPAM address outside the subnet",
					"declare each static address as a valid IPv4 within the subnet",
					fmt.Errorf(
						"app %q network %q declares address %q outside subnet %q",
						app.AppID,
						network.Name,
						address.IPv4Address,
						network.IPAM.Subnet,
					),
				)
			}
			if !serviceDeclaredInCatalog(app, address.Service) {
				return nil, catalogVerificationError(
					"catalog declares an IPAM address for an unknown service",
					"point each IPAM address at a service the catalog declares",
					fmt.Errorf(
						"app %q network %q declares address for service %q not present in the catalog",
						app.AppID,
						network.Name,
						address.Service,
					),
				)
			}
			declared[ipamAddressKey{network: network.Name, service: address.Service}] = addr.String()
		}
	}
	return declared, nil
}

// serviceDeclaredInCatalog reports whether the catalog names the service through
// an image pin or a port declaration — the two surfaces that enumerate the real
// compose services. An IPAM address pointing elsewhere is a catalog error.
func serviceDeclaredInCatalog(app catalog.App, service string) bool {
	for _, pin := range app.ImagePins {
		if pin.Service == service {
			return true
		}
	}
	for _, port := range app.Ports {
		if port.Service == service {
			return true
		}
	}
	return false
}

// verifyRenderedIPAMParity enforces the parity check: every declared static
// address must equal the address the rendered compose pins for that service on
// that network. Refusals reference rendered compose content, so they are
// redacted.
func verifyRenderedIPAMParity(
	redactor security.Redactor,
	app catalog.App,
	projection composeIPAMProjection,
	declared map[ipamAddressKey]string,
) error {
	for _, key := range sortedIPAMKeys(declared) {
		service, ok := projection.Services[key.service]
		if !ok {
			return redactedVerificationError(
				redactor,
				"catalog declares an IPAM address for a service absent from the rendered compose",
				"align the catalog IPAM address service names with the compose template",
				fmt.Errorf(
					"app %q declares an IPAM address for service %q but the rendered compose declares no such service",
					app.AppID,
					key.service,
				),
			)
		}
		rendered, attached := service.Networks.ipv4ByNetwork[key.network]
		if !attached || rendered != declared[key] {
			return redactedVerificationError(
				redactor,
				"rendered compose static IP does not match the catalog IPAM declaration",
				"align the compose template ipv4_address with the catalog IPAM address",
				fmt.Errorf(
					"app %q service %q network %q: catalog declares static IP %q but the rendered compose pins %q",
					app.AppID,
					key.service,
					key.network,
					declared[key],
					rendered,
				),
			)
		}
	}
	return nil
}

// verifyRenderedIPAMBounds enforces the universal bound: no rendered service may
// pin an ipv4_address the catalog IPAM does not declare. Refusals reference
// rendered compose content, so they are redacted.
func verifyRenderedIPAMBounds(
	redactor security.Redactor,
	app catalog.App,
	projection composeIPAMProjection,
	declared map[ipamAddressKey]string,
) error {
	for _, serviceName := range sortedIPAMServiceNames(projection) {
		networks := projection.Services[serviceName].Networks.ipv4ByNetwork
		for _, networkName := range sortedStringKeys(networks) {
			if networks[networkName] == "" {
				continue
			}
			if _, ok := declared[ipamAddressKey{network: networkName, service: serviceName}]; !ok {
				return redactedVerificationError(
					redactor,
					"rendered compose pins a static IP the catalog IPAM does not declare",
					"a static ipv4_address must be declared in the catalog network ipam addresses",
					fmt.Errorf(
						"app %q service %q network %q pins a static IP with no catalog IPAM declaration",
						app.AppID,
						serviceName,
						networkName,
					),
				)
			}
		}
	}
	return nil
}

func sortedIPAMKeys(declared map[ipamAddressKey]string) []ipamAddressKey {
	keys := make([]ipamAddressKey, 0, len(declared))
	for key := range declared {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].network != keys[j].network {
			return keys[i].network < keys[j].network
		}
		return keys[i].service < keys[j].service
	})
	return keys
}

func sortedIPAMServiceNames(projection composeIPAMProjection) []string {
	names := make([]string, 0, len(projection.Services))
	for name := range projection.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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

type redactedCause struct {
	message string
	unwrap  error
}

func (e redactedCause) Error() string {
	return e.message
}

func (e redactedCause) Unwrap() error {
	return e.unwrap
}

func redactedVerificationError(
	redactor security.Redactor,
	message string,
	hint string,
	cause error,
) error {
	return types.WrapError(
		types.ErrCodeVerificationFailed,
		message,
		hint,
		newRedactedCause(redactor, cause),
	)
}

func newRedactedCause(redactor security.Redactor, cause error) redactedCause {
	message := fmt.Sprint(cause)
	if redactor != nil {
		message = redactor.Redact(message)
	}
	return redactedCause{
		message: message,
		unwrap:  redactedUnwrap(cause),
	}
}

func redactedUnwrap(cause error) error {
	for _, sentinel := range []error{
		render.ErrEnvTemplateParse,
		render.ErrEnvTemplateExecute,
		render.ErrComposeTemplateParse,
		render.ErrComposeTemplateExecute,
		render.ErrComposeYAMLParse,
		render.ErrComposeYAMLEncode,
		render.ErrComposeServicesMissing,
		render.ErrServiceMissingLabel,
		render.ErrAdditionalFileMountMissing,
		render.ErrAdditionalFileTemplateParse,
		render.ErrAdditionalFileTemplateExecute,
	} {
		if errors.Is(cause, sentinel) {
			return sentinel
		}
	}
	return nil
}

func selectCatalogApp(cat *catalog.Catalog, appID string) (catalog.App, error) {
	var selected catalog.App
	found := false
	for _, app := range cat.Apps {
		if app.AppID != appID {
			continue
		}
		if found {
			return catalog.App{}, catalogVerificationError(
				"catalog contains duplicate app ids",
				"refresh the catalog and retry",
				fmt.Errorf("duplicate app_id %q", appID),
			)
		}
		selected = app
		found = true
	}
	if !found {
		return catalog.App{}, usageValidationError(
			"app is not available in the selected catalog",
			"run apps list and choose one of the listed app ids",
			fmt.Errorf("unknown app_id %q", appID),
		)
	}
	return selected, nil
}

func (e *Engine) planInstallStackPath(req types.InstallRequest, app catalog.App) (string, error) {
	if err := security.RejectUnsafeRoot(e.stackBase); err != nil {
		return "", usageValidationError(
			"stack base path is unsafe",
			"choose a stack base under your home directory",
			err,
		)
	}

	if req.StackPath == "" {
		stackPath, err := security.SafeJoin(e.stackBase, app.AppID)
		if err != nil {
			return "", usageValidationError(
				"stack path is unsafe",
				"choose a stack path under the configured stack base",
				err,
			)
		}
		return stackPath, nil
	}

	if hasTraversalSegment(req.StackPath) {
		return "", usageValidationError(
			"stack path must not contain parent traversal",
			"remove any .. path segments from --stack-path",
			fmt.Errorf("stack path %q contains parent traversal", req.StackPath),
		)
	}
	expanded, err := expandHome(req.StackPath)
	if err != nil {
		return "", fmt.Errorf("core.install: expanding stack path: %w", err)
	}
	if !filepath.IsAbs(expanded) {
		return "", usageValidationError(
			"stack path must be absolute",
			"pass an absolute --stack-path",
			fmt.Errorf("stack path %q is not absolute", req.StackPath),
		)
	}
	if err := security.RejectUnsafeRoot(expanded); err != nil {
		return "", usageValidationError(
			"stack path is unsafe",
			"choose a stack path under your home directory",
			err,
		)
	}
	return filepath.Clean(expanded), nil
}

func hasTraversalSegment(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func (p *installPlan) planPlaceholders(
	req types.InstallRequest,
	settingsTimezone string,
	tzDeps timezoneLookupDeps,
) error {
	declared := make(map[string]catalog.Placeholder, len(p.app.Placeholders))
	for _, ph := range p.app.Placeholders {
		if _, ok := declared[ph.Name]; ok {
			return catalogVerificationError(
				"catalog contains duplicate placeholders",
				"refresh the catalog and retry",
				fmt.Errorf("duplicate placeholder %q", ph.Name),
			)
		}
		declared[ph.Name] = ph
		p.placeholders = append(p.placeholders, render.Placeholder{
			Name:        ph.Name,
			Type:        render.Type(ph.Type),
			Required:    ph.Required,
			Default:     ph.Default,
			Regenerable: ph.Regenerable,
		})
	}

	for key := range req.PlaceholderValues {
		if _, ok := declared[key]; !ok {
			return usageValidationError(
				"placeholder value is not declared by the catalog",
				"remove unknown placeholder values from the install request",
				fmt.Errorf("placeholder %q is not declared", key),
			)
		}
	}

	for _, ph := range p.app.Placeholders {
		value, hasRequestValue := req.PlaceholderValues[ph.Name]
		var err error
		switch render.Type(ph.Type) {
		case render.TypeSecret:
			p.generatedFields = append(p.generatedFields, ph.Name)
			continue
		case render.TypeDomain:
			if !hasRequestValue {
				value = req.Domain
			}
			value, err = resolveDomainPlaceholder(ph, value)
		case render.TypeTimezone:
			if !hasRequestValue {
				value = settingsTimezone
			}
			value, err = resolveTimezone(value, p.stackPath, tzDeps)
		case render.TypePath:
			value, err = p.resolvePathPlaceholder(ph, value, hasRequestValue)
		case render.TypeString:
			value, err = resolveStringPlaceholder(ph, value, hasRequestValue)
		case render.TypeBool:
			value, err = resolveBoolPlaceholder(ph, value, hasRequestValue)
		case render.TypePort:
			value, err = resolvePortPlaceholder(ph, value, hasRequestValue)
		default:
			err = catalogVerificationError(
				"catalog contains an unknown placeholder type",
				"refresh the catalog and retry",
				fmt.Errorf("placeholder %q has type %q", ph.Name, ph.Type),
			)
		}
		if err != nil {
			return err
		}
		if render.Type(ph.Type) == render.TypeDomain && p.selectedDomain == "" {
			p.selectedDomain = value
		}
		p.resolvedValues[ph.Name] = value
	}

	if err := p.addSyntheticResolvedValue("UID", strconv.Itoa(os.Getuid())); err != nil {
		return err
	}
	return p.addSyntheticResolvedValue("GID", strconv.Itoa(os.Getgid()))
}

func (p *installPlan) addSyntheticResolvedValue(name, value string) error {
	if _, ok := p.resolvedValues[name]; ok {
		return catalogVerificationError(
			"catalog placeholder collides with a built-in template value",
			"refresh the catalog and retry",
			fmt.Errorf("placeholder %q is reserved by wdm", name),
		)
	}
	p.placeholders = append(p.placeholders, render.Placeholder{
		Name:     name,
		Type:     render.TypeString,
		Required: true,
	})
	p.resolvedValues[name] = value
	return nil
}

func resolveDomainPlaceholder(ph catalog.Placeholder, value string) (string, error) {
	if value == "" && ph.Required {
		return "", usageValidationError(
			"domain is required",
			"pass a domain for the selected app",
			fmt.Errorf("placeholder %q is required", ph.Name),
		)
	}
	normalized, err := normalizeDomain(value)
	if err != nil {
		return "", usageValidationError(
			"domain is invalid",
			"pass an ASCII hostname such as app.example.com",
			err,
		)
	}
	return normalized, nil
}

func normalizeDomain(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("domain must not be empty")
	}
	if strings.Contains(value, "://") || strings.ContainsAny(value, "/:@") {
		return "", fmt.Errorf("domain %q must be a hostname, not a URL", value)
	}
	if err := validateDomainASCII(value); err != nil {
		return "", err
	}
	host := strings.ToLower(strings.TrimSuffix(value, "."))
	if host == "" || host == "localhost" || strings.HasPrefix(host, "*.") {
		return "", fmt.Errorf("domain %q is not allowed", value)
	}
	if ip := net.ParseIP(host); ip != nil {
		return "", fmt.Errorf("domain %q must not be an IP literal", value)
	}
	if len(host) > 253 {
		return "", fmt.Errorf("domain %q is too long", value)
	}
	if err := validateDomainLabels(host); err != nil {
		return "", err
	}
	return host, nil
}

func validateDomainASCII(value string) error {
	for _, r := range value {
		if r > 127 {
			return fmt.Errorf("domain %q must be ASCII", value)
		}
	}
	return nil
}

func validateDomainLabels(host string) error {
	for _, label := range strings.Split(host, ".") {
		if err := validateDomainLabel(label); err != nil {
			return err
		}
	}
	return nil
}

func validateDomainLabel(label string) error {
	if len(label) == 0 || len(label) > 63 {
		return fmt.Errorf("domain label %q has invalid length", label)
	}
	for i, r := range label {
		isLetter := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		isHyphen := r == '-'
		if !isLetter && !isDigit && !isHyphen {
			return fmt.Errorf("domain label %q contains invalid character %q", label, r)
		}
		if isHyphen && (i == 0 || i == len(label)-1) {
			return fmt.Errorf("domain label %q must not start or end with hyphen", label)
		}
	}
	return nil
}

func resolveTimezone(value, _ string, deps timezoneLookupDeps) (string, error) {
	deps = completeTimezoneLookupDeps(deps)
	if value != "" {
		return validateTimezone(value, deps)
	}
	if envTZ, ok := deps.LookupEnv("TZ"); ok && strings.TrimSpace(envTZ) != "" {
		return validateTimezone(strings.TrimSpace(envTZ), deps)
	}
	if raw, err := deps.ReadFile("/etc/timezone"); err == nil {
		if tz := strings.TrimSpace(string(raw)); tz != "" {
			return validateTimezone(tz, deps)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", usageValidationError(
			"timezone could not be detected",
			"set timezone in config.toml",
			err,
		)
	}
	if link, err := deps.ReadLink("/etc/localtime"); err == nil {
		if tz, ok := timezoneFromLocaltimeLink(link); ok {
			return validateTimezone(tz, deps)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", usageValidationError(
			"timezone could not be detected",
			"set timezone in config.toml",
			err,
		)
	}
	return "", types.NewError(
		types.ErrCodeUsageValidation,
		"timezone could not be detected",
		"set timezone in config.toml",
	)
}

func completeTimezoneLookupDeps(deps timezoneLookupDeps) timezoneLookupDeps {
	if deps.LookupEnv == nil {
		deps.LookupEnv = os.LookupEnv
	}
	if deps.ReadFile == nil {
		deps.ReadFile = os.ReadFile
	}
	if deps.ReadLink == nil {
		deps.ReadLink = os.Readlink
	}
	if deps.LoadLocation == nil {
		deps.LoadLocation = time.LoadLocation
	}
	return deps
}

func validateTimezone(value string, deps timezoneLookupDeps) (string, error) {
	tz := strings.TrimSpace(value)
	if tz == "" {
		return "", types.NewError(
			types.ErrCodeUsageValidation,
			"timezone is invalid",
			"set timezone in config.toml",
		)
	}
	if _, err := deps.LoadLocation(tz); err != nil {
		return "", usageValidationError(
			"timezone is invalid",
			"set timezone to a valid IANA timezone such as Europe/Bratislava",
			err,
		)
	}
	return tz, nil
}

func timezoneFromLocaltimeLink(link string) (string, bool) {
	const marker = "zoneinfo/"
	idx := strings.LastIndex(link, marker)
	if idx < 0 {
		return "", false
	}
	tz := strings.TrimPrefix(link[idx:], marker)
	return tz, tz != ""
}

func (p *installPlan) resolvePathPlaceholder(ph catalog.Placeholder, value string, hasRequestValue bool) (string, error) {
	if !hasRequestValue || value == "" {
		if ph.Required {
			return "", usageValidationError(
				"path placeholder is required",
				"pass the required host path for this app",
				fmt.Errorf("placeholder %q is required", ph.Name),
			)
		}
		defaultValue, ok := stringDefault(ph.Default)
		if !ok || defaultValue == "" {
			return "", nil
		}
		value = defaultValue
	}
	expanded, err := expandHome(value)
	if err != nil {
		return "", fmt.Errorf("core.install: expanding path placeholder %q: %w", ph.Name, err)
	}
	if !filepath.IsAbs(expanded) {
		return "", usageValidationError(
			"path placeholder must be absolute",
			"pass an absolute host path",
			fmt.Errorf("placeholder %q has relative path %q", ph.Name, value),
		)
	}
	resolved, err := filepath.EvalSymlinks(expanded)
	if err != nil {
		return "", usageValidationError(
			"path placeholder does not exist",
			"create the host path or choose an existing directory",
			err,
		)
	}
	if err := security.EnsureWithinRoot(filepath.Clean(p.stackPath), filepath.Clean(resolved)); err == nil {
		return "", usageValidationError(
			"path placeholder must be outside the stack directory",
			"choose a host path outside the managed stack",
			fmt.Errorf("placeholder %q path %q is inside stack %q", ph.Name, resolved, p.stackPath),
		)
	}
	return resolved, nil
}

func resolveStringPlaceholder(ph catalog.Placeholder, value string, hasRequestValue bool) (string, error) {
	if hasRequestValue {
		return value, validateStringPlaceholderValue(ph.Name, value)
	}
	if value, ok := stringDefault(ph.Default); ok {
		return value, validateStringPlaceholderValue(ph.Name, value)
	}
	if ph.Required {
		return "", usageValidationError(
			"placeholder value is required",
			"pass all required placeholder values for this app",
			fmt.Errorf("placeholder %q is required", ph.Name),
		)
	}
	return "", nil
}

// validateStringPlaceholderValue rejects CR/LF/NUL in a string placeholder
// value before it reaches the .env template. These control characters have no
// legitimate place in an env value and a newline would let a single --set value
// inject extra KEY=VALUE lines (overriding later secrets), so it fails closed.
func validateStringPlaceholderValue(name, value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return usageValidationError(
			"placeholder value contains control characters",
			"remove carriage return, newline, or NUL characters from the value",
			fmt.Errorf("placeholder %q value contains a control character", name),
		)
	}
	return nil
}

func resolveBoolPlaceholder(ph catalog.Placeholder, value string, hasRequestValue bool) (string, error) {
	if !hasRequestValue {
		if ph.Default != nil {
			value = fmt.Sprint(ph.Default)
		} else if ph.Required {
			return "", usageValidationError(
				"boolean placeholder value is required",
				"pass true or false",
				fmt.Errorf("placeholder %q is required", ph.Name),
			)
		}
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return "", usageValidationError(
			"boolean placeholder value is invalid",
			"pass true or false",
			err,
		)
	}
	return strconv.FormatBool(parsed), nil
}

func resolvePortPlaceholder(ph catalog.Placeholder, value string, hasRequestValue bool) (string, error) {
	if !hasRequestValue {
		if ph.Default != nil {
			value = fmt.Sprint(ph.Default)
		} else if ph.Required {
			return "", usageValidationError(
				"port placeholder value is required",
				"pass a port between 1 and 65535",
				fmt.Errorf("placeholder %q is required", ph.Name),
			)
		}
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return "", usageValidationError(
			"port placeholder value is invalid",
			"pass a port between 1 and 65535",
			fmt.Errorf("placeholder %q has invalid port %q", ph.Name, value),
		)
	}
	return strconv.Itoa(port), nil
}

func stringDefault(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	return fmt.Sprint(v), true
}

// planPorts builds the local port bindings for the install plan from the
// verified catalog. The bind interface is derived SOLELY from the catalog
// port.public field (PRD §11.1): a public port binds 0.0.0.0, every other
// port binds 127.0.0.1. No InstallRequest field influences the interface,
// so a user cannot force a public bind — only a signed catalog can. Range
// ports (host_range/container_range) are expanded to one binding per port
// so availability probing, the deploy confirmation, and the rendered-compose
// public-bind scan all operate on the exact set of ports Compose will bind.
// A public declaration for an app admin/web-UI port is refused as a
// defense-in-depth backstop (PRD §11.1(d)) before any port is probed.
func (p *installPlan) planPorts(ctx context.Context) error {
	seen := map[string]struct{}{}
	var planned []types.PortBinding
	publicPorts := map[int]struct{}{}
	for _, port := range p.app.Ports {
		bindings, err := portBindings(port)
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			key := fmt.Sprintf("%s/%d", binding.Protocol, binding.HostPort)
			if _, ok := seen[key]; ok {
				return catalogVerificationError(
					"catalog contains duplicate host ports",
					"refresh the catalog and retry",
					fmt.Errorf("duplicate host port %s", key),
				)
			}
			seen[key] = struct{}{}
			if port.Public {
				publicPorts[binding.HostPort] = struct{}{}
			}
			planned = append(planned, binding)
		}
	}

	// Admin-port detection falls back to the first planned port when the app
	// declares no local_target_url_template, so identify admin ports only
	// after the full plan is known. The refusal precedes any availability
	// probe so a catalog defect fails fast (PRD §11.1(d)).
	if err := refusePublicAdminPorts(p, planned, publicPorts); err != nil {
		return err
	}

	for _, binding := range planned {
		if err := p.probePort(ctx, binding); err != nil {
			return err
		}
		p.localPorts = append(p.localPorts, binding)
	}
	return nil
}

// refusePublicAdminPorts refuses any public-declared host port that is also
// the app's web-UI/admin surface (PRD §11.1(d)). Admin surfaces stay
// localhost-only and front a reverse proxy; the primary protection is that
// admin ports are simply not declared public, and this is the backstop.
func refusePublicAdminPorts(plan *installPlan, planned []types.PortBinding, publicPorts map[int]struct{}) error {
	if len(publicPorts) == 0 {
		return nil
	}
	adminPorts, err := identifyAdminHostPorts(plan, planned, publicPorts)
	if err != nil {
		return err
	}
	for _, binding := range planned {
		if _, isPublic := publicPorts[binding.HostPort]; !isPublic {
			continue
		}
		if _, isAdmin := adminPorts[binding.HostPort]; !isAdmin {
			continue
		}
		return catalogVerificationError(
			"catalog declares an admin port public",
			"keep the web-UI/admin port localhost-only and front it with a reverse proxy",
			fmt.Errorf(
				"service %q host port %d is the app's web-UI/admin surface and must not be declared public",
				binding.Service,
				binding.HostPort,
			),
		)
	}
	return nil
}

// portBindings expands one catalog port entry into its concrete host
// bindings. A plain entry (no range fields) yields one binding from
// Host/Container. A range entry carries host_range/container_range alongside
// Host/Container, where Host and Container are the range low ends (the schema
// contract): it is expanded to one binding per port in the span. A range whose
// Host/Container do not equal the declared range low ends is a malformed
// mix and is refused. The bind interface is set from port.public per PRD §11.1.
func portBindings(port catalog.Port) ([]types.PortBinding, error) {
	protocol := port.Protocol
	if protocol == "" {
		protocol = "tcp"
	}
	hostIP := "127.0.0.1"
	if port.Public {
		hostIP = "0.0.0.0"
	}

	if port.HostRange != "" || port.ContainerRange != "" {
		return rangePortBindings(port, protocol, hostIP)
	}

	if port.Host < 1 || port.Host > 65535 || port.Container < 1 || port.Container > 65535 {
		return nil, catalogVerificationError(
			"catalog contains an invalid port",
			"refresh the catalog and retry",
			fmt.Errorf("service %q has host/container ports %d/%d", port.Service, port.Host, port.Container),
		)
	}
	return []types.PortBinding{{
		Service:       port.Service,
		HostIP:        hostIP,
		HostPort:      port.Host,
		ContainerPort: port.Container,
		Protocol:      protocol,
	}}, nil
}

// rangePortBindings validates a host_range/container_range pair and expands
// it to one binding per port. Both bounds must lie in 1..65535, lo<=hi, and
// the host and container spans must be equal length so the port-for-port
// mapping is well defined (the contract documented on [catalog.Port]). The
// schema pairs each range with Host/Container; those must equal the range low
// ends, otherwise the entry is a malformed single+range mix.
func rangePortBindings(port catalog.Port, protocol, hostIP string) ([]types.PortBinding, error) {
	if port.HostRange == "" || port.ContainerRange == "" {
		return nil, catalogVerificationError(
			"catalog port range is incomplete",
			"refresh the catalog and retry",
			fmt.Errorf(
				"service %q must declare both host_range and container_range (got %q/%q)",
				port.Service,
				port.HostRange,
				port.ContainerRange,
			),
		)
	}
	hostLo, hostHi, err := parsePortRange(port.Service, port.HostRange)
	if err != nil {
		return nil, err
	}
	containerLo, containerHi, err := parsePortRange(port.Service, port.ContainerRange)
	if err != nil {
		return nil, err
	}
	if hostHi-hostLo != containerHi-containerLo {
		return nil, catalogVerificationError(
			"catalog port range spans do not match",
			"refresh the catalog and retry",
			fmt.Errorf(
				"service %q host range %q and container range %q have different lengths",
				port.Service,
				port.HostRange,
				port.ContainerRange,
			),
		)
	}
	// The schema sets Host/Container to the range low ends; a contradiction is
	// a malformed single+range mix that would make the bound ambiguous.
	if (port.Host != 0 && port.Host != hostLo) || (port.Container != 0 && port.Container != containerLo) {
		return nil, catalogVerificationError(
			"catalog port mixes single and range declarations",
			"refresh the catalog and retry",
			fmt.Errorf(
				"service %q single ports (%d/%d) do not match range low ends (%d/%d)",
				port.Service,
				port.Host,
				port.Container,
				hostLo,
				containerLo,
			),
		)
	}
	bindings := make([]types.PortBinding, 0, hostHi-hostLo+1)
	for offset := 0; hostLo+offset <= hostHi; offset++ {
		bindings = append(bindings, types.PortBinding{
			Service:       port.Service,
			HostIP:        hostIP,
			HostPort:      hostLo + offset,
			ContainerPort: containerLo + offset,
			Protocol:      protocol,
		})
	}
	return bindings, nil
}

// parsePortRange parses an inclusive "<lo>-<hi>" port span, enforcing both
// bounds in 1..65535 and lo<=hi. A malformed span is a catalog defect.
func parsePortRange(service, spec string) (lo, hi int, err error) {
	rangeErr := func(cause error) error {
		return catalogVerificationError(
			"catalog contains an invalid port range",
			"refresh the catalog and retry",
			cause,
		)
	}
	before, after, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, rangeErr(fmt.Errorf("service %q has malformed port range %q", service, spec))
	}
	lo, loErr := strconv.Atoi(before)
	hi, hiErr := strconv.Atoi(after)
	if loErr != nil || hiErr != nil {
		return 0, 0, rangeErr(fmt.Errorf("service %q has non-numeric port range %q", service, spec))
	}
	if lo < 1 || lo > 65535 || hi < 1 || hi > 65535 {
		return 0, 0, rangeErr(fmt.Errorf("service %q port range %q is out of the 1..65535 bounds", service, spec))
	}
	if lo > hi {
		return 0, 0, rangeErr(fmt.Errorf("service %q port range %q has lo greater than hi", service, spec))
	}
	return lo, hi, nil
}

// identifyAdminHostPorts collects the host ports wdm treats as the app's
// web-UI/admin surface (PRD §11.1(d)). The primary signal is the host port
// embedded in the rendered local_target_url_template; when the app declares
// no template, the local target URL falls back to the first NON-public planned
// port, so that port is the admin surface. A public-declared port is, by
// §11.1, deliberately public and is never the admin surface, so the fallback
// skips public ports — otherwise a public-first app whose first port is its
// data port (no web UI, no local_target_url_template) would be mis-refused. If
// every planned port is public, the fallback contributes no admin port. The
// PangolinGuidance.TargetURL host port is included when set and parseable.
// These ports must stay localhost-only, so a public declaration for any of
// them is refused.
func identifyAdminHostPorts(
	plan *installPlan,
	planned []types.PortBinding,
	publicPorts map[int]struct{},
) (map[int]struct{}, error) {
	admin := map[int]struct{}{}
	if plan.app.LocalTargetURLTemplate == "" {
		for _, binding := range planned {
			if _, isPublic := publicPorts[binding.HostPort]; isPublic {
				continue
			}
			admin[binding.HostPort] = struct{}{}
			break
		}
	} else {
		localTargetURL, err := renderInstallLocalTargetURL(plan)
		if err != nil {
			return nil, err
		}
		if port, ok := hostPortFromURL(localTargetURL); ok {
			admin[port] = struct{}{}
		}
	}
	if port, ok := hostPortFromURL(plan.app.PangolinGuidance.TargetURL); ok {
		admin[port] = struct{}{}
	}
	return admin, nil
}

// hostPortFromURL extracts the numeric host port from a service URL, if
// present and parseable. A URL without an explicit port (and any value that
// does not parse) yields ok=false, because no admin port can be derived from
// it — the public-bind refusal then has nothing to match against.
func hostPortFromURL(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	portText := parsed.Port()
	if portText == "" {
		return 0, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, false
	}
	return port, true
}

// recheckPorts re-verifies every planned localhost port immediately
// before the deployment point — the second of the two
// checks that closes the TOCTOU window between planning and
// `docker compose up -d`. Conflicts surface as
// [types.ErrCodeUsageValidation] with the port named in the hint.
func (p *installPlan) recheckPorts(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, binding := range p.localPorts {
		if err := p.probePort(ctx, binding); err != nil {
			return err
		}
	}
	return nil
}

func checkPortAvailable(ctx context.Context, binding types.PortBinding) error {
	addr := net.JoinHostPort(binding.HostIP, strconv.Itoa(binding.HostPort))
	var listenConfig net.ListenConfig
	switch binding.Protocol {
	case "tcp":
		ln, err := listenConfig.Listen(ctx, "tcp", addr)
		if err != nil {
			return classifyPortBindError(binding.HostPort, err)
		}
		if err := ln.Close(); err != nil {
			return fmt.Errorf("core.install: closing port probe listener: %w", err)
		}
	case "udp":
		conn, err := listenConfig.ListenPacket(ctx, "udp", addr)
		if err != nil {
			return classifyPortBindError(binding.HostPort, err)
		}
		if err := conn.Close(); err != nil {
			return fmt.Errorf("core.install: closing port probe listener: %w", err)
		}
	default:
		return catalogVerificationError(
			"catalog contains an invalid port protocol",
			"refresh the catalog and retry",
			fmt.Errorf("service %q has protocol %q", binding.Service, binding.Protocol),
		)
	}
	return nil
}

// classifyPortBindError turns a failed localhost-port probe into a
// typed [types.ErrCodeUsageValidation] error, distinguishing an EACCES
// bind (the port needs elevated privileges, e.g. a curated sub-1024
// host port) from an already-in-use bind. wdm runs unprivileged by
// invariant (PRD §11), so a sub-1024 bind reports honestly that the
// port requires elevated privileges and the hint suggests an
// unprivileged (>1024) port rather than the misleading "already in
// use" text. Every other bind failure keeps the byte-compatible
// already-in-use message. The error code is the same on both arms so
// the PRD §27 exit-code mapping is unchanged.
// It is split out from [checkPortAvailable] so the classification can
// be unit-tested against constructed wrapped syscall errors — a real
// sub-1024 bind is not portable (macOS permits unprivileged low-port
// binds and CI may run as root). errors.Is walks the net.OpError →
// os.SyscallError → syscall.Errno chain that the listener returns.
func classifyPortBindError(hostPort int, err error) error {
	if errors.Is(err, syscall.EACCES) {
		return usageValidationError(
			"local port requires elevated privileges",
			fmt.Sprintf(
				"127.0.0.1:%d needs elevated privileges to bind; choose an unprivileged port above 1024",
				hostPort,
			),
			err,
		)
	}
	return usageValidationError(
		"local port is already in use",
		fmt.Sprintf("free 127.0.0.1:%d or choose another port", hostPort),
		err,
	)
}

func (p *installPlan) planResources(
	req types.InstallRequest,
	host system.HostResources,
	onProgress types.ProgressFn,
) error {
	if len(p.app.Resources) == 0 {
		return nil
	}
	if err := validateInstallResourceInputs(req, host); err != nil {
		return err
	}
	profiles, err := indexResourceProfiles(p.app.Resources)
	if err != nil {
		return err
	}

	recMemory, recCPU, err := sumResourceBand(p.app.Resources, types.ResourceProfileRecommended)
	if err != nil {
		return err
	}
	// Recommended totals are persisted into .wdm.lock so status, update, and
	// future planning surfaces can report the catalog's normal sizing guidance.
	// They are not a hard host-capacity reservation; Docker resource limits are
	// caps, not guaranteed allocations.
	p.recommendedResources = &state.RecommendedResources{
		MemoryBytes: recMemory,
		CPUs:        recCPU,
	}

	useMin, err := p.chooseMinimumResourceProfile(req, host, onProgress)
	if err != nil {
		return err
	}
	selected := selectResourceProfiles(p.app.Resources, useMin)
	if err := applyResourceOverrides(selected, profiles, req.ResourceOverrides); err != nil {
		return err
	}
	return p.addSelectedResourceValues(selected)
}

func validateInstallResourceInputs(req types.InstallRequest, host system.HostResources) error {
	if host.CPUCores <= 0 || host.TotalMemoryBytes == 0 {
		return usageValidationError(
			"host resources could not be detected",
			"run wdm on a supported host or retry after fixing host resource detection",
			fmt.Errorf("cpu=%d memory=%d", host.CPUCores, host.TotalMemoryBytes),
		)
	}
	if req.ResourceProfile == "" ||
		req.ResourceProfile == types.ResourceProfileRecommended ||
		req.ResourceProfile == types.ResourceProfileMin {
		return nil
	}
	return usageValidationError(
		"resource profile is invalid",
		"choose recommended or min",
		fmt.Errorf("unknown resource profile %q", req.ResourceProfile),
	)
}

func indexResourceProfiles(resources []catalog.ResourceProfile) (map[string]catalog.ResourceProfile, error) {
	profiles := make(map[string]catalog.ResourceProfile, len(resources))
	serviceKeys := map[string]string{}
	for _, profile := range resources {
		if err := indexResourceProfile(profiles, serviceKeys, profile); err != nil {
			return nil, err
		}
	}
	return profiles, nil
}

func indexResourceProfile(
	profiles map[string]catalog.ResourceProfile,
	serviceKeys map[string]string,
	profile catalog.ResourceProfile,
) error {
	if _, ok := profiles[profile.Service]; ok {
		return catalogVerificationError(
			"catalog contains duplicate resource profiles",
			"refresh the catalog and retry",
			fmt.Errorf("duplicate resource service %q", profile.Service),
		)
	}
	key := serviceKey(profile.Service)
	if key == "" {
		return catalogVerificationError(
			"catalog contains an invalid resource service",
			"refresh the catalog and retry",
			fmt.Errorf("service %q derives an empty service key", profile.Service),
		)
	}
	if other, ok := serviceKeys[key]; ok {
		return catalogVerificationError(
			"catalog contains colliding resource service keys",
			"refresh the catalog and retry",
			fmt.Errorf("services %q and %q both derive %q", other, profile.Service, key),
		)
	}
	serviceKeys[key] = profile.Service
	profiles[profile.Service] = profile
	return nil
}

func (p *installPlan) chooseMinimumResourceProfile(
	req types.InstallRequest,
	host system.HostResources,
	onProgress types.ProgressFn,
) (bool, error) {
	availableMemory, availableCPU := installResourceGuidanceBudget(host)
	recMemory := p.recommendedResources.MemoryBytes
	recCPU := p.recommendedResources.CPUs
	useMin := req.ResourceProfile == types.ResourceProfileMin
	if !useMin {
		useMin = recMemory > availableMemory || recCPU > availableCPU
	}
	if !useMin {
		return false, nil
	}
	if _, _, err := sumResourceBand(p.app.Resources, types.ResourceProfileMin); err != nil {
		return false, err
	}
	if req.ResourceProfile != types.ResourceProfileMin && onProgress != nil {
		onProgress(types.StepInstallResourceDegraded, 15, "using minimum resource profile")
	}
	return true, nil
}

func selectResourceProfiles(
	resources []catalog.ResourceProfile,
	useMin bool,
) map[string]selectedResource {
	selected := make(map[string]selectedResource, len(resources))
	for _, profile := range resources {
		chosen := selectedResource{
			memory: profile.Memory.Recommended,
			cpus:   profile.CPUs.Recommended,
			pids:   profile.PIDs.Default,
		}
		if useMin {
			chosen.memory = profile.Memory.Min
			chosen.cpus = profile.CPUs.Min
		}
		selected[profile.Service] = chosen
	}
	return selected
}

func applyResourceOverrides(
	selected map[string]selectedResource,
	profiles map[string]catalog.ResourceProfile,
	overrides []types.ResourceOverride,
) error {
	for _, override := range overrides {
		if err := applyResourceOverride(selected, profiles, override); err != nil {
			return err
		}
	}
	return nil
}

func applyResourceOverride(
	selected map[string]selectedResource,
	profiles map[string]catalog.ResourceProfile,
	override types.ResourceOverride,
) error {
	profile, ok := profiles[override.Service]
	if !ok {
		return usageValidationError(
			"resource override targets an unknown service",
			"choose a service declared by the selected app",
			fmt.Errorf("unknown service %q", override.Service),
		)
	}
	if !profile.AllowOverride {
		return usageValidationError(
			"resource override is not allowed for this service",
			"remove the resource override for this service",
			fmt.Errorf("service %q disallows overrides", override.Service),
		)
	}
	chosen := selected[override.Service]
	var err error
	chosen, err = applyResourceLimitOverride(chosen, profile, override)
	if err != nil {
		return err
	}
	selected[override.Service] = chosen
	return nil
}

func applyResourceLimitOverride(
	chosen selectedResource,
	profile catalog.ResourceProfile,
	override types.ResourceOverride,
) (selectedResource, error) {
	if override.Memory != "" {
		if err := validateMemoryOverride(profile, override.Memory); err != nil {
			return selectedResource{}, err
		}
		chosen.memory = override.Memory
	}
	if override.CPUs != "" {
		if err := validateCPUOverride(profile, override.CPUs); err != nil {
			return selectedResource{}, err
		}
		chosen.cpus = override.CPUs
	}
	if override.PIDs != 0 {
		if err := validatePIDsOverride(profile, override.PIDs); err != nil {
			return selectedResource{}, err
		}
		chosen.pids = override.PIDs
	}
	return chosen, nil
}

func validatePIDsOverride(profile catalog.ResourceProfile, pids int) error {
	if pids >= 1 && pids <= profile.PIDs.Max {
		return nil
	}
	return usageValidationError(
		fmt.Sprintf("pids limit must be between 1 and %d", profile.PIDs.Max),
		fmt.Sprintf("choose a pids value between 1 and %d for %s", profile.PIDs.Max, profile.Service),
		fmt.Errorf("service %q pids override %d is outside [1,%d]", profile.Service, pids, profile.PIDs.Max),
	)
}

func (p *installPlan) addSelectedResourceValues(selected map[string]selectedResource) error {
	for _, profile := range p.app.Resources {
		key := serviceKey(profile.Service)
		chosen := selected[profile.Service]
		if err := p.addSyntheticResolvedValue("MEMORY_LIMIT_"+key, chosen.memory); err != nil {
			return err
		}
		if err := p.addSyntheticResolvedValue("CPUS_LIMIT_"+key, chosen.cpus); err != nil {
			return err
		}
		if err := p.addSyntheticResolvedValue("PIDS_LIMIT_"+key, strconv.Itoa(chosen.pids)); err != nil {
			return err
		}
	}
	return nil
}

type selectedResource struct {
	memory string
	cpus   string
	pids   int
}

func installResourceGuidanceBudget(host system.HostResources) (uint64, float64) {
	memoryBudget := uint64(0)
	if host.TotalMemoryBytes > installHostMemoryReserveBytes {
		memoryBudget = host.TotalMemoryBytes - installHostMemoryReserveBytes
	}
	return memoryBudget, float64(host.CPUCores)
}

func sumResourceBand(resources []catalog.ResourceProfile, profile types.ResourceProfile) (uint64, float64, error) {
	var memory uint64
	var cpus float64
	for _, resource := range resources {
		var memText, cpuText string
		switch profile {
		case types.ResourceProfileRecommended:
			memText = resource.Memory.Recommended
			cpuText = resource.CPUs.Recommended
		case types.ResourceProfileMin:
			memText = resource.Memory.Min
			cpuText = resource.CPUs.Min
		default:
			return 0, 0, usageValidationError(
				"resource profile is invalid",
				"choose recommended or min",
				fmt.Errorf("unknown resource profile %q", profile),
			)
		}
		memBytes, err := parseMemoryBytes(memText)
		if err != nil {
			return 0, 0, catalogVerificationError(
				"catalog contains an invalid memory limit",
				"refresh the catalog and retry",
				err,
			)
		}
		cpuValue, err := strconv.ParseFloat(cpuText, 64)
		if err != nil || cpuValue <= 0 {
			return 0, 0, catalogVerificationError(
				"catalog contains an invalid cpu limit",
				"refresh the catalog and retry",
				fmt.Errorf("cpu limit %q is invalid", cpuText),
			)
		}
		memory += memBytes
		cpus += cpuValue
	}
	return memory, cpus, nil
}

func validateMemoryOverride(profile catalog.ResourceProfile, value string) error {
	got, err := parseMemoryBytes(value)
	if err != nil {
		return usageValidationError(
			"memory override is invalid",
			"pass a Docker memory value such as 512m or 1g",
			err,
		)
	}
	minValue, err := parseMemoryBytes(profile.Memory.Min)
	if err != nil {
		return catalogVerificationError("catalog contains an invalid memory limit", "refresh the catalog and retry", err)
	}
	maxValue, err := parseMemoryBytes(profile.Memory.Max)
	if err != nil {
		return catalogVerificationError("catalog contains an invalid memory limit", "refresh the catalog and retry", err)
	}
	if got < minValue || got > maxValue {
		return usageValidationError(
			fmt.Sprintf("memory limit must be between %s and %s", profile.Memory.Min, profile.Memory.Max),
			fmt.Sprintf("choose memory between %s and %s for %s", profile.Memory.Min, profile.Memory.Max, profile.Service),
			fmt.Errorf("service %q memory override %q outside [%s,%s]", profile.Service, value, profile.Memory.Min, profile.Memory.Max),
		)
	}
	return nil
}

func validateCPUOverride(profile catalog.ResourceProfile, value string) error {
	got, err := strconv.ParseFloat(value, 64)
	if err != nil || got <= 0 {
		return usageValidationError(
			"cpu override is invalid",
			"pass a positive decimal CPU value such as 0.5 or 1.0",
			fmt.Errorf("cpu override %q is invalid", value),
		)
	}
	minValue, err := strconv.ParseFloat(profile.CPUs.Min, 64)
	if err != nil {
		return catalogVerificationError("catalog contains an invalid cpu limit", "refresh the catalog and retry", err)
	}
	maxValue, err := strconv.ParseFloat(profile.CPUs.Max, 64)
	if err != nil {
		return catalogVerificationError("catalog contains an invalid cpu limit", "refresh the catalog and retry", err)
	}
	if got < minValue || got > maxValue {
		return usageValidationError(
			fmt.Sprintf("cpus limit must be between %s and %s", profile.CPUs.Min, profile.CPUs.Max),
			fmt.Sprintf("choose cpus between %s and %s for %s", profile.CPUs.Min, profile.CPUs.Max, profile.Service),
			fmt.Errorf("service %q cpu override %q outside [%s,%s]", profile.Service, value, profile.CPUs.Min, profile.CPUs.Max),
		)
	}
	return nil
}

func parseMemoryBytes(value string) (uint64, error) {
	if value == "" {
		return 0, fmt.Errorf("memory value must not be empty")
	}
	unit := value[len(value)-1]
	multiplier := uint64(1)
	var number string
	switch unit {
	case 'b':
		number = value[:len(value)-1]
	case 'k':
		number = value[:len(value)-1]
		multiplier = 1024
	case 'm':
		number = value[:len(value)-1]
		multiplier = 1024 * 1024
	case 'g':
		number = value[:len(value)-1]
		multiplier = 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("memory value %q must end in b, k, m, or g", value)
	}
	amount, err := strconv.ParseUint(number, 10, 64)
	if err != nil || amount == 0 {
		return 0, fmt.Errorf("memory value %q is invalid", value)
	}
	if amount > math.MaxUint64/multiplier {
		return 0, fmt.Errorf("memory value %q overflows uint64", value)
	}
	return amount * multiplier, nil
}

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

// completedServiceNamePattern is the conservative Compose service-name
// shape a completed_services entry must match: a leading alphanumeric
// followed by alphanumerics, underscores, dots, or hyphens. It rejects
// empty, whitespace, and path-like names before the membership checks
// run, so a tampered catalog cannot smuggle an odd name past the
// cross-reference.
var completedServiceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// verifyCompletedServicesMatchCatalog cross-checks every catalog
// completed_services entry against the install's authoritative service
// sets before the manifest is written. A name earns the
// completed-by-design exemption from container_exited only when it (1)
// matches the conservative Compose service-name shape, (2) is pinned in
// image_pins, and (3) renders as a real service in the compose template
// (serviceLabels keys, populated before the verify chain runs). Any
// other name fails the install closed via the shared
// [catalogVerificationError] so a drifted or tampered catalog can never
// mark an unknown — or unexpected-exit — service "completed". An empty
// completed_services list is valid and verifies as a no-op.
func verifyCompletedServicesMatchCatalog(app catalog.App, serviceLabels map[string]map[string]string) error {
	if len(app.CompletedServices) == 0 {
		return nil
	}

	pinned := make(map[string]struct{}, len(app.ImagePins))
	for _, pin := range app.ImagePins {
		if pin.Service == "" {
			continue
		}
		pinned[pin.Service] = struct{}{}
	}

	for _, service := range app.CompletedServices {
		if !completedServiceNamePattern.MatchString(service) {
			return catalogVerificationError(
				"catalog completed_services names an invalid compose service",
				"list only plain compose service names in completed_services",
				fmt.Errorf(
					"app %q completed service %q is not a valid compose service name",
					app.AppID,
					service,
				),
			)
		}
		if _, ok := pinned[service]; !ok {
			return catalogVerificationError(
				"catalog completed_services names a service absent from image_pins",
				"every completed service must also be pinned in image_pins",
				fmt.Errorf(
					"app %q lists completed service %q with no matching image_pins entry",
					app.AppID,
					service,
				),
			)
		}
		if _, ok := serviceLabels[service]; !ok {
			return catalogVerificationError(
				"catalog completed_services names a service absent from the rendered compose",
				"align completed_services with the compose template service names",
				fmt.Errorf(
					"app %q lists completed service %q with no matching rendered compose service",
					app.AppID,
					service,
				),
			)
		}
	}
	return nil
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
