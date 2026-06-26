package core_test

import (
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

// occupyLoopbackPort binds a real TCP listener on an ephemeral 127.0.0.1 port
// and returns the port number, holding it for the test lifetime. The real bind
// is what makes the port a genuine conflict for the production probe — no probe
// mock (ADR 0004 / issue #145).
func occupyLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func planHosts(t *testing.T) system.HostResources {
	t.Helper()
	return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}
}

// TestPlanPorts_OverrideRewritesLoopbackPort proves a PortOverrides entry
// remaps a planned single loopback host port to the new port before the probe,
// so the plan binds the new port. The original port is occupied by a real
// listener; without the override the install would fail closed.
func TestPlanPorts_OverrideRewritesLoopbackPort(t *testing.T) {
	t.Parallel()

	busy := occupyLoopbackPort(t)
	free := freeLocalTCPPort(t)

	app := appFixture("override-remap-app", busy)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

	plan, err := core.PlanInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{busy: free}},
		planHosts(t),
		nil,
	)
	require.NoError(t, err)
	require.Len(t, plan.LocalPorts, 1)
	assert.Equal(t, free, plan.LocalPorts[0].HostPort)
	assert.Equal(t, "127.0.0.1", plan.LocalPorts[0].HostIP, "a remap never changes the host IP")
}

// TestPlanPorts_OverrideTargetIsReprobed proves the remapped port is itself
// probed: remapping onto a busy port fails closed (no new TOCTOU hole), as a
// typed conflict naming the new port.
func TestPlanPorts_OverrideTargetIsReprobed(t *testing.T) {
	t.Parallel()

	free := freeLocalTCPPort(t)
	busyTarget := occupyLoopbackPort(t)
	app := appFixture("override-reprobe-app", free)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

	_, err := core.PlanInstallForTest(eng, t.Context(),
		types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{free: busyTarget}},
		planHosts(t), nil)
	require.Error(t, err)
	var conflict *types.PortConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, busyTarget, conflict.ConflictingHostPort, "the re-probe conflicts on the remapped port")
}

// TestPlanPorts_OverrideRefusals proves the fail-closed validation arms of the
// override applicator: an override naming no planned binding, a range port, a
// public port, or a privileged/out-of-range target is a usage-validation error.
func TestPlanPorts_OverrideRefusals(t *testing.T) {
	t.Parallel()

	t.Run("unknown binding refused", func(t *testing.T) {
		t.Parallel()
		free := freeLocalTCPPort(t)
		app := appFixture("override-unknown-app", free)
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		_, err := core.PlanInstallForTest(eng, t.Context(),
			types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{free + 1: freeLocalTCPPort(t)}},
			planHosts(t), nil)
		require.Error(t, err)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "no planned host port")
	})

	t.Run("range port refused", func(t *testing.T) {
		t.Parallel()
		app := appFixture("override-range-app", 0)
		app.Ports = []catalog.Port{
			{Service: "media", Container: 50000, Host: 50000, Protocol: "udp", HostRange: "50000-50002", ContainerRange: "50000-50002"},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		_, err := core.PlanInstallForTest(eng, t.Context(),
			types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{50001: freeLocalTCPPort(t)}},
			planHosts(t), nil)
		require.Error(t, err)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "range")
	})

	t.Run("public port refused", func(t *testing.T) {
		t.Parallel()
		publicPort := freeLocalTCPPort(t)
		app := appFixture("override-public-app", publicPort)
		// Keep the admin surface off the public port so §11.1(d) does not refuse.
		app.LocalTargetURLTemplate = "http://127.0.0.1:1"
		app.PangolinGuidance.TargetURL = "http://127.0.0.1:1"
		app.Ports = []catalog.Port{
			{Service: "bt", Container: 6881, Host: publicPort, Protocol: "tcp", Public: true},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		_, err := core.PlanInstallForTest(eng, t.Context(),
			types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{publicPort: freeLocalTCPPort(t)}},
			planHosts(t), nil)
		require.Error(t, err)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "public")
	})

	t.Run("privileged target refused", func(t *testing.T) {
		t.Parallel()
		free := freeLocalTCPPort(t)
		app := appFixture("override-priv-app", free)
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		_, err := core.PlanInstallForTest(eng, t.Context(),
			types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{free: 80}},
			planHosts(t), nil)
		require.Error(t, err)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "1025")
	})
}

// TestPlanPorts_LoopbackConflictReturnsTypedSuggestion proves a plan-time
// conflict on a remappable single loopback port surfaces as a typed
// PortConflictError carrying the binding detail and a deterministic, actually
// free suggested port, while still mapping to the usage-validation exit code.
func TestPlanPorts_LoopbackConflictReturnsTypedSuggestion(t *testing.T) {
	t.Parallel()

	busy := occupyLoopbackPort(t)
	app := appFixture("conflict-suggest-app", busy)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

	_, err := core.PlanInstallForTest(eng, t.Context(),
		types.InstallRequest{AppID: app.AppID}, planHosts(t), nil)
	require.Error(t, err)

	var conflict *types.PortConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "app", conflict.Service)
	assert.Equal(t, 8080, conflict.ContainerPort)
	assert.Equal(t, busy, conflict.ConflictingHostPort)
	assert.Greater(t, conflict.SuggestedHostPort, busy, "suggestion scans upward from the conflict")
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation))

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(conflict.SuggestedHostPort)))
	require.NoError(t, err, "the suggested port must actually be free")
	_ = ln.Close()
}

// TestPlanPorts_RangeAndPublicConflictsStayPlain proves a conflict on a
// non-remappable binding (a range port or a public port) keeps the plain
// fail-closed usage-validation error — never the typed remap suggestion.
func TestPlanPorts_RangeAndPublicConflictsStayPlain(t *testing.T) {
	t.Parallel()

	t.Run("range conflict stays plain", func(t *testing.T) {
		t.Parallel()
		busy := occupyLoopbackPort(t)
		app := appFixture("range-conflict-app", 0)
		app.Ports = []catalog.Port{
			{Service: "media", Container: busy, Host: busy, Protocol: "tcp",
				HostRange: strconv.Itoa(busy) + "-" + strconv.Itoa(busy+2), ContainerRange: strconv.Itoa(busy) + "-" + strconv.Itoa(busy+2)},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		_, err := core.PlanInstallForTest(eng, t.Context(),
			types.InstallRequest{AppID: app.AppID}, planHosts(t), nil)
		require.Error(t, err)
		assertUsageValidation(t, err)
		var conflict *types.PortConflictError
		assert.False(t, errors.As(err, &conflict), "range conflicts are not remappable")
	})

}

// TestEnrichPortConflict_NonRemappableArmsStayPlain proves the classifier
// leaves a conflict plain for the arms that a real OS bind cannot deterministically
// reproduce on every platform: an EACCES (elevated-privileges) failure, a public
// (non-loopback) binding, and a range binding. A loopback non-range non-public
// conflict is the only one enriched into a typed PortConflictError.
func TestEnrichPortConflict_NonRemappableArmsStayPlain(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	loopback := func() types.PortBinding {
		return types.PortBinding{Service: "app", HostIP: "127.0.0.1", HostPort: occupyLoopbackPort(t), ContainerPort: 8080, Protocol: "tcp"}
	}
	inUse := bindError(syscall.EADDRINUSE)

	t.Run("eacces stays plain", func(t *testing.T) {
		t.Parallel()
		err := core.EnrichPortConflictForTest(eng, loopback(), false, false, bindError(syscall.EACCES))
		var conflict *types.PortConflictError
		assert.False(t, errors.As(err, &conflict))
	})

	t.Run("public binding stays plain", func(t *testing.T) {
		t.Parallel()
		b := loopback()
		b.HostIP = "0.0.0.0"
		err := core.EnrichPortConflictForTest(eng, b, false, true, inUse)
		var conflict *types.PortConflictError
		assert.False(t, errors.As(err, &conflict))
	})

	t.Run("range binding stays plain", func(t *testing.T) {
		t.Parallel()
		err := core.EnrichPortConflictForTest(eng, loopback(), true, false, inUse)
		var conflict *types.PortConflictError
		assert.False(t, errors.As(err, &conflict))
	})

	t.Run("loopback conflict is enriched", func(t *testing.T) {
		t.Parallel()
		err := core.EnrichPortConflictForTest(eng, loopback(), false, false, inUse)
		var conflict *types.PortConflictError
		assert.True(t, errors.As(err, &conflict))
	})
}

// TestSuggestFreePort proves the deterministic next-free scan over the real
// probe: it skips ports planned by the same install, and fails closed with 0
// when the scan range is exhausted.
func TestSuggestFreePort(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)

	t.Run("skips same-install planned ports", func(t *testing.T) {
		t.Parallel()
		busy := occupyLoopbackPort(t)
		conflict := types.PortBinding{Service: "app", HostIP: "127.0.0.1", HostPort: busy, ContainerPort: 8080, Protocol: "tcp"}
		// Reserve the next three ports as same-install planned; the suggestion
		// must land strictly above them even though they are not bound.
		planned := map[int]struct{}{busy: {}, busy + 1: {}, busy + 2: {}, busy + 3: {}}
		got := core.SuggestFreePortForTest(eng, t.Context(), conflict, planned)
		assert.Greater(t, got, busy+3, "must skip every same-install planned port")
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(got)))
		require.NoError(t, err, "the suggestion must be a genuinely free port")
		_ = ln.Close()
	})

	t.Run("fails closed when none free", func(t *testing.T) {
		t.Parallel()
		// Conflict at the top of the range leaves no candidate above it.
		conflict := types.PortBinding{Service: "app", HostIP: "127.0.0.1", HostPort: 65535, ContainerPort: 8080, Protocol: "tcp"}
		got := core.SuggestFreePortForTest(eng, t.Context(), conflict, map[int]struct{}{})
		assert.Equal(t, 0, got, "no free port above the conflict means fail-closed 0")
	})
}
