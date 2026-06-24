package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

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
	for key, value := range baseEnv {
		result.Entries = append(result.Entries, redactEnvEntry(redactor, key, value))
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
