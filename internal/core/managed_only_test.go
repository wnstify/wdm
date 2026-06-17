package core_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/pkg/types"
)

// handRolledStackContents are the plausible files a user's own,
// non-wdm Compose stack would carry: a docker-compose.yml and a .env
// but no.wdm.lock. This is THE managed-only protection target — wdm
// must refuse to update, remove, inspect, or stream such a directory
// because it never installed it (PRD §9, §10).
func writeHandRolledStack(t *testing.T, stackBase, appID string) string {
	t.Helper()

	dir := filepath.Join(stackBase, appID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, ".env"),
		[]byte("SECRET=hand-rolled\n"),
		0o600,
	))
	return dir
}

// TestManagedOnlyProtection_RefusesHandRolledStack proves the central
// managed-only guarantee: a directory that looks exactly like an installed
// app — a real docker-compose.yml plus a .env — but lacks the wdm-written
// .wdm.lock manifest is NOT managed by wdm,
// and every state-changing or read path (Update / Remove / Status /
// Logs) refuses it with a typed ErrCodeUsageValidation BEFORE any
// Docker command runs. The fake docker
// client's zero call count is the structural proof that the refusal
// precedes client construction and invocation.
func TestManagedOnlyProtection_RefusesHandRolledStack(t *testing.T) {
	t.Parallel()

	const appID = "hand-rolled-app"

	tests := []struct {
		name string
		call func(t *testing.T, fx *managedOnlyFixture) error
	}{
		{
			name: "update",
			call: func(t *testing.T, fx *managedOnlyFixture) error {
				t.Helper()
				res, err := fx.eng.Update(
					t.Context(),
					types.UpdateRequest{AppID: appID, DryRun: true},
					nil,
					&fakeConfirmer{},
				)
				assert.Nil(t, res)
				return err
			},
		},
		{
			name: "remove",
			call: func(t *testing.T, fx *managedOnlyFixture) error {
				t.Helper()
				res, err := fx.eng.Remove(
					t.Context(),
					types.RemoveRequest{AppID: appID},
					nil,
					&fakeConfirmer{},
				)
				assert.Nil(t, res)
				return err
			},
		},
		{
			name: "status",
			call: func(t *testing.T, fx *managedOnlyFixture) error {
				t.Helper()
				status, err := fx.eng.Status(t.Context(), appID)
				assert.Nil(t, status)
				return err
			},
		},
		{
			name: "logs",
			call: func(t *testing.T, fx *managedOnlyFixture) error {
				t.Helper()
				return fx.eng.Logs(
					t.Context(),
					types.LogsRequest{AppID: appID},
					func(types.LogLine) {},
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fx := newManagedOnlyFixture(t, appID)
			writeHandRolledStack(t, fx.stackBase, appID)

			err := tt.call(t, fx)
			require.Error(t, err)
			assertUsageValidation(t, err)
			assert.ErrorContains(t, err, "not managed by wdm")
			assert.NotErrorIs(t, err, types.ErrNotImplemented,
				"a managed-only refusal must precede any execution boundary")
			assert.Zero(t, fx.fake.calls,
				"the managed-only refusal must run zero docker commands")
			assert.Zero(t, fx.fake.streamCalls,
				"the managed-only refusal must never open a log stream")
		})
	}
}

// TestManagedOnlyProtection_RefusesMismatchedAppID proves the second
// managed-identity arm of: a stack directory that DOES carry a
// readable.wdm.lock — but whose recorded app_id names a DIFFERENT app
// — is not a valid managed target for the requested app. resolveManagedStack
// matches on lock.AppID, so a manifest for "other-app" under the
// requested app's conventional directory refuses with a typed
// ErrCodeUsageValidation before any Docker call rather than silently
// operating on the neighbor's stack (PRD §10).
func TestManagedOnlyProtection_RefusesMismatchedAppID(t *testing.T) {
	t.Parallel()

	const requestedApp = "requested-app"
	const recordedApp = "other-app"

	tests := []struct {
		name string
		call func(t *testing.T, fx *managedOnlyFixture) error
	}{
		{
			name: "update",
			call: func(t *testing.T, fx *managedOnlyFixture) error {
				t.Helper()
				res, err := fx.eng.Update(
					t.Context(),
					types.UpdateRequest{AppID: requestedApp, DryRun: true},
					nil,
					&fakeConfirmer{},
				)
				assert.Nil(t, res)
				return err
			},
		},
		{
			name: "remove",
			call: func(t *testing.T, fx *managedOnlyFixture) error {
				t.Helper()
				res, err := fx.eng.Remove(
					t.Context(),
					types.RemoveRequest{AppID: requestedApp},
					nil,
					&fakeConfirmer{},
				)
				assert.Nil(t, res)
				return err
			},
		},
		{
			name: "status",
			call: func(t *testing.T, fx *managedOnlyFixture) error {
				t.Helper()
				status, err := fx.eng.Status(t.Context(), requestedApp)
				assert.Nil(t, status)
				return err
			},
		},
		{
			name: "logs",
			call: func(t *testing.T, fx *managedOnlyFixture) error {
				t.Helper()
				return fx.eng.Logs(
					t.Context(),
					types.LogsRequest{AppID: requestedApp},
					func(types.LogLine) {},
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fx := newManagedOnlyFixture(t, requestedApp)
			// Write a valid manifest under the requested app's
			// conventional directory, but recorded for a different app.
			otherLock := removeStackLockForApp(
				appFixture(recordedApp, 18080),
				filepath.Join(fx.stackBase, requestedApp),
			)
			writeStatusStackLock(t, fx.stackBase, requestedApp, otherLock)

			err := tt.call(t, fx)
			require.Error(t, err)
			assertUsageValidation(t, err)
			assert.NotErrorIs(t, err, types.ErrNotImplemented,
				"an app_id-mismatch refusal must precede any execution boundary")
			assert.Zero(t, fx.fake.calls,
				"the app_id-mismatch refusal must run zero docker commands")
			assert.Zero(t, fx.fake.streamCalls,
				"the app_id-mismatch refusal must never open a log stream")
		})
	}
}

// managedOnlyFixture wires an engine over a catalog that DOES contain
// the requested app (so the refusal cannot be attributed to a missing
// catalog entry — it is purely the on-disk managed-identity gate) plus
// the observable fake docker client seam.
type managedOnlyFixture struct {
	eng       *core.Engine
	stackBase string
	fake      *fakeDockerClient
}

func newManagedOnlyFixture(t *testing.T, appID string) *managedOnlyFixture {
	t.Helper()

	app := appFixture(appID, 18080)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	return &managedOnlyFixture{
		eng:       eng,
		stackBase: stackBase,
		fake:      fake,
	}
}
