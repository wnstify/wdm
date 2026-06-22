package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

const (
	minTerminalWidth  = 80
	minTerminalHeight = 24
)

var dashboardActions = []string{
	"Install an app",
	"Update apps",
	"Check my apps",
	"Stop all apps",
	"Backups",
	"Settings",
	"Update catalog",
	"Update wdm",
	"Uninstall wdm",
}

type screen int

const (
	screenDashboard screen = iota
	screenCheckApps
	screenStopAll
	screenStopAllResult
	screenUninstall
	screenUninstallResult
	screenAppActions
	screenFirstRunWelcome
	screenFirstRunSystemCheck
	screenInstallCatalog
	screenInstallForm
	screenInstallResult
	screenUpdateApps
	screenUpdateResult
	screenRemoveActions
	screenDeleteName
	screenRemoveResult
	screenDeleteResult
	screenBackupsApps
	screenBackupsList
	screenRestoreResult
	screenSettings
	screenRuntimeLock
	screenCatalogUpdate
	screenCatalogUpdateResult
	screenSelfUpdate
	screenSelfUpdateResult
	screenResources
)

var checkAppActions = []string{
	"View details",
	"Restart app",
	"Manage resources",
	"Remove app",
	"Validate config",
	"Return to dashboard",
}

type model struct {
	ctx                context.Context
	eng                engine.Engine
	bridge             engineBridge
	keys               keyMap
	width              int
	height             int
	cursor             int
	appCursor          int
	actionCursor       int
	screen             screen
	exiting            bool
	busy               bool
	firstRun           bool
	err                error
	progress           progressMsg
	modal              *confirmationRequestedMsg
	apps               []types.AppRuntimeStatus
	status             *types.AppStatus
	validation         *types.ValidationResult
	actionMessage      string
	catalogApps        []types.CatalogApp
	catalogCursor      int
	catalogDetail      *types.CatalogApp
	installFields      []installField
	installFieldCursor int
	installResult      *types.InstallResult
	updateResult       *types.UpdateResult
	removeActionCursor int
	removeResult       *types.RemoveResult
	deleteNameInput    string
	deleteResult       *types.DeleteResult
	backupAppID        string
	backups            []types.BackupInfo
	backupCursor       int
	restoreResult      *types.RestoreBackupResult
	settings           *types.Settings
	settingsFields     []settingsField
	settingsCursor     int
	settingsMessage    string
	runtimeLockStatus  *types.RuntimeLockStatus
	runtimeLockCursor  int
	runtimeLockMessage string

	resourceSettings    *types.ResourceSettings
	resourceLoadErr     error
	resourceService     string
	resourceFields      []resourceField
	resourceFieldCursor int
	reconfigureResult   *types.ReconfigureResult

	catalogUpdateStatus *types.CatalogUpdateStatus
	catalogUpdateResult *types.CatalogUpdateResult

	selfUpdateStatus *types.SelfUpdateStatus
	selfUpdateResult *types.SelfUpdateResult

	stopAllResult *types.StopAllResult

	uninstallResult *types.UninstallResult

	// launchCheckActive is true while the daily-on-launch update check runs, so
	// the dashboard renders launchCheckNotice. launchCheckBanner holds
	// the transient update-available summary after a successful check; it is
	// empty when no update is available or after the next keypress dismisses it.
	launchCheckActive bool
	launchCheckBanner string
}

var _ tea.Model = model{}

func newModel(eng engine.Engine) model {
	return newModelWithContextSender(context.Background(), eng, nil)
}

func newModelWithContextSender(ctx context.Context, eng engine.Engine, send func(tea.Msg)) model {
	if ctx == nil {
		ctx = context.Background()
	}
	return model{
		ctx:    ctx,
		eng:    eng,
		bridge: newEngineBridge(send),
		keys:   defaultKeyMap(),
		width:  minTerminalWidth,
		height: minTerminalHeight,
	}
}

func (m model) Init() tea.Cmd {
	// The due-check is a fast local-disk read with no network work, so it
	// runs concurrently with the runtime-lock probe and never blocks the
	// first render.
	return tea.Batch(m.loadRuntimeLockStatusCmd(), m.dailyLaunchCheckDueCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case progressMsg:
		m.progress = msg
	case confirmationRequestedMsg:
		m.modal = &msg
	case tea.KeyMsg:
		if m.modal != nil {
			return m.updateConfirmation(msg)
		}
		return m.updateKey(msg)
	default:
		if next, cmd, ok := m.updateLaunchCheckMsg(msg); ok {
			return next, cmd
		}
		if next, ok := m.updateLoadMsg(msg); ok {
			return next, nil
		}
		if next, ok := m.updateStatusMsg(msg); ok {
			return next, nil
		}
		return m.updateFinishedMsg(msg)
	}

	return m, nil
}

// updateLaunchCheckMsg handles the daily-on-launch update-check chain
// the gate answer, the bounded check result, and the record
// acknowledgement. It stays silent on failure — a failed check never sets
// m.err or shows an error banner; the gate stays open so the next launch
// retries.
func (m model) updateLaunchCheckMsg(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case dailyLaunchCheckDueMsg:
		// A gate error or a not-due answer (manual/disabled, or already
		// checked today) means no announce and no network work.
		if msg.err != nil || !msg.due {
			return m, nil, true
		}
		m.launchCheckActive = true
		return m, m.runDailyLaunchCheckCmd(), true
	case dailyLaunchCheckFinishedMsg:
		// The check is done either way, so the announce stops here. Record
		// only if both checks succeeded; any error leaves the gate
		// open for the next launch and surfaces nothing to the user.
		m.launchCheckActive = false
		if msg.catalogErr != nil || msg.selfErr != nil {
			return m, nil, true
		}
		m.launchCheckBanner = launchCheckSummary(msg.catalog, msg.self)
		return m, m.recordDailyLaunchCheckCmd(), true
	case dailyLaunchCheckRecordedMsg:
		// The record outcome is discarded: a record-write failure is not
		// actionable for the user, so it is neither logged nor surfaced — the
		// gate stays open and the check retries on the next launch.
		return m, nil, true
	default:
		return m, nil, false
	}
}

func (m model) updateLoadMsg(msg tea.Msg) (tea.Model, bool) {
	switch msg := msg.(type) {
	case appsLoadedMsg:
		m.busy = false
		m.err = msg.err
		m.apps = msg.apps
		if m.appCursor >= len(m.apps) {
			m.appCursor = max(0, len(m.apps)-1)
		}
	case appStatusLoadedMsg:
		m.busy = false
		m.err = msg.err
		m.status = msg.status
		m.validation = nil
		m.actionMessage = ""
		m.screen = screenAppActions
		m.actionCursor = 0
	case catalogAppsLoadedMsg:
		m.busy = false
		m.err = msg.err
		m.catalogApps = msg.apps
		if m.catalogCursor >= len(m.catalogApps) {
			m.catalogCursor = max(0, len(m.catalogApps)-1)
		}
	case catalogAppLoadedMsg:
		m.busy = false
		m.err = msg.err
		m.catalogDetail = msg.app
		m.installResult = nil
		m.installFieldCursor = 0
		if msg.err == nil && msg.app != nil {
			m.installFields = newInstallFields(msg.app)
			m.screen = screenInstallForm
		}
	case backupsLoadedMsg:
		m.busy = false
		m.err = msg.err
		m.backups = msg.backups
		if m.backupCursor >= len(m.backups) {
			m.backupCursor = max(0, len(m.backups)-1)
		}
	case settingsLoadedMsg:
		m.busy = false
		m.err = msg.err
		m.settings = msg.settings
		m.settingsMessage = ""
		if msg.err == nil && msg.settings != nil {
			m.settingsFields = newSettingsFields(msg.settings)
			m.settingsCursor = 0
		}
	case runtimeLockStatusLoadedMsg:
		m.busy = false
		m.err = msg.err
		m.runtimeLockStatus = msg.status
		m.runtimeLockMessage = ""
		if msg.err != nil || (msg.status != nil && msg.status.Stale) {
			m.screen = screenRuntimeLock
			m.runtimeLockCursor = 0
		}
	case catalogUpdateCheckedMsg:
		m.busy = false
		m.err = msg.err
		m.catalogUpdateStatus = msg.status
	case selfUpdateCheckedMsg:
		m.busy = false
		m.err = msg.err
		m.selfUpdateStatus = msg.status
	default:
		return m.updateResourceLoadMsg(msg)
	}
	return m, true
}

// updateResourceLoadMsg settles the model after Engine.ResourceSettings
// returns (issue #28). It is split out of updateLoadMsg to keep that
// dispatcher under the cyclomatic budget; the routing default forwards here.
func (m model) updateResourceLoadMsg(msg tea.Msg) (tea.Model, bool) {
	settingsMsg, ok := msg.(resourceSettingsLoadedMsg)
	if !ok {
		return m, false
	}

	m.busy = false
	m.err = nil
	m.resourceLoadErr = settingsMsg.err
	m.resourceSettings = settingsMsg.settings
	m.resourceFieldCursor = 0
	m.resourceFields = nil
	m.resourceService = ""
	if settingsMsg.err == nil {
		if svc := resourceServiceSettings(settingsMsg.settings); svc != nil {
			m.resourceService = svc.Service
			m.resourceFields = newResourceFields(svc)
		}
	}
	return m, true
}

func (m model) updateStatusMsg(msg tea.Msg) (tea.Model, bool) {
	switch msg := msg.(type) {
	case settingsSavedMsg:
		m.busy = false
		m.err = msg.err
		if msg.err == nil {
			m.settings = &msg.settings
			m.settingsFields = newSettingsFields(&msg.settings)
			m.settingsCursor = 0
			m.settingsMessage = "Settings saved."
		}
	case runtimeLockClearedMsg:
		m.busy = false
		m.err = msg.err
		if msg.status != nil {
			m.runtimeLockStatus = msg.status
		}
		if msg.err == nil {
			m.screen = screenRuntimeLock
			m.runtimeLockCursor = 0
			m.runtimeLockMessage = "Runtime lock cleared."
		}
	default:
		return m, false
	}
	return m, true
}

func (m model) updateFinishedMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case restartFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.validation = nil
		if msg.result != nil && msg.result.Status != nil {
			m.status = msg.result.Status
		}
		if msg.err == nil {
			m.actionMessage = "Restart complete."
		}
	case validationFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.validation = msg.result
		m.actionMessage = ""
	case reconfigureFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.reconfigureResult = msg.result
	case installFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.installResult = msg.result
		if msg.err == nil {
			m.screen = screenInstallResult
		}
	case updateFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.updateResult = msg.result
		if msg.err == nil {
			m.screen = screenUpdateResult
		}
	case removeFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.removeResult = msg.result
		if msg.err == nil {
			m.screen = screenRemoveResult
		}
	case deleteFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.deleteResult = msg.result
		if msg.err == nil {
			m.screen = screenDeleteResult
		}
	case restoreFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.restoreResult = msg.result
		if msg.err == nil {
			m.screen = screenRestoreResult
		}
	case uninstallFinishedMsg:
		return m.applyUninstallFinished(msg)
	default:
		if next, ok := m.updateDistributionFinishedMsg(msg); ok {
			return next, nil
		}
	}
	return m, nil
}

// applyUninstallFinished settles the model after Engine.Uninstall returns
// and decides whether to quit. Uninstall is fail-closed: on a full success
// (no error, no failed stacks) the running binary is already gone from
// disk, so the model advances to the result screen and returns tea.Quit so
// the program exits after the result has been rendered. On an abort (a
// non-empty Failed) or a whole-operation error (a declined confirmation,
// lock contention, cancellation), wdm is still installed: the result/error
// is shown on the uninstall screens and the program keeps running so the
// user can read the outcome and return to the dashboard.
func (m model) applyUninstallFinished(msg uninstallFinishedMsg) (tea.Model, tea.Cmd) {
	m.busy = false
	m.err = msg.err
	m.uninstallResult = msg.result

	if msg.result == nil {
		// A whole-operation error returned no result; stay on the uninstall
		// screen and surface the error there.
		return m, nil
	}

	m.screen = screenUninstallResult
	if msg.err == nil && len(msg.result.Failed) == 0 {
		return m, tea.Quit
	}
	return m, nil
}

// updateDistributionFinishedMsg handles the trust/distribution and batch
// finished messages (catalog update, stop-all, self-update). It is split
// out of updateFinishedMsg to keep each switch under the cyclomatic
// budget; the routing default in updateFinishedMsg forwards here.
func (m model) updateDistributionFinishedMsg(msg tea.Msg) (tea.Model, bool) {
	switch msg := msg.(type) {
	case catalogUpdateFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.catalogUpdateResult = msg.result
		if msg.err == nil {
			m.screen = screenCatalogUpdateResult
		}
	case stopAllFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.stopAllResult = msg.result
		// StopAll is continue-on-error: a non-nil result with per-stack
		// failures is still a completed batch, so the result screen shows
		// whenever a result is present. A whole-operation error (declined
		// confirmation, lock contention, cancellation) returns no result and
		// keeps the stop screen, surfacing the error there.
		if msg.result != nil {
			m.screen = screenStopAllResult
		}
	case selfUpdateFinishedMsg:
		m.busy = false
		m.err = msg.err
		m.selfUpdateResult = msg.result
		// ApplySelfUpdate uniquely returns a result ALONGSIDE an error on the
		// rollback paths (smoke-check failed -> RolledBack; rollback itself
		// failed -> Replaced with a manual-recovery Message). Show the result
		// screen whenever a result is present so the user sees the
		// rollback/trust-failure outcome. When the apply fails
		// with no result (verification, network, writability, or a declined
		// confirmation), stay on the check screen and surface the error there,
		// mirroring the catalog-update flow.
		if msg.result != nil {
			m.screen = screenSelfUpdateResult
		}
	default:
		return m, false
	}
	return m, true
}

func (m model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The launch-check update-available summary is transient: the next
	// keypress dismisses it. Clear it only while it is visible (the dashboard)
	// so a keypress on another screen cannot wipe it before the user reaches
	// the dashboard.
	if m.screen == screenDashboard {
		m.launchCheckBanner = ""
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		// Text-entry screens must receive printable runes, including 'q'.
		// Quit also binds ctrl+c (a non-rune key), so gate only the rune case
		// and let it fall through to updateScreenSpecificKey for typing.
		if m.isTextEntryScreen() && msg.Type == tea.KeyRunes {
			break
		}
		m.exiting = true
		return m, tea.Quit
	case key.Matches(msg, m.keys.Back):
		// Same as Quit: 'b' is a printable rune that must reach the field on
		// text-entry screens, while esc (a non-rune key) still cancels/goes back.
		if m.isTextEntryScreen() && msg.Type == tea.KeyRunes {
			break
		}
		m.back()
	case m.tooSmall():
		return m, nil
	}
	if next, cmd, ok := m.updateScreenSpecificKey(msg); ok {
		return next, cmd
	}

	switch {
	case key.Matches(msg, m.keys.Up):
		m.moveCursor(-1)
	case key.Matches(msg, m.keys.Down):
		m.moveCursor(1)
	case key.Matches(msg, m.keys.Select):
		return m.selectCurrent()
	}

	return m, nil
}

// isTextEntryScreen reports whether the current screen accepts typed text, so
// the global 'b'/'q' shortcuts must not shadow runes destined for a field.
func (m model) isTextEntryScreen() bool {
	switch m.screen {
	case screenInstallForm, screenSettings, screenDeleteName, screenResources:
		return true
	default:
		return false
	}
}

func (m model) updateScreenSpecificKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	switch m.screen {
	case screenInstallForm:
		next, cmd := m.updateInstallFormKey(msg)
		return next, cmd, true
	case screenSettings:
		next, cmd := m.updateSettingsKey(msg)
		return next, cmd, true
	case screenRuntimeLock:
		next, cmd := m.updateRuntimeLockKey(msg)
		return next, cmd, true
	case screenDeleteName:
		next, cmd := m.updateDeleteNameKey(msg)
		return next, cmd, true
	case screenResources:
		next, cmd := m.updateResourcesKey(msg)
		return next, cmd, true
	default:
		return m, nil, false
	}
}

func (m *model) moveCursor(delta int) {
	switch m.screen {
	case screenDashboard:
		m.cursor = boundedCursor(m.cursor, delta, len(dashboardActions))
	case screenCheckApps, screenUpdateApps, screenBackupsApps:
		m.appCursor = boundedCursor(m.appCursor, delta, len(m.apps))
	case screenAppActions:
		m.actionCursor = boundedCursor(m.actionCursor, delta, len(checkAppActions))
	case screenRemoveActions:
		m.removeActionCursor = boundedCursor(m.removeActionCursor, delta, len(removeActions))
	case screenInstallCatalog:
		m.catalogCursor = boundedCursor(m.catalogCursor, delta, len(m.catalogApps))
	case screenBackupsList:
		m.backupCursor = boundedCursor(m.backupCursor, delta, len(m.backups))
	}
}

func boundedCursor(current, delta, length int) int {
	if length <= 0 {
		return 0
	}
	next := current + delta
	if next < 0 {
		return 0
	}
	if next >= length {
		return length - 1
	}
	return next
}

func (m model) selectCurrent() (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenDashboard:
		return m.selectDashboardAction()
	case screenCheckApps:
		return m.selectManagedApp()
	case screenUpdateApps:
		return m.selectUpdateApp()
	case screenAppActions:
		return m.selectAppAction()
	case screenRemoveActions:
		return m.selectRemoveAction()
	case screenBackupsApps:
		return m.selectBackupsApp()
	case screenBackupsList:
		return m.selectBackupSnapshot()
	case screenFirstRunWelcome:
		m.screen = screenFirstRunSystemCheck
	case screenFirstRunSystemCheck:
		return m.startFirstRunInstall()
	case screenInstallCatalog:
		return m.selectCatalogApp()
	case screenCatalogUpdate:
		return m.selectCatalogUpdate()
	case screenSelfUpdate:
		return m.selectSelfUpdate()
	}
	return m, nil
}

func (m *model) back() {
	switch m.screen {
	case screenAppActions:
		m.screen = screenCheckApps
		m.err = nil
		m.validation = nil
		m.actionMessage = ""
	case screenRemoveActions, screenDeleteName:
		m.screen = screenAppActions
		m.err = nil
		m.removeResult = nil
		m.deleteResult = nil
	case screenResources:
		m.screen = screenAppActions
		m.err = nil
		m.resourceLoadErr = nil
		m.resourceSettings = nil
		m.resourceService = ""
		m.resourceFields = nil
		m.reconfigureResult = nil
	case screenBackupsList:
		m.screen = screenBackupsApps
		m.err = nil
		m.restoreResult = nil
	case screenInstallForm:
		m.screen = screenInstallCatalog
		m.err = nil
	case screenFirstRunWelcome, screenFirstRunSystemCheck, screenCheckApps, screenStopAll, screenStopAllResult, screenUninstall, screenUninstallResult, screenInstallCatalog, screenInstallResult, screenUpdateApps, screenUpdateResult, screenRemoveResult, screenDeleteResult, screenBackupsApps, screenRestoreResult, screenSettings, screenRuntimeLock, screenCatalogUpdate, screenCatalogUpdateResult, screenSelfUpdate, screenSelfUpdateResult:
		m.screen = screenDashboard
		m.firstRun = false
		m.err = nil
	default:
		m.screen = screenDashboard
	}
}

func (m model) selectDashboardAction() (tea.Model, tea.Cmd) {
	if dashboardActions[m.cursor] == "Install an app" {
		m.screen = screenInstallCatalog
		m.busy = true
		m.err = nil
		m.installResult = nil
		return m, m.loadCatalogAppsCmd()
	}

	if dashboardActions[m.cursor] == "Update apps" {
		m.screen = screenUpdateApps
		m.busy = true
		m.err = nil
		m.updateResult = nil
		m.progress = progressMsg{}
		return m, m.loadAppsCmd()
	}

	if dashboardActions[m.cursor] == "Stop all apps" {
		m.screen = screenStopAll
		m.busy = true
		m.err = nil
		m.stopAllResult = nil
		m.progress = progressMsg{}
		return m, m.stopAllCmd()
	}

	if dashboardActions[m.cursor] == "Backups" {
		m.screen = screenBackupsApps
		m.busy = true
		m.err = nil
		m.backupAppID = ""
		m.backups = nil
		m.backupCursor = 0
		m.restoreResult = nil
		return m, m.loadAppsCmd()
	}

	if dashboardActions[m.cursor] == "Settings" {
		m.screen = screenSettings
		m.busy = true
		m.err = nil
		m.settingsMessage = ""
		return m, m.loadSettingsCmd()
	}

	if dashboardActions[m.cursor] == "Update catalog" {
		m.screen = screenCatalogUpdate
		m.busy = true
		m.err = nil
		m.catalogUpdateStatus = nil
		m.catalogUpdateResult = nil
		m.progress = progressMsg{}
		return m, m.checkCatalogUpdateCmd()
	}

	if dashboardActions[m.cursor] == "Update wdm" {
		m.screen = screenSelfUpdate
		m.busy = true
		m.err = nil
		m.selfUpdateStatus = nil
		m.selfUpdateResult = nil
		m.progress = progressMsg{}
		return m, m.checkSelfUpdateCmd()
	}

	if dashboardActions[m.cursor] == "Uninstall wdm" {
		m.screen = screenUninstall
		m.busy = true
		m.err = nil
		m.uninstallResult = nil
		m.progress = progressMsg{}
		return m, m.uninstallCmd()
	}

	m.screen = screenCheckApps
	m.busy = true
	m.err = nil
	return m, m.loadAppsCmd()
}

func (m model) selectManagedApp() (tea.Model, tea.Cmd) {
	if len(m.apps) == 0 {
		return m, nil
	}

	m.screen = screenAppActions
	m.busy = true
	m.err = nil
	m.status = nil
	m.validation = nil
	m.actionMessage = ""
	return m, m.loadStatusCmd(m.apps[m.appCursor].AppID)
}

func (m model) selectAppAction() (tea.Model, tea.Cmd) {
	switch checkAppActions[m.actionCursor] {
	case "View details":
		m.actionMessage = "Details are shown above."
	case "Restart app":
		m.busy = true
		m.err = nil
		m.actionMessage = ""
		return m, m.restartAppCmd(m.activeAppID())
	case "Manage resources":
		m.screen = screenResources
		m.busy = true
		m.err = nil
		m.actionMessage = ""
		m.resourceLoadErr = nil
		m.resourceSettings = nil
		m.resourceService = ""
		m.resourceFields = nil
		m.resourceFieldCursor = 0
		m.reconfigureResult = nil
		m.progress = progressMsg{}
		return m, m.loadResourceSettingsCmd(m.activeAppID())
	case "Remove app":
		m.screen = screenRemoveActions
		m.err = nil
		m.actionMessage = ""
		m.removeActionCursor = 0
		m.removeResult = nil
		m.deleteNameInput = ""
		m.deleteResult = nil
	case "Validate config":
		m.busy = true
		m.err = nil
		m.actionMessage = ""
		return m, m.validateConfigCmd(m.activeAppID())
	case "Return to dashboard":
		m.screen = screenDashboard
	}
	return m, nil
}

type appsLoadedMsg struct {
	apps []types.AppRuntimeStatus
	err  error
}

type appStatusLoadedMsg struct {
	status *types.AppStatus
	err    error
}

type restartFinishedMsg struct {
	result *types.RestartResult
	err    error
}

type validationFinishedMsg struct {
	result *types.ValidationResult
	err    error
}

func (m model) loadAppsCmd() tea.Cmd {
	return func() tea.Msg {
		apps, err := m.eng.ListStatus(m.ctx)
		return appsLoadedMsg{apps: apps, err: err}
	}
}

func (m model) loadStatusCmd(appID string) tea.Cmd {
	return func() tea.Msg {
		status, err := m.eng.Status(m.ctx, appID)
		return appStatusLoadedMsg{status: status, err: err}
	}
}

func (m model) restartAppCmd(appID string) tea.Cmd {
	return engineCommand(m.ctx, m.bridge, func(
		ctx context.Context,
		progress types.ProgressFn,
		confirmer types.Confirmer,
	) tea.Msg {
		result, err := m.eng.Restart(ctx, types.RestartRequest{AppID: appID}, progress, confirmer)
		return restartFinishedMsg{result: result, err: err}
	})
}

func (m model) validateConfigCmd(appID string) tea.Cmd {
	return func() tea.Msg {
		result, err := m.eng.ValidateConfig(m.ctx, appID)
		return validationFinishedMsg{result: result, err: err}
	}
}

func (m model) activeAppID() string {
	if m.status != nil && m.status.AppID != "" {
		return m.status.AppID
	}
	if m.appCursor >= 0 && m.appCursor < len(m.apps) {
		return m.apps[m.appCursor].AppID
	}
	return ""
}

func (m model) updateConfirmation(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.answerConfirmation(confirmationReply{accepted: false})
		m.exiting = true
		return m, tea.Quit
	case key.Matches(msg, m.keys.Confirm):
		m.answerConfirmation(confirmationReply{accepted: true})
	case key.Matches(msg, m.keys.Decline):
		m.answerConfirmation(confirmationReply{accepted: false})
	}

	return m, nil
}

func (m *model) answerConfirmation(reply confirmationReply) {
	if m.modal == nil {
		return
	}

	select {
	case m.modal.reply <- reply:
	default:
	}
	m.modal = nil
}

func (m model) View() string {
	switch {
	case m.exiting:
		return "Goodbye.\n"
	case m.tooSmall():
		return m.resizeView()
	case m.modal != nil:
		if m.firstRun {
			return m.firstRunConfirmationView()
		}
		return m.confirmationView()
	default:
		return m.screenView()
	}
}

func (m model) screenView() string {
	switch m.screen {
	case screenCheckApps:
		return m.checkAppsView()
	case screenStopAll, screenStopAllResult:
		return m.stopAllScreenView()
	case screenAppActions:
		return m.appActionsView()
	case screenFirstRunWelcome:
		return m.firstRunWelcomeView()
	case screenFirstRunSystemCheck:
		return m.firstRunSystemCheckView()
	case screenInstallCatalog, screenInstallForm, screenInstallResult:
		return m.installScreenView()
	case screenUpdateApps:
		return m.updateAppsView()
	case screenUpdateResult:
		return m.updateResultView()
	case screenRemoveActions:
		return m.removeActionsView()
	case screenDeleteName:
		return m.deleteNameView()
	case screenRemoveResult:
		return m.removeResultView()
	case screenDeleteResult:
		return m.deleteResultView()
	case screenBackupsApps:
		return m.backupsAppsView()
	case screenBackupsList:
		return m.backupsListView()
	case screenRestoreResult:
		return m.restoreResultView()
	case screenSettings:
		return m.settingsView()
	case screenRuntimeLock:
		return m.runtimeLockView()
	default:
		if view, ok := m.distributionScreenView(); ok {
			return view
		}
		return m.dashboardView()
	}
}

// distributionScreenView renders the trust/distribution screens (catalog
// update, self-update). It is split out of screenView to keep that
// dispatcher under the cyclomatic budget; the default arm forwards here
// before falling back to the dashboard.
func (m model) distributionScreenView() (string, bool) {
	switch m.screen {
	case screenCatalogUpdate, screenCatalogUpdateResult:
		return m.catalogUpdateScreenView(), true
	case screenSelfUpdate, screenSelfUpdateResult:
		return m.selfUpdateScreenView(), true
	case screenUninstall, screenUninstallResult:
		return m.uninstallScreenView(), true
	case screenResources:
		return m.resourcesView(), true
	default:
		return "", false
	}
}

func (m model) installCatalogScreenView() string {
	if m.firstRun {
		return m.firstRunInstallView(m.installCatalogView(), 3)
	}
	return m.installCatalogView()
}

func (m model) installFormScreenView() string {
	if m.firstRun {
		return m.firstRunInstallView(m.installFormView(), m.firstRunInstallStep())
	}
	return m.installFormView()
}

func (m model) installResultScreenView() string {
	if m.firstRun {
		return m.firstRunInstallView(m.installResultView(), 10)
	}
	return m.installResultView()
}

func (m model) installScreenView() string {
	switch m.screen {
	case screenInstallForm:
		return m.installFormScreenView()
	case screenInstallResult:
		return m.installResultScreenView()
	default:
		return m.installCatalogScreenView()
	}
}

func (m model) tooSmall() bool {
	return m.width < minTerminalWidth || m.height < minTerminalHeight
}

func (m model) dashboardView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("wdm"))
	b.WriteString("\n\n")
	b.WriteString("What do you want to do?\n\n")

	writeMenu(&b, dashboardActions, m.cursor)

	m.writeLaunchCheckStatus(&b)

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

// writeLaunchCheckStatus renders the transient daily-launch-check status line
// on the dashboard: the network announce while the check runs, then
// the update-available summary. It is a no-op when neither is present, so it
// never disturbs the layout.
func (m model) writeLaunchCheckStatus(b *strings.Builder) {
	switch {
	case m.launchCheckActive:
		b.WriteString("\n")
		b.WriteString(launchCheckNotice)
		b.WriteByte('\n')
	case m.launchCheckBanner != "":
		b.WriteString("\n")
		b.WriteString(m.launchCheckBanner)
		b.WriteByte('\n')
	}
}

func (m model) resizeView() string {
	return fmt.Sprintf(
		"%s\n\nPlease resize your terminal to at least %dx%d (current %dx%d).\n\nBack: b    Quit: q\n",
		titleStyle().Render("wdm"),
		minTerminalWidth,
		minTerminalHeight,
		m.width,
		m.height,
	)
}

func (m model) checkAppsView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Check my apps"))
	b.WriteString("\n\n")

	switch {
	case m.busy:
		b.WriteString("Loading managed apps...\n")
	case m.err != nil:
		b.WriteString("Could not load managed apps: ")
		b.WriteString(m.err.Error())
		b.WriteByte('\n')
	case len(m.apps) == 0:
		b.WriteString("No managed apps found.\n")
	default:
		for i, app := range m.apps {
			prefix := "  "
			suffix := ""
			if i == m.appCursor {
				prefix = "> "
				suffix = " [selected]"
			}
			b.WriteString(prefix)
			b.WriteString(app.AppID)
			if app.TemplateName != "" && app.TemplateName != app.AppID {
				b.WriteString("  ")
				b.WriteString(app.TemplateName)
			}
			b.WriteString("  ")
			b.WriteString(appListState(app))
			b.WriteString(suffix)
			b.WriteByte('\n')
		}
	}

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

func (m model) appActionsView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render("Check my apps"))
	b.WriteString("\n\n")

	if m.busy {
		b.WriteString("Working on ")
		b.WriteString(m.activeAppID())
		b.WriteString("...\n\n")
		b.WriteString(m.helpLine())
		return b.String()
	}

	if m.err != nil {
		b.WriteString("Action failed: ")
		b.WriteString(m.err.Error())
		b.WriteString("\n")
		m.writeLogPathNotice(&b)
		b.WriteByte('\n')
	}

	m.writeStatusSummary(&b)
	if m.actionMessage != "" {
		b.WriteString("\n")
		b.WriteString(m.actionMessage)
		b.WriteByte('\n')
	}
	if m.validation != nil {
		b.WriteString("\n")
		if m.validation.Valid {
			b.WriteString("Compose config is valid.\n")
		} else {
			b.WriteString("Compose config is invalid.\n")
		}
		if m.validation.Detail != "" {
			b.WriteString(m.validation.Detail)
			b.WriteByte('\n')
		}
	}

	b.WriteString("\nNext actions\n\n")
	writeMenu(&b, checkAppActions, m.actionCursor)

	b.WriteString("\n")
	b.WriteString(m.helpLine())
	return b.String()
}

// writeLogPathNotice surfaces the §24 failure UX inside the TUI: on a failed
// operation it points the operator at the log file and reminds them to review
// it before sharing publicly. Most TUI failures are handled in-screen, so the
// cmd/wdm stderr notice never fires for them; this keeps the notice visible.
// It is a no-op when no engine-owned file sink exists (WithLogger callers own
// their sink and LogPath reports the empty string).
func (m model) writeLogPathNotice(b *strings.Builder) {
	if m.eng == nil {
		return
	}
	path := m.eng.LogPath()
	if path == "" {
		return
	}
	b.WriteString("See ")
	b.WriteString(path)
	b.WriteString("; review the log before sharing it publicly (e.g. on GitHub).\n")
}

// writeMenu renders a plain selectable menu: one item per line, prefixed
// with "> " for the row at cursor and "  " otherwise, with a " [selected]"
// suffix on the active row. It reproduces the hand-rolled menu loop shared
// across the dashboard, app-actions, remove, and runtime-lock screens.
func writeMenu(b *strings.Builder, items []string, cursor int) {
	for i, item := range items {
		prefix := "  "
		suffix := ""
		if i == cursor {
			prefix = "> "
			suffix = " [selected]"
		}
		b.WriteString(prefix)
		b.WriteString(item)
		b.WriteString(suffix)
		b.WriteByte('\n')
	}
}

func (m model) writeStatusSummary(b *strings.Builder) {
	if m.status == nil {
		b.WriteString(m.activeAppID())
		b.WriteString("\nStatus unavailable.\n")
		return
	}

	b.WriteString(m.status.AppID)
	b.WriteString("\n")
	if m.status.State != "" {
		b.WriteString("State: ")
		b.WriteString(m.status.State)
		b.WriteByte('\n')
	}
	if m.status.Message != "" {
		b.WriteString(m.status.Message)
		b.WriteByte('\n')
	}
	if len(m.status.AttentionReasons) > 0 {
		b.WriteString("Attention: ")
		b.WriteString(strings.Join(m.status.AttentionReasons, ", "))
		b.WriteByte('\n')
	}
	for _, service := range m.status.Services {
		b.WriteString("- ")
		b.WriteString(service.Service)
		b.WriteString(": ")
		b.WriteString(service.State)
		if service.NeedsAttention {
			b.WriteString(" needs attention")
		}
		if service.Message != "" {
			b.WriteString(" - ")
			b.WriteString(service.Message)
		}
		b.WriteByte('\n')
	}
}

// appListState renders the live runtime state of one managed app for the
// "Check my apps" list, fed by Engine.ListStatus (PRD §18). It maps the
// engine's State vocabulary to the user-facing label, rendering "stopped" as
// a calm off state rather than a problem; an empty State (an app the engine
// could not summarize) and any unrecognized value fall back to the
// needs-attention reading rather than implying the app is healthy.
func appListState(app types.AppRuntimeStatus) string {
	switch app.State {
	case "running":
		return "running"
	case "stopped":
		return "stopped"
	case "removed":
		return "removed"
	default:
		return "needs attention"
	}
}

func (m model) confirmationView() string {
	var b strings.Builder
	b.WriteString(titleStyle().Render(m.modal.confirmation.Title))
	b.WriteString("\n\n")
	b.WriteString(m.modal.confirmation.Message)
	b.WriteString("\n\n")
	b.WriteString("Yes: y    No: n    Back: b    Quit: q\n")
	return b.String()
}

func titleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true)
}

func (m model) helpLine() string {
	if m.isTextEntryScreen() {
		return "Enter: select    Esc: back    Ctrl+C: quit\n"
	}
	return "Enter: select    Back: b    Quit: q\n"
}
