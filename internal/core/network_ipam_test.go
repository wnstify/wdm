package core_test

// Network IPAM enforcement (PRD §9). Static per-service IP addressing is
// declared per network in the signed catalog; templates author the matching
// ipv4_address literally; internal/core validates the catalog declaration's
// octet bounds and subnet membership, then cross-checks the rendered compose so
// a tampered template can neither pin an unintended static IP nor drift away
// from a declared one. This file proves the internal/core enforcement machinery
// against synthetic fixtures (wg-adguard is the first curated app to declare
// IPAM — its vpn_net pins adguard and wg-easy):
//
//   - a catalog declaration with a bad CIDR, a gateway outside the subnet, an
//     address outside the subnet, or an address for an unknown service is
//     refused;
//   - a rendered compose whose ipv4_address matches the catalog declaration
//     passes; a missing, different, or extra static IP is refused;
//   - both Compose service-networks forms (the bare-name sequence and the
//     attachment mapping) classify identically, and an unclassifiable node fails
//     closed; and
//   - across the nineteen curated apps only wg-adguard declares IPAM (and pins
//     the matching ipv4_address for each service), so the scan is a clean pass
//     on every real template — wg-adguard via parity, the rest via the no-IPAM
//     no-op.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
)

// ipamApp builds a two-service fixture (app + worker) carrying one network with
// the supplied IPAM block, so the address-service-membership check has real
// services to resolve against.
func ipamApp(appID string, networkName string, ipam *catalog.NetworkIPAM) catalog.App {
	app := appFixture(appID, 8080)
	app.ImagePins = append(app.ImagePins, catalog.ImagePin{
		Service: "worker",
		Image:   "docker.io/example/worker",
		Tag:     "1.0.0",
	})
	app.Networks = []catalog.Network{{Name: networkName, Internal: false, IPAM: ipam}}
	return app
}

func TestVerifyNetworkIPAMMatchCatalog_CatalogDeclarationRefusals(t *testing.T) {
	t.Parallel()

	// A clean compose, so each case isolates a catalog-declaration arm.
	cleanCompose := "services:\n  app:\n    image: docker.io/example/app:1.0.0\n"

	cases := []struct {
		name string
		ipam *catalog.NetworkIPAM
	}{
		{
			name: "invalid subnet CIDR",
			ipam: &catalog.NetworkIPAM{Subnet: "10.8.0.0"},
		},
		{
			name: "subnet octet out of range",
			ipam: &catalog.NetworkIPAM{Subnet: "10.8.300.0/24"},
		},
		{
			name: "gateway outside subnet",
			ipam: &catalog.NetworkIPAM{Subnet: "10.8.0.0/24", Gateway: "10.9.0.1"},
		},
		{
			name: "gateway not an IPv4",
			ipam: &catalog.NetworkIPAM{Subnet: "10.8.0.0/24", Gateway: "nope"},
		},
		{
			name: "address outside subnet",
			ipam: &catalog.NetworkIPAM{
				Subnet:    "10.8.0.0/24",
				Addresses: []catalog.IPAMAddress{{Service: "app", IPv4Address: "10.9.0.2"}},
			},
		},
		{
			name: "address octet out of range",
			ipam: &catalog.NetworkIPAM{
				Subnet:    "10.8.0.0/24",
				Addresses: []catalog.IPAMAddress{{Service: "app", IPv4Address: "10.8.0.300"}},
			},
		},
		{
			name: "address for unknown service",
			ipam: &catalog.NetworkIPAM{
				Subnet:    "10.8.0.0/24",
				Addresses: []catalog.IPAMAddress{{Service: "ghost", IPv4Address: "10.8.0.2"}},
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := ipamApp("ipam-bad", "wgnet", tt.ipam)
			err := core.VerifyNetworkIPAMMatchCatalogForTest(app, []byte(cleanCompose))
			assertVerificationFailed(t, err)
		})
	}
}

func TestVerifyNetworkIPAMMatchCatalog_RenderedParityPasses(t *testing.T) {
	t.Parallel()

	app := ipamApp("ipam-ok", "wgnet", &catalog.NetworkIPAM{
		Subnet:  "10.8.0.0/24",
		Gateway: "10.8.0.1",
		Addresses: []catalog.IPAMAddress{
			{Service: "app", IPv4Address: "10.8.0.2"},
			{Service: "worker", IPv4Address: "10.8.0.3"},
		},
	})

	compose := `services:
  app:
    image: docker.io/example/app:1.0.0
    networks:
      wgnet:
        ipv4_address: 10.8.0.2
  worker:
    image: docker.io/example/worker:1.0.0
    networks:
      wgnet:
        ipv4_address: 10.8.0.3
`

	require.NoError(t, core.VerifyNetworkIPAMMatchCatalogForTest(app, []byte(compose)))
}

func TestVerifyNetworkIPAMMatchCatalog_RenderedParityRefusals(t *testing.T) {
	t.Parallel()

	app := ipamApp("ipam-parity", "wgnet", &catalog.NetworkIPAM{
		Subnet:    "10.8.0.0/24",
		Addresses: []catalog.IPAMAddress{{Service: "app", IPv4Address: "10.8.0.2"}},
	})

	cases := []struct {
		name    string
		compose string
	}{
		{
			name: "declared address missing in rendered compose",
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    networks:
      - wgnet
`,
		},
		{
			name: "declared address different in rendered compose",
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    networks:
      wgnet:
        ipv4_address: 10.8.0.99
`,
		},
		{
			name: "declared address on a different network in rendered compose",
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    networks:
      othernet:
        ipv4_address: 10.8.0.2
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

			err := core.VerifyNetworkIPAMMatchCatalogForTest(app, []byte(tt.compose))
			assertVerificationFailed(t, err)
		})
	}
}

func TestVerifyNetworkIPAMMatchCatalog_UndeclaredStaticIPRefused(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		ipam    *catalog.NetworkIPAM
		compose string
	}{
		{
			name: "static IP with no catalog IPAM at all",
			ipam: nil,
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    networks:
      wgnet:
        ipv4_address: 10.8.0.2
`,
		},
		{
			name: "static IP for a service the catalog IPAM does not address",
			ipam: &catalog.NetworkIPAM{
				Subnet:    "10.8.0.0/24",
				Addresses: []catalog.IPAMAddress{{Service: "app", IPv4Address: "10.8.0.2"}},
			},
			compose: `services:
  app:
    image: docker.io/example/app:1.0.0
    networks:
      wgnet:
        ipv4_address: 10.8.0.2
  worker:
    image: docker.io/example/worker:1.0.0
    networks:
      wgnet:
        ipv4_address: 10.8.0.3
`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := ipamApp("ipam-bound", "wgnet", tt.ipam)
			err := core.VerifyNetworkIPAMMatchCatalogForTest(app, []byte(tt.compose))
			assertVerificationFailed(t, err)
		})
	}
}

func TestVerifyNetworkIPAMMatchCatalog_SequenceFormWithoutStaticIPPasses(t *testing.T) {
	t.Parallel()

	// No catalog IPAM and the bare-name sequence attachment form: nothing is
	// pinned, so the scan is a clean no-op.
	app := ipamApp("ipam-seq", "wgnet", nil)
	compose := `services:
  app:
    image: docker.io/example/app:1.0.0
    networks:
      - wgnet
  worker:
    image: docker.io/example/worker:1.0.0
    networks:
      - wgnet
`

	require.NoError(t, core.VerifyNetworkIPAMMatchCatalogForTest(app, []byte(compose)))
}

func TestVerifyNetworkIPAMMatchCatalog_UnclassifiableNetworksNodeFailsClosed(t *testing.T) {
	t.Parallel()

	app := ipamApp("ipam-bad-node", "wgnet", &catalog.NetworkIPAM{
		Subnet:    "10.8.0.0/24",
		Addresses: []catalog.IPAMAddress{{Service: "app", IPv4Address: "10.8.0.2"}},
	})

	// A scalar where Compose expects a sequence or mapping for service networks
	// is unclassifiable and must fail closed.
	compose := `services:
  app:
    image: docker.io/example/app:1.0.0
    networks: wgnet
`

	err := core.VerifyNetworkIPAMMatchCatalogForTest(app, []byte(compose))
	assertVerificationFailed(t, err)
}

func TestVerifyNetworkIPAMMatchCatalog_RealStableCatalogPassesClean(t *testing.T) {
	t.Parallel()

	// Only wg-adguard declares network IPAM (and renders the matching static
	// ipv4_address for each service); every other curated app declares none. The
	// scan must be a clean pass on every real template — wg-adguard via parity,
	// the rest via the no-IPAM no-op — guarding both against a false positive that
	// would break every install and against a regression in wg-adguard's real
	// declaration.
	for _, app := range loadRealStableCatalogApps(t) {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			composeBytes := readRepoFile(t, app.ComposeTemplate)
			require.NoError(t,
				core.VerifyNetworkIPAMMatchCatalogForTest(app, composeBytes),
				"the real stable catalog network IPAM declarations must pass the scan for %s",
				app.AppID,
			)
		})
	}
}
