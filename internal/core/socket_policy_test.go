package core_test

// Docker-socket policy enforcement (PRD §12.1). Docker API access reaches an
// app only through a declared docker-socket-proxy sidecar on an --internal
// network; a direct /var/run/docker.sock bind is never permitted. This file
// proves the internal/core enforcement machinery against synthetic fixtures
// and the one curated app that declares socket_proxy (dockhand):
//
//   - the catalog socket_proxy declaration is validated — every allowed_api
//     flag must be in the recognized closed set, the named network must
//     reference a networks[] entry with internal:true, and the proxy service
//     must be image-pinned;
//   - the rendered compose is scanned for direct docker.sock binds in every
//     mount form (short string with/without mode, alternate socket paths, the
//     long mapping form, and a bare relative source), each refused;
//   - the declared, enabled proxy sidecar is the sole exemption — a non-proxy
//     service that mounts the socket, or the same mount when the proxy is
//     disabled, is refused;
//   - unrelated volumes (named volumes, ordinary host binds, a docker.sock.conf
//     file) raise no false positive, while an unclassifiable volume node fails
//     closed;
//   - the install confirmation's DOCKER SOCKET ACCESS WARNING block states
//     read-vs-read-and-control access and is omitted when no enabled proxy is
//     declared; and
//   - all nineteen curated apps pass the scan clean: dockhand is the first real
//     socket_proxy app and must pass the full A (declaration) / B (no direct
//     mount on a non-proxy service) / C (proxy attaches only to internal
//     networks) scan against its rendered template, while the other eighteen
//     declare no socket_proxy and mount no docker.sock.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
)

func socketApp(appID string, proxy *catalog.SocketProxy, networks []catalog.Network, extraPins ...catalog.ImagePin) catalog.App {
	app := appFixture(appID, 8080)
	app.SocketProxy = proxy
	app.Networks = networks
	app.ImagePins = append(app.ImagePins, extraPins...)
	return app
}

func TestVerifySocketPolicyMatchCatalog_CatalogDeclarations(t *testing.T) {
	t.Parallel()

	// A compose with no direct socket bind, so each case isolates a
	// catalog-declaration arm rather than the rendered-mount arm.
	cleanCompose := "services:\n  app:\n    image: docker.io/example/app:1.0.0\n"

	// A compose where the enabled proxy sidecar mounts the socket and attaches
	// only to the declared internal network, used by the fully-valid case so the
	// declaration arm (A) and both rendered arms (B direct-mount, C
	// network-attachment) pass together.
	validProxyCompose := `services:
  app:
    image: docker.io/example/app:1.0.0
  socket-proxy:
    image: docker.io/example/socket-proxy:1.0.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks:
      - socket-net
`

	proxyPin := catalog.ImagePin{Service: "socket-proxy", Image: "docker.io/example/socket-proxy", Tag: "1.0.0"}

	t.Run("allowed_api flag outside the closed set fails", func(t *testing.T) {
		t.Parallel()

		proxy := &catalog.SocketProxy{
			Enabled:    true,
			Service:    "socket-proxy",
			AllowedAPI: []string{"CONTAINERS", "EXEC"},
			Network:    "socket-net",
		}
		networks := []catalog.Network{{Name: "socket-net", Internal: true}}
		app := socketApp("socket-bad-api-app", proxy, networks, proxyPin)
		err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(cleanCompose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})

	t.Run("network declared but not internal fails", func(t *testing.T) {
		t.Parallel()

		proxy := &catalog.SocketProxy{
			Enabled:    true,
			Service:    "socket-proxy",
			AllowedAPI: []string{"CONTAINERS", "IMAGES"},
			Network:    "socket-net",
		}
		networks := []catalog.Network{{Name: "socket-net", Internal: false}}
		app := socketApp("socket-public-net-app", proxy, networks, proxyPin)
		err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(cleanCompose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})

	t.Run("network absent from the declaration fails", func(t *testing.T) {
		t.Parallel()

		proxy := &catalog.SocketProxy{
			Enabled:    true,
			Service:    "socket-proxy",
			AllowedAPI: []string{"CONTAINERS", "IMAGES"},
			Network:    "socket-net",
		}
		app := socketApp("socket-missing-net-app", proxy, nil, proxyPin)
		err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(cleanCompose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})

	t.Run("proxy service not image-pinned fails", func(t *testing.T) {
		t.Parallel()

		proxy := &catalog.SocketProxy{
			Enabled:    true,
			Service:    "socket-proxy",
			AllowedAPI: []string{"CONTAINERS", "IMAGES"},
			Network:    "socket-net",
		}
		networks := []catalog.Network{{Name: "socket-net", Internal: true}}
		app := socketApp("socket-unpinned-app", proxy, networks)
		err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(cleanCompose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})

	t.Run("fully valid declaration with a clean compose passes", func(t *testing.T) {
		t.Parallel()

		proxy := &catalog.SocketProxy{
			Enabled:    true,
			Service:    "socket-proxy",
			AllowedAPI: []string{"CONTAINERS", "IMAGES", "NETWORKS"},
			Network:    "socket-net",
		}
		networks := []catalog.Network{{Name: "socket-net", Internal: true}}
		app := socketApp("socket-valid-app", proxy, networks, proxyPin)
		require.NoError(t, core.VerifySocketPolicyMatchCatalogForTest(app, []byte(validProxyCompose)))
	})
}

func TestVerifySocketPolicyMatchCatalog_DirectMountRefused(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		compose string
	}{
		{
			name: "short form read-only",
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
`,
		},
		{
			name: "short form no mode",
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
`,
		},
		{
			name: "alternate run path source",
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - /run/docker.sock:/var/run/docker.sock
`,
		},
		{
			name: "long mapping form",
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - type: bind
        source: /var/run/docker.sock
        target: /var/run/docker.sock
`,
		},
		{
			name: "bare relative source",
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - docker.sock:/var/run/docker.sock
`,
		},
		{
			// An empty service name must not alias the "no exemption" zero
			// value: with no proxy declared, an empty-named service mounting
			// the socket is still refused (fail closed, not exempted).
			name: "empty-named service",
			compose: `services:
  "":
    image: docker.io/example/app:1.0.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := socketApp("socket-direct-mount-app", nil, nil)
			err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(tc.compose))
			require.Error(t, err)
			assertVerificationFailed(t, err)
		})
	}
}

func TestVerifySocketPolicyMatchCatalog_ProxyExemption(t *testing.T) {
	t.Parallel()

	proxyPin := catalog.ImagePin{Service: "socket-proxy", Image: "docker.io/example/socket-proxy", Tag: "1.0.0"}
	validNetworks := []catalog.Network{{Name: "socket-net", Internal: true}}

	enabledProxy := func() *catalog.SocketProxy {
		return &catalog.SocketProxy{
			Enabled:    true,
			Service:    "socket-proxy",
			AllowedAPI: []string{"CONTAINERS", "IMAGES"},
			Network:    "socket-net",
		}
	}

	t.Run("enabled proxy sidecar mounting the socket is exempt", func(t *testing.T) {
		t.Parallel()

		compose := `services:
  app:
    image: docker.io/example/app:1.0.0
  socket-proxy:
    image: docker.io/example/socket-proxy:1.0.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks:
      - socket-net
`
		app := socketApp("socket-exempt-app", enabledProxy(), validNetworks, proxyPin)
		require.NoError(t, core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose)))
	})

	t.Run("non-proxy service mounting the socket is refused", func(t *testing.T) {
		t.Parallel()

		compose := `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
  socket-proxy:
    image: docker.io/example/socket-proxy:1.0.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
`
		app := socketApp("socket-leak-app", enabledProxy(), validNetworks, proxyPin)
		err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})

	t.Run("disabled proxy mounting the socket is refused", func(t *testing.T) {
		t.Parallel()

		// Enabled:false grants no exemption, but the declaration shape is still
		// validated, so allowed_api / network / pin stay valid here to isolate
		// the rendered-mount failure arm.
		disabledProxy := enabledProxy()
		disabledProxy.Enabled = false
		compose := `services:
  app:
    image: docker.io/example/app:1.0.0
  socket-proxy:
    image: docker.io/example/socket-proxy:1.0.0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
`
		app := socketApp("socket-disabled-app", disabledProxy, validNetworks, proxyPin)
		err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})
}

func TestVerifySocketPolicyMatchCatalog_ProxyNetworkAttachment(t *testing.T) {
	t.Parallel()

	proxyPin := catalog.ImagePin{Service: "socket-proxy", Image: "docker.io/example/socket-proxy", Tag: "1.0.0"}

	enabledProxy := func() *catalog.SocketProxy {
		return &catalog.SocketProxy{
			Enabled:    true,
			Service:    "socket-proxy",
			AllowedAPI: []string{"CONTAINERS", "IMAGES"},
			Network:    "socket-net",
		}
	}

	// Two networks: the internal socket network the proxy belongs on, and a
	// non-internal front network it must never join.
	internalAndFront := []catalog.Network{
		{Name: "socket-net", Internal: true},
		{Name: "front-net", Internal: false},
	}
	internalOnly := []catalog.Network{{Name: "socket-net", Internal: true}}

	// The proxy legitimately mounts the socket, so check (B) passes and each
	// case isolates the network-attachment arm (check C).
	socketMount := "    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock:ro\n"

	t.Run("sequence form attaching only the internal network passes", func(t *testing.T) {
		t.Parallel()

		compose := "services:\n" +
			"  app:\n    image: docker.io/example/app:1.0.0\n" +
			"  socket-proxy:\n    image: docker.io/example/socket-proxy:1.0.0\n" +
			socketMount +
			"    networks:\n      - socket-net\n"
		app := socketApp("socket-net-seq-ok-app", enabledProxy(), internalOnly, proxyPin)
		require.NoError(t, core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose)))
	})

	t.Run("mapping form attaching only the internal network passes", func(t *testing.T) {
		t.Parallel()

		compose := "services:\n" +
			"  app:\n    image: docker.io/example/app:1.0.0\n" +
			"  socket-proxy:\n    image: docker.io/example/socket-proxy:1.0.0\n" +
			socketMount +
			"    networks:\n      socket-net:\n"
		app := socketApp("socket-net-map-ok-app", enabledProxy(), internalOnly, proxyPin)
		require.NoError(t, core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose)))
	})

	t.Run("sequence form also attaching a non-internal network is refused", func(t *testing.T) {
		t.Parallel()

		compose := "services:\n" +
			"  app:\n    image: docker.io/example/app:1.0.0\n" +
			"  socket-proxy:\n    image: docker.io/example/socket-proxy:1.0.0\n" +
			socketMount +
			"    networks:\n      - socket-net\n      - front-net\n"
		app := socketApp("socket-net-front-app", enabledProxy(), internalAndFront, proxyPin)
		err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})

	t.Run("mapping form also attaching a non-internal network is refused", func(t *testing.T) {
		t.Parallel()

		compose := "services:\n" +
			"  app:\n    image: docker.io/example/app:1.0.0\n" +
			"  socket-proxy:\n    image: docker.io/example/socket-proxy:1.0.0\n" +
			socketMount +
			"    networks:\n      socket-net:\n      front-net:\n"
		app := socketApp("socket-net-front-map-app", enabledProxy(), internalAndFront, proxyPin)
		err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})

	t.Run("attaching a network absent from the catalog is refused", func(t *testing.T) {
		t.Parallel()

		compose := "services:\n" +
			"  app:\n    image: docker.io/example/app:1.0.0\n" +
			"  socket-proxy:\n    image: docker.io/example/socket-proxy:1.0.0\n" +
			socketMount +
			"    networks:\n      - socket-net\n      - mystery-net\n"
		app := socketApp("socket-net-undeclared-app", enabledProxy(), internalOnly, proxyPin)
		err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})

	t.Run("proxy service with no networks block is refused", func(t *testing.T) {
		t.Parallel()

		compose := "services:\n" +
			"  app:\n    image: docker.io/example/app:1.0.0\n" +
			"  socket-proxy:\n    image: docker.io/example/socket-proxy:1.0.0\n" +
			socketMount
		app := socketApp("socket-net-none-app", enabledProxy(), internalOnly, proxyPin)
		err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose))
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})
}

func TestVerifySocketPolicyMatchCatalog_NonSocketVolumesPass(t *testing.T) {
	t.Parallel()

	// A named volume, an ordinary host bind, and a file whose basename is
	// docker.sock.conf (not docker.sock) must all pass without a false positive.
	compose := `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - data:/var/lib/app
      - /srv/app/config:/config:ro
      - /etc/docker.sock.conf:/c:ro
`
	app := socketApp("socket-clean-volumes-app", nil, nil)
	require.NoError(t, core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose)))
}

func TestVerifySocketPolicyMatchCatalog_VolumeFailClosed(t *testing.T) {
	t.Parallel()

	// A volumes entry that is neither a scalar nor a mapping but a nested
	// sequence drives the custom UnmarshalYAML default arm, which fails closed
	// on an unclassifiable volume node.
	compose := `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - - nested
        - seq
`
	app := socketApp("socket-unclassifiable-app", nil, nil)
	err := core.VerifySocketPolicyMatchCatalogForTest(app, []byte(compose))
	require.Error(t, err)
	assertVerificationFailed(t, err)
}

func TestSocketProxyWarning(t *testing.T) {
	t.Parallel()

	t.Run("read-only access surfaces a READ warning", func(t *testing.T) {
		t.Parallel()

		proxy := &catalog.SocketProxy{
			Enabled:    true,
			Service:    "socket-proxy",
			AllowedAPI: []string{"CONTAINERS", "IMAGES"},
			Network:    "socket-net",
		}
		app := socketApp("socket-warn-read-app", proxy, nil)
		lines := core.SocketProxyWarningLinesForTest(app)
		require.NotEmpty(t, lines)
		require.Equal(t, "", lines[0])
		require.Equal(t, "DOCKER SOCKET ACCESS WARNING", lines[1])

		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, "DOCKER SOCKET ACCESS WARNING")
		assert.Contains(t, joined, "READ")
		assert.NotContains(t, joined, "CONTROL")
	})

	t.Run("POST access surfaces a READ AND CONTROL warning", func(t *testing.T) {
		t.Parallel()

		proxy := &catalog.SocketProxy{
			Enabled:    true,
			Service:    "socket-proxy",
			AllowedAPI: []string{"CONTAINERS", "POST"},
			Network:    "socket-net",
		}
		app := socketApp("socket-warn-control-app", proxy, nil)
		joined := strings.Join(core.SocketProxyWarningLinesForTest(app), "\n")
		assert.Contains(t, joined, "READ AND CONTROL")
	})

	t.Run("app with no socket proxy emits no warning", func(t *testing.T) {
		t.Parallel()

		app := socketApp("socket-warn-none-app", nil, nil)
		require.Empty(t, core.SocketProxyWarningLinesForTest(app))
	})

	t.Run("disabled socket proxy emits no warning", func(t *testing.T) {
		t.Parallel()

		proxy := &catalog.SocketProxy{
			Enabled:    false,
			Service:    "socket-proxy",
			AllowedAPI: []string{"CONTAINERS", "IMAGES"},
			Network:    "socket-net",
		}
		app := socketApp("socket-warn-disabled-app", proxy, nil)
		require.Empty(t, core.SocketProxyWarningLinesForTest(app))
	})
}

func TestVerifySocketPolicyMatchCatalog_RealStableCatalogPassesClean(t *testing.T) {
	t.Parallel()

	// All nineteen curated apps pass the scan clean, guarding against a false
	// positive that would break every install. dockhand is the first curated app
	// to declare socket_proxy: its socket-proxy sidecar is the sole docker.sock
	// mount, attaches only to the --internal dockhand-socket network, and is
	// image-pinned, so it exercises checks A, B, and C against a real template
	// for real. authentik is the first curated app to drop an upstream
	// docker.sock mount (its worker's embedded-outpost socket is out of scope),
	// so this scan also covers a template whose upstream original carried one.
	// The remaining sixteen declare no socket_proxy and mount no docker.sock.
	for _, app := range loadRealStableCatalogApps(t) {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			composeBytes := readRepoFile(t, app.ComposeTemplate)
			require.NoError(t,
				core.VerifySocketPolicyMatchCatalogForTest(app, composeBytes),
				"the real stable catalog must pass the socket-policy scan for %s",
				app.AppID,
			)
		})
	}
}
