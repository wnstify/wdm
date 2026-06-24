package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// fakeEngine is a configurable [engine.Engine] double used by the
// envelope-contract tests. It records what each leaf passed it — most
// importantly whether the [types.ProgressFn] was nil (the --json
// progress-suppression contract, PRD §32) and which [types.Confirmer]
// arrived — and returns caller-configured results and errors so a test
// can drive a leaf's RunE end-to-end without a real catalog, Docker
// daemon, or filesystem.
// The double deliberately does NOT invoke the recorded Confirmer. These
// tests pin the JSON envelope a command emits on stdout, not the
// confirmation flow (prompts write to stderr; the engine-side confirm
// tests cover the accept/decline contract, and cliConfirmer's own prompt
// rendering is 's coverage scope); not calling Confirm keeps
// stdout limited to envelope bytes so the line-count and decode
// assertions stay exact.
type fakeEngine struct {
	// Configured return values. The result pointers are returned as-is so
	// a test controls the exact payload the leaf serializes.
	installResult       *types.InstallResult
	updateResult        *types.UpdateResult
	removeResult        *types.RemoveResult
	statusResult        *types.AppStatus
	listResult          []types.AppInfo
	listStatusResult    []types.AppRuntimeStatus
	settings            *types.Settings
	restartResult       *types.RestartResult
	redeployResult      *types.RestartResult
	stopAllResult       *types.StopAllResult
	uninstallResult     *types.UninstallResult
	validationResult    *types.ValidationResult
	backupsResult       []types.BackupInfo
	restoreResult       *types.RestoreBackupResult
	availableAppsResult []types.CatalogApp
	availableAppResult  *types.CatalogApp
	deleteResult        *types.DeleteResult
	runtimeLockResult   *types.RuntimeLockStatus
	resourceSettings    *types.ResourceSettings
	reconfigureResult   *types.ReconfigureResult

	catalogUpdateStatus *types.CatalogUpdateStatus
	catalogUpdateResult *types.CatalogUpdateResult
	selfUpdateStatus    *types.SelfUpdateStatus
	selfUpdateResult    *types.SelfUpdateResult
	imageUpdateReport   *types.ImageUpdateReport

	// logsLines is replayed one-by-one through the onLine callback when
	// Logs is invoked, modeling the engine streaming N parsed lines.
	logsLines []types.LogLine

	// err, when non-nil, is returned by every state-changing/read method
	// (the leaf's `if err != nil { return err }` path) so the typed-error
	// contract can be exercised. Logs returns it after replaying any
	// configured lines (the error test configures none).
	err error

	// Recorded inputs from the most recent state-changing/read call.
	installReq     types.InstallRequest
	updateReq      types.UpdateRequest
	removeReq      types.RemoveRequest
	logsReq        types.LogsRequest
	statusAppID    string
	redeployReq    types.RestartRequest
	reconfigureReq types.ReconfigureRequest
	resourcesAppID string
	progressWasNil bool
	confirmer      types.Confirmer
	logLineWasNil  bool

	// User-overlay edit/view doubles (T9). The Ensure* methods return a
	// canned path and record the app id; ViewEnvRedacted returns a canned
	// redacted view; ValidateStack returns canned warnings/err and records
	// that it was called, so the edit leaf's post-edit validation is testable.
	viewEnvResult       *types.ViewEnvResult
	ensureOverridePath  string
	ensureEnvPath       string
	validateStackWarn   []string
	validateStackErr    error
	ensureOverrideAppID string
	ensureEnvAppID      string
	viewEnvAppID        string
	validateStackAppID  string
	validateStackCalled bool

	// RewireStack double (T8 edit-env migration). rewireCalled records that
	// the edit-env path consulted the migration; rewireAppID the target;
	// rewireDone/rewireErr the canned (rewired, err) outcome a test drives to
	// exercise the rewired / declined / hard-error branches.
	rewireCalled bool
	rewireAppID  string
	rewireDone   bool
	rewireErr    error
}

// Compile-time proof the double satisfies the full surface; if the
// interface grows a method, this fails to build and the double must
// follow (golang-structs-interfaces: assert interface satisfaction).
var _ engine.Engine = (*fakeEngine)(nil)

func (f *fakeEngine) List(_ context.Context) ([]types.AppInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.listResult, nil
}

func (f *fakeEngine) ListStatus(_ context.Context) ([]types.AppRuntimeStatus, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.listStatusResult, nil
}

func (f *fakeEngine) Status(_ context.Context, appID string) (*types.AppStatus, error) {
	f.statusAppID = appID
	if f.err != nil {
		return nil, f.err
	}
	return f.statusResult, nil
}

func (f *fakeEngine) Logs(_ context.Context, req types.LogsRequest, onLine engine.LogLineFn) error {
	f.logsReq = req
	f.logLineWasNil = onLine == nil
	if onLine != nil {
		for _, line := range f.logsLines {
			onLine(line)
		}
	}
	return f.err
}

func (f *fakeEngine) Install(
	_ context.Context,
	req types.InstallRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.InstallResult, error) {
	f.installReq = req
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.installResult, nil
}

func (f *fakeEngine) Update(
	_ context.Context,
	req types.UpdateRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.UpdateResult, error) {
	f.updateReq = req
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.updateResult, nil
}

func (f *fakeEngine) Remove(
	_ context.Context,
	req types.RemoveRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.RemoveResult, error) {
	f.removeReq = req
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.removeResult, nil
}

func (f *fakeEngine) Settings(_ context.Context) (*types.Settings, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.settings, nil
}

func (f *fakeEngine) UpdateSettings(_ context.Context, _ types.Settings) error {
	return f.err
}

// The engine-gap methods follow the same recording conventions
// as the leaves: state-changing methods record whether the
// progress callback was nil and which confirmer arrived, and every
// method returns its configured result/error. Leaf tests add per-request
// recording fields only when they need them, keeping the double minimal.

func (f *fakeEngine) Restart(
	_ context.Context,
	_ types.RestartRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.RestartResult, error) {
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.restartResult, nil
}

func (f *fakeEngine) RedeployStack(
	_ context.Context,
	req types.RestartRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.RestartResult, error) {
	f.redeployReq = req
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.redeployResult, nil
}

func (f *fakeEngine) StopAll(
	_ context.Context,
	_ types.StopAllRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.StopAllResult, error) {
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.stopAllResult, nil
}

func (f *fakeEngine) Uninstall(
	_ context.Context,
	_ types.UninstallRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.UninstallResult, error) {
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.uninstallResult, nil
}

func (f *fakeEngine) ValidateConfig(_ context.Context, _ string) (*types.ValidationResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.validationResult, nil
}

func (f *fakeEngine) ListBackups(_ context.Context, _ string) ([]types.BackupInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.backupsResult, nil
}

func (f *fakeEngine) RestoreBackup(
	_ context.Context,
	_ types.RestoreBackupRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.RestoreBackupResult, error) {
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.restoreResult, nil
}

func (f *fakeEngine) AvailableApps(_ context.Context, _ types.CatalogQuery) ([]types.CatalogApp, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.availableAppsResult, nil
}

func (f *fakeEngine) AvailableApp(_ context.Context, _ types.CatalogAppQuery) (*types.CatalogApp, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.availableAppResult, nil
}

func (f *fakeEngine) DeleteApp(
	_ context.Context,
	_ types.DeleteRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.DeleteResult, error) {
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.deleteResult, nil
}

func (f *fakeEngine) ResourceSettings(_ context.Context, appID string) (*types.ResourceSettings, error) {
	f.resourcesAppID = appID
	if f.err != nil {
		return nil, f.err
	}
	if f.resourceSettings != nil {
		return f.resourceSettings, nil
	}
	// The reconfigure path resolves an omitted --service through
	// ResourceSettings, so a reconfigure-only fixture that wires only a
	// reconfigureResult still needs a primary service to resolve. Default
	// to a single adjustable "app" service when no settings are set.
	return &types.ResourceSettings{
		AppID:    appID,
		Services: []types.ResourceServiceSettings{{Service: "app", Adjustable: true}},
	}, nil
}

func (f *fakeEngine) Reconfigure(
	_ context.Context,
	req types.ReconfigureRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.ReconfigureResult, error) {
	f.reconfigureReq = req
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.reconfigureResult, nil
}

func (f *fakeEngine) EnsureUserOverride(_ context.Context, appID string) (string, error) {
	f.ensureOverrideAppID = appID
	if f.err != nil {
		return "", f.err
	}
	return f.ensureOverridePath, nil
}

func (f *fakeEngine) EnsureUserEnv(_ context.Context, appID string) (string, error) {
	f.ensureEnvAppID = appID
	if f.err != nil {
		return "", f.err
	}
	return f.ensureEnvPath, nil
}

func (f *fakeEngine) ViewEnvRedacted(_ context.Context, appID string) (*types.ViewEnvResult, error) {
	f.viewEnvAppID = appID
	if f.err != nil {
		return nil, f.err
	}
	return f.viewEnvResult, nil
}

func (f *fakeEngine) ValidateStack(_ context.Context, appID string) ([]string, error) {
	f.validateStackAppID = appID
	f.validateStackCalled = true
	return f.validateStackWarn, f.validateStackErr
}

func (f *fakeEngine) RewireStack(_ context.Context, appID string, _ types.Confirmer) (bool, string, error) {
	f.rewireCalled = true
	f.rewireAppID = appID
	return f.rewireDone, "", f.rewireErr
}

func (f *fakeEngine) RuntimeLockStatus(_ context.Context) (*types.RuntimeLockStatus, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.runtimeLockResult, nil
}

func (f *fakeEngine) ClearStaleRuntimeLock(_ context.Context, confirmer types.Confirmer) (*types.RuntimeLockStatus, error) {
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.runtimeLockResult, nil
}

// The trust/distribution methods follow the
// same recording conventions: the Apply* methods record whether the
// progress callback was nil and which confirmer arrived (mirroring the
// state-changing Update path), and the read-only Check* methods just
// return their configured result/error (mirroring Status). Leaf tests add
// per-request recording fields only when they need them, keeping the double
// minimal.

func (f *fakeEngine) CheckCatalogUpdate(_ context.Context, _ types.CatalogUpdateQuery) (*types.CatalogUpdateStatus, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.catalogUpdateStatus, nil
}

func (f *fakeEngine) ApplyCatalogUpdate(
	_ context.Context,
	_ types.CatalogUpdateRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.CatalogUpdateResult, error) {
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.catalogUpdateResult, nil
}

func (f *fakeEngine) CheckSelfUpdate(_ context.Context, _ types.SelfUpdateQuery) (*types.SelfUpdateStatus, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.selfUpdateStatus, nil
}

func (f *fakeEngine) ApplySelfUpdate(
	_ context.Context,
	_ types.SelfUpdateRequest,
	onProgress engine.ProgressFn,
	confirmer types.Confirmer,
) (*types.SelfUpdateResult, error) {
	f.progressWasNil = onProgress == nil
	f.confirmer = confirmer
	if f.err != nil {
		return nil, f.err
	}
	return f.selfUpdateResult, nil
}

func (f *fakeEngine) CheckImageUpdates(_ context.Context, _ types.ImageUpdateQuery) (*types.ImageUpdateReport, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.imageUpdateReport, nil
}

// The daily-on-launch update-check gate is consumed only by the TUI;
// the CLI never calls it, so the double returns zero values to keep the
// interface satisfied.

func (f *fakeEngine) DailyLaunchCheckDue(_ context.Context) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return false, nil
}

func (f *fakeEngine) RecordDailyLaunchCheck(_ context.Context) error { return f.err }

func (f *fakeEngine) Close() error { return nil }

func (f *fakeEngine) LogPath() string { return "" }

// runLeaf drives one CLI invocation through [NewRootCmd] — the honest
// end-to-end path, since the persistent --json flag only resolves
// through the root — wiring fake as the lazy engine the factory returns.
// It returns captured stdout, stderr, and the Execute error so a test
// can assert on all three. Stdin is an empty buffer: the fake never
// calls Confirm, so no prompt ever reads it.
func runLeaf(t *testing.T, fake *fakeEngine, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	root := NewRootCmd("test", func() (engine.Engine, error) {
		return fake, nil
	})

	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs(args)
	root.SetContext(t.Context())

	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}
