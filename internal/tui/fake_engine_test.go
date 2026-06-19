package tui

import (
	"context"
	"sync"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

type fakeEngine struct {
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
	stopAllResult          *types.StopAllResult
	stopAllErr             error
	stopAllCalls           int
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

func (f *fakeEngine) Install(
	_ context.Context,
	req types.InstallRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.InstallResult, error) {
	f.installRequests = append(f.installRequests, req)
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

func (f *fakeEngine) StopAll(
	_ context.Context,
	_ types.StopAllRequest,
	_ engine.ProgressFn,
	_ types.Confirmer,
) (*types.StopAllResult, error) {
	f.stopAllCalls++
	return f.stopAllResult, f.stopAllErr
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

func (f *fakeEngine) closeCount() int {
	f.closeMu.Lock()
	defer f.closeMu.Unlock()

	return f.closeN
}
