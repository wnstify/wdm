package core_test

import (
	"errors"
	"fmt"
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

// TestRewriteComposeHostPorts proves the rendered-compose host-port remap only
// touches remappable loopback bindings: short-form loopback ports are rewritten
// (protocol preserved), while a container-port match, an all-interfaces entry, a
// non-loopback host IP, a range, and a no-op override all leave the bytes
// untouched. The long-form loopback mapping is rewritten too.
func TestRewriteComposeHostPorts(t *testing.T) {
	t.Parallel()

	short := func(p string) string {
		return "services:\n  app:\n    ports:\n      - \"" + p + "\"\n"
	}

	t.Run("short loopback port rewritten", func(t *testing.T) {
		t.Parallel()
		out, err := core.RewriteComposeHostPortsForTest([]byte(short("127.0.0.1:8080:8080")), map[int]int{8080: 9090})
		require.NoError(t, err)
		assert.Contains(t, string(out), "127.0.0.1:9090:8080")
		assert.NotContains(t, string(out), "127.0.0.1:8080:8080")
	})

	t.Run("protocol preserved", func(t *testing.T) {
		t.Parallel()
		out, err := core.RewriteComposeHostPortsForTest([]byte(short("127.0.0.1:5353:53/udp")), map[int]int{5353: 15353})
		require.NoError(t, err)
		assert.Contains(t, string(out), "127.0.0.1:15353:53/udp")
	})

	t.Run("long-form loopback port rewritten", func(t *testing.T) {
		t.Parallel()
		in := "services:\n  app:\n    ports:\n      - target: 8080\n        published: \"8080\"\n        host_ip: 127.0.0.1\n"
		out, err := core.RewriteComposeHostPortsForTest([]byte(in), map[int]int{8080: 9090})
		require.NoError(t, err)
		assert.Contains(t, string(out), "9090")
		assert.NotContains(t, string(out), "\"8080\"")
	})

	identity := []struct {
		name string
		raw  string
	}{
		{"container port not rewritten", short("127.0.0.1:1234:8080")},
		{"all-interfaces entry untouched", short("8080:8080")},
		{"non-loopback host untouched", short("0.0.0.0:8080:8080")},
		{"range untouched", short("127.0.0.1:8000-8002:8000-8002")},
	}
	for _, tc := range identity {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := []byte(tc.raw)
			out, err := core.RewriteComposeHostPortsForTest(in, map[int]int{8080: 9090, 8000: 9000})
			require.NoError(t, err)
			assert.Equal(t, string(in), string(out), "a non-remappable entry must leave the compose byte-identical")
		})
	}

	t.Run("no overrides is identity", func(t *testing.T) {
		t.Parallel()
		in := []byte(short("127.0.0.1:8080:8080"))
		out, err := core.RewriteComposeHostPortsForTest(in, nil)
		require.NoError(t, err)
		assert.Equal(t, string(in), string(out))
	})
}

// TestRenderInstall_OverrideRewritesComposeHostPort proves the override flows
// all the way into the rendered compose end-to-end: the deployed binding moves
// to the new host port, and the bind-scan verification (which runs on the
// rewritten compose) still passes.
func TestRenderInstall_OverrideRewritesComposeHostPort(t *testing.T) {
	t.Parallel()

	oldPort := freeLocalTCPPort(t)
	newPort := freeLocalTCPPort(t)
	app := appFixture("render-remap-app", oldPort)
	compose := fmt.Sprintf("services:\n  app:\n    image: docker.io/example/app:1.0.0\n    ports:\n      - \"127.0.0.1:%d:8080\"\n", oldPort)
	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		app.ComposeTemplate: compose,
		app.EnvTemplate:     "",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})

	snap, err := core.RenderInstallForTest(eng, t.Context(),
		types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{oldPort: newPort}},
		planHosts(t), nil)
	require.NoError(t, err)
	rendered := string(snap.ComposeBytes)
	assert.Contains(t, rendered, fmt.Sprintf("127.0.0.1:%d:8080", newPort), "the rendered compose must bind the new host port")
	assert.NotContains(t, rendered, fmt.Sprintf("127.0.0.1:%d:8080", oldPort), "the original host port must be gone from the rendered compose")
}

// TestGuidance_RemapRewritesTargetURLs proves a --port/--auto-port remap also
// rewrites the post-install guidance URLs to the actually-bound host port. The
// catalog admin URLs (local_target_url_template and pangolin target_url) point at
// the original catalog port; after the override they must name the new port, not
// the stale catalog one, so the reverse-proxy hint stays correct (issue #146).
func TestGuidance_RemapRewritesTargetURLs(t *testing.T) {
	t.Parallel()

	oldPort := freeLocalTCPPort(t)
	newPort := freeLocalTCPPort(t)
	app := appFixture("guidance-remap-app", oldPort)
	app.LocalTargetURLTemplate = fmt.Sprintf("http://127.0.0.1:%d/", oldPort)
	app.PangolinGuidance.TargetURL = fmt.Sprintf("http://127.0.0.1:%d", oldPort)
	compose := fmt.Sprintf("services:\n  app:\n    image: docker.io/example/app:1.0.0\n    ports:\n      - \"127.0.0.1:%d:8080\"\n", oldPort)
	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		app.ComposeTemplate: compose,
		app.EnvTemplate:     "",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})

	snap, err := core.RenderInstallForTest(eng, t.Context(),
		types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{oldPort: newPort}},
		planHosts(t), nil)
	require.NoError(t, err)
	require.NotNil(t, snap.Guidance)
	require.NotNil(t, snap.Guidance.Pangolin)

	newHostPort := fmt.Sprintf("127.0.0.1:%d", newPort)
	oldHostPort := fmt.Sprintf("127.0.0.1:%d", oldPort)
	assert.Contains(t, snap.Guidance.LocalTargetURL, newHostPort, "local target URL must point at the bound port")
	assert.NotContains(t, snap.Guidance.LocalTargetURL, oldHostPort, "the stale catalog port must be gone from the local target URL")
	assert.Contains(t, snap.Guidance.Pangolin.TargetURL, newHostPort, "pangolin target URL must point at the bound port")
	assert.NotContains(t, snap.Guidance.Pangolin.TargetURL, oldHostPort, "the stale catalog port must be gone from the pangolin target URL")
}

// TestGuidance_RemapRewritesNotePorts proves a --port/--auto-port remap also
// rewrites the loopback host port embedded in free-text guidance notes (the
// "point your reverse proxy at http://127.0.0.1:<port>" hint). The catalog notes
// name the original catalog port; after the override they must name the new
// bound port, matching the already-remapped target URLs (issue #161).
func TestGuidance_RemapRewritesNotePorts(t *testing.T) {
	t.Parallel()

	oldPort := freeLocalTCPPort(t)
	newPort := freeLocalTCPPort(t)
	app := appFixture("guidance-note-remap-app", oldPort)
	app.FirstRunNotes = []string{fmt.Sprintf("Open http://127.0.0.1:%d and create the admin account.", oldPort)}
	app.PangolinGuidance.Notes = []string{fmt.Sprintf("Using your own reverse proxy? Point it to http://127.0.0.1:%d", oldPort)}
	compose := fmt.Sprintf("services:\n  app:\n    image: docker.io/example/app:1.0.0\n    ports:\n      - \"127.0.0.1:%d:8080\"\n", oldPort)
	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		app.ComposeTemplate: compose,
		app.EnvTemplate:     "",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})

	snap, err := core.RenderInstallForTest(eng, t.Context(),
		types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{oldPort: newPort}},
		planHosts(t), nil)
	require.NoError(t, err)
	require.NotNil(t, snap.Guidance)
	require.NotNil(t, snap.Guidance.Pangolin)

	newHostPort := fmt.Sprintf("127.0.0.1:%d", newPort)
	oldHostPort := fmt.Sprintf("127.0.0.1:%d", oldPort)
	require.Len(t, snap.Guidance.FirstRunNotes, 1)
	assert.Contains(t, snap.Guidance.FirstRunNotes[0], newHostPort, "first-run note must point at the bound port")
	assert.NotContains(t, snap.Guidance.FirstRunNotes[0], oldHostPort, "the stale catalog port must be gone from the first-run note")
	require.Len(t, snap.Guidance.Pangolin.Notes, 1)
	assert.Contains(t, snap.Guidance.Pangolin.Notes[0], newHostPort, "pangolin note must point at the bound port")
	assert.NotContains(t, snap.Guidance.Pangolin.Notes[0], oldHostPort, "the stale catalog port must be gone from the pangolin note")
}

// TestRemapGuidanceURL pins the guidance-URL port rewriter: a loopback host —
// whether the 127.0.0.1 literal or the localhost DNS name — has an override-key
// port rewritten, while a non-loopback host, a missing/unparseable port, a port
// that is not an override key, and an empty override map all leave the URL
// untouched.
func TestRemapGuidanceURL(t *testing.T) {
	t.Parallel()

	overrides := map[int]int{8080: 9090}
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"loopback ip rewritten", "http://127.0.0.1:8080/admin", "http://127.0.0.1:9090/admin"},
		{"localhost name rewritten", "http://localhost:8080/", "http://localhost:9090/"},
		{"non-loopback host untouched", "http://10.0.0.5:8080/", "http://10.0.0.5:8080/"},
		{"port not an override key untouched", "http://127.0.0.1:7000/", "http://127.0.0.1:7000/"},
		{"no port untouched", "http://127.0.0.1/", "http://127.0.0.1/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, core.RemapGuidanceURLForTest(tc.raw, overrides))
		})
	}

	t.Run("empty overrides untouched", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "http://127.0.0.1:8080/", core.RemapGuidanceURLForTest("http://127.0.0.1:8080/", nil))
	})
}

// TestRenderInstall_OverrideUnmatchedInComposeFailsClosed proves the drift
// guard: when the catalog host port (which the override matches at plan time)
// does not appear in the rendered compose — a catalog/template host-port drift —
// the remap finds no binding to rewrite and render fails closed rather than
// silently deploying on the template's literal port.
func TestRenderInstall_OverrideUnmatchedInComposeFailsClosed(t *testing.T) {
	t.Parallel()

	catalogPort := freeLocalTCPPort(t)
	newPort := freeLocalTCPPort(t)
	driftedPort := freeLocalTCPPort(t)
	app := appFixture("render-remap-drift-app", catalogPort)
	// The template binds a DIFFERENT host port than the catalog declares.
	compose := fmt.Sprintf("services:\n  app:\n    image: docker.io/example/app:1.0.0\n    ports:\n      - \"127.0.0.1:%d:8080\"\n", driftedPort)
	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		app.ComposeTemplate: compose,
		app.EnvTemplate:     "",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})

	_, err := core.RenderInstallForTest(eng, t.Context(),
		types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{catalogPort: newPort}},
		planHosts(t), nil)
	require.Error(t, err)
	assertVerificationFailed(t, err)
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

	t.Run("collision with another planned port refused", func(t *testing.T) {
		t.Parallel()
		portA := freeLocalTCPPort(t)
		portB := freeLocalTCPPort(t)
		app := appFixture("override-collide-app", portA)
		app.Ports = []catalog.Port{
			{Service: "app", Container: 8080, Host: portA, Protocol: "tcp"},
			{Service: "two", Container: 9090, Host: portB, Protocol: "tcp"},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		// Remap A onto B's already-planned host port: the two bindings would
		// collide. Refused before the probe, deterministically.
		_, err := core.PlanInstallForTest(eng, t.Context(),
			types.InstallRequest{AppID: app.AppID, PortOverrides: map[int]int{portA: portB}},
			planHosts(t), nil)
		require.Error(t, err)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), "collide")
	})
}

// TestPlanPorts_AutoPortResolvesConflict proves --auto-port resolves a single
// remappable loopback conflict non-interactively: the occupied catalog port is
// rebound to the deterministic next-free port without any explicit --port.
func TestPlanPorts_AutoPortResolvesConflict(t *testing.T) {
	t.Parallel()

	busy := occupyLoopbackPort(t)
	app := appFixture("autoport-resolve-app", busy)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

	plan, err := core.PlanInstallForTest(eng, t.Context(),
		types.InstallRequest{AppID: app.AppID, AutoPort: true},
		planHosts(t), nil)
	require.NoError(t, err)
	require.Len(t, plan.LocalPorts, 1)
	// The planner only returns a binding after probing it free, so a port above
	// the conflict is sufficient proof; re-listening here would just re-race the
	// (unheld) port against sibling parallel tests for no added guarantee.
	assert.Greater(t, plan.LocalPorts[0].HostPort, busy, "auto-port scans upward from the conflict")
	assert.Equal(t, "127.0.0.1", plan.LocalPorts[0].HostIP, "auto-port never changes the host IP")
}

// TestPlanPorts_AutoPortFailsClosedWhenNoFreePort proves --auto-port fails
// closed when no free port exists above the conflict: a catalog port at 65535
// (above the Linux ephemeral range) is occupied, leaving no candidate, so the
// plan returns the typed conflict with a zero suggestion and the usage exit.
func TestPlanPorts_AutoPortFailsClosedWhenNoFreePort(t *testing.T) {
	// Not parallel: binds the fixed top-of-range port 65535.
	ln, err := net.Listen("tcp", "127.0.0.1:65535")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	app := appFixture("autoport-noport-app", 65535)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

	_, err = core.PlanInstallForTest(eng, t.Context(),
		types.InstallRequest{AppID: app.AppID, AutoPort: true},
		planHosts(t), nil)
	require.Error(t, err)
	var conflict *types.PortConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 0, conflict.SuggestedHostPort, "no free port above 65535 means fail-closed 0")
	assert.True(t, types.IsCode(err, types.ErrCodeUsageValidation))
}

// TestPlanPorts_AutoPortHonorsExplicitAndResolvesRest proves explicit and
// auto remaps coexist: an explicit --port remap of a free port is honored
// verbatim, while a second occupied port the user did not remap is resolved by
// auto-port. Both bindings stay on 127.0.0.1.
func TestPlanPorts_AutoPortHonorsExplicitAndResolvesRest(t *testing.T) {
	// Not parallel: freeA2 is an unheld free port the plan must still find free
	// when it probes; running in the serial phase shrinks the window in which a
	// parallel ephemeral :0 bind could steal it.
	freeA1 := freeLocalTCPPort(t)
	freeA2 := freeLocalTCPPort(t)
	busyB := occupyLoopbackPort(t)

	app := appFixture("autoport-mixed-app", freeA1)
	app.Ports = []catalog.Port{
		{Service: "app", Container: 8080, Host: freeA1, Protocol: "tcp"},
		{Service: "two", Container: 9090, Host: busyB, Protocol: "tcp"},
	}
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

	plan, err := core.PlanInstallForTest(eng, t.Context(),
		types.InstallRequest{AppID: app.AppID, AutoPort: true, PortOverrides: map[int]int{freeA1: freeA2}},
		planHosts(t), nil)
	require.NoError(t, err)
	require.Len(t, plan.LocalPorts, 2)

	hostPorts := map[int]string{}
	for _, b := range plan.LocalPorts {
		hostPorts[b.HostPort] = b.HostIP
		assert.Equal(t, "127.0.0.1", b.HostIP, "neither remap changes the host IP")
	}
	assert.Contains(t, hostPorts, freeA2, "the explicit --port remap is honored")

	resolvedB := false
	for hostPort := range hostPorts {
		if hostPort > busyB {
			ln, lerr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)))
			if lerr == nil {
				_ = ln.Close()
				resolvedB = true
			}
		}
	}
	assert.True(t, resolvedB, "the unremapped occupied port is auto-resolved to a free port above it")
}

// TestPlanPorts_AutoPortDoesNotRescueExplicitConflict proves explicit --port
// wins over --auto-port: when the user's own explicit target is occupied,
// auto-port does NOT rescue it — the typed conflict naming the explicit target
// surfaces, so the user sees their chosen port failed.
func TestPlanPorts_AutoPortDoesNotRescueExplicitConflict(t *testing.T) {
	t.Parallel()

	free := freeLocalTCPPort(t)
	busyTarget := occupyLoopbackPort(t)
	app := appFixture("autoport-explicit-conflict-app", free)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

	_, err := core.PlanInstallForTest(eng, t.Context(),
		types.InstallRequest{AppID: app.AppID, AutoPort: true, PortOverrides: map[int]int{free: busyTarget}},
		planHosts(t), nil)
	require.Error(t, err)
	var conflict *types.PortConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, busyTarget, conflict.ConflictingHostPort, "the explicit choice is not auto-rescued")
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

// TestPlanPorts_RangeConflictStaysPlain proves a conflict on a range port (a
// non-remappable binding) keeps the plain fail-closed usage-validation error —
// never the typed remap suggestion. The public-port arm is covered
// deterministically by TestEnrichPortConflict_NonRemappableArmsStayPlain, since
// a real 0.0.0.0 bind does not reliably conflict with a 127.0.0.1 listener.
func TestPlanPorts_RangeConflictStaysPlain(t *testing.T) {
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

	t.Run("respects the scan window", func(t *testing.T) {
		t.Parallel()
		busy := occupyLoopbackPort(t)
		conflict := types.PortBinding{Service: "app", HostIP: "127.0.0.1", HostPort: busy, ContainerPort: 8080, Protocol: "tcp"}
		start := busy + 1
		if start <= 1024 {
			start = 1025
		}
		// Reserve every candidate inside the scan window as same-install
		// planned, so the only free ports lie past the window. The bound must
		// make the suggestion give up (0) instead of scanning toward 65535.
		planned := map[int]struct{}{}
		for port := start; port < start+core.PortSuggestScanWindow; port++ {
			planned[port] = struct{}{}
		}
		got := core.SuggestFreePortForTest(eng, t.Context(), conflict, planned)
		assert.Equal(t, 0, got, "no free port inside the scan window means fail-closed 0")
	})
}
