package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// composeEnvFileProjection is the minimal slice of a rendered
// docker-compose.yml needed to read each service's env_file entries.
// yaml.v3 accepts both the scalar (env_file: .env.user) and sequence
// (env_file: [.env.user]) Compose forms because the field is decoded as
// a []string with a permissive UnmarshalYAML on the alias type below.
type composeEnvFileProjection struct {
	Services map[string]struct {
		EnvFile composeEnvFileList `yaml:"env_file"`
	} `yaml:"services"`
}

// composeEnvFileList accepts either a single scalar or a sequence for a
// service's env_file, normalizing both into a string slice so the
// detection gate is form-agnostic.
type composeEnvFileList []string

func (l *composeEnvFileList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*l = composeEnvFileList{value.Value}
		return nil
	}
	var seq []string
	if err := value.Decode(&seq); err != nil {
		return err
	}
	*l = seq
	return nil
}

// composeWiresUserEnv reports whether every service in the rendered
// compose lists installEnvUserFilename in its env_file. A stack already
// wired this way needs no rewire; a pre-feature stack (rendered before
// the env_file overlay was added to the templates) reports false. A parse
// failure is surfaced so a corrupt compose never silently rewires.
func composeWiresUserEnv(composeBytes []byte) (bool, error) {
	var projection composeEnvFileProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return false, fmt.Errorf("parse compose for env_file detection: %w", err)
	}
	if len(projection.Services) == 0 {
		return false, nil
	}
	for _, service := range projection.Services {
		wired := false
		for _, entry := range service.EnvFile {
			if strings.TrimSpace(entry) == installEnvUserFilename {
				wired = true
				break
			}
		}
		if !wired {
			return false, nil
		}
	}
	return true, nil
}

// userOverrideHeader seeds a freshly-created docker-compose.override.yml so
// the user sees a documented, ready-to-edit starting point. Native Compose
// merges this file over the wdm-rendered base, so the user can add services,
// volumes, networks, or ports without losing edits on update. The examples
// stay commented so an untouched override is whitespace/comment-only and the
// content-gate skips the extra `-f` until the user adds real content.
const userOverrideHeader = `# docker-compose.override.yml — user-owned overlay (wdm creates, never regenerates).
# Native Compose merges this over the wdm base compose file. Add your own
# services, volumes, networks, or ports here; your edits survive wdm update.
# WARNING: an override can re-add capabilities, expose ports (0.0.0.0), or break
# wdm tracking if you remove wdm.managed labels — single-tenant, your call.
#
# Add a service:
# services:
#   my-extra:
#     image: nginx:alpine
#     restart: unless-stopped
#
# Attach to an external network (e.g. a shared Ollama network):
# networks:
#   ollama-net:
#     external: true
`

// secretishKeyPattern flags env keys whose name implies a secret value, so
// ViewEnvRedacted masks them even when the active redactor has no literal for
// the value (defense-in-depth: a user-added SMTP password in .env.user has no
// catalog placeholder, so only the key heuristic catches it).
var secretishKeyPattern = regexp.MustCompile(`(?i)(password|secret|token|key|salt)`)

// EnsureUserOverride resolves and create-if-missing seeds the user-owned
// docker-compose.override.yml (0644) inside appID's managed stack, returning
// its path. The seeded header documents the overlay and carries commented
// examples; the create is idempotent and NEVER truncates an existing file, so
// repeated edits keep the user's content. The override is not secret-bearing
// (it holds compose structure, not literal secrets), hence 0644 rather than
// the 0600 used for .env.user.
func (e *Engine) EnsureUserOverride(ctx context.Context, appID string) (string, error) {
	stackPath, err := e.resolveUserEditStack(ctx, appID)
	if err != nil {
		return "", err
	}
	return ensureUserOverrideFile(stackPath)
}

// EnsureUserEnv resolves and create-if-missing seeds the user-owned .env.user
// (empty, 0600) inside appID's managed stack, returning its path. It reuses
// the install-time [ensureUserEnvFile] primitive so install, edit, and rewire
// all produce a byte-identical empty file; the file stays empty by design so
// `wdm update` never has content to diverge from.
func (e *Engine) EnsureUserEnv(ctx context.Context, appID string) (string, error) {
	stackPath, err := e.resolveUserEditStack(ctx, appID)
	if err != nil {
		return "", err
	}
	return ensureUserEnvFile(stackPath)
}

// ViewEnvRedacted returns the effective environment of appID's managed stack —
// the base .env merged with the user overlay .env.user — with every secret
// value masked before it leaves the engine. It loads the app and base .env the
// way [Engine.ResourceSettings] does, builds the per-stack active redactor from
// the catalog's secret-typed values, and masks each entry two ways: by literal
// secret-value match (the redactor) and by secret-ish key name. A masked entry
// reports Secret true. The result never carries a raw secret.
func (e *Engine) ViewEnvRedacted(ctx context.Context, appID string) (*types.ViewEnvResult, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if appID == "" {
		return nil, usageValidationError(
			"app id is required",
			"pass the app id of an installed stack",
			nil,
		)
	}

	stackPath, _, err := e.resolveManagedStack(ctx, appID)
	if err != nil {
		return nil, err
	}

	cat, err := e.loadInstallCatalog(ctx)
	if err != nil {
		return nil, err
	}
	app, err := selectCatalogApp(cat, appID)
	if err != nil {
		return nil, err
	}

	baseEnv, err := state.ReadStackEnv(stackPath)
	if err != nil {
		return nil, err
	}
	userEntries, err := readUserEnvEntries(stackPath)
	if err != nil {
		return nil, err
	}

	// .env.user is user-controlled and MAY hold bare secrets (SMTP password,
	// API keys) with non-secret-ish keys that have no catalog placeholder, so
	// fold ALL its values into the redactor too — over-redaction is acceptable
	// and fail-closed. Without this a bare secret-valued user var would render
	// raw in the redacted view.
	userValues := make([]string, 0, len(userEntries))
	for _, kv := range userEntries {
		if kv.value != "" {
			userValues = append(userValues, kv.value)
		}
	}
	redactor := security.NewActiveRedactor(append(collectStackSecretValues(app, baseEnv), userValues...))

	result := &types.ViewEnvResult{
		AppID:   appID,
		Entries: make([]types.EnvEntry, 0, len(baseEnv)+len(userEntries)),
	}
	// baseEnv is a map; sort keys so --json Entries order is stable across calls.
	for _, key := range slices.Sorted(maps.Keys(baseEnv)) {
		result.Entries = append(result.Entries, redactEnvEntry(redactor, key, baseEnv[key]))
	}
	for _, kv := range userEntries {
		result.Entries = append(result.Entries, redactEnvEntry(redactor, kv.key, kv.value))
	}
	return result, nil
}

// ValidateStack runs `docker compose config` against appID's live stack — the
// merged base compose plus the content-gated docker-compose.override.yml —
// and returns any warnings. It lets the CLI and TUI validate after an edit
// WITHOUT importing internal/docker (depguard): both the compose and env edit
// flows call it. A validation failure surfaces as a returned error; the
// caller decides warn-but-allow. The compose-config output is discarded by
// the docker wrapper, so no interpolated secret leaks through.
func (e *Engine) ValidateStack(ctx context.Context, appID string) ([]string, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if appID == "" {
		return nil, usageValidationError(
			"app id is required",
			"pass the app id of an installed stack",
			nil,
		)
	}

	stackPath, _, err := e.resolveManagedStack(ctx, appID)
	if err != nil {
		return nil, err
	}

	redactor, err := validateConfigRedactor(stackPath)
	if err != nil {
		return nil, err
	}
	client, err := e.buildDockerClient(redactor)
	if err != nil {
		return nil, err
	}

	composePath, err := security.SafeJoin(stackPath, installComposeFilename)
	if err != nil {
		return nil, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}

	if validateErr := docker.ValidateComposeConfig(ctx, client, stackPath, composePath); validateErr != nil {
		if ctx.Err() != nil {
			return nil, validateErr
		}
		return nil, fmt.Errorf("%s", redactor.Redact(validateErr.Error()))
	}
	return nil, nil
}

// RewireStack migrates a pre-feature managed stack so its user overlay
// (.env.user) goes live. A stack installed before the env_file overlay
// landed in the catalog templates has an on-disk compose that does not
// reference .env.user, so the user's edits never reach the containers.
// RewireStack detects that case, re-renders the compose from the INSTALLED
// catalog version reusing the existing .env values verbatim, seeds the
// empty .env.user, writes the new compose atomically, and restarts the
// stack so the overlay takes effect.
//
// Safety: the .env is NEVER rewritten, so secrets stay byte-identical;
// the re-render reuses the on-disk resolved values (no secret regeneration,
// no input prompt). Because no historical catalog exists, "installed
// version" is enforced fail-closed by comparing the re-rendered service
// image references against the on-disk ones — any image change means the
// catalog template moved past the installed version, so RewireStack aborts
// and points the user at `wdm update` rather than silently changing images.
//
// Flow: detect -> confirm (destructive: rewrites compose + restarts) ->
// rewire -> restart. An already-wired stack is a no-op: rewired is false,
// nothing is written, and no confirmation is requested. The returned path
// is the resolved .env.user path on a successful rewire (empty on a no-op).
func (e *Engine) RewireStack(
	ctx context.Context,
	appID string,
	confirmer types.Confirmer,
) (rewired bool, path string, err error) {
	if e.isClosed() {
		return false, "", ErrClosed
	}
	if err := requireAppID(appID); err != nil {
		return false, "", err
	}

	lg := e.newOpLogger(e.logger, "rewire")
	lg.start(ctx, appID)

	handle, err := e.acquireRuntimeLock(ctx, "rewire")
	if err != nil {
		lg.failure(ctx, appID, "", "acquire_runtime_lock", err)
		return false, "", err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	stackPath, lock, err := e.resolveManagedStack(ctx, appID)
	if err != nil {
		lg.failure(ctx, appID, "", "resolve_managed_stack", err)
		return false, "", err
	}
	if err := requireComposeProject(lock.ComposeProject, appID); err != nil {
		lg.failure(ctx, appID, stackPath, "require_compose_project", err)
		return false, "", err
	}

	onDiskCompose, err := readStackFile(stackPath, installComposeFilename)
	if err != nil {
		lg.failure(ctx, appID, stackPath, "read_compose", err)
		return false, "", err
	}
	alreadyWired, err := composeWiresUserEnv(onDiskCompose)
	if err != nil {
		lg.failure(ctx, appID, stackPath, "detect_env_file", err)
		return false, "", usageValidationError(
			"stack compose could not be parsed for overlay detection",
			"the stack docker-compose.yml is corrupt; reinstall the app to restore managed state",
			err,
		)
	}
	if alreadyWired {
		lg.success(ctx, appID, stackPath)
		return false, "", nil
	}

	newCompose, err := e.rewireRenderCompose(ctx, appID, stackPath, onDiskCompose)
	if err != nil {
		lg.failure(ctx, appID, stackPath, "render_compose", err)
		return false, "", err
	}

	if err := confirmRewire(ctx, confirmer, appID, stackPath); err != nil {
		lg.failure(ctx, appID, stackPath, "confirm_rewire", err)
		return false, "", err
	}

	envUserPath, err := e.applyRewire(ctx, appID, stackPath, lock.ComposeProject, newCompose)
	if err != nil {
		lg.failure(ctx, appID, stackPath, "apply_rewire", err)
		return false, "", err
	}

	lg.success(ctx, appID, stackPath)
	return true, envUserPath, nil
}

// rewireRenderCompose re-renders the stack's compose from the installed
// catalog version, reusing the on-disk .env values, and returns the new
// compose bytes. It runs the same render + input-assembly + catalog-vs-
// compose guards the update path uses (resolveUpdateRewritePlan ->
// installRenderInput -> render.RenderLabels), so the only intended
// difference from the on-disk compose is the added env_file overlay.
//
// The installed-version invariant is enforced fail-closed: the re-rendered
// service image references are compared against the on-disk ones, and any
// drift aborts the rewire (the user must `wdm update` to adopt the newer
// template). The .env is never re-rendered to disk, so secrets stay
// byte-identical regardless of any regenerable-secret handling in the
// shared render plan.
func (e *Engine) rewireRenderCompose(
	ctx context.Context,
	appID, stackPath string,
	onDiskCompose []byte,
) ([]byte, error) {
	rewrite, err := e.resolveUpdateRewritePlan(&updateCheckPlan{appID: appID, stackPath: stackPath})
	if err != nil {
		return nil, err
	}

	cat, err := e.loadInstallCatalog(ctx)
	if err != nil {
		return nil, err
	}
	app, err := selectCatalogApp(cat, appID)
	if err != nil {
		return nil, err
	}
	rewrite.app = app

	secretLiterals := append(slices.Clone(rewrite.generatedValues), rewrite.reusedSecretValues...)
	redactor := security.NewActiveRedactor(secretLiterals)

	input, err := e.installRenderInput(ctx, rewrite)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, redactedVerificationError(
			redactor,
			"rewire templates could not be loaded",
			"refresh the catalog and retry",
			err,
		)
	}
	composeStack, err := render.RenderLabels(input)
	if err != nil {
		return nil, redactedVerificationError(
			redactor,
			"compose template could not be rendered",
			"refresh the catalog and retry",
			err,
		)
	}
	rewrite.rendered = render.RenderedStack{
		ComposeBytes:  composeStack.ComposeBytes,
		ServiceLabels: composeStack.ServiceLabels,
	}

	// The re-rendered compose deploys the stack, so re-run the install-arc
	// catalog-vs-compose guards against it before it can be written.
	if err := verifyImagePinsMatchTemplate(redactor, app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifyPublicBindsMatchCatalog(redactor, app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifyContainerPrivilegeMatchCatalog(redactor, app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifySocketPolicyMatchCatalog(redactor, app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifyHostModuleMountMatchCatalog(redactor, app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifyNetworkIPAMMatchCatalog(redactor, app, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	if err := verifyRenderedNonSecretArtifacts(redactor, secretLiterals, rewrite.rendered, nil); err != nil {
		return nil, err
	}

	// Fail-closed installed-version gate: with no historical catalog, the
	// only way to guarantee the re-render did not change images is to compare
	// the re-rendered image references against the on-disk ones. Any drift
	// means the template moved past the installed version; abort and send the
	// user to `wdm update` rather than silently changing images.
	if err := verifyRewireImagesUnchanged(redactor, appID, onDiskCompose, rewrite.rendered.ComposeBytes); err != nil {
		return nil, err
	}
	return rewrite.rendered.ComposeBytes, nil
}

// verifyRewireImagesUnchanged refuses the rewire if any service's image
// reference in the re-rendered compose differs from the on-disk one. This
// is the byte-for-byte image guarantee: the rewire may add the env_file
// overlay but must never change what `docker compose up` pulls. A new or
// removed service is also drift. Image references are non-secret, so the
// diagnostic names them; the cause is redacted defensively for parity with
// the sibling render-stage errors.
func verifyRewireImagesUnchanged(redactor security.Redactor, appID string, onDisk, rerendered []byte) error {
	oldImages, err := projectComposeImages(onDisk)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"on-disk compose could not be parsed for image comparison",
			"the stack docker-compose.yml is corrupt; reinstall the app to restore managed state",
			fmt.Errorf("app %q: parse on-disk compose: %w", appID, err),
		)
	}
	newImages, err := projectComposeImages(rerendered)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"re-rendered compose could not be parsed for image comparison",
			"refresh the catalog and retry",
			fmt.Errorf("app %q: parse re-rendered compose: %w", appID, err),
		)
	}
	if !maps.Equal(oldImages, newImages) {
		return redactedVerificationError(
			redactor,
			"re-rendering would change the stack images",
			"the catalog template is newer than the installed version; run `wdm update "+appID+"` to adopt it",
			fmt.Errorf("app %q: rewire image set drifted from the installed compose", appID),
		)
	}
	return nil
}

// projectComposeImages decodes the per-service image references from a
// rendered compose into a service->image map, reusing the minimal
// [composeImageProjection] projection (only the image field is read).
func projectComposeImages(composeBytes []byte) (map[string]string, error) {
	var projection composeImageProjection
	if err := yaml.Unmarshal(composeBytes, &projection); err != nil {
		return nil, err
	}
	images := make(map[string]string, len(projection.Services))
	for service, def := range projection.Services {
		images[service] = def.Image
	}
	return images, nil
}

// confirmRewire gates the compose rewrite + restart on the [types.Confirmer]
// after detection and render and before any byte change, mirroring the
// lifecycle confirm posture: a nil confirmer refuses with
// [types.ErrCodeUsageValidation], a decline maps to
// [types.ErrCodeUserCanceled], and a confirmer error propagates wrapped. A
// decline writes nothing and restarts nothing.
func confirmRewire(ctx context.Context, confirmer types.Confirmer, appID, stackPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmer == nil {
		return types.NewError(
			types.ErrCodeUsageValidation,
			"confirmer is required before rewire",
			"pass a confirmer that can authorize the compose rewrite and restart",
		)
	}
	confirmed, err := confirmer.Confirm(ctx, types.Confirmation{
		Kind:  "rewire_overlay",
		Title: "activate user overlay for " + appID,
		Message: strings.Join([]string{
			"app: " + appID,
			"stack path: " + stackPath,
			"re-renders docker-compose.yml to inject the .env.user overlay",
			"keeps your .env and secrets unchanged",
			"restarts the stack (brief downtime)",
		}, "\n"),
	})
	if err != nil {
		return fmt.Errorf("core.rewire: confirming rewire: %w", err)
	}
	if !confirmed {
		return types.NewError(
			types.ErrCodeUserCanceled,
			"rewire canceled before any change",
			"re-run the rewire and confirm the prompt to activate the overlay",
		)
	}
	return nil
}

// applyRewire performs the byte-changing span under a freshly-taken
// per-stack flock: it reconfirms managed identity through the held fd,
// writes the re-rendered compose atomically, seeds the empty .env.user, and
// restarts the stack so the overlay takes effect. The compose write and
// .env.user seed are both inside the stack dir via SafeJoin; the .env is
// never touched. It returns the resolved .env.user path.
func (e *Engine) applyRewire(
	ctx context.Context,
	appID, stackPath, composeProject string,
	newCompose []byte,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	handle, err := acquireInstallStackLock(ctx, stackPath)
	if err != nil {
		return "", err
	}
	defer handle.Release() //nolint:errcheck // best-effort cleanup; kernel releases on process exit regardless

	if _, err := reconfirmManagedStack(handle, appID); err != nil {
		return "", err
	}

	composePath, err := security.SafeJoin(stackPath, installComposeFilename)
	if err != nil {
		return "", usageValidationError(
			"stack path is unsafe",
			"remove symlinks from the stack path and retry",
			err,
		)
	}
	if err := validateInstallWritePath(stackPath, composePath); err != nil {
		return "", usageValidationError(
			"rewire compose path is unsafe",
			"remove symlinks from the stack path and retry",
			err,
		)
	}
	if err := state.WriteFileAtomic(composePath, newCompose, installComposeFileMode); err != nil {
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"rewired compose could not be written",
			"check stack directory permissions and retry",
			err,
		)
	}

	envUserPath, err := ensureUserEnvFile(stackPath)
	if err != nil {
		return "", err
	}

	// ComposeRestart sets project.EnvFile to .env below, so compose interpolates
	// .env values and MAY echo a .env secret in a restart error. Build the
	// redactor over the stack's on-disk .env + .env.user secret set — the same
	// fail-closed builder ValidateConfig/redeploy use — so any interpolated
	// secret is scrubbed before it leaves the engine (PRD §11, §24).
	redactor, err := validateConfigRedactor(stackPath)
	if err != nil {
		return "", err
	}
	client, err := e.buildDockerClient(redactor)
	if err != nil {
		return "", err
	}
	project := docker.ComposeProject{
		ComposeFile: composePath,
		ProjectName: composeProject,
	}
	if envPath, joinErr := security.SafeJoin(stackPath, installEnvFilename); joinErr == nil {
		project.EnvFile = envPath
	}
	if err := docker.ComposeRestart(ctx, client, project); err != nil {
		return "", err
	}
	return envUserPath, nil
}

// resolveUserEditStack resolves appID to its managed stack path for the
// create-if-missing user-edit primitives, applying the shared closed/ctx/empty
// guards first so EnsureUserOverride and EnsureUserEnv stay thin.
func (e *Engine) resolveUserEditStack(ctx context.Context, appID string) (string, error) {
	if e.isClosed() {
		return "", ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if appID == "" {
		return "", usageValidationError(
			"app id is required",
			"pass the app id of an installed stack",
			nil,
		)
	}
	stackPath, _, err := e.resolveManagedStack(ctx, appID)
	if err != nil {
		return "", err
	}
	return stackPath, nil
}

// ensureUserOverrideFile seeds the user-owned docker-compose.override.yml
// (0644, header content) inside stackPath only when absent, returning its
// resolved path. O_EXCL makes the create idempotent: an already-present file
// surfaces fs.ErrExist, treated as "kept as-is", so an existing override is
// never truncated. The file is not secret-bearing, so it is created plainly
// at 0644 rather than through the 0600 secret-file path.
func ensureUserOverrideFile(stackPath string) (string, error) {
	path, err := security.SafeJoin(stackPath, installComposeOverrideFilename)
	if err != nil {
		return "", usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, installComposeFileMode) //nolint:gosec // G304: path is SafeJoin-validated against the managed stack root above
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return path, nil
		}
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"could not create compose override file",
			"verify the stack directory exists and is writable",
			err,
		)
	}
	if _, writeErr := f.WriteString(userOverrideHeader); writeErr != nil {
		_ = f.Close() //nolint:errcheck // best-effort cleanup; primary error is writeErr
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"could not seed compose override file",
			"verify the stack directory is writable",
			writeErr,
		)
	}
	if closeErr := f.Close(); closeErr != nil {
		return "", types.WrapError(
			types.ErrCodeGeneric,
			"compose override file could not be finalized",
			"check stack directory permissions and retry",
			closeErr,
		)
	}
	return path, nil
}

// redactEnvEntry projects one key/value into a redaction-safe EnvEntry. The
// value is masked when the active redactor changes it (literal secret match)
// OR when the key name matches the secret-ish heuristic; either path sets
// Secret. The masked form is the redactor placeholder, never the raw value.
func redactEnvEntry(redactor security.Redactor, key, value string) types.EnvEntry {
	if secretishKeyPattern.MatchString(key) {
		return types.EnvEntry{Key: key, Value: security.RedactedPlaceholder, Secret: true}
	}
	redacted := redactor.Redact(value)
	return types.EnvEntry{Key: key, Value: redacted, Secret: redacted != value}
}

// userEnvKV is one parsed .env.user assignment, preserving file order so the
// view renders deterministically.
type userEnvKV struct {
	key   string
	value string
}

// readUserEnvEntries parses <stackPath>/.env.user as ordered KEY=VALUE lines,
// applying the same split/trim rules as [state.ReadStackEnv]: first "=" only,
// trim the key, preserve value bytes, skip blanks and comments. A missing
// .env.user yields no entries (it is created lazily on first edit), so absence
// is not an error. Malformed lines are skipped rather than failing the
// read-only view.
func readUserEnvEntries(stackPath string) ([]userEnvKV, error) {
	path, err := security.SafeJoin(stackPath, installEnvUserFilename)
	if err != nil {
		return nil, usageValidationError(
			"stack path is unsafe",
			"choose a stack path under the configured stack base",
			err,
		)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is SafeJoin-validated against the managed stack root above
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, types.WrapError(
			types.ErrCodeGeneric,
			"could not read user env file",
			"ensure the stack .env.user is readable",
			err,
		)
	}
	var entries []userEnvKV
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "#") {
			continue
		}
		sep := strings.IndexByte(line, '=')
		if sep < 0 {
			continue
		}
		key := strings.TrimSpace(line[:sep])
		if key == "" {
			continue
		}
		entries = append(entries, userEnvKV{key: key, value: line[sep+1:]})
	}
	return entries, nil
}

// readUserEnvValues returns the non-empty values from .env.user so a stack op
// can fold them into its active redactor: .env.user is user-controlled and MAY
// hold user secrets (SMTP password, API keys), so over-redaction is acceptable
// and fail-closed. A missing file yields nothing.
func readUserEnvValues(stackPath string) ([]string, error) {
	entries, err := readUserEnvEntries(stackPath)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(entries))
	for _, kv := range entries {
		if kv.value == "" {
			continue
		}
		values = append(values, kv.value)
	}
	return values, nil
}
