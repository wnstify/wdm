package core_test

import (
	"context"
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
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// scriptValidateCompose wires the fixture's fake to answer ONLY the
// compose-config invocation ValidateConfig issues, returning composeErr
// (nil for a valid config). Any other invocation type is a test bug —
// ValidateConfig is read-only and must issue no container list/inspect.
func scriptValidateCompose(f *statusTestFixture, composeErr error) {
	f.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.composeConfigInvocation" {
			if composeErr != nil {
				return docker.CommandResult{ExitCode: 1}, composeErr
			}
			return docker.CommandResult{}, nil
		}
		return docker.CommandResult{}, fmt.Errorf("unexpected invocation %T in ValidateConfig", inv)
	}
}

// writeValidateStackEnv writes a .env file into the managed stack dir so
// ValidateConfig's redactor can register its values as literal secrets.
func writeValidateStackEnv(t *testing.T, stackPath, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".env"), []byte(contents), 0o600))
}

// TestValidateConfig_InvalidComposeReportsValidFalse proves an
// invalid-but-readable on-disk Compose file is a SUCCESS payload
// (Valid:false, nil error) with a non-empty scrubbed Detail, not an
// error. It also pins the read-only discipline: only
// the compose-config invocation runs, no runtime.lock is created, and
// the stack dir is byte-identical afterward.
func TestValidateConfig_InvalidComposeReportsValidFalse(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	scriptValidateCompose(fixture, errors.New("services.app.image must be a string"))

	manifestBefore, err := os.ReadFile(filepath.Join(fixture.stackPath, ".wdm.lock"))
	require.NoError(t, err)

	result, err := fixture.eng.ValidateConfig(t.Context(), fixture.appID)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, fixture.appID, result.AppID)
	assert.Equal(t, "wdm-"+fixture.appID, result.ComposeProject)
	assert.Equal(t, filepath.Join(fixture.stackPath, "docker-compose.yml"), result.ComposeFile)
	assert.False(t, result.Valid)
	assert.NotEmpty(t, result.Detail)
	assert.Contains(t, result.Detail, "services.app.image must be a string")

	// Read-only discipline: exactly one docker call (compose config),
	// no container list/inspect, no runtime.lock, manifest unchanged.
	assert.Equal(t, []string{"docker.composeConfigInvocation"}, fixture.fake.invocationTypes)
	_, statErr := os.Stat(filepath.Join(fixture.stateDir, "runtime.lock"))
	assert.True(t, os.IsNotExist(statErr), "ValidateConfig must not create runtime.lock")
	manifestAfter, err := os.ReadFile(filepath.Join(fixture.stackPath, ".wdm.lock"))
	require.NoError(t, err)
	assert.Equal(t, manifestBefore, manifestAfter, "ValidateConfig must not rewrite the manifest")
	entries, err := os.ReadDir(fixture.stackPath)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, ".wdm.lock", entries[0].Name())
}

// TestValidateConfig_ValidComposeReportsValidTrue proves a config that
// passes validation reports Valid:true with empty Detail and the
// project/file fields populated from the manifest and stack.
func TestValidateConfig_ValidComposeReportsValidTrue(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	scriptValidateCompose(fixture, nil)

	result, err := fixture.eng.ValidateConfig(t.Context(), fixture.appID)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, fixture.appID, result.AppID)
	assert.True(t, result.Valid)
	assert.Empty(t, result.Detail)
	assert.Equal(t, "wdm-"+fixture.appID, result.ComposeProject)
	assert.Equal(t, filepath.Join(fixture.stackPath, "docker-compose.yml"), result.ComposeFile)
	assert.Equal(t, []string{"docker.composeConfigInvocation"}, fixture.fake.invocationTypes)
}

// TestValidateConfig_SecretInEnvNeverLeaksIntoDetail is the binding
// exit criterion: a known secret in the stack .env is
// registered with the per-operation redactor, so even a deliberately
// leaky docker error that embeds the literal produces ZERO matches of
// that literal in Detail (PRD §11, §24).
// The bare-form subtest is the load-bearing one: it embeds the secret
// OUTSIDE any KEY=VALUE shape, so the structural env-assignment pattern
// cannot scrub it — the only thing that can is the literal registered
// from a well-formed.env. It therefore fails unless the redactor
// builder actually reads and registers.env VALUES (the F2 fix).
func TestValidateConfig_SecretInEnvNeverLeaksIntoDetail(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-DB-pAsSw0rd-9f2a"

	tests := []struct {
		name string
		// leak is the verbatim docker error text embedding the secret.
		// The fake returns it raw — the redaction under test is
		// ValidateConfig's, not the docker client's.
		leak string
	}{
		{
			// KEY=VALUE shape: the structural env-assignment pattern
			// alone could scrub this, so it does NOT bind literal
			// registration — it stays for the empty-value-skip coverage.
			name: "key=value shape",
			leak: "compose config rejected: DB_PASSWORD=" + secret + " is not a valid value",
		},
		{
			// BARE value: not a KEY=VALUE assignment, so NO structural
			// pattern matches it. Only the literal registered from the
			// well-formed.env scrubs it — this case is the F2 binding.
			name: "bare value shape",
			leak: "compose config rejected: value " + secret + " rejected for service app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newStatusFixture(t, nil)
			// EMPTY_VALUE exercises the empty-value skip in the redactor
			// builder (an empty literal must not be registered — it would
			// match every string); the secret is still registered and
			// scrubbed.
			writeValidateStackEnv(t, fixture.stackPath, "DB_PASSWORD="+secret+"\nNON_SECRET=plain\nEMPTY_VALUE=\n")
			scriptValidateCompose(fixture, errors.New(tt.leak))

			result, err := fixture.eng.ValidateConfig(t.Context(), fixture.appID)
			require.NoError(t, err)
			require.NotNil(t, result)

			assert.False(t, result.Valid)
			assert.NotEmpty(t, result.Detail)
			assert.Equal(t, 0, strings.Count(result.Detail, secret),
				"the .env secret literal must not appear in Detail")
			assert.Contains(t, result.Detail, security.RedactedPlaceholder)
		})
	}
}

// TestValidateConfig_MissingEnvStillValidates proves a stack with no
// .env file (or an unreadable one) does NOT fail the validation: the
// redactor degrades to structural patterns only and the compose result
// is still reported. A bare leaky value (not matched by any structural
// pattern) can pass through Detail — that is the documented graceful
// degrade, and the env-assignment structural pattern still scrubs
// KEY=VALUE forms.
func TestValidateConfig_MissingEnvStillValidates(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	// No .env written. The leaky error uses a secret-typed KEY=VALUE so
	// the structural env-assignment pattern still redacts it even
	// without literal registration.
	scriptValidateCompose(fixture, errors.New("invalid: API_TOKEN=leakyvalue rejected"))

	result, err := fixture.eng.ValidateConfig(t.Context(), fixture.appID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.Valid)
	assert.NotEmpty(t, result.Detail)
	assert.NotContains(t, result.Detail, "leakyvalue",
		"the structural env-assignment redactor still scrubs KEY=VALUE forms when .env is absent")
}

// TestValidateConfig_MalformedEnvFailsClosed proves the F1 fix: a
// PRESENT-but-malformed.env (here a duplicate key — a form docker
// compose itself tolerates) defeats the literal-registration redaction
// guarantee, so ValidateConfig FAILS CLOSED with a typed
// ErrCodeUsageValidation error naming the .env problem rather than
// risking a bare secret leaking into Detail. The refusal precedes
// Docker client construction, so zero docker calls run and no
// ValidationResult is produced (PRD §11, §24).
func TestValidateConfig_MalformedEnvFailsClosed(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	// Duplicate DB_PASSWORD: state.ReadStackEnv rejects this with a typed
	// usage-validation error that carries NO fs.ErrNotExist in its chain,
	// so the redactor builder propagates it instead of degrading.
	writeValidateStackEnv(t, fixture.stackPath, "DB_PASSWORD=first\nDB_PASSWORD=second\n")
	// The fake must never be consulted — if it were, the test would
	// report the "unexpected invocation" error from the run function.
	scriptValidateCompose(fixture, nil)

	result, err := fixture.eng.ValidateConfig(t.Context(), fixture.appID)
	require.Error(t, err)
	assert.Nil(t, result, "a malformed .env must not yield a ValidationResult")
	assertUsageValidation(t, err)
	assert.Contains(t, err.Error(), "existing stack env file is malformed",
		"the refusal must name the .env problem so the user fixes the file first")
	assert.Zero(t, fixture.fake.calls,
		"the malformed-.env refusal must precede any docker call")
}

// TestValidateConfig_RefusesMissingAndUnmanagedStacks proves the
// managed-only refusal ordering (PRD §10): an uninstalled app and an
// unmanaged directory both refuse with ErrCodeUsageValidation BEFORE
// any docker call.
func TestValidateConfig_RefusesMissingAndUnmanagedStacks(t *testing.T) {
	t.Parallel()

	t.Run("app not installed", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		fake := &fakeDockerClient{}
		core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

		result, err := eng.ValidateConfig(t.Context(), "ghost-app")
		require.Error(t, err)
		assert.Nil(t, result)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "app is not installed")
		assert.Zero(t, fake.calls, "managed-only refusal must precede any docker call")
	})

	t.Run("directory exists but is unmanaged", func(t *testing.T) {
		t.Parallel()

		eng, stateDir := newTestEngine(t)
		stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
		require.NoError(t, os.MkdirAll(filepath.Join(stackBase, "user-dir"), 0o755))
		fake := &fakeDockerClient{}
		core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

		result, err := eng.ValidateConfig(t.Context(), "user-dir")
		require.Error(t, err)
		assert.Nil(t, result)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "stack directory is not managed by wdm")
		assert.Zero(t, fake.calls, "managed-only refusal must precede any docker call")
	})

	t.Run("empty app id", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		result, err := eng.ValidateConfig(t.Context(), "")
		require.Error(t, err)
		assert.Nil(t, result)
		assertUsageValidation(t, err)
	})
}

// TestValidateConfig_RefusesBusyStackWithoutBlocking proves the PRD §26
// read-only lock posture: a held per-stack exclusive flock makes
// ValidateConfig refuse fast with ErrCodeRuntimeLockHeld and issue no
// docker command.
func TestValidateConfig_RefusesBusyStackWithoutBlocking(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	holdFlockExclusive(t, filepath.Join(fixture.stackPath, ".wdm.lock"))

	result, err := fixture.eng.ValidateConfig(t.Context(), fixture.appID)
	require.Error(t, err)
	assert.Nil(t, result)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeRuntimeLockHeld, typedErr.Code)
	require.ErrorIs(t, err, state.ErrStackLockBusy)
	assert.Zero(t, fixture.fake.calls)
}

// TestValidateConfig_PropagatesDockerUnavailableUnchanged proves an
// unreachable daemon surfaces as the hard ErrCodeDockerUnavailable
// error unchanged — never downgraded to a Valid:false payload.
func TestValidateConfig_PropagatesDockerUnavailableUnchanged(t *testing.T) {
	t.Parallel()

	fixture := newStatusFixture(t, nil)
	unavailableErr := types.WrapError(
		types.ErrCodeDockerUnavailable,
		"docker is unavailable",
		"",
		errors.New("cannot connect to the docker daemon"),
	)
	scriptValidateCompose(fixture, unavailableErr)

	result, err := fixture.eng.ValidateConfig(t.Context(), fixture.appID)
	require.Error(t, err)
	assert.Nil(t, result)
	require.ErrorIs(t, err, unavailableErr)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeDockerUnavailable, typedErr.Code)
}

// TestValidateConfig_ContextCancellation proves cancellation always
// propagates as an error: a pre-canceled context refuses before any
// docker call, and a cancellation during compose validation surfaces
// as an error, never a Valid:false condition.
func TestValidateConfig_ContextCancellation(t *testing.T) {
	t.Parallel()

	t.Run("pre-canceled context", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		result, err := fixture.eng.ValidateConfig(ctx, fixture.appID)
		require.Error(t, err)
		assert.Nil(t, result)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, fixture.fake.calls)
	})

	t.Run("canceled during compose validation propagates as error", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
			if fmt.Sprintf("%T", inv) == "docker.composeConfigInvocation" {
				cancel()
				return docker.CommandResult{}, context.Canceled
			}
			return docker.CommandResult{}, fmt.Errorf("unexpected invocation %T", inv)
		}

		result, err := fixture.eng.ValidateConfig(ctx, fixture.appID)
		require.Error(t, err)
		assert.Nil(t, result)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// TestValidateConfig_DockerClientFactoryFailures proves the docker
// client construction failures surface as typed errors before any
// validation, mirroring Status.
func TestValidateConfig_DockerClientFactoryFailures(t *testing.T) {
	t.Parallel()

	t.Run("nil factory", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		core.SetInstallDockerClientFactoryForTest(fixture.eng, nil)

		result, err := fixture.eng.ValidateConfig(t.Context(), fixture.appID)
		require.Error(t, err)
		assert.Nil(t, result)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "docker client factory is required")
	})

	t.Run("factory error", func(t *testing.T) {
		t.Parallel()

		fixture := newStatusFixture(t, nil)
		factoryErr := errors.New("docker socket vanished")
		core.SetInstallDockerClientFactoryForTest(fixture.eng, func(security.Redactor) (docker.Client, error) {
			return nil, factoryErr
		})

		result, err := fixture.eng.ValidateConfig(t.Context(), fixture.appID)
		require.Error(t, err)
		assert.Nil(t, result)
		require.ErrorIs(t, err, factoryErr)
	})
}

// TestValidateConfig_HonorsClosed keeps the closed-engine pin: a
// closed engine returns ErrClosed with a nil result.
func TestValidateConfig_HonorsClosed(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	result, err := eng.ValidateConfig(t.Context(), "uptime-kuma")
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, result)
}
