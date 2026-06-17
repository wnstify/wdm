package core_test

// Public-port declaration enforcement (PRD §11.1). A host port may bind all
// interfaces (0.0.0.0) only when the signed catalog declares that port public;
// every undeclared port stays localhost-only. This file proves the
// internal/core enforcement machinery against synthetic fixtures (no curated
// app uses the v2 port.public/host_range fields yet):
//
//   - planPorts derives the bind interface SOLELY from catalog port.public —
//     0.0.0.0 when public, 127.0.0.1 otherwise — and no InstallRequest field
//     can force a public bind (§11.1(a)(b));
//   - range ports (host_range/container_range) plan correctly and malformed
//     ranges refuse with a verification error;
//   - a public declaration for the app's web-UI/admin port is refused
//     (§11.1(d));
//   - the rendered-compose scan refuses any public bind with no backing
//     public:true declaration and any public declaration that drifted to a
//     loopback bind (§11.1(a)(b)); and
//   - the deploy confirmation names every public port + protocol, and carries
//     no warning noise when nothing binds publicly (§11.1(e)).

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

func TestPlanPorts_BindInterfaceDerivesFromCatalogPublic(t *testing.T) {
	t.Parallel()

	t.Run("public port binds 0.0.0.0", func(t *testing.T) {
		t.Parallel()

		publicPort := freeLocalTCPPort(t)
		app := appFixture("public-bind-app", publicPort)
		// The single port is the public data port. Move the admin surface off
		// it so the §11.1(d) backstop does not refuse the public declaration.
		app.LocalTargetURLTemplate = "http://127.0.0.1:1"
		app.PangolinGuidance.TargetURL = "http://127.0.0.1:1"
		app.Ports = []catalog.Port{
			{Service: "bt", Container: 6881, Host: publicPort, Protocol: "tcp", Public: true},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		plan, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: app.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		require.NoError(t, err)
		require.Len(t, plan.LocalPorts, 1)
		assert.Equal(t, "0.0.0.0", plan.LocalPorts[0].HostIP)
	})

	t.Run("undeclared port stays 127.0.0.1", func(t *testing.T) {
		t.Parallel()

		app := appFixture("loopback-bind-app", freeLocalTCPPort(t))
		app.Ports = []catalog.Port{
			{Service: "web", Container: 8080, Host: freeLocalTCPPort(t), Protocol: "tcp"},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		plan, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: app.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		require.NoError(t, err)
		require.Len(t, plan.LocalPorts, 1)
		assert.Equal(t, "127.0.0.1", plan.LocalPorts[0].HostIP)
	})

	t.Run("no install request field forces a public bind", func(t *testing.T) {
		t.Parallel()

		// The catalog declares no public port. Driving install with every
		// InstallRequest field that could plausibly touch ports must never
		// produce a 0.0.0.0 bind — the interface derives solely from the
		// catalog (§11.1(a)(b)).
		app := appFixture("no-user-public-app", freeLocalTCPPort(t))
		app.Ports = []catalog.Port{
			{Service: "web", Container: 8080, Host: freeLocalTCPPort(t), Protocol: "tcp"},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		req := types.InstallRequest{
			AppID:           app.AppID,
			Domain:          "example.invalid",
			StackPath:       "",
			ResourceProfile: types.ResourceProfileMin,
		}
		plan, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			req,
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		require.NoError(t, err)
		require.Len(t, plan.LocalPorts, 1)
		assert.Equal(t, "127.0.0.1", plan.LocalPorts[0].HostIP)
	})
}

func TestPortBindings_RangeExpansion(t *testing.T) {
	t.Parallel()

	t.Run("valid range expands to one binding per port", func(t *testing.T) {
		t.Parallel()

		// The schema pairs the range with Host/Container at the low ends; the
		// expansion produces one binding per port across the inclusive span,
		// all on the public interface.
		port := catalog.Port{
			Service:        "media",
			Container:      60000,
			Host:           50000,
			Protocol:       "udp",
			Public:         true,
			HostRange:      "50000-50002",
			ContainerRange: "60000-60002",
		}
		bindings, err := core.PortBindingsForTest(port)
		require.NoError(t, err)
		require.Len(t, bindings, 3)
		for i, binding := range bindings {
			assert.Equal(t, "0.0.0.0", binding.HostIP)
			assert.Equal(t, "udp", binding.Protocol)
			assert.Equal(t, 50000+i, binding.HostPort)
			assert.Equal(t, 60000+i, binding.ContainerPort)
		}
	})

	t.Run("undeclared range binds loopback", func(t *testing.T) {
		t.Parallel()

		port := catalog.Port{
			Service:        "media",
			Container:      50000,
			Host:           50000,
			Protocol:       "udp",
			HostRange:      "50000-50001",
			ContainerRange: "50000-50001",
		}
		bindings, err := core.PortBindingsForTest(port)
		require.NoError(t, err)
		require.Len(t, bindings, 2)
		for _, binding := range bindings {
			assert.Equal(t, "127.0.0.1", binding.HostIP)
		}
	})

	t.Run("invalid ranges refuse", func(t *testing.T) {
		t.Parallel()

		// internal/core is the defense-in-depth layer behind the catalog JSON
		// schema, so these malformed entries (some of which the schema would
		// also reject) must still fail closed here.
		cases := []struct {
			name string
			port catalog.Port
		}{
			{
				name: "lo greater than hi",
				port: catalog.Port{Service: "x", Host: 6000, Container: 6000, HostRange: "6000-5000", ContainerRange: "6000-5000"},
			},
			{
				name: "out of bounds",
				port: catalog.Port{Service: "x", Host: 70000, Container: 70000, HostRange: "70000-70010", ContainerRange: "70000-70010"},
			},
			{
				name: "host and container spans differ",
				port: catalog.Port{Service: "x", Host: 6000, Container: 6000, HostRange: "6000-6009", ContainerRange: "6000-6000"},
			},
			{
				name: "single host contradicts range low end",
				port: catalog.Port{Service: "x", Host: 7000, Container: 6000, HostRange: "6000-6009", ContainerRange: "6000-6009"},
			},
			{
				name: "non-numeric range",
				port: catalog.Port{Service: "x", Host: 6000, Container: 6000, HostRange: "abc-def", ContainerRange: "abc-def"},
			},
			{
				name: "range without its pair",
				port: catalog.Port{Service: "x", Host: 6000, Container: 6000, HostRange: "6000-6009"},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				_, err := core.PortBindingsForTest(tc.port)
				require.Error(t, err)
				assertVerificationFailed(t, err)
			})
		}
	})
}

func TestPlanPorts_AdminPortPublicRefused(t *testing.T) {
	t.Parallel()

	t.Run("local_target_url_template admin port refused", func(t *testing.T) {
		t.Parallel()

		adminPort := freeLocalTCPPort(t)
		app := appFixture("admin-url-app", adminPort)
		app.LocalTargetURLTemplate = "http://127.0.0.1:" + strconv.Itoa(adminPort)
		app.Ports = []catalog.Port{
			{Service: "web", Container: 8080, Host: adminPort, Protocol: "tcp", Public: true},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		_, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: app.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		require.Error(t, err)
		assertVerificationFailed(t, err)
		assert.Contains(t, err.Error(), strconv.Itoa(adminPort))
		assert.Contains(t, err.Error(), "web-UI/admin")
	})

	t.Run("pangolin target url admin port refused", func(t *testing.T) {
		t.Parallel()

		adminPort := freeLocalTCPPort(t)
		app := appFixture("admin-pangolin-app", adminPort)
		// No local_target_url_template: the admin port comes from the Pangolin
		// guidance TargetURL instead.
		app.LocalTargetURLTemplate = ""
		app.PangolinGuidance.TargetURL = "http://127.0.0.1:" + strconv.Itoa(adminPort)
		app.Ports = []catalog.Port{
			// First port is the admin surface (loopback fallback target) but it
			// is declared public, which the Pangolin signal must catch.
			{Service: "web", Container: 8080, Host: adminPort, Protocol: "tcp", Public: true},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		_, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: app.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		require.Error(t, err)
		assertVerificationFailed(t, err)
		assert.Contains(t, err.Error(), strconv.Itoa(adminPort))
	})
}

func TestPlanPorts_PublicFirstPortNotMisRefused(t *testing.T) {
	t.Parallel()

	t.Run("public-first app with no admin signal is not refused", func(t *testing.T) {
		t.Parallel()

		// A public-first app: no local_target_url_template and no Pangolin
		// target URL, so the only admin signal is the positional fallback. Its
		// single (first) port is the public data port. The fallback must skip
		// the public port and contribute no admin port, so the legitimately
		// public declaration is NOT refused (§11.1).
		publicPort := freeLocalTCPPort(t)
		app := appFixture("public-first-app", publicPort)
		app.LocalTargetURLTemplate = ""
		app.PangolinGuidance.TargetURL = ""
		app.Ports = []catalog.Port{
			{Service: "bt", Container: 6881, Host: publicPort, Protocol: "tcp", Public: true},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		plan, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: app.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		require.NoError(t, err)
		require.Len(t, plan.LocalPorts, 1)
		assert.Equal(t, "0.0.0.0", plan.LocalPorts[0].HostIP)
	})

	t.Run("public first port that is also the pangolin admin surface is still refused", func(t *testing.T) {
		t.Parallel()

		// Regression backstop: when the first port is genuinely the admin
		// surface AND declared public, the URL signal (here Pangolin's
		// target_url) still identifies it as admin and refuses the public
		// declaration even though the positional fallback now skips public
		// ports (§11.1(d)).
		adminPort := freeLocalTCPPort(t)
		app := appFixture("public-admin-first-app", adminPort)
		app.LocalTargetURLTemplate = ""
		app.PangolinGuidance.TargetURL = "http://127.0.0.1:" + strconv.Itoa(adminPort)
		app.Ports = []catalog.Port{
			{Service: "web", Container: 8080, Host: adminPort, Protocol: "tcp", Public: true},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		_, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: app.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		require.Error(t, err)
		assertVerificationFailed(t, err)
		assert.Contains(t, err.Error(), strconv.Itoa(adminPort))
		assert.Contains(t, err.Error(), "web-UI/admin")
	})
}

func TestVerifyPublicBindsMatchCatalog(t *testing.T) {
	t.Parallel()

	// publicApp declares one public:true data port; tests vary the rendered
	// compose to drive each classification arm.
	publicApp := func() catalog.App {
		app := appFixture("public-scan-app", 6881)
		app.Ports = []catalog.Port{
			{Service: "bt", Container: 6881, Host: 6881, Protocol: "tcp", Public: true},
		}
		return app
	}
	localApp := func() catalog.App {
		app := appFixture("local-scan-app", 8080)
		app.Ports = []catalog.Port{
			{Service: "web", Container: 8080, Host: 8080, Protocol: "tcp"},
		}
		return app
	}

	cases := []struct {
		name    string
		app     catalog.App
		compose string
		wantErr bool
		content []string
	}{
		{
			name: "matched public pair passes",
			app:  publicApp(),
			compose: `services:
  bt:
    ports:
      - "0.0.0.0:6881:6881"
`,
			wantErr: false,
		},
		{
			name: "short-form 0.0.0.0 bind with no declaration refuses",
			app:  localApp(),
			compose: `services:
  web:
    ports:
      - "0.0.0.0:8080:8080"
`,
			wantErr: true,
			content: []string{"local-scan-app", "tcp/8080"},
		},
		{
			name: "bare host:container bind with no declaration refuses",
			app:  localApp(),
			compose: `services:
  web:
    ports:
      - "8080:8080"
`,
			wantErr: true,
			content: []string{"tcp/8080"},
		},
		{
			name: "long-form host_ip 0.0.0.0 with no declaration refuses",
			app:  localApp(),
			compose: `services:
  web:
    ports:
      - target: 8080
        published: "8080"
        host_ip: 0.0.0.0
        protocol: tcp
`,
			wantErr: true,
			content: []string{"tcp/8080"},
		},
		{
			name: "public declaration rendered as loopback refuses",
			app:  publicApp(),
			compose: `services:
  bt:
    ports:
      - "127.0.0.1:6881:6881"
`,
			wantErr: true,
			content: []string{"public-scan-app", "tcp/6881"},
		},
		{
			name: "loopback-only app passes",
			app:  localApp(),
			compose: `services:
  web:
    ports:
      - "127.0.0.1:8080:8080"
`,
			wantErr: false,
		},
		{
			name: "host networking refused even with no ports list",
			app:  localApp(),
			compose: `services:
  web:
    network_mode: host
`,
			wantErr: true,
			content: []string{"local-scan-app", "web", "network_mode"},
		},
		{
			name: "host networking refused even with a loopback ports entry",
			app:  localApp(),
			compose: `services:
  web:
    network_mode: host
    ports:
      - "127.0.0.1:8080:8080"
`,
			wantErr: true,
			content: []string{"web", "network_mode"},
		},
		{
			name: "long-form loopback host_ip passes",
			app:  localApp(),
			compose: `services:
  web:
    ports:
      - target: 8080
        published: "8080"
        host_ip: 127.0.0.1
        protocol: tcp
`,
			wantErr: false,
		},
		{
			name: "127.0.0.0/8 short-form loopback passes",
			app:  localApp(),
			compose: `services:
  web:
    ports:
      - "127.0.0.5:8080:8080"
`,
			wantErr: false,
		},
		{
			name: "ipv6 loopback long-form passes",
			app:  localApp(),
			compose: `services:
  web:
    ports:
      - target: 8080
        published: "8080"
        host_ip: "::1"
        protocol: tcp
`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := core.VerifyPublicBindsMatchCatalogForTest(tc.app, []byte(tc.compose))
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assertVerificationFailed(t, err)
			for _, want := range tc.content {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestVerifyPublicBindsMatchCatalog_RangePairPasses(t *testing.T) {
	t.Parallel()

	app := appFixture("public-range-scan-app", 50000)
	app.Ports = []catalog.Port{
		{
			Service:        "media",
			Protocol:       "udp",
			Public:         true,
			HostRange:      "50000-50100",
			ContainerRange: "50000-50100",
		},
	}
	compose := `services:
  media:
    ports:
      - "0.0.0.0:50000-50100:50000-50100/udp"
`
	require.NoError(t, core.VerifyPublicBindsMatchCatalogForTest(app, []byte(compose)))
}

func TestVerifyPublicBindsMatchCatalog_RealStableCatalogPassesClean(t *testing.T) {
	t.Parallel()

	// Most curated apps bind 127.0.0.1 and declare no public ports; qbittorrent
	// (BitTorrent 6881) and syncthing (BEP 22000) declare their sync ports public
	// (tcp+udp) and their templates render the matching all-interface binds.
	// Either way the rendered binds must equal the
	// catalog public declarations exactly, so the scan is a clean pass on every
	// real template — guarding against a drift that would break every install.
	for _, app := range loadRealStableCatalogApps(t) {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			composeBytes := readRepoFile(t, app.ComposeTemplate)
			require.NoError(t,
				core.VerifyPublicBindsMatchCatalogForTest(app, composeBytes),
				"the real stable catalog public binds must match the rendered template for %s",
				app.AppID,
			)
		})
	}
}

func TestInstallConfirmation_PublicPortWarning(t *testing.T) {
	t.Parallel()

	t.Run("names each public port and protocol", func(t *testing.T) {
		t.Parallel()

		adminPort := freeLocalTCPPort(t)
		publicPort := freeLocalTCPPort(t)
		app := installConfirmationApp(t, "public-warn-app", adminPort, publicPort)

		confirmer := &fakeConfirmer{}
		eng := newPublicInstallEngine(t, app)
		_, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, confirmer)
		require.NoError(t, err)

		require.Len(t, confirmer.calls, 1)
		message := confirmer.calls[0].Message
		assert.Contains(t, message, "PUBLIC PORT WARNING")
		assert.Contains(t, message, "0.0.0.0:"+strconv.Itoa(publicPort)+"/tcp")
		assert.Contains(t, message, "reachable from any network interface")
		assert.Contains(t, message, "service bt")
		// The loopback admin port must not be named in the warning block.
		warningBlock := message[strings.Index(message, "PUBLIC PORT WARNING"):]
		assert.NotContains(t, warningBlock, strconv.Itoa(adminPort))
	})

	t.Run("no warning when nothing binds publicly", func(t *testing.T) {
		t.Parallel()

		app := appFixture("no-warn-app", freeLocalTCPPort(t))
		app.ComposeTemplate = "templates/no-warn-app/docker-compose.yml.tmpl"
		app.EnvTemplate = "templates/no-warn-app/.env.tmpl"
		app.Ports = []catalog.Port{
			{Service: "app", Container: 8080, Host: freeLocalTCPPort(t), Protocol: "tcp"},
		}
		confirmer := &fakeConfirmer{}
		eng := newPublicInstallEngine(t, app)
		_, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, confirmer)
		require.NoError(t, err)

		require.Len(t, confirmer.calls, 1)
		assert.NotContains(t, confirmer.calls[0].Message, "PUBLIC PORT WARNING")
	})
}

// installConfirmationApp builds a two-port app: the first port is the loopback
// admin/web-UI surface, the second is a public data port that renders 0.0.0.0
// in the compose template. The admin port is kept off the public port so the
// §11.1(d) backstop does not refuse the install.
func installConfirmationApp(t *testing.T, appID string, adminPort, publicPort int) catalog.App {
	t.Helper()

	app := appFixture(appID, adminPort)
	app.LocalTargetURLTemplate = "http://127.0.0.1:" + strconv.Itoa(adminPort)
	app.PangolinGuidance.TargetURL = "http://127.0.0.1:" + strconv.Itoa(adminPort)
	app.ComposeTemplate = "templates/" + appID + "/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/" + appID + "/.env.tmpl"
	app.Ports = []catalog.Port{
		{Service: "web", Container: 8080, Host: adminPort, Protocol: "tcp"},
		{Service: "bt", Container: 6881, Host: publicPort, Protocol: "tcp", Public: true},
	}
	// The image-pin check requires every pinned service to exist in the
	// rendered compose; pin the two services this fixture renders.
	app.ImagePins = []catalog.ImagePin{
		{Service: "web", Image: "docker.io/example/web", Tag: "1.0.0"},
		{Service: "bt", Image: "docker.io/example/bt", Tag: "1.0.0"},
	}
	return app
}

// newPublicInstallEngine wires an engine over the app fixture with a compose
// template whose bind interfaces match the catalog declarations, so the
// public-bind scan passes and the full install path runs to the confirmation
// gate with the fake Docker client.
func newPublicInstallEngine(t *testing.T, app catalog.App) *core.Engine {
	t.Helper()

	compose := renderConfirmationCompose(app)
	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		app.ComposeTemplate: compose,
		app.EnvTemplate:     "",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(&fakeDockerClient{}))
	return eng
}

// renderConfirmationCompose emits a compose whose port binds mirror the
// catalog: loopback for undeclared ports, 0.0.0.0 for public ones, matching
// the image pin so the sibling image-pin check also passes.
func renderConfirmationCompose(app catalog.App) string {
	var b strings.Builder
	b.WriteString("services:\n")
	services := map[string][]catalog.Port{}
	var order []string
	for _, port := range app.Ports {
		if _, ok := services[port.Service]; !ok {
			order = append(order, port.Service)
		}
		services[port.Service] = append(services[port.Service], port)
	}
	image := map[string]string{}
	for _, pin := range app.ImagePins {
		image[pin.Service] = pin.Image + ":" + pin.Tag
	}
	for _, service := range order {
		b.WriteString("  " + service + ":\n")
		if img, ok := image[service]; ok {
			b.WriteString("    image: " + img + "\n")
		}
		b.WriteString("    ports:\n")
		for _, port := range services[service] {
			hostIP := "127.0.0.1"
			if port.Public {
				hostIP = "0.0.0.0"
			}
			b.WriteString("      - \"" + hostIP + ":" + strconv.Itoa(port.Host) + ":" + strconv.Itoa(port.Container) + "\"\n")
		}
	}
	return b.String()
}
