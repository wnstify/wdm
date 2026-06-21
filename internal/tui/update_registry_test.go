package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the TUI "Update apps" flow: because Engine.Update makes a
// registry round-trip during planning, the flow must disclose the network
// contact before/while it matters and surface the registry planning progress,
// all while staying source-agnostic (the catalog is the sole update
// source) and never crossing into a catalog-update method (no crossover).

// TestUpdateAppsView_BusyDisclosesNetworkContact pins that the update-busy
// view names the registry network contact (the invariant — no silent
// network work) while an app update is running. The disclosure must NOT
// appear on the app-list-loading state (no registry contact yet) nor after
// the update completes.
func TestUpdateAppsView_BusyDisclosesNetworkContact(t *testing.T) {
	t.Parallel()

	t.Run("busy_updating_discloses", func(t *testing.T) {
		t.Parallel()

		m := newReadyModel(&fakeEngine{
			listStatusApps: []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha", TemplateName: "Alpha"}, State: "running"}},
		})
		m.screen = screenUpdateApps
		m.apps = []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha", TemplateName: "Alpha"}, State: "running"}}
		m.busy = true

		view := m.updateAppsView()
		assert.Contains(t, view, updateNetworkNotice,
			"the update-busy view must disclose the registry network contact")
		assert.Contains(t, view, "image registry",
			"the disclosure must name the image registry as the contacted endpoint")
		assert.Contains(t, view, "catalog remains the source",
			"the disclosure must keep the catalog as the source of the update (source-agnostic)")
		assert.Contains(t, view, "Updating alpha",
			"the busy view still names the app being updated")
	})

	t.Run("loading_apps_does_not_disclose", func(t *testing.T) {
		t.Parallel()

		// While the app list is still loading there has been no registry
		// contact, so the network notice must not appear yet.
		m := newReadyModel(&fakeEngine{})
		m.screen = screenUpdateApps
		m.apps = nil
		m.busy = true

		view := m.updateAppsView()
		assert.NotContains(t, view, updateNetworkNotice,
			"the app-list-loading state must not yet disclose a registry contact")
	})

	t.Run("result_view_does_not_disclose", func(t *testing.T) {
		t.Parallel()

		// After the update completes the network contact is done, so the
		// finished-result view carries no in-progress network notice.
		m := newReadyModel(&fakeEngine{})
		m.updateResult = &types.UpdateResult{AppID: "alpha", NewTemplateVersion: "2"}

		view := m.updateResultView()
		assert.NotContains(t, view, updateNetworkNotice,
			"the completed-result view must not show an in-progress network notice")
	})
}

// TestUpdateAppsView_BusySurfacesRegistryProgress pins that the registry
// planning message (relayed through m.progress.message) is visible to the
// user during the busy state. This proves the registry disclosure reaches
// the user through the TUI, not only the CLI.
func TestUpdateAppsView_BusySurfacesRegistryProgress(t *testing.T) {
	t.Parallel()

	const registryProgress = "checking the registry for image digests"

	m := newReadyModel(&fakeEngine{})
	m.screen = screenUpdateApps
	m.apps = []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha", TemplateName: "Alpha"}, State: "running"}}
	m.busy = true
	m.progress = progressMsg{
		step:    types.StepUpdatePlanning,
		message: registryProgress,
	}

	view := m.updateAppsView()
	assert.Contains(t, view, registryProgress,
		"the registry planning progress message must surface during the busy state")
}

// TestUpdateFlow_RegistryProgressFlowsThroughBridge drives the flow
// end-to-end: a fake Update that emits the registry planning events
// through the progress callback, proving the registry disclosure is
// delivered to the model's progress sink (and therefore the view) during a
// real update command. It also pins the no-crossover property — the flow
// calls only Engine.Update, never a catalog-update method or a registry
// method.
func TestUpdateFlow_RegistryProgressFlowsThroughBridge(t *testing.T) {
	t.Parallel()

	sender := newRecordingSender()
	fake := &registryProgressEngine{
		fakeEngine: &fakeEngine{
			listStatusApps: []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha", TemplateName: "Alpha"}, State: "running"}},
			updateResult:   &types.UpdateResult{AppID: "alpha", NewTemplateVersion: "2"},
		},
	}
	m := newModelWithContextSender(t.Context(), fake, sender.Send)
	m.screen = screenUpdateApps
	m.apps = []types.AppRuntimeStatus{{AppInfo: types.AppInfo{AppID: "alpha", TemplateName: "Alpha"}, State: "running"}}
	m.appCursor = 0

	// Selecting an app launches the update command; run it on a goroutine
	// because it emits progress through the bridge sender before returning.
	next, cmd := m.Update(enterKey())
	m = assertModel(t, next)
	require.NotNil(t, cmd)

	resultC := make(chan tea.Msg, 1)
	go func() { resultC <- cmd() }()

	// The pre-lookup disclosure event arrives first, then the
	// per-service digest disclosure. Both reach the model's progress sink, so
	// the busy view renders them to the user.
	first := sender.waitProgress(t)
	assert.Equal(t, "checking the registry for image digests", first.message,
		"the pre-lookup disclosure must reach the progress sink before the digest")
	m = updateModel(t, m, first)
	assert.Contains(t, m.View(), first.message,
		"the pre-lookup disclosure must render in the busy view")

	second := sender.waitProgress(t)
	assert.Contains(t, second.message, "registry digest for",
		"the per-service registry digest disclosure must reach the progress sink")
	m = updateModel(t, m, second)
	assert.Contains(t, m.View(), "registry digest for",
		"the registry digest disclosure must render in the busy view")

	// Drain the finished message and confirm the result screen renders.
	result := requireResult[updateFinishedMsg](t, resultC)
	m = updateModel(t, m, result)
	assert.Contains(t, m.View(), "Update complete")

	// No-crossover / no-shell-out: the flow drove exactly one Engine.Update
	// and touched no catalog-update or registry method.
	require.Len(t, fake.updateRequests, 1)
	assert.Equal(t, "alpha", fake.updateRequests[0].AppID)
	assert.Empty(t, fake.updateRequests[0].TargetTemplateVersion,
		"the TUI update flow exposes no update-source target (source-agnostic)")
	assert.Zero(t, fake.checkCatalogUpdateN, "update flow must not check catalog updates")
	assert.Empty(t, fake.catalogUpdateRequests, "update flow must not apply catalog updates")
	assert.Empty(t, fake.checkImageUpdatesCalls, "update flow must not call a registry/image-update method")
}

// registryProgressEngine wraps fakeEngine and emits the registry planning
// events through the progress callback (mirroring the
// engine's planUpdateCheck disclosure) so the bridge delivery and view
// rendering can be exercised without a real catalog, daemon, or registry.
type registryProgressEngine struct {
	*fakeEngine
}

func (e *registryProgressEngine) Update(
	_ context.Context,
	req types.UpdateRequest,
	progress engine.ProgressFn,
	_ types.Confirmer,
) (*types.UpdateResult, error) {
	e.updateRequests = append(e.updateRequests, req)
	if progress != nil {
		progress(types.StepUpdatePlanning, 8, "checking the registry for image digests")
		progress(types.StepUpdatePlanning, 10, "service web: registry digest for example/web:2 is sha256:abc")
	}
	return e.updateResult, e.updateErr
}
