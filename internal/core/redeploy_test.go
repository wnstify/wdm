package core_test

import (
	"context"
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
	"github.com/wnstify/wdm/pkg/types"
)

// TestRedeploy_ClosedEngineReturnsErrClosed keeps the closed-engine arm: a
// closed engine returns ErrClosed with a nil result.
func TestRedeploy_ClosedEngineReturnsErrClosed(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	result, err := eng.RedeployStack(t.Context(), types.RestartRequest{AppID: "uptime-kuma"}, nil, nil)
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, result)
}

// TestRedeploy_HappyPathRunsComposeUpUnderLock is the end-to-end arc:
// planning -> confirm -> `docker compose up -d` (the recreate that applies
// overlay edits) -> post-redeploy status verify -> populated result. It proves
// the step stream, the redeploy_safe confirm payload, and that the mutating
// Docker call is ComposeUp (NOT plain restart). The runtime.lock is released
// (a second redeploy succeeds).
func TestRedeploy_HappyPathRunsComposeUpUnderLock(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("redeploy-happy-app", 18080), nil)
	scriptRestartRunning(fx, t)

	confirmer := &fakeConfirmer{}
	var steps []string
	res, err := fx.eng.RedeployStack(t.Context(), types.RestartRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, confirmer)
	require.NoError(t, err, "the redeploy runs to completion")
	require.NotNil(t, res)

	assert.Equal(t, []string{
		types.StepRedeployPlanning,
		types.StepRedeployPlanning,
		types.StepRedeployConfirm,
		types.StepRedeployExecute,
		types.StepRedeployStatus,
	}, steps)

	require.Len(t, confirmer.calls, 1, "the confirmer is asked exactly once before the redeploy")
	payload := confirmer.calls[0]
	assert.Equal(t, "redeploy_safe", payload.Kind)
	assert.Contains(t, payload.Title, fx.appID)
	assert.Contains(t, payload.Message, "compose project: wdm-"+fx.appID)
	assert.Contains(t, payload.Message, "no data loss")

	// The recreate ran via `docker compose up`, NOT plain restart.
	assert.Contains(t, fx.fake.invocationTypes, "docker.composeUpInvocation",
		"redeploy must run docker compose up -d to apply overlay edits")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeRestartInvocation",
		"redeploy must not run plain docker compose restart")

	assert.Equal(t, fx.appID, res.AppID)
	assert.Equal(t, "wdm-"+fx.appID, res.ComposeProject)
	assert.Equal(t, []string{"app"}, res.RestartedServices)
	require.NotNil(t, res.Status)
	assert.Equal(t, "running", res.Status.State)

	res2, err := fx.eng.RedeployStack(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err, "RedeployStack must release runtime.lock so a second call succeeds")
	require.NotNil(t, res2)
}

// TestRedeploy_RunsComposeUpFirstThenStatus proves the exact Docker sequence:
// `docker compose up -d` is the FIRST and only mutating Docker call, then the
// post-redeploy status verify lists containers. No restart, no down, no pull.
func TestRedeploy_RunsComposeUpFirstThenStatus(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("redeploy-seq-app", 18080), nil)
	scriptRestartRunning(fx, t)

	_, err := fx.eng.RedeployStack(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)

	require.NotEmpty(t, fx.fake.invocationTypes)
	assert.Equal(t, "docker.composeUpInvocation", fx.fake.invocationTypes[0],
		"the redeploy must be the first Docker call")
	assert.Contains(t, fx.fake.invocationTypes, "docker.projectContainerListInvocation",
		"the post-redeploy status verify lists containers")
	for _, inv := range fx.fake.invocationTypes {
		assert.NotContains(t, inv, "composeRestartInvocation",
			"a redeploy must never run plain docker compose restart")
		assert.NotContains(t, inv, "composeDownInvocation",
			"a redeploy must never run docker compose down")
		assert.NotContains(t, inv, "composePullInvocation",
			"a redeploy must never pull images")
	}
}

// TestRedeploy_EmitsOnlyRedeployPrefixedStepIDs guards the frozen redeploy
// progress API: every emitted step ID carries the step_redeploy_ prefix.
func TestRedeploy_EmitsOnlyRedeployPrefixedStepIDs(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("redeploy-step-guard-app", 18080), nil)
	scriptRestartRunning(fx, t)

	var steps []string
	_, err := fx.eng.RedeployStack(t.Context(), types.RestartRequest{AppID: fx.appID}, func(step string, _ float64, _ string) {
		steps = append(steps, step)
	}, &fakeConfirmer{})
	require.NoError(t, err)

	require.NotEmpty(t, steps)
	for _, step := range steps {
		assert.True(t, strings.HasPrefix(step, "step_redeploy_"),
			"the redeploy progress stream must only carry step_redeploy_* IDs, got %q", step)
	}
}

// TestRedeploy_ComposeUpFailureSurfacesFailClosed proves the fail-closed
// contract for an invalid override: a `docker compose up` error (the typed
// code internal/docker would surface for a compose-config rejection)
// propagates unchanged rather than being swallowed, and the redeploy aborts
// before the status verify.
func TestRedeploy_ComposeUpFailureSurfacesFailClosed(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("redeploy-invalid-override-app", 18080), nil)
	fx.fake.runFn = func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		if fmt.Sprintf("%T", inv) == "docker.composeUpInvocation" {
			return docker.CommandResult{}, types.NewError(
				types.ErrCodeDockerUnavailable,
				"docker compose up failed",
				"fix the override and retry",
			)
		}
		return docker.CommandResult{}, nil
	}

	res, err := fx.eng.RedeployStack(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeDockerUnavailable, typed.Code)
	assert.NotContains(t, fx.fake.invocationTypes, "docker.projectContainerListInvocation",
		"a compose up failure must abort before the status verify")
}

// TestRedeploy_UserEnvSecretNeverLeaksIntoError binds the redeploy redactor
// upgrade: redeploy re-reads .env.user, so its values (which may be bare user
// secrets) MUST be folded into the active redactor that wraps the Docker
// client, scrubbing any interpolated secret a compose-up error would echo.
// The redactor is what the real docker command executor applies to errors
// (the fake factory bypasses that), so this captures the redactor the
// redeploy seam hands the factory and asserts it redacts the planted
// .env.user secret — the REAL redactor on the real seam. Swap the
// executeRedeploy builder back to NewActiveRedactor(nil) and this fails: a
// bare value not in KEY=VALUE shape has no structural pattern to scrub it, so
// only the literal folded from .env.user redacts it.
func TestRedeploy_UserEnvSecretNeverLeaksIntoError(t *testing.T) {
	t.Parallel()

	const userSecret = "redeploy-env-user-bare-secret-7c2d9e"

	fx := newRestartFixture(t, appFixture("redeploy-secret-leak-app", 18080), nil)
	require.NoError(t, os.WriteFile(
		filepath.Join(fx.stackPath, ".env.user"),
		[]byte("API_KEY="+userSecret+"\n"),
		0o600,
	))

	var capturedRedactor security.Redactor
	core.SetInstallDockerClientFactoryForTest(fx.eng, func(redactor security.Redactor) (docker.Client, error) {
		capturedRedactor = redactor
		fx.fake.runFn = func(_ int, _ docker.Invocation) (docker.CommandResult, error) {
			return docker.CommandResult{}, nil
		}
		return fx.fake, nil
	})

	res, err := fx.eng.RedeployStack(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	require.NotNil(t, capturedRedactor, "executeRedeploy must build a docker client with a redactor")
	redacted := capturedRedactor.Redact("compose up rejected: value " + userSecret + " for service app")
	assert.Equal(t, 0, strings.Count(redacted, userSecret),
		"the .env.user secret literal must be scrubbed by the redeploy redactor")
	assert.Contains(t, redacted, security.RedactedPlaceholder,
		"the redacted placeholder must replace the leaked .env.user secret")
}

// TestRedeploy_NilConfirmerRefuses proves a nil confirmer refuses with
// ErrCodeUsageValidation and runs no compose up.
func TestRedeploy_NilConfirmerRefuses(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("redeploy-nil-confirmer-app", 18080), nil)
	scriptRestartHappyPath(fx)

	res, err := fx.eng.RedeployStack(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, nil)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.ErrorContains(t, err, "confirmer is required")
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeUpInvocation",
		"a nil confirmer must refuse before any compose up")
}

// TestRedeploy_DeclineCancelsWithoutComposeUp proves the confirm gate: a
// decline maps to ErrCodeUserCanceled and runs no compose up.
func TestRedeploy_DeclineCancelsWithoutComposeUp(t *testing.T) {
	t.Parallel()

	fx := newRestartFixture(t, appFixture("redeploy-decline-app", 18080), nil)
	scriptRestartHappyPath(fx)

	confirmer := &fakeConfirmer{confirmFn: func(context.Context, types.Confirmation) (bool, error) {
		return false, nil
	}}
	res, err := fx.eng.RedeployStack(t.Context(), types.RestartRequest{AppID: fx.appID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUserCanceled, typed.Code)
	assert.NotContains(t, fx.fake.invocationTypes, "docker.composeUpInvocation",
		"a decline must run no compose up")
}

// TestRedeploy_RefusesUnmanagedAndEmptyAppID covers the managed-only refusals:
// an empty app id and an uninstalled app surface usage-validation errors before
// any Docker call.
func TestRedeploy_RefusesUnmanagedAndEmptyAppID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		appID        string
		wantContains string
	}{
		{name: "empty app id", appID: "", wantContains: "app id is required"},
		{name: "app not installed", appID: "ghost-app", wantContains: "app is not installed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, appFixture("redeploy-refusal-app", 18080))))
			_ = stateDir
			fake := &fakeDockerClient{}
			core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

			res, err := eng.RedeployStack(t.Context(), types.RestartRequest{AppID: tt.appID}, nil, &fakeConfirmer{})
			require.Error(t, err)
			assert.Nil(t, res)
			assertUsageValidation(t, err)
			assert.ErrorContains(t, err, tt.wantContains)
			assert.Zero(t, fake.calls, "refusals must happen before any docker command")
		})
	}
}
