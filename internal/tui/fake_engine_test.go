package tui

import (
	"context"
	"sync"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

type fakeEngine struct {
	logPath                string
	listApps               []types.AppInfo
	listErr                error
	listCalls              int
	listStatusApps         []types.AppRuntimeStatus
	listStatusErr          error
	listStatusCalls        int
	statuses               map[string]*types.AppStatus
	statusErr              error
	statusCalls            []string
	restartResult          *types.RestartResult
	restartErr             error
	restartCalls           []string
	redeployResult         *types.RestartResult
	redeployErr            error
	redeployCalls          []string
	stopAllResult          *types.StopAllResult
	stopAllErr             error
	stopAllCalls           int
	uninstallResult        *types.UninstallResult
	uninstallErr           error
	uninstallCalls         int
	validationResult       *types.ValidationResult
	validationErr          error
	validateCalls          []string
	catalogApps            []types.CatalogApp
	catalogAppsErr         error
	availableAppsCalls     int
	catalogDetails         map[string]*types.CatalogApp
	catalogDetailErr       error
	availableAppCalls      []types.CatalogAppQuery
	installResult          *types.InstallResult
	installErr             error
	installOutcomes        []installOutcome
	installRequests        []types.InstallRequest
	updateResult           *types.UpdateResult
	updateErr              error
	updateRequests         []types.UpdateRequest
	removeResult           *types.RemoveResult
	removeErr              error
	removeRequests         []types.RemoveRequest
	deleteResult           *types.DeleteResult
	deleteErr              error
	deleteRequests         []types.DeleteRequest
	backups                []types.BackupInfo
	backupsErr             error
	listBackupCalls        []string
	restoreResult          *types.RestoreBackupResult
	restoreErr             error
	restoreRequests        []types.RestoreBackupRequest
	settings               *types.Settings
	settingsErr            error
	settingsCalls          int
	updateSettingsErr      error
	updatedSettings        []types.Settings
	runtimeLockStatus      *types.RuntimeLockStatus
	runtimeLockErr         error
	runtimeLockStatusCalls int
	clearRuntimeLockResult *types.RuntimeLockStatus
	clearRuntimeLockErr    error
	clearRuntimeLockCalls  int

	resourceSettings      *types.ResourceSettings
	resourceSettingsErr   error
	resourceSettingsCalls []string
	reconfigureResult     *types.ReconfigureResult
	reconfigureErr        error
	reconfigureRequests   []types.ReconfigureRequest

	ensureOverridePath  string
	ensureOverrideErr   error
	ensureOverrideCalls []string
	ensureEnvPath       string
	ensureEnvErr        error
	ensureEnvCalls      []string
	viewEnvResult       *types.ViewEnvResult
	viewEnvErr          error
	viewEnvCalls        []string
	validateStackWarn   []string
	validateStackErr    error
	validateStackCalls  []string
	rewireStackDone     bool
	rewireStackPath     string
	rewireStackErr      error
	rewireStackCalls    []string

	catalogUpdateStatus    *types.CatalogUpdateStatus
	catalogUpdateStatusErr error
	checkCatalogUpdateN    int
	catalogUpdateResult    *types.CatalogUpdateResult
	catalogUpdateApplyErr  error
	catalogUpdateRequests  []types.CatalogUpdateRequest
	selfUpdateStatus       *types.SelfUpdateStatus
	selfUpdateStatusErr    error
	checkSelfUpdateN       int
	selfUpdateResult       *types.SelfUpdateResult
	selfUpdateApplyErr     error
	selfUpdateRequests     []types.SelfUpdateRequest
	imageUpdateReport      *types.ImageUpdateReport
	imageUpdateErr         error
	checkImageUpdatesCalls []string

	// Daily-on-launch update-check gate.
	dailyLaunchCheckDue       bool
	dailyLaunchCheckDueErr    error
	dailyLaunchCheckDueN      int
	recordDailyLaunchCheckErr error
	recordDailyLaunchCheckN   int

	closeErr error
	closeMu  sync.Mutex
	closeN   int
}

var _ engine.Engine = (*fakeEngine)(nil)

func (f *fakeEngine) List(context.Context) ([]types.AppInfo, error) {
	f.listCalls++
	return f.listApps, f.listErr
}

func (f *fakeEngine) ListStatus(context.Context) ([]types.AppRuntimeStatus, error) {
	f.listStatusCalls++
	return f.listStatusApps, f.listStatusErr
}

func (f *fakeEngine) Status(_ context.Context, appID string) (*types.AppStatus, error) {
	f.statusCalls = append(f.statusCalls, appID)
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if f.statuses != nil {
		return f.statuses[appID], nil
	}
	return nil, nil
}

func (f *fakeEngine) Logs(context.Context, types.LogsRequest, engine.LogLineFn) error {
	return nil
}

// installOutcome is one queued Install return, so a test can drive the
// port-remap re-invoke loop (conflict, then conflict again, then success)
// through repeated Install calls.
type installOutcome struct {
	result *types.InstallResult
	err    error
}

func (f *fakeEngine) Install(
	_ context.Context,
	req types.InstallRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.InstallResult, error) {
	f.installRequests = append(f.installRequests, req)
	if len(f.installOutcomes) > 0 {
		out := f.installOutcomes[0]
		f.installOutcomes = f.installOutcomes[1:]
		return out.result, out.err
	}
	return f.installResult, f.installErr
}

func (f *fakeEngine) Update(
	_ context.Context,
	req types.UpdateRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.UpdateResult, error) {
	f.updateRequests = append(f.updateRequests, req)
	return f.updateResult, f.updateErr
}

func (f *fakeEngine) Remove(
	_ context.Context,
	req types.RemoveRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.RemoveResult, error) {
	f.removeRequests = append(f.removeRequests, req)
	return f.removeResult, f.removeErr
}

func (f *fakeEngine) Restart(
	_ context.Context,
	req types.RestartRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.RestartResult, error) {
	f.restartCalls = append(f.restartCalls, req.AppID)
	return f.restartResult, f.restartErr
}

func (f *fakeEngine) RedeployStack(
	_ context.Context,
	req types.RestartRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.RestartResult, error) {
	f.redeployCalls = append(f.redeployCalls, req.AppID)
	return f.redeployResult, f.redeployErr
}

func (f *fakeEngine) StopAll(
	_ context.Context,
	_ types.StopAllRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.StopAllResult, error) {
	f.stopAllCalls++
	return f.stopAllResult, f.stopAllErr
}

// ResourceSettings and Reconfigure back the per-app resource-management
// TUI surface (issue #28): ResourceSettings feeds the read-only current
// values and bands, Reconfigure records the request the screen builds.

func (f *fakeEngine) ResourceSettings(_ context.Context, appID string) (*types.ResourceSettings, error) {
	f.resourceSettingsCalls = append(f.resourceSettingsCalls, appID)
	return f.resourceSettings, f.resourceSettingsErr
}

func (f *fakeEngine) Reconfigure(
	_ context.Context,
	req types.ReconfigureRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.ReconfigureResult, error) {
	f.reconfigureRequests = append(f.reconfigureRequests, req)
	return f.reconfigureResult, f.reconfigureErr
}

// EnsureUserOverride, EnsureUserEnv, ViewEnvRedacted, and ValidateStack back
// the user-overlay edit/view TUI surface (issue #97): the Ensure* methods
// return the seeded file path, ViewEnvRedacted returns a pre-redacted view,
// and ValidateStack returns the warn-but-allow post-edit outcome.

func (f *fakeEngine) EnsureUserOverride(_ context.Context, appID string) (string, error) {
	f.ensureOverrideCalls = append(f.ensureOverrideCalls, appID)
	return f.ensureOverridePath, f.ensureOverrideErr
}

func (f *fakeEngine) EnsureUserEnv(_ context.Context, appID string) (string, error) {
	f.ensureEnvCalls = append(f.ensureEnvCalls, appID)
	return f.ensureEnvPath, f.ensureEnvErr
}

func (f *fakeEngine) ViewEnvRedacted(_ context.Context, appID string) (*types.ViewEnvResult, error) {
	f.viewEnvCalls = append(f.viewEnvCalls, appID)
	return f.viewEnvResult, f.viewEnvErr
}

func (f *fakeEngine) ValidateStack(_ context.Context, appID string) ([]string, error) {
	f.validateStackCalls = append(f.validateStackCalls, appID)
	return f.validateStackWarn, f.validateStackErr
}

func (f *fakeEngine) RewireStack(_ context.Context, appID string, _ engine.Confirmer) (bool, string, error) {
	f.rewireStackCalls = append(f.rewireStackCalls, appID)
	return f.rewireStackDone, f.rewireStackPath, f.rewireStackErr
}

func (f *fakeEngine) Uninstall(
	_ context.Context,
	_ types.UninstallRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.UninstallResult, error) {
	f.uninstallCalls++
	return f.uninstallResult, f.uninstallErr
}

func (f *fakeEngine) ValidateConfig(_ context.Context, appID string) (*types.ValidationResult, error) {
	f.validateCalls = append(f.validateCalls, appID)
	return f.validationResult, f.validationErr
}

func (f *fakeEngine) ListBackups(_ context.Context, appID string) ([]types.BackupInfo, error) {
	f.listBackupCalls = append(f.listBackupCalls, appID)
	return f.backups, f.backupsErr
}

func (f *fakeEngine) RestoreBackup(
	_ context.Context,
	req types.RestoreBackupRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.RestoreBackupResult, error) {
	f.restoreRequests = append(f.restoreRequests, req)
	return f.restoreResult, f.restoreErr
}

func (f *fakeEngine) AvailableApps(context.Context, types.CatalogQuery) ([]types.CatalogApp, error) {
	f.availableAppsCalls++
	return f.catalogApps, f.catalogAppsErr
}

func (f *fakeEngine) AvailableApp(_ context.Context, query types.CatalogAppQuery) (*types.CatalogApp, error) {
	f.availableAppCalls = append(f.availableAppCalls, query)
	if f.catalogDetailErr != nil {
		return nil, f.catalogDetailErr
	}
	if f.catalogDetails != nil {
		return f.catalogDetails[query.AppID], nil
	}
	return nil, nil
}

func (f *fakeEngine) DeleteApp(
	_ context.Context,
	req types.DeleteRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.DeleteResult, error) {
	f.deleteRequests = append(f.deleteRequests, req)
	return f.deleteResult, f.deleteErr
}

func (f *fakeEngine) RuntimeLockStatus(context.Context) (*types.RuntimeLockStatus, error) {
	f.runtimeLockStatusCalls++
	return f.runtimeLockStatus, f.runtimeLockErr
}

func (f *fakeEngine) ClearStaleRuntimeLock(context.Context, types.Confirmer) (*types.RuntimeLockStatus, error) {
	f.clearRuntimeLockCalls++
	return f.clearRuntimeLockResult, f.clearRuntimeLockErr
}

func (f *fakeEngine) CheckCatalogUpdate(context.Context, types.CatalogUpdateQuery) (*types.CatalogUpdateStatus, error) {
	f.checkCatalogUpdateN++
	return f.catalogUpdateStatus, f.catalogUpdateStatusErr
}

func (f *fakeEngine) ApplyCatalogUpdate(
	_ context.Context,
	req types.CatalogUpdateRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.CatalogUpdateResult, error) {
	f.catalogUpdateRequests = append(f.catalogUpdateRequests, req)
	return f.catalogUpdateResult, f.catalogUpdateApplyErr
}

func (f *fakeEngine) CheckSelfUpdate(context.Context, types.SelfUpdateQuery) (*types.SelfUpdateStatus, error) {
	f.checkSelfUpdateN++
	return f.selfUpdateStatus, f.selfUpdateStatusErr
}

func (f *fakeEngine) ApplySelfUpdate(
	_ context.Context,
	req types.SelfUpdateRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.SelfUpdateResult, error) {
	f.selfUpdateRequests = append(f.selfUpdateRequests, req)
	return f.selfUpdateResult, f.selfUpdateApplyErr
}

func (f *fakeEngine) CheckImageUpdates(_ context.Context, req types.ImageUpdateQuery) (*types.ImageUpdateReport, error) {
	f.checkImageUpdatesCalls = append(f.checkImageUpdatesCalls, req.AppID)
	return f.imageUpdateReport, f.imageUpdateErr
}

func (f *fakeEngine) DailyLaunchCheckDue(context.Context) (bool, error) {
	f.dailyLaunchCheckDueN++
	return f.dailyLaunchCheckDue, f.dailyLaunchCheckDueErr
}

func (f *fakeEngine) RecordDailyLaunchCheck(context.Context) error {
	f.recordDailyLaunchCheckN++
	return f.recordDailyLaunchCheckErr
}

func (f *fakeEngine) Settings(context.Context) (*types.Settings, error) {
	f.settingsCalls++
	if f.settingsErr != nil {
		return nil, f.settingsErr
	}
	if f.settings == nil {
		return nil, nil
	}
	settings := *f.settings
	return &settings, nil
}

func (f *fakeEngine) UpdateSettings(_ context.Context, settings types.Settings) error {
	f.updatedSettings = append(f.updatedSettings, settings)
	return f.updateSettingsErr
}

func (f *fakeEngine) Close() error {
	f.closeMu.Lock()
	defer f.closeMu.Unlock()

	f.closeN++
	return f.closeErr
}

func (f *fakeEngine) LogPath() string { return f.logPath }

func (f *fakeEngine) closeCount() int {
	f.closeMu.Lock()
	defer f.closeMu.Unlock()

	return f.closeN
}
