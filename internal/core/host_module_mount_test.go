package core_test

// Host /lib/modules mount enforcement (PRD §9, §12.2). SYS_MODULE is excluded
// from the capability allow-list, so a service needing a host-loaded kernel
// module (e.g. WireGuard) is granted a host modprobe prerequisite plus a
// read-only /lib/modules mount declared via service_hardening host_module_mount.
// The rendered compose is then cross-checked so a tampered template cannot mount
// the host module tree past the catalog declaration. This file proves the
// internal/core enforcement machinery against synthetic fixtures (wg-adguard's
// wg-easy service is the first curated app to declare host_module_mount):
//
//   - a service the catalog declares host_module_mount:true for that renders a
//     read-only /lib/modules:/lib/modules mount passes;
//   - a declared mount that is absent, read-write, or targets a different
//     container path is refused;
//   - a non-declaring service that mounts host /lib/modules is refused (the
//     universal bound a tampered template cannot evade);
//   - both Compose mount forms (short string with :ro, long mapping with
//     read_only) classify identically, and an unclassifiable volume node fails
//     closed; and
//   - across the nineteen curated apps only wg-adguard's wg-easy service mounts
//     /lib/modules (declared host_module_mount:true), so the scan is a clean
//     pass on every real template — wg-adguard via parity, the rest via the
//     no-mount baseline.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
)

func moduleMountApp(appID string, hardening ...catalog.ServiceHardening) catalog.App {
	app := appFixture(appID, 8080)
	app.ServiceHardening = hardening
	return app
}

func TestVerifyHostModuleMountMatchCatalog_DeclaredReadOnlyPasses(t *testing.T) {
	t.Parallel()

	app := moduleMountApp("wg", catalog.ServiceHardening{
		Service:         "wireguard",
		HostModuleMount: true,
	})

	cases := []struct {
		name    string
		compose string
	}{
		{
			name: "short form with ro mode",
			compose: `services:
  wireguard:
    image: docker.io/example/wireguard:1.0.0
    volumes:
      - /lib/modules:/lib/modules:ro
`,
		},
		{
			name: "short form with ro mode and extra flag",
			compose: `services:
  wireguard:
    image: docker.io/example/wireguard:1.0.0
    volumes:
      - /lib/modules:/lib/modules:ro,z
`,
		},
		{
			name: "long mapping form read_only true",
			compose: `services:
  wireguard:
    image: docker.io/example/wireguard:1.0.0
    volumes:
      - type: bind
        source: /lib/modules
        target: /lib/modules
        read_only: true
`,
		},
		{
			name: "trailing slash source normalizes",
			compose: `services:
  wireguard:
    image: docker.io/example/wireguard:1.0.0
    volumes:
      - /lib/modules/:/lib/modules:ro
`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, core.VerifyHostModuleMountMatchCatalogForTest(app, []byte(tt.compose)))
		})
	}
}

func TestVerifyHostModuleMountMatchCatalog_DeclaredRefusals(t *testing.T) {
	t.Parallel()

	app := moduleMountApp("wg", catalog.ServiceHardening{
		Service:         "wireguard",
		HostModuleMount: true,
	})

	cases := []struct {
		name    string
		compose string
	}{
		{
			name: "declared but no module mount",
			compose: `services:
  wireguard:
    image: docker.io/example/wireguard:1.0.0
    volumes:
      - wg-data:/etc/wireguard
`,
		},
		{
			name: "declared but read-write short form",
			compose: `services:
  wireguard:
    image: docker.io/example/wireguard:1.0.0
    volumes:
      - /lib/modules:/lib/modules
`,
		},
		{
			name: "declared but explicit rw mode",
			compose: `services:
  wireguard:
    image: docker.io/example/wireguard:1.0.0
    volumes:
      - /lib/modules:/lib/modules:rw
`,
		},
		{
			name: "declared but read_only false long form",
			compose: `services:
  wireguard:
    image: docker.io/example/wireguard:1.0.0
    volumes:
      - type: bind
        source: /lib/modules
        target: /lib/modules
        read_only: false
`,
		},
		{
			name: "declared but wrong container target",
			compose: `services:
  wireguard:
    image: docker.io/example/wireguard:1.0.0
    volumes:
      - /lib/modules:/host/modules:ro
`,
		},
		{
			name: "declared service absent from rendered compose",
			compose: `services:
  other:
    image: docker.io/example/other:1.0.0
`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := core.VerifyHostModuleMountMatchCatalogForTest(app, []byte(tt.compose))
			assertVerificationFailed(t, err)
		})
	}
}

func TestVerifyHostModuleMountMatchCatalog_UndeclaredServiceMountRefused(t *testing.T) {
	t.Parallel()

	// No service_hardening at all: any /lib/modules mount is unbacked.
	app := moduleMountApp("plain")

	cases := []struct {
		name    string
		compose string
	}{
		{
			name: "short form read-only on undeclared service",
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - /lib/modules:/lib/modules:ro
`,
		},
		{
			name: "long form on undeclared service",
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - type: bind
        source: /lib/modules
        target: /lib/modules
        read_only: true
`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := core.VerifyHostModuleMountMatchCatalogForTest(app, []byte(tt.compose))
			assertVerificationFailed(t, err)
		})
	}
}

func TestVerifyHostModuleMountMatchCatalog_DeclaringServiceOnlyBoundsThatService(t *testing.T) {
	t.Parallel()

	// The declaration exempts only the named service; a second service mounting
	// host /lib/modules is still refused by the universal bound.
	app := moduleMountApp("wg", catalog.ServiceHardening{
		Service:         "wireguard",
		HostModuleMount: true,
	})

	compose := `services:
  wireguard:
    image: docker.io/example/wireguard:1.0.0
    volumes:
      - /lib/modules:/lib/modules:ro
  sidecar:
    image: docker.io/example/sidecar:1.0.0
    volumes:
      - /lib/modules:/lib/modules:ro
`

	err := core.VerifyHostModuleMountMatchCatalogForTest(app, []byte(compose))
	assertVerificationFailed(t, err)
}

func TestVerifyHostModuleMountMatchCatalog_UnrelatedVolumesPass(t *testing.T) {
	t.Parallel()

	// A named volume "libmodules" and an unrelated /lib/modules.bak host bind
	// must not be misclassified as the host module tree.
	app := moduleMountApp("plain")

	compose := `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - libmodules:/data
      - /lib/modules.bak:/backup:ro
      - app-data:/var/lib/app
`

	require.NoError(t, core.VerifyHostModuleMountMatchCatalogForTest(app, []byte(compose)))
}

func TestVerifyHostModuleMountMatchCatalog_UnclassifiableVolumeFailsClosed(t *testing.T) {
	t.Parallel()

	app := moduleMountApp("plain")

	// A sequence node where Compose expects a scalar or mapping volume entry is
	// unclassifiable and must fail closed rather than pass silently.
	compose := `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - [unexpected, sequence, node]
`

	err := core.VerifyHostModuleMountMatchCatalogForTest(app, []byte(compose))
	assertVerificationFailed(t, err)
}

func TestVerifyHostModuleMountMatchCatalog_RealStableCatalogPassesClean(t *testing.T) {
	t.Parallel()

	// Only wg-adguard's wg-easy service declares host_module_mount (and renders
	// the matching read-only /lib/modules mount); every other curated app mounts
	// none. The scan must be a clean pass on every real template — wg-adguard via
	// parity, the rest via the no-mount baseline — guarding both against a false
	// positive that would break every install and against a regression in
	// wg-adguard's real declaration.
	for _, app := range loadRealStableCatalogApps(t) {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			composeBytes := readRepoFile(t, app.ComposeTemplate)
			require.NoError(t,
				core.VerifyHostModuleMountMatchCatalogForTest(app, composeBytes),
				"the real stable catalog host_module_mount declarations must pass the scan for %s",
				app.AppID,
			)
		})
	}
}
