package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the `wdm apps update` registry disclosure contract: because
// Engine.Update resolves registry digests during planning and discloses
// them through the progress stream, the CLI must relay that
// disclosure to the user in plain mode (stderr) and keep the --json result
// envelope source-agnostic (no registry/digest key). The leaf must also
// never cross into a catalog-update method — the registry is reached only
// behind Engine.Update, never via a Docker registry shell-out.

// registryProgressEngine wraps the shared fakeEngine and emits the registry
// planning events through the progress callback when one
// is supplied. It mirrors what internal/core.planUpdateCheck streams: the
// pre-lookup network disclosure
// followed by the per-service digest disclosure. progressWasNil and the
// recorded UpdateRequest stay observable via the embedded fakeEngine.
type registryProgressUpdateEngine struct {
	*fakeEngine
}

func (f *registryProgressUpdateEngine) Update(
	_ context.Context,
	req types.UpdateRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.UpdateResult, error) {
	f.updateReq = req
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if onProgress != nil {
		onProgress(types.StepUpdatePlanning, 8, "checking the registry for image digests")
		onProgress(types.StepUpdatePlanning, 10, "service web: registry digest for example/web:2 is sha256:abc")
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.updateResult, nil
}

func newRegistryUpdateFake() *registryProgressUpdateEngine {
	return &registryProgressUpdateEngine{
		fakeEngine: &fakeEngine{
			updateResult: &types.UpdateResult{
				AppID:                   "vaultwarden",
				PreviousTemplateVersion: "1.0.0",
				NewTemplateVersion:      "1.1.0",
				UpdatedServices:         []string{"web example/web:1 -> example/web:2"},
				RiskClassifications:     []string{"safe"},
			},
		},
	}
}

// runRegistryUpdateLeaf drives `apps update` through NewRootCmd with the
// registry-progress engine, mirroring runLeaf but threading the wrapper as
// the lazy engine the factory returns.
func runRegistryUpdateLeaf(t *testing.T, fake *registryProgressUpdateEngine, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	root := NewRootCmd("test", func() (engine.Engine, error) {
		return fake, nil
	})

	var outBuf, errBuf strings.Builder
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	root.SetContext(t.Context())

	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// TestAppsUpdate_Plain_RelaysRegistryDisclosureToStderr pins that in plain
// mode the registry network disclosure and the per-service digest line both
// reach the user on STDERR (the progress channel) while STDOUT carries only
// the PRD §20 update block — never a registry/digest line. Both the dry-run
// (check) and the apply path stream the disclosure because planUpdateCheck
// runs for both.
func TestAppsUpdate_Plain_RelaysRegistryDisclosureToStderr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"dry_run", []string{"apps", "update", "vaultwarden", "--dry-run"}},
		{"apply", []string{"apps", "update", "vaultwarden", "--yes"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newRegistryUpdateFake()
			stdout, stderr, err := runRegistryUpdateLeaf(t, fake, tc.args...)
			require.NoError(t, err)

			// Plain mode wires a non-nil ProgressFn so the disclosure can flow.
			assert.False(t, fake.progressWasNil, "plain mode must wire a non-nil ProgressFn for the disclosure")

			// The disclosure reaches the user on stderr.
			assert.Contains(t, stderr, "checking the registry for image digests",
				"the pre-lookup network disclosure must reach the user on stderr")
			assert.Contains(t, stderr, "registry digest for",
				"the per-service registry digest must reach the user on stderr")

			// Source-agnostic stdout: the update block, no registry/digest line.
			assert.Contains(t, stdout, "vaultwarden", "stdout carries the §20 update block")
			assert.NotContains(t, stdout, "registry digest",
				"the registry digest is a progress disclosure, not part of the stdout result block")
			assert.NotContains(t, stdout, "checking the registry",
				"the network disclosure belongs on stderr, never stdout")
		})
	}
}

// TestAppsUpdate_JSON_EnvelopeIsSourceAgnostic pins that under --json the
// stdout envelope is a single wdm.v1 UpdateResult with NO registry/digest
// key (the result type is source-agnostic — the registry is disclosure-only,
// so no disclosure line ever lands on stdout.
func TestAppsUpdate_JSON_EnvelopeIsSourceAgnostic(t *testing.T) {
	t.Parallel()

	fake := newRegistryUpdateFake()
	stdout, _, err := runRegistryUpdateLeaf(t, fake, "apps", "update", "vaultwarden", "--yes", "--json")
	require.NoError(t, err)

	// --json suppresses progress entirely (PRD §32) so the registry
	// disclosure can never appear on the single-envelope stdout.
	assert.True(t, fake.progressWasNil, "--json must hand the engine a nil ProgressFn (no disclosure on stdout)")

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 1, "update --json must emit exactly one envelope on stdout")
	assert.Equal(t, lines[0]+"\n", stdout, "stdout must be exactly the envelope line")

	data := decodeEnvelopeData(t, lines[0])
	assert.Equal(t, "vaultwarden", data["app_id"])
	assert.Equal(t, "1.1.0", data["new_template_version"])

	// The envelope data is the UpdateResult verbatim: no registry/digest
	// surface leaks into it. These keys must be absent on both the result
	// type and the rendered envelope (source-agnostic).
	for _, forbidden := range []string{"registry", "digest", "registry_digest", "image_digest"} {
		_, present := data[forbidden]
		assert.False(t, present, "envelope data must not carry a %q key (UpdateResult is source-agnostic)", forbidden)
	}
}

// TestAppsUpdate_CallsOnlyUpdate proves, via per-method call counters, that
// `apps update` invokes Engine.Update and nothing else on the trust/
// distribution surface — no catalog-update or registry/image-update method.
func TestAppsUpdate_CallsOnlyUpdate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
	}{
		{"apply", []string{"apps", "update", "vaultwarden", "--yes"}},
		{"dry_run", []string{"apps", "update", "vaultwarden", "--dry-run"}},
		{"json", []string{"apps", "update", "vaultwarden", "--yes", "--json"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			counter := &crossoverCountingEngine{
				updateResult: &types.UpdateResult{AppID: "vaultwarden", NewTemplateVersion: "1.1.0"},
			}

			root := NewRootCmd("test", func() (engine.Engine, error) { return counter, nil })
			root.SetOut(&strings.Builder{})
			root.SetErr(&strings.Builder{})
			root.SetIn(strings.NewReader(""))
			root.SetArgs(tc.args)
			root.SetContext(t.Context())
			require.NoError(t, root.Execute())

			assert.Equal(t, 1, counter.updateCalls, "apps update must call Engine.Update exactly once")
			assert.Zero(t, counter.checkCatalogUpdateCalls, "apps update must not call CheckCatalogUpdate")
			assert.Zero(t, counter.applyCatalogUpdateCalls, "apps update must not call ApplyCatalogUpdate")
			assert.Zero(t, counter.checkImageUpdatesCalls, "apps update must not call CheckImageUpdates (no registry shell-out crossover)")
			assert.Zero(t, counter.checkSelfUpdateCalls, "apps update must not call CheckSelfUpdate")
			assert.Zero(t, counter.applySelfUpdateCalls, "apps update must not call ApplySelfUpdate")
		})
	}
}

// crossoverCountingEngine is a minimal engine.Engine that counts the
// trust/distribution methods relevant to the no-crossover assertion. Only
// Update returns a configured result; the catalog/self/image methods record
// their (forbidden) invocation so a crossover would flip a counter.
type crossoverCountingEngine struct {
	updateResult *types.UpdateResult

	updateCalls             int
	checkCatalogUpdateCalls int
	applyCatalogUpdateCalls int
	checkImageUpdatesCalls  int
	checkSelfUpdateCalls    int
	applySelfUpdateCalls    int
}

var _ engine.Engine = (*crossoverCountingEngine)(nil)

func (c *crossoverCountingEngine) Update(
	_ context.Context,
	_ types.UpdateRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.UpdateResult, error) {
	c.updateCalls++
	return c.updateResult, nil
}

func (c *crossoverCountingEngine) CheckCatalogUpdate(context.Context, types.CatalogUpdateQuery) (*types.CatalogUpdateStatus, error) {
	c.checkCatalogUpdateCalls++
	return nil, nil
}

func (c *crossoverCountingEngine) ApplyCatalogUpdate(
	context.Context,
	types.CatalogUpdateRequest,
	engine.ProgressFn,
	types.Confirmer,
) (*types.CatalogUpdateResult, error) {
	c.applyCatalogUpdateCalls++
	return nil, nil
}

func (c *crossoverCountingEngine) CheckImageUpdates(context.Context, types.ImageUpdateQuery) (*types.ImageUpdateReport, error) {
	c.checkImageUpdatesCalls++
	return nil, nil
}

func (c *crossoverCountingEngine) CheckSelfUpdate(context.Context, types.SelfUpdateQuery) (*types.SelfUpdateStatus, error) {
	c.checkSelfUpdateCalls++
	return nil, nil
}

func (c *crossoverCountingEngine) ApplySelfUpdate(
	context.Context,
	types.SelfUpdateRequest,
	engine.ProgressFn,
	types.Confirmer,
) (*types.SelfUpdateResult, error) {
	c.applySelfUpdateCalls++
	return nil, nil
}

// The remaining engine.Engine methods are unreachable on the apps-update
// path; they satisfy the interface and return zero values.

func (c *crossoverCountingEngine) List(context.Context) ([]types.AppInfo, error) { return nil, nil }

func (c *crossoverCountingEngine) ListStatus(context.Context) ([]types.AppRuntimeStatus, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) Status(context.Context, string) (*types.AppStatus, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) Logs(context.Context, types.LogsRequest, engine.LogLineFn) error {
	return nil
}

func (c *crossoverCountingEngine) Install(
	context.Context,
	types.InstallRequest,
	engine.ProgressFn,
	types.Confirmer,
) (*types.InstallResult, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) Remove(
	context.Context,
	types.RemoveRequest,
	engine.ProgressFn,
	types.Confirmer,
) (*types.RemoveResult, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) Restart(
	context.Context,
	types.RestartRequest,
	engine.ProgressFn,
	types.Confirmer,
) (*types.RestartResult, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) StopAll(
	context.Context,
	types.StopAllRequest,
	engine.ProgressFn,
	types.Confirmer,
) (*types.StopAllResult, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) ValidateConfig(context.Context, string) (*types.ValidationResult, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) ListBackups(context.Context, string) ([]types.BackupInfo, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) RestoreBackup(
	context.Context,
	types.RestoreBackupRequest,
	engine.ProgressFn,
	types.Confirmer,
) (*types.RestoreBackupResult, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) AvailableApps(context.Context, types.CatalogQuery) ([]types.CatalogApp, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) AvailableApp(context.Context, types.CatalogAppQuery) (*types.CatalogApp, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) DeleteApp(
	context.Context,
	types.DeleteRequest,
	engine.ProgressFn,
	types.Confirmer,
) (*types.DeleteResult, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) RuntimeLockStatus(context.Context) (*types.RuntimeLockStatus, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) ClearStaleRuntimeLock(context.Context, types.Confirmer) (*types.RuntimeLockStatus, error) {
	return nil, nil
}

func (c *crossoverCountingEngine) DailyLaunchCheckDue(context.Context) (bool, error) {
	return false, nil
}

func (c *crossoverCountingEngine) RecordDailyLaunchCheck(context.Context) error { return nil }

func (c *crossoverCountingEngine) Settings(context.Context) (*types.Settings, error) { return nil, nil }

func (c *crossoverCountingEngine) UpdateSettings(context.Context, types.Settings) error { return nil }

func (c *crossoverCountingEngine) Close() error { return nil }
