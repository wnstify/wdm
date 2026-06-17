package core_test

// Container-privilege allow-list enforcement (PRD §12.2). Linux capabilities,
// sysctls, and device maps an app requires are declared per service in the
// signed catalog against a closed wdm allow-list; the rendered compose is then
// cross-checked so a tampered template cannot escalate past the catalog or the
// allow-list. This file proves the internal/core enforcement machinery against
// synthetic fixtures plus the sixteen cap-using curated apps (uptime-kuma,
// freshrss, jellyfin, n8n, qbittorrent, syncthing, baserow, docuseal,
// vaultwarden, authentik, meshcentral, nextcloud, wg-adguard, zulip, dockhand,
// stoat) that declare service_hardening:
//
//   - a catalog declaration outside the allow-list (capability, sysctl,
//     non-empty devices, privileged:true) is refused;
//   - the rendered compose is bounded for EVERY service — declaring or not —
//     so an out-of-allow-list cap, an out-of-allow-list sysctl, any device, or
//     privileged:true is refused even on a service the catalog hardens nothing
//     for, and any service that re-adds a capability must drop ALL first;
//   - a service the catalog hardens must render exactly the declared capability
//     and sysctl sets, in both directions;
//   - both Compose sysctls forms (mapping and name=value sequence) classify
//     identically, and CAP_-prefixed / case-variant capability names normalize;
//   - the nineteen curated apps pass the scan clean (the sixteen cap-using
//     apps via parity, the three zero-cap apps via the universal baseline); and
//   - the ELEVATED PRIVILEGE finish-screen block surfaces declared elevation
//     and is omitted entirely when nothing is declared.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
)

func hardenedApp(appID string, hardening ...catalog.ServiceHardening) catalog.App {
	app := appFixture(appID, 8080)
	app.ServiceHardening = hardening
	return app
}

func capabilities(add ...string) *catalog.Capabilities {
	return &catalog.Capabilities{Add: add}
}

func TestVerifyContainerPrivilegeMatchCatalog_CatalogDeclarations(t *testing.T) {
	t.Parallel()

	// A rendered compose that satisfies the universal bounds, so each case
	// isolates the catalog-declaration arm. The router service keeps the
	// cap_drop:ALL baseline and re-adds the allow-list pair.
	cleanCompose := `services:
  router:
    cap_drop:
      - ALL
    cap_add:
      - NET_ADMIN
      - NET_RAW
    sysctls:
      net.ipv4.ip_forward: "1"
`

	cases := []struct {
		name      string
		hardening catalog.ServiceHardening
		content   []string
	}{
		{
			name: "capability outside allow-list (SYS_MODULE)",
			hardening: catalog.ServiceHardening{
				Service:      "router",
				Capabilities: capabilities("SYS_MODULE"),
			},
			content: []string{"SYS_MODULE", "router"},
		},
		{
			name: "capability outside allow-list (SYS_ADMIN)",
			hardening: catalog.ServiceHardening{
				Service:      "router",
				Capabilities: capabilities("SYS_ADMIN"),
			},
			content: []string{"SYS_ADMIN"},
		},
		{
			name: "sysctl outside allow-list",
			hardening: catalog.ServiceHardening{
				Service: "router",
				Sysctls: []catalog.Sysctl{{Name: "kernel.shmmax", Value: "1"}},
			},
			content: []string{"kernel.shmmax"},
		},
		{
			name: "non-empty devices",
			hardening: catalog.ServiceHardening{
				Service: "router",
				Devices: []string{"/dev/net/tun:/dev/net/tun"},
			},
			content: []string{"device", "router"},
		},
		{
			name: "privileged true",
			hardening: catalog.ServiceHardening{
				Service:    "router",
				Privileged: true,
			},
			content: []string{"privileged"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := hardenedApp("catalog-decl-app", tc.hardening)
			err := core.VerifyContainerPrivilegeMatchCatalogForTest(app, []byte(cleanCompose))
			require.Error(t, err)
			assertVerificationFailed(t, err)
			for _, want := range tc.content {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestVerifyContainerPrivilegeMatchCatalog_RenderedBounds(t *testing.T) {
	t.Parallel()

	// declareApp hardens service "app" with the given caps so a case can
	// exercise the allow-list / cap_drop arms (which apply to declaring
	// services) without tripping the non-declaring refusal.
	declareApp := func(appID string, caps ...string) catalog.App {
		return hardenedApp(appID, catalog.ServiceHardening{
			Service:      "app",
			Capabilities: capabilities(caps...),
		})
	}

	cases := []struct {
		name    string
		app     catalog.App
		compose string
		wantErr bool
		content []string
	}{
		{
			name: "in-allow-list cap on a non-declaring service refuses",
			app:  appFixture("undeclared-cap-app", 8080),
			compose: `services:
  app:
    cap_drop:
      - ALL
    cap_add:
      - NET_BIND_SERVICE
`,
			wantErr: true,
			content: []string{"does not declare", "app"},
		},
		{
			name: "sysctl on a non-declaring service refuses",
			app:  appFixture("undeclared-sysctl-app", 8080),
			compose: `services:
  app:
    sysctls:
      net.ipv4.ip_forward: "1"
`,
			wantErr: true,
			content: []string{"does not declare", "app"},
		},
		{
			name: "out-of-allow-list cap on a declaring service refuses",
			app:  declareApp("tampered-cap-app", "NET_ADMIN"),
			compose: `services:
  app:
    cap_drop:
      - ALL
    cap_add:
      - SYS_MODULE
`,
			wantErr: true,
			content: []string{"SYS_MODULE", "app"},
		},
		{
			name: "out-of-allow-list sysctl on a declaring service refuses",
			app: hardenedApp("tampered-sysctl-app", catalog.ServiceHardening{
				Service: "app",
				Sysctls: []catalog.Sysctl{{Name: "net.ipv4.ip_forward", Value: "1"}},
			}),
			compose: `services:
  app:
    sysctls:
      kernel.shmmax: "1"
`,
			wantErr: true,
			content: []string{"kernel.shmmax"},
		},
		{
			name: "rendered devices entry refuses",
			app:  appFixture("tampered-device-app", 8080),
			compose: `services:
  app:
    devices:
      - /dev/net/tun:/dev/net/tun
`,
			wantErr: true,
			content: []string{"device"},
		},
		{
			name: "rendered privileged true refuses",
			app:  appFixture("tampered-priv-app", 8080),
			compose: `services:
  app:
    privileged: true
`,
			wantErr: true,
			content: []string{"privileged"},
		},
		{
			name: "cap_add without cap_drop ALL refuses",
			app:  declareApp("missing-drop-app", "NET_ADMIN"),
			compose: `services:
  app:
    cap_add:
      - NET_ADMIN
`,
			wantErr: true,
			content: []string{"drop", "app"},
		},
		{
			name: "declared cap_add with cap_drop ALL passes",
			app:  declareApp("drop-all-app", "NET_BIND_SERVICE"),
			compose: `services:
  app:
    cap_drop:
      - ALL
    cap_add:
      - NET_BIND_SERVICE
`,
			wantErr: false,
		},
		{
			name: "no privilege keys passes",
			app:  appFixture("baseline-app", 8080),
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := core.VerifyContainerPrivilegeMatchCatalogForTest(tc.app, []byte(tc.compose))
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

func TestVerifyContainerPrivilegeMatchCatalog_DeclaringServiceParity(t *testing.T) {
	t.Parallel()

	declaringApp := func() catalog.App {
		return hardenedApp("parity-app", catalog.ServiceHardening{
			Service:      "router",
			Capabilities: capabilities("NET_ADMIN", "NET_RAW"),
			Sysctls: []catalog.Sysctl{
				{Name: "net.ipv4.ip_forward", Value: "1"},
			},
		})
	}

	cases := []struct {
		name    string
		compose string
		wantErr bool
	}{
		{
			name: "rendered set equals declared set passes",
			compose: `services:
  router:
    cap_drop:
      - ALL
    cap_add:
      - NET_ADMIN
      - NET_RAW
    sysctls:
      net.ipv4.ip_forward: "1"
`,
			wantErr: false,
		},
		{
			name: "rendered omits a declared capability refuses",
			compose: `services:
  router:
    cap_drop:
      - ALL
    cap_add:
      - NET_ADMIN
    sysctls:
      net.ipv4.ip_forward: "1"
`,
			wantErr: true,
		},
		{
			name: "rendered adds a capability the catalog does not declare refuses",
			compose: `services:
  router:
    cap_drop:
      - ALL
    cap_add:
      - NET_ADMIN
      - NET_RAW
      - CHOWN
    sysctls:
      net.ipv4.ip_forward: "1"
`,
			wantErr: true,
		},
		{
			name: "rendered omits a declared sysctl refuses",
			compose: `services:
  router:
    cap_drop:
      - ALL
    cap_add:
      - NET_ADMIN
      - NET_RAW
`,
			wantErr: true,
		},
		{
			name: "rendered sysctl value differs from declared refuses",
			compose: `services:
  router:
    cap_drop:
      - ALL
    cap_add:
      - NET_ADMIN
      - NET_RAW
    sysctls:
      net.ipv4.ip_forward: "0"
`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := core.VerifyContainerPrivilegeMatchCatalogForTest(declaringApp(), []byte(tc.compose))
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assertVerificationFailed(t, err)
		})
	}
}

func TestVerifyContainerPrivilegeMatchCatalog_SysctlForms(t *testing.T) {
	t.Parallel()

	app := func() catalog.App {
		return hardenedApp("sysctl-form-app", catalog.ServiceHardening{
			Service:      "router",
			Capabilities: capabilities("NET_ADMIN"),
			Sysctls: []catalog.Sysctl{
				{Name: "net.ipv4.ip_forward", Value: "1"},
				{Name: "net.ipv4.conf.all.src_valid_mark", Value: "1"},
			},
		})
	}

	t.Run("mapping form classifies and passes parity", func(t *testing.T) {
		t.Parallel()

		compose := `services:
  router:
    cap_drop:
      - ALL
    cap_add:
      - NET_ADMIN
    sysctls:
      net.ipv4.ip_forward: "1"
      net.ipv4.conf.all.src_valid_mark: "1"
`
		require.NoError(t, core.VerifyContainerPrivilegeMatchCatalogForTest(app(), []byte(compose)))
	})

	t.Run("name=value sequence form classifies and passes parity", func(t *testing.T) {
		t.Parallel()

		compose := `services:
  router:
    cap_drop:
      - ALL
    cap_add:
      - NET_ADMIN
    sysctls:
      - net.ipv4.ip_forward=1
      - net.ipv4.conf.all.src_valid_mark=1
`
		require.NoError(t, core.VerifyContainerPrivilegeMatchCatalogForTest(app(), []byte(compose)))
	})

	t.Run("sequence entry without an equals fails closed", func(t *testing.T) {
		t.Parallel()

		compose := `services:
  app:
    sysctls:
      - net.ipv4.ip_forward
`
		err := core.VerifyContainerPrivilegeMatchCatalogForTest(appFixture("bad-sysctl-app", 8080), []byte(compose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})
}

func TestVerifyContainerPrivilegeMatchCatalog_CapabilityNormalization(t *testing.T) {
	t.Parallel()

	// CAP_-prefixed and lower-cased catalog declarations must match a rendered
	// compose that spells the same capabilities the canonical way (and vice
	// versa) so the parity comparison is normalization-insensitive.
	app := hardenedApp("normalize-app", catalog.ServiceHardening{
		Service:      "router",
		Capabilities: capabilities("cap_net_admin"),
	})
	compose := `services:
  router:
    cap_drop:
      - all
    cap_add:
      - CAP_NET_ADMIN
`
	require.NoError(t, core.VerifyContainerPrivilegeMatchCatalogForTest(app, []byte(compose)))
}

func TestVerifyContainerPrivilegeMatchCatalog_ValidHardenedAppPasses(t *testing.T) {
	t.Parallel()

	app := hardenedApp("valid-hardened-app", catalog.ServiceHardening{
		Service:      "router",
		Capabilities: capabilities("NET_ADMIN", "NET_RAW"),
		Sysctls: []catalog.Sysctl{
			{Name: "net.ipv4.ip_forward", Value: "1"},
			{Name: "net.ipv4.conf.all.src_valid_mark", Value: "1"},
		},
	})
	compose := `services:
  router:
    cap_drop:
      - ALL
    cap_add:
      - NET_ADMIN
      - NET_RAW
    sysctls:
      net.ipv4.ip_forward: "1"
      net.ipv4.conf.all.src_valid_mark: "1"
  sidecar:
    image: docker.io/example/sidecar:1.0.0
`
	require.NoError(t, core.VerifyContainerPrivilegeMatchCatalogForTest(app, []byte(compose)))
}

func TestVerifyContainerPrivilegeMatchCatalog_RealStableCatalogPassesClean(t *testing.T) {
	t.Parallel()

	// The fifteen cap-using curated apps declare service_hardening matching
	// their templates exactly (parity check C), and the three zero-cap apps add
	// no capabilities, sysctls, devices, or privileged flags, so the scan is a
	// clean pass on every real template — guarding against a declaration that
	// drifts from the rendered cap_add and would break every install.
	for _, app := range loadRealStableCatalogApps(t) {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			composeBytes := readRepoFile(t, app.ComposeTemplate)
			require.NoError(t,
				core.VerifyContainerPrivilegeMatchCatalogForTest(app, composeBytes),
				"the real stable catalog declarations must match the rendered template for %s",
				app.AppID,
			)
		})
	}
}

func TestContainerPrivilegeDisclosure(t *testing.T) {
	t.Parallel()

	t.Run("hardened app surfaces the elevated privilege block", func(t *testing.T) {
		t.Parallel()

		app := hardenedApp("disclosure-app",
			catalog.ServiceHardening{
				Service:      "router",
				Capabilities: capabilities("NET_ADMIN", "NET_RAW"),
				Sysctls: []catalog.Sysctl{
					{Name: "net.ipv4.ip_forward", Value: "1"},
				},
			},
		)
		lines := core.ContainerPrivilegeDisclosureLinesForTest(app)
		require.NotEmpty(t, lines)

		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, "ELEVATED PRIVILEGE")
		assert.Contains(t, joined, "service router")
		assert.Contains(t, joined, "NET_ADMIN")
		assert.Contains(t, joined, "NET_RAW")
		assert.Contains(t, joined, "net.ipv4.ip_forward")
	})

	t.Run("service declaring no elevation is skipped", func(t *testing.T) {
		t.Parallel()

		app := hardenedApp("partial-disclosure-app",
			catalog.ServiceHardening{Service: "quiet"},
			catalog.ServiceHardening{
				Service:      "router",
				Capabilities: capabilities("NET_ADMIN"),
			},
		)
		lines := core.ContainerPrivilegeDisclosureLinesForTest(app)
		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, "service router")
		assert.NotContains(t, joined, "service quiet")
	})

	t.Run("curated apps surface a block only when they declare hardening", func(t *testing.T) {
		t.Parallel()

		// The sixteen cap-using curated apps declare service_hardening and must
		// surface the ELEVATED PRIVILEGE block naming their re-added caps; the
		// three zero-cap apps (navidrome, openwebui, serpbear) declare none and
		// must emit no block. docuseal, vaultwarden, and authentik declare
		// hardening for their postgres only, and meshcentral for its mongodb
		// only (their app services run with zero added caps, so those services
		// are not named in the block). nextcloud hardens postgres, app, and cron
		// (its redis and nginx run as their image users with zero added caps).
		// wg-adguard hardens both services: wg-easy with NET_ADMIN + NET_RAW and
		// adguard with NET_BIND_SERVICE + DAC_OVERRIDE. zulip hardens postgres
		// (the four init caps) and its zulip server (six caps), while memcached,
		// rabbitmq, and redis run as their image users with zero added caps.
		// dockhand hardens postgresql and its dockhand server (the four init caps
		// each), while its socket-proxy runs as its image user with zero added
		// caps. stoat hardens its three gosu-drop data services (database, redis,
		// rabbit, each with the five init / privilege-drop caps), garage (just
		// DAC_OVERRIDE, so container root can read its 0600 bind-mounted
		// garage.toml), and caddy (just NET_BIND_SERVICE); its eight Rust
		// services, web, livekit, and both init containers run with zero added
		// caps.
		expectedCaps := map[string][]string{
			"uptime-kuma": {"mariadb", "uptime-kuma", "NET_RAW", "CHOWN"},
			"freshrss":    {"freshrss", "NET_BIND_SERVICE"},
			"jellyfin":    {"jellyfin", "DAC_OVERRIDE"},
			"n8n":         {"postgres", "FOWNER"},
			"qbittorrent": {"qbittorrent", "CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE"},
			"syncthing":   {"syncthing", "CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE"},
			"baserow":     {"postgres", "baserow", "NET_BIND_SERVICE", "FOWNER", "CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE"},
			"docuseal":    {"postgres", "CHOWN", "SETGID", "SETUID", "DAC_OVERRIDE", "FOWNER"},
			"vaultwarden": {"postgres", "CHOWN", "SETGID", "SETUID", "DAC_OVERRIDE", "FOWNER"},
			"authentik":   {"postgres", "CHOWN", "SETGID", "SETUID", "DAC_OVERRIDE", "FOWNER"},
			"meshcentral": {"mongodb", "CHOWN", "SETGID", "SETUID", "DAC_OVERRIDE", "FOWNER"},
			"nextcloud":   {"postgres", "app", "cron", "CHOWN", "SETGID", "SETUID", "DAC_OVERRIDE", "FOWNER"},
			"wg-adguard":  {"wg-easy", "adguard", "NET_ADMIN", "NET_RAW", "NET_BIND_SERVICE", "DAC_OVERRIDE"},
			"zulip":       {"postgres", "zulip", "NET_BIND_SERVICE", "CHOWN", "SETGID", "SETUID", "DAC_OVERRIDE", "FOWNER"},
			"dockhand":    {"postgresql", "dockhand", "CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE"},
			"stoat":       {"database", "redis", "rabbit", "garage", "caddy", "CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID", "NET_BIND_SERVICE"},
		}
		for _, app := range loadRealStableCatalogApps(t) {
			lines := core.ContainerPrivilegeDisclosureLinesForTest(app)
			wantCaps, declares := expectedCaps[app.AppID]
			if !declares {
				assert.Empty(t, lines,
					"curated app %s declares no service hardening and must emit no disclosure block",
					app.AppID,
				)
				continue
			}
			joined := strings.Join(lines, "\n")
			assert.Contains(t, joined, "ELEVATED PRIVILEGE")
			for _, want := range wantCaps {
				assert.Contains(t, joined, want,
					"curated app %s disclosure block must name %q", app.AppID, want)
			}
		}
	})
}
