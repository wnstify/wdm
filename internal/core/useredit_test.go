package core_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
)

// scriptComposeConfigErr returns a fake runFn that answers ONLY the
// compose-config invocation the live validators issue, returning composeErr
// (nil for a valid config). Any other invocation is a test bug — these
// read-only paths must issue no container list/inspect.
func scriptComposeConfigErr(t *testing.T, composeErr error) func(int, docker.Invocation) (docker.CommandResult, error) {
	t.Helper()
	return func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.composeConfigInvocation" {
			if composeErr != nil {
				return docker.CommandResult{ExitCode: 1}, composeErr
			}
			return docker.CommandResult{}, nil
		}
		return docker.CommandResult{}, fmt.Errorf("unexpected invocation %T", inv)
	}
}

// TestEnsureUserOverride_CreatesSeededIdempotentNeverTruncates proves the
// override primitive: a fresh stack gets a 0644 docker-compose.override.yml
// carrying the documented header, the create is idempotent across calls, and
// an existing override with user content is NEVER truncated.
func TestEnsureUserOverride_CreatesSeededIdempotentNeverTruncates(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("override-app"), nil)
	overridePath := filepath.Join(fx.stackPath, "docker-compose.override.yml")

	path, err := fx.eng.EnsureUserOverride(t.Context(), fx.appID)
	require.NoError(t, err)
	assert.Equal(t, overridePath, path)

	info, err := os.Stat(overridePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "override must be 0644")

	seeded, err := os.ReadFile(overridePath)
	require.NoError(t, err)
	assert.Contains(t, string(seeded), "user-owned overlay", "override must carry the documented header")
	assert.Contains(t, string(seeded), "external: true", "override must carry the external-network example")

	// User edits the file, then a second ensure call must keep it byte-identical.
	userContent := string(seeded) + "services:\n  extra:\n    image: nginx:alpine\n"
	require.NoError(t, os.WriteFile(overridePath, []byte(userContent), 0o644))

	path2, err := fx.eng.EnsureUserOverride(t.Context(), fx.appID)
	require.NoError(t, err)
	assert.Equal(t, overridePath, path2)

	after, err := os.ReadFile(overridePath)
	require.NoError(t, err)
	assert.Equal(t, userContent, string(after), "ensure must never truncate an existing override")
}

// TestEnsureUserEnv_SeedsEmpty0600Idempotent proves EnsureUserEnv resolves and
// seeds an empty .env.user at 0600 via the shared primitive, and never
// truncates user content on repeat calls.
func TestEnsureUserEnv_SeedsEmpty0600Idempotent(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("userenv-app"), nil)
	envUserPath := filepath.Join(fx.stackPath, ".env.user")

	path, err := fx.eng.EnsureUserEnv(t.Context(), fx.appID)
	require.NoError(t, err)
	assert.Equal(t, envUserPath, path)

	info, err := os.Stat(envUserPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), ".env.user must be 0600")
	data, err := os.ReadFile(envUserPath)
	require.NoError(t, err)
	assert.Empty(t, data, ".env.user must be seeded empty")

	require.NoError(t, os.WriteFile(envUserPath, []byte("SMTP_HOST=mail.example.com\n"), 0o600))
	path2, err := fx.eng.EnsureUserEnv(t.Context(), fx.appID)
	require.NoError(t, err)
	assert.Equal(t, envUserPath, path2)
	after, err := os.ReadFile(envUserPath)
	require.NoError(t, err)
	assert.Equal(t, "SMTP_HOST=mail.example.com\n", string(after), "ensure must never truncate user .env.user")
}

// TestViewEnvRedacted_MasksSecretValueAndSecretishKey is the binding view-env
// exit criterion: a secret-VALUED env var (the catalog's DB_PASSWORD literal)
// and a secret-ish-KEYED user var (.env.user SMTP_PASSWORD) are both masked,
// with NO raw secret anywhere in the result and Secret flags set. It exercises
// the REAL active redactor, not a mock.
func TestViewEnvRedacted_MasksSecretValueAndSecretishKey(t *testing.T) {
	t.Parallel()

	const userSecret = "user-smtp-pw-do-not-leak-7c1d"

	fx := newReconfigureFixture(t, reconfigureApp("view-app"), nil)
	// Plant a secret-ish-keyed user var in .env.user (no catalog placeholder,
	// so only the key heuristic catches it) plus a plain non-secret var.
	require.NoError(t, os.WriteFile(
		filepath.Join(fx.stackPath, ".env.user"),
		[]byte("SMTP_PASSWORD="+userSecret+"\nSMTP_HOST=mail.example.com\n"),
		0o600,
	))

	result, err := fx.eng.ViewEnvRedacted(t.Context(), fx.appID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, fx.appID, result.AppID)

	byKey := map[string]struct {
		value  string
		secret bool
	}{}
	for _, e := range result.Entries {
		byKey[e.Key] = struct {
			value  string
			secret bool
		}{e.Value, e.Secret}
	}

	// No raw secret survives anywhere in the rendered result.
	for _, e := range result.Entries {
		assert.NotContains(t, e.Value, reconfigureSecretValue, "base .env secret value must be masked")
		assert.NotContains(t, e.Value, userSecret, "user .env.user secret value must be masked")
	}

	dbPw, ok := byKey["DB_PASSWORD"]
	require.True(t, ok, "DB_PASSWORD must be present in the view")
	assert.Equal(t, security.RedactedPlaceholder, dbPw.value, "secret-valued var masked by the redactor")
	assert.True(t, dbPw.secret, "secret-valued var flagged Secret")

	smtpPw, ok := byKey["SMTP_PASSWORD"]
	require.True(t, ok, "SMTP_PASSWORD must be present in the view")
	assert.Equal(t, security.RedactedPlaceholder, smtpPw.value, "secret-ish-keyed var masked by the key heuristic")
	assert.True(t, smtpPw.secret, "secret-ish-keyed var flagged Secret")

	// .env.user is treated as fail-closed: ALL its values are folded into the
	// redactor, so even a non-secret-looking user value is masked
	// (over-redaction is acceptable per the security posture).
	smtpHost, ok := byKey["SMTP_HOST"]
	require.True(t, ok, "SMTP_HOST must be present in the view")
	assert.Equal(t, security.RedactedPlaceholder, smtpHost.value, "every .env.user value is masked fail-closed")
	assert.True(t, smtpHost.secret)

	// A NON-.env.user base var (a resource limit) is not a secret and not
	// folded, so it passes through unmasked — proving masking is targeted,
	// not blanket.
	memLimit, ok := byKey["MEMORY_LIMIT_APP"]
	require.True(t, ok, "MEMORY_LIMIT_APP must be present in the view")
	assert.Equal(t, reconfigureInstallMemory, memLimit.value, "non-secret base var passes through unmasked")
	assert.False(t, memLimit.secret)
}

// TestViewEnvRedacted_MasksBareUserEnvSecret is the binding fail-closed view
// criterion: a .env.user var with a NON-secretish KEY whose VALUE is a bare
// secret absent from the catalog secret set is still masked. Without folding
// ALL .env.user values into the view redactor, this value would render raw —
// the seam this test pins. Exercises the REAL redactor.
func TestViewEnvRedacted_MasksBareUserEnvSecret(t *testing.T) {
	t.Parallel()

	const bareSecret = "mailgun-bare-creds-not-in-catalog-4d2e"

	fx := newReconfigureFixture(t, reconfigureApp("bareview-app"), nil)
	// MAILGUN_CREDS: non-secretish key (no password/secret/token/key/salt),
	// value not a catalog placeholder — only the value-fold catches it.
	require.NoError(t, os.WriteFile(
		filepath.Join(fx.stackPath, ".env.user"),
		[]byte("MAILGUN_CREDS="+bareSecret+"\n"),
		0o600,
	))

	result, err := fx.eng.ViewEnvRedacted(t.Context(), fx.appID)
	require.NoError(t, err)
	require.NotNil(t, result)

	var found bool
	for _, e := range result.Entries {
		assert.NotContains(t, e.Value, bareSecret, "no entry may render the bare user secret raw")
		if e.Key == "MAILGUN_CREDS" {
			found = true
			assert.Equal(t, security.RedactedPlaceholder, e.Value, "bare user secret value must be masked")
			assert.True(t, e.Secret, "bare user secret entry must be flagged Secret")
		}
	}
	assert.True(t, found, "MAILGUN_CREDS must be present in the view")
}

// TestValidateConfig_UserEnvSecretNeverLeaksIntoDetail proves the op-redactor
// fold-in: a secret planted ONLY in .env.user (not the base .env, not a
// catalog placeholder) is registered with the per-operation redactor, so a
// leaky compose error embedding it produces ZERO matches in the redacted
// output — exercising the REAL redactor.
func TestValidateConfig_UserEnvSecretNeverLeaksIntoDetail(t *testing.T) {
	t.Parallel()

	const userSecret = "env-user-bare-secret-9a3f1e"

	fx := newReconfigureFixture(t, reconfigureApp("redact-app"), nil)
	require.NoError(t, os.WriteFile(
		filepath.Join(fx.stackPath, ".env.user"),
		[]byte("API_KEY="+userSecret+"\n"),
		0o600,
	))
	// A BARE-value leak: not a KEY=VALUE shape, so no structural pattern can
	// scrub it. Only the literal registered from .env.user redacts it.
	fx.fake.runFn = scriptComposeConfigErr(
		t,
		errors.New("compose config rejected: value "+userSecret+" rejected for service app"),
	)

	result, err := fx.eng.ValidateConfig(t.Context(), fx.appID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.Valid)
	assert.Equal(t, 0, strings.Count(result.Detail, userSecret),
		"the .env.user secret literal must not appear in ValidateConfig Detail")
	assert.Contains(t, result.Detail, security.RedactedPlaceholder)
}

// TestValidateStack_ValidatesBaseAndOverride proves ValidateStack runs the
// live compose-config (which auto-includes the content-gated override) and
// returns no warnings on success; it lets CLI/TUI validate without importing
// internal/docker.
func TestValidateStack_ValidatesBaseAndOverride(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("validate-app"), nil)
	// Seed a real-content override so the content-gate appends the second -f.
	overridePath, err := fx.eng.EnsureUserOverride(t.Context(), fx.appID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(overridePath,
		[]byte("services:\n  extra:\n    image: nginx:alpine\n"), 0o644))

	fx.fake.runFn = scriptComposeConfigErr(t, nil)

	warnings, err := fx.eng.ValidateStack(t.Context(), fx.appID)
	require.NoError(t, err)
	assert.Empty(t, warnings)

	// A compose-config failure surfaces as a returned error (warn-but-allow
	// is the caller's choice), with the secret-bearing detail redacted.
	fx.fake.runFn = scriptComposeConfigErr(t,
		errors.New("services.extra.image must be a string: "+reconfigureSecretValue))
	_, err = fx.eng.ValidateStack(t.Context(), fx.appID)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), reconfigureSecretValue, "validate error must be redacted")
}

// TestUserEdit_ClosedEngineRefusesAllEntries proves every user-edit entry
// point fails closed on a closed engine: EnsureUserOverride, EnsureUserEnv,
// ViewEnvRedacted, ValidateStack, and RewireStack each return ErrClosed and do
// nothing. A closed engine has released its locks, so no state-touching work
// may proceed.
func TestUserEdit_ClosedEngineRefusesAllEntries(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	_, err := eng.EnsureUserOverride(t.Context(), "any-app")
	require.ErrorIs(t, err, core.ErrClosed)

	_, err = eng.EnsureUserEnv(t.Context(), "any-app")
	require.ErrorIs(t, err, core.ErrClosed)

	_, err = eng.ViewEnvRedacted(t.Context(), "any-app")
	require.ErrorIs(t, err, core.ErrClosed)

	_, err = eng.ValidateStack(t.Context(), "any-app")
	require.ErrorIs(t, err, core.ErrClosed)

	_, _, err = eng.RewireStack(t.Context(), "any-app", &fakeConfirmer{})
	require.ErrorIs(t, err, core.ErrClosed)
}

// TestUserEdit_EmptyAppIDRefusesAllEntries proves the shared empty-app-id
// guard on every user-edit entry point: a blank app id surfaces a
// usage-validation error before any stack resolution, so a caller is told to
// pass an app id rather than silently no-op.
func TestUserEdit_EmptyAppIDRefusesAllEntries(t *testing.T) {
	t.Parallel()

	fx := newReconfigureFixture(t, reconfigureApp("empty-id-app"), nil)

	_, err := fx.eng.EnsureUserOverride(t.Context(), "")
	assertUsageValidation(t, err)

	_, err = fx.eng.EnsureUserEnv(t.Context(), "")
	assertUsageValidation(t, err)

	_, err = fx.eng.ViewEnvRedacted(t.Context(), "")
	assertUsageValidation(t, err)

	_, err = fx.eng.ValidateStack(t.Context(), "")
	assertUsageValidation(t, err)

	_, _, err = fx.eng.RewireStack(t.Context(), "", &fakeConfirmer{})
	assertUsageValidation(t, err)
}

// TestRewireStack_CorruptComposeRefusesWithGuidance proves the fail-closed
// parse gate: a pre-feature stack whose on-disk docker-compose.yml is not
// valid YAML cannot be probed for the env_file overlay, so RewireStack aborts
// with a usage-validation error pointing the user at reinstall rather than
// silently rewriting an unparseable compose. No restart runs.
func TestRewireStack_CorruptComposeRefusesWithGuidance(t *testing.T) {
	t.Parallel()

	fx := newRewireFixture(t, rewireApp("rewire-corrupt-app"), "services: [this: is: not: valid: yaml")

	rewired, path, err := fx.eng.RewireStack(t.Context(), fx.appID, &fakeConfirmer{})
	require.Error(t, err)
	assert.False(t, rewired)
	assert.Empty(t, path)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "could not be parsed")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"a corrupt compose must not restart the stack")
}

// TestRewireStack_ImageDriftAbortsBeforeWrite is the binding installed-version
// invariant: with no historical catalog, the only image guarantee is that the
// re-rendered image set equals the on-disk one. A pre-feature stack whose
// on-disk image differs from the current template means the template moved
// past the installed version, so the rewire aborts (sending the user to
// `wdm update`) BEFORE any compose write or restart — never silently changing
// what `docker compose up` pulls.
func TestRewireStack_ImageDriftAbortsBeforeWrite(t *testing.T) {
	t.Parallel()

	// Same service, DIFFERENT image than the template's docker.io/example/app:1.0.0,
	// and no env_file (pre-feature) so detection proceeds to the render+drift gate.
	const driftedCompose = `services:
  app:
    image: docker.io/example/app:9.9.9
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
`
	fx := newRewireFixture(t, rewireApp("rewire-drift-app"), driftedCompose)
	composePath := filepath.Join(fx.stackPath, "docker-compose.yml")

	rewired, path, err := fx.eng.RewireStack(t.Context(), fx.appID, &fakeConfirmer{})
	require.Error(t, err)
	assert.False(t, rewired)
	assert.Empty(t, path)
	assert.ErrorContains(t, err, "would change the stack images",
		"image drift must abort with the installed-version guard message")

	after, err := os.ReadFile(composePath)
	require.NoError(t, err)
	assert.Equal(t, driftedCompose, string(after), "an aborted rewire must not rewrite the compose")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"an aborted rewire must not restart the stack")
	_, statErr := os.Stat(filepath.Join(fx.stackPath, ".env.user"))
	assert.True(t, os.IsNotExist(statErr), "an aborted rewire must not seed .env.user")
}

// TestRewireStack_NilConfirmerRefusesBeforeWrite proves a pre-feature stack
// with a nil confirmer refuses at the confirm gate (after detection and
// render, before any byte change): no compose rewrite, no .env.user seed, no
// restart. Rewire rewrites the compose and restarts, so it MUST be authorized.
func TestRewireStack_NilConfirmerRefusesBeforeWrite(t *testing.T) {
	t.Parallel()

	fx := newRewireFixture(t, rewireApp("rewire-app"), preFeatureCompose)
	composePath := filepath.Join(fx.stackPath, "docker-compose.yml")

	rewired, path, err := fx.eng.RewireStack(t.Context(), fx.appID, nil)
	require.Error(t, err)
	assert.False(t, rewired)
	assert.Empty(t, path)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "confirmer is required")

	after, err := os.ReadFile(composePath)
	require.NoError(t, err)
	assert.Equal(t, preFeatureCompose, string(after), "a nil-confirmer rewire must write nothing")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"a nil-confirmer rewire must not restart the stack")
	_, statErr := os.Stat(filepath.Join(fx.stackPath, ".env.user"))
	assert.True(t, os.IsNotExist(statErr), "a nil-confirmer rewire must not seed .env.user")
}
