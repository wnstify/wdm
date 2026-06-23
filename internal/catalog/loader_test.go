package catalog_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
)

// validCatalogYAML is the minimal manifest that satisfies every
// required field in catalog/schema.json. Used as the baseline that
// negative cases mutate one field at a time. Two apps so any future
// "apps must be non-empty" regression in the schema fails this baseline
// test rather than silently being absorbed by a single-app fixture.
const validCatalogYAML = `
schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: Bitwarden-compatible password manager
    description: A self-hosted Bitwarden-compatible password vault
    template_name: vaultwarden
    template_version: "1.2.3"
    compose_template: vaultwarden/docker-compose.yaml
    env_template: vaultwarden/env.template
    placeholders:
      - name: DB_PASSWORD
        type: secret
        required: true
        encoding: base64url
    supported_versions:
      docker: ">=20.10"
      compose: ">=2.0"
    ports:
      - service: app
        container: 80
        host: 8080
        protocol: tcp
    image_pins:
      - service: app
        image: vaultwarden/server
        tag: "1.30.1"
    local_target_url_template: "http://127.0.0.1:8080/"
    pangolin_guidance:
      target_url: "http://127.0.0.1:8080"
      recommended_subdomain: vault
      notes:
        - Configure DNS pointing to your reverse proxy.
    first_run_notes:
      - Open the local URL and create the admin account.
    risk_classification: [database]
`

func TestLoadCatalogBytes_AcceptsMinimalValid(t *testing.T) {
	t.Parallel()

	cat, err := catalog.LoadCatalogBytes(t.Context(), []byte(validCatalogYAML))
	require.NoError(t, err)
	require.NotNil(t, cat)

	assert.Equal(t, 1, cat.SchemaVersion)
	assert.Equal(t, "stable", cat.Channel)
	assert.Equal(t,
		time.Date(2026, 5, 19, 9, 14, 33, 0, time.UTC),
		cat.GeneratedAt.UTC())
	require.Len(t, cat.Apps, 1)

	app := cat.Apps[0]
	assert.Equal(t, "vaultwarden", app.AppID)
	assert.Equal(t, "Vaultwarden", app.Name)
	assert.Equal(t, []string{"database"}, app.RiskClassification)
	assert.Equal(t, "http://127.0.0.1:8080/", app.LocalTargetURLTemplate)
	assert.Equal(t, "http://127.0.0.1:8080", app.PangolinGuidance.TargetURL)
	assert.Equal(t, "vault", app.PangolinGuidance.RecommendedSubdomain)
	assert.Equal(t,
		[]string{"Configure DNS pointing to your reverse proxy."},
		app.PangolinGuidance.Notes)
	assert.Equal(t,
		[]string{"Open the local URL and create the admin account."},
		app.FirstRunNotes)
	require.Len(t, app.Placeholders, 1)
	assert.Equal(t, "DB_PASSWORD", app.Placeholders[0].Name)
	require.Len(t, app.Ports, 1)
	assert.Equal(t, "tcp", app.Ports[0].Protocol)
	require.Len(t, app.ImagePins, 1)
	assert.Equal(t, "1.30.1", app.ImagePins[0].Tag)
}

// TestLoadCatalogBytes_AcceptsCompletedServices proves the schema admits
// the optional completed_services array (non-empty, unique string items)
// and that the loader decodes it onto App.CompletedServices. The field is
// additive and optional, so the baseline schema_version: 1 manifest with
// the field added still validates.
func TestLoadCatalogBytes_AcceptsCompletedServices(t *testing.T) {
	t.Parallel()

	withField := validCatalogYAML + `    completed_services:
      - mongo-init
      - garage-init
`

	cat, err := catalog.LoadCatalogBytes(t.Context(), []byte(withField))
	require.NoError(t, err)
	require.Len(t, cat.Apps, 1)
	assert.Equal(t,
		[]string{"mongo-init", "garage-init"},
		cat.Apps[0].CompletedServices)
}

func TestLoadCatalogBytes_AcceptsSecretEncodingMetadata(t *testing.T) {
	t.Parallel()

	cat, err := catalog.LoadCatalogBytes(t.Context(), []byte(`
schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: n8n
    name: n8n
    summary: workflow automation
    description: workflow automation
    template_name: n8n
    template_version: "1.2.3"
    compose_template: templates/n8n/docker-compose.yml.tmpl
    env_template: templates/n8n/.env.tmpl
    placeholders:
      - name: N8N_ENCRYPTION_KEY
        type: secret
        required: true
        encoding: base64url
      - name: N8N_RUNNERS_AUTH_TOKEN
        type: secret
        required: true
        encoding: hex
    supported_versions:
      docker: ">=20.10"
      compose: ">=2.0"
    ports: []
    image_pins: []
    pangolin_guidance: {}
    risk_classification: [database]
`))
	require.NoError(t, err)
	require.Len(t, cat.Apps, 1)
	require.Len(t, cat.Apps[0].Placeholders, 2)

	assert.Equal(t, "base64url", cat.Apps[0].Placeholders[0].Encoding)
	assert.Equal(t, "hex", cat.Apps[0].Placeholders[1].Encoding)
}

func TestLoadCatalogBytes_RejectsInvalidYAML(t *testing.T) {
	t.Parallel()

	_, err := catalog.LoadCatalogBytes(t.Context(), []byte("not: [valid: yaml"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, catalog.ErrCatalogInvalid))
}

func TestLoadCatalogBytes_RejectsEmptyPayload(t *testing.T) {
	t.Parallel()

	// Empty YAML parses to a nil map → schema rejects (required
	// fields missing).
	_, err := catalog.LoadCatalogBytes(t.Context(), []byte(""))
	require.Error(t, err)
	assert.True(t, errors.Is(err, catalog.ErrCatalogInvalid))
}

// TestLoadCatalogBytes_RejectsSchemaViolations covers exit
// criterion line 393 ("catalog/schema.json rejects a hand-crafted
// invalid catalog.yaml fixture"). Each case mutates one schema
// constraint at a time from the baseline so the failure attribution
// stays clean.
func TestLoadCatalogBytes_RejectsSchemaViolations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
	}{
		{
			"bad_schema_version",
			`schema_version: 3
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps: []
`,
		},
		{
			"bad_channel",
			`schema_version: 1
channel: verified
generated_at: "2026-05-19T09:14:33Z"
apps: []
`,
		},
		{
			"bad_generated_at_not_rfc3339",
			`schema_version: 1
channel: stable
generated_at: "not-a-date"
apps: []
`,
		},
		{
			"missing_required_apps_field",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
`,
		},
		{
			"additional_top_level_property",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps: []
unknown_field: value
`,
		},
		{
			"bad_app_id_pattern",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: Vaultwarden
    name: Bad
    summary: bad app_id case
    description: should fail
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
		{
			"pangolin_guidance_legacy_string_form",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: legacy guidance shape
    description: should fail
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    pangolin_guidance: free-form guidance string
    risk_classification: [database]
`,
		},
		{
			"bad_risk_classification",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    pangolin_guidance: {}
    risk_classification: [extreme]
`,
		},
		{
			"bad_port_out_of_range",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports:
      - service: app
        container: 0
        host: 8080
    image_pins: []
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
		{
			"bad_image_digest_format",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins:
      - service: app
        image: vaultwarden/server
        tag: "1.30.1"
        digest: "md5:not-a-sha256"
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
		{
			"bad_placeholder_name_lowercase",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders:
      - name: db_password
        type: secret
        required: true
        encoding: base64url
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
		{
			"bad_secret_encoding_value",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders:
      - name: DB_PASSWORD
        type: secret
        required: true
        encoding: rot13
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
		{
			"bad_secret_missing_encoding",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders:
      - name: DB_PASSWORD
        type: secret
        required: true
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
		{
			"bad_encoding_on_non_secret",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders:
      - name: SITE_NAME
        type: string
        required: true
        encoding: base64url
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
		{
			"sensitive_on_non_string",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders:
      - name: API_TOKEN
        type: secret
        required: true
        encoding: hex
        sensitive: true
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
		{
			"completed_services_non_string_item",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    completed_services: [42]
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
		{
			"completed_services_empty_string_item",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    completed_services: [""]
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
		{
			"completed_services_duplicate_item",
			`schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: ok
    description: ok
    template_name: vaultwarden
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    completed_services: [mongo-init, mongo-init]
    pangolin_guidance: {}
    risk_classification: [database]
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := catalog.LoadCatalogBytes(t.Context(), []byte(tc.yaml))
			require.Error(t, err)
			assert.True(t, errors.Is(err, catalog.ErrCatalogInvalid),
				"want wrapped ErrCatalogInvalid; got %v", err)
		})
	}
}

// TestLoadCatalogBytes_AcceptsSchemaVersion2WithNewFields proves the v2
// schema admits the new optional declaration fields: public-port intent
// and ranges, the per-service container-privilege allow-list, the
// docker-socket-proxy declaration, config generation, and network IPAM.
// It asserts only that validation passes and SchemaVersion is 2 — the
// new fields have no Go struct counterparts yet (a later commit adds
// the decode), so yaml.Unmarshal ignores them. Backward compatibility is
// covered by TestLoadCatalogBytes_AcceptsMinimalValid continuing to pass:
// its schema_version: 1 manifest still validates against the v2 schema.
func TestLoadCatalogBytes_AcceptsSchemaVersion2WithNewFields(t *testing.T) {
	t.Parallel()

	cat, err := catalog.LoadCatalogBytes(t.Context(), []byte(`
schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: wg-easy
    name: wg-easy
    summary: WireGuard VPN with a web UI
    description: A self-hosted WireGuard VPN server with a management UI
    template_name: wg-easy
    template_version: "1.2.3"
    compose_template: wg-easy/docker-compose.yaml
    env_template: wg-easy/env.template
    placeholders:
      - name: WG_PASSWORD
        type: secret
        required: true
        encoding: base64url
    supported_versions:
      docker: ">=20.10"
      compose: ">=2.0"
    ports:
      - service: wg
        container: 51820
        host: 51820
        protocol: udp
        public: true
      - service: media
        container: 50000
        host: 50000
        protocol: udp
        host_range: "50000-50100"
        container_range: "50000-50100"
    image_pins:
      - service: wg
        image: weejewel/wg-easy
        tag: "14"
      - service: proxy
        image: tecnativa/docker-socket-proxy
        tag: "0.2.0"
    networks:
      - name: wg-internal
        internal: true
        ipam:
          subnet: "10.8.0.0/24"
          gateway: "10.8.0.1"
          addresses:
            - service: wg
              ipv4_address: "10.8.0.2"
    service_hardening:
      - service: wg
        capabilities:
          add: [NET_ADMIN, NET_RAW]
        sysctls:
          - name: net.ipv4.ip_forward
            value: "1"
        ulimits:
          nofile:
            soft: 1024
            hard: 4096
        host_module_mount: true
    socket_proxy:
      enabled: true
      service: proxy
      allowed_api: [CONTAINERS, IMAGES, NETWORKS, POST]
      network: wg-internal
    config_generation:
      - template: wg-easy/wg0.conf.tmpl
        dest: config/wg0.conf
        mode: "0640"
        mount: "./config/wg0.conf:/etc/wireguard/wg0.conf:ro"
    pangolin_guidance: {}
    risk_classification: [complex]
`))
	require.NoError(t, err)
	require.NotNil(t, cat)
	assert.Equal(t, 2, cat.SchemaVersion)
}

// TestLoadCatalogBytes_DecodesSchemaV2Fields proves the version-2
// declaration fields decode into their Go counterparts, not just that
// validation passes (which TestLoadCatalogBytes_AcceptsSchemaVersion2WithNewFields
// already covers). It exercises public-port intent, port ranges, the
// per-service hardening block (capabilities, sysctls, ulimits, host
// module mount), the socket-proxy declaration including the POST
// write/control flag, a config-generation artifact, and network IPAM.
func TestLoadCatalogBytes_DecodesSchemaV2Fields(t *testing.T) {
	t.Parallel()

	cat, err := catalog.LoadCatalogBytes(t.Context(), []byte(`
schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: wg-easy
    name: wg-easy
    summary: WireGuard VPN with a web UI
    description: A self-hosted WireGuard VPN server with a management UI
    template_name: wg-easy
    template_version: "1.2.3"
    compose_template: wg-easy/docker-compose.yaml
    env_template: wg-easy/env.template
    placeholders:
      - name: WG_PASSWORD
        type: secret
        required: true
        encoding: base64url
    supported_versions:
      docker: ">=20.10"
      compose: ">=2.0"
    ports:
      - service: wg
        container: 51820
        host: 51820
        protocol: udp
        public: true
      - service: media
        container: 50000
        host: 50000
        protocol: udp
        host_range: "50000-50100"
        container_range: "50000-50100"
    image_pins:
      - service: wg
        image: weejewel/wg-easy
        tag: "14"
      - service: proxy
        image: tecnativa/docker-socket-proxy
        tag: "0.2.0"
    networks:
      - name: wg-internal
        internal: true
        ipam:
          subnet: "10.8.0.0/24"
          gateway: "10.8.0.1"
          addresses:
            - service: wg
              ipv4_address: "10.8.0.2"
    service_hardening:
      - service: wg
        capabilities:
          add: [NET_ADMIN, NET_RAW]
        sysctls:
          - name: net.ipv4.ip_forward
            value: "1"
        ulimits:
          nofile:
            soft: 1024
            hard: 4096
        host_module_mount: true
    socket_proxy:
      enabled: true
      service: proxy
      allowed_api: [CONTAINERS, IMAGES, NETWORKS, POST]
      network: wg-internal
    config_generation:
      - template: wg-easy/wg0.conf.tmpl
        dest: config/wg0.conf
        mode: "0640"
        mount: "./config/wg0.conf:/etc/wireguard/wg0.conf:ro"
    pangolin_guidance: {}
    risk_classification: [complex]
`))
	require.NoError(t, err)
	require.NotNil(t, cat)
	require.Len(t, cat.Apps, 1)
	app := cat.Apps[0]

	require.Len(t, app.Ports, 2)
	assert.True(t, app.Ports[0].Public)
	assert.Equal(t, "udp", app.Ports[0].Protocol)
	assert.Equal(t, "50000-50100", app.Ports[1].HostRange)
	assert.Equal(t, "50000-50100", app.Ports[1].ContainerRange)

	require.Len(t, app.ServiceHardening, 1)
	hardening := app.ServiceHardening[0]
	assert.Equal(t, "wg", hardening.Service)
	require.NotNil(t, hardening.Capabilities)
	assert.Equal(t, []string{"NET_ADMIN", "NET_RAW"}, hardening.Capabilities.Add)
	require.Len(t, hardening.Sysctls, 1)
	assert.Equal(t, "net.ipv4.ip_forward", hardening.Sysctls[0].Name)
	assert.Equal(t, "1", hardening.Sysctls[0].Value)
	require.NotNil(t, hardening.Ulimits)
	require.NotNil(t, hardening.Ulimits.Nofile)
	assert.Equal(t, 1024, hardening.Ulimits.Nofile.Soft)
	assert.Equal(t, 4096, hardening.Ulimits.Nofile.Hard)
	assert.True(t, hardening.HostModuleMount)

	require.NotNil(t, app.SocketProxy)
	assert.True(t, app.SocketProxy.Enabled)
	assert.Equal(t, "proxy", app.SocketProxy.Service)
	assert.Equal(t,
		[]string{"CONTAINERS", "IMAGES", "NETWORKS", "POST"},
		app.SocketProxy.AllowedAPI)
	assert.Contains(t, app.SocketProxy.AllowedAPI, "POST")
	assert.Equal(t, "wg-internal", app.SocketProxy.Network)

	require.Len(t, app.ConfigGeneration, 1)
	artifact := app.ConfigGeneration[0]
	assert.Equal(t, "wg-easy/wg0.conf.tmpl", artifact.Template)
	assert.Equal(t, "config/wg0.conf", artifact.Dest)
	assert.Equal(t, "0640", artifact.Mode)
	assert.Equal(t, "./config/wg0.conf:/etc/wireguard/wg0.conf:ro", artifact.Mount)

	require.Len(t, app.Networks, 1)
	require.NotNil(t, app.Networks[0].IPAM)
	ipam := app.Networks[0].IPAM
	assert.Equal(t, "10.8.0.0/24", ipam.Subnet)
	assert.Equal(t, "10.8.0.1", ipam.Gateway)
	require.Len(t, ipam.Addresses, 1)
	assert.Equal(t, "wg", ipam.Addresses[0].Service)
	assert.Equal(t, "10.8.0.2", ipam.Addresses[0].IPv4Address)
}

// TestLoadCatalogBytes_V1CatalogLeavesV2FieldsZero proves a version-1
// manifest still loads and fabricates no version-2 state: the new
// optional fields decode to their zero/nil values (PRD §30 — a
// version-1 catalog loads unchanged against the version-2 schema).
func TestLoadCatalogBytes_V1CatalogLeavesV2FieldsZero(t *testing.T) {
	t.Parallel()

	cat, err := catalog.LoadCatalogBytes(t.Context(), []byte(`
schema_version: 1
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: vaultwarden
    name: Vaultwarden
    summary: Bitwarden-compatible password manager
    description: A self-hosted Bitwarden-compatible password vault
    template_name: vaultwarden
    template_version: "1.2.3"
    compose_template: vaultwarden/docker-compose.yaml
    env_template: vaultwarden/env.template
    placeholders:
      - name: DB_PASSWORD
        type: secret
        required: true
        encoding: base64url
    supported_versions:
      docker: ">=20.10"
      compose: ">=2.0"
    ports:
      - service: app
        container: 80
        host: 8080
        protocol: tcp
    image_pins:
      - service: app
        image: vaultwarden/server
        tag: "1.30.1"
    networks:
      - name: vw-internal
        internal: true
    pangolin_guidance: {}
    risk_classification: [database]
`))
	require.NoError(t, err)
	require.NotNil(t, cat)
	assert.Equal(t, 1, cat.SchemaVersion)
	require.Len(t, cat.Apps, 1)
	app := cat.Apps[0]

	assert.Nil(t, app.ServiceHardening)
	assert.Nil(t, app.SocketProxy)
	assert.Nil(t, app.ConfigGeneration)

	require.Len(t, app.Ports, 1)
	assert.False(t, app.Ports[0].Public)
	assert.Empty(t, app.Ports[0].HostRange)
	assert.Empty(t, app.Ports[0].ContainerRange)

	require.Len(t, app.Networks, 1)
	assert.Nil(t, app.Networks[0].IPAM)
}

// TestLoadCatalogBytes_RejectsSchemaV2Violations covers the version-2
// declaration fields. Each case is a schema_version: 2 manifest that
// mutates one constraint at a time so failure attribution stays clean.
// The capability and sysctl cases are security-critical: they prove the
// PRD §12.2 allow-list stays closed (SYS_MODULE and SYS_ADMIN rejected,
// only the listed sysctls accepted) and that no device map slips through.
func TestLoadCatalogBytes_RejectsSchemaV2Violations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
	}{
		{
			"capability_sys_module",
			`schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: wg-easy
    name: wg-easy
    summary: ok
    description: ok
    template_name: wg-easy
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    service_hardening:
      - service: wg
        capabilities:
          add: [SYS_MODULE]
    pangolin_guidance: {}
    risk_classification: [complex]
`,
		},
		{
			"capability_unknown",
			`schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: wg-easy
    name: wg-easy
    summary: ok
    description: ok
    template_name: wg-easy
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    service_hardening:
      - service: wg
        capabilities:
          add: [SYS_ADMIN]
    pangolin_guidance: {}
    risk_classification: [complex]
`,
		},
		{
			"sysctl_not_in_allowlist",
			`schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: wg-easy
    name: wg-easy
    summary: ok
    description: ok
    template_name: wg-easy
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    service_hardening:
      - service: wg
        sysctls:
          - name: net.ipv4.conf.all.forwarding
            value: "1"
    pangolin_guidance: {}
    risk_classification: [complex]
`,
		},
		{
			"device_declared",
			`schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: wg-easy
    name: wg-easy
    summary: ok
    description: ok
    template_name: wg-easy
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    service_hardening:
      - service: wg
        devices: ["/dev/net/tun:/dev/net/tun"]
    pangolin_guidance: {}
    risk_classification: [complex]
`,
		},
		{
			"socket_proxy_unknown_permission",
			`schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: dockhand
    name: dockhand
    summary: ok
    description: ok
    template_name: dockhand
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    socket_proxy:
      enabled: true
      service: proxy
      allowed_api: [DELETE]
      network: dockhand-internal
    pangolin_guidance: {}
    risk_classification: [complex]
`,
		},
		{
			"socket_proxy_missing_network",
			`schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: dockhand
    name: dockhand
    summary: ok
    description: ok
    template_name: dockhand
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    socket_proxy:
      enabled: true
      service: proxy
      allowed_api: [CONTAINERS]
    pangolin_guidance: {}
    risk_classification: [complex]
`,
		},
		{
			"config_generation_traversal",
			`schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: wg-easy
    name: wg-easy
    summary: ok
    description: ok
    template_name: wg-easy
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    config_generation:
      - template: wg-easy/wg0.conf.tmpl
        dest: "../escape"
        mode: "0640"
    pangolin_guidance: {}
    risk_classification: [complex]
`,
		},
		{
			"service_hardening_additional_property",
			`schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: wg-easy
    name: wg-easy
    summary: ok
    description: ok
    template_name: wg-easy
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    service_hardening:
      - service: wg
        unknown_field: value
    pangolin_guidance: {}
    risk_classification: [complex]
`,
		},
		{
			"ipam_bad_subnet",
			`schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: wg-easy
    name: wg-easy
    summary: ok
    description: ok
    template_name: wg-easy
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports: []
    image_pins: []
    networks:
      - name: wg-internal
        internal: true
        ipam:
          subnet: "not-a-cidr"
    pangolin_guidance: {}
    risk_classification: [complex]
`,
		},
		{
			"port_range_without_pair",
			`schema_version: 2
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps:
  - app_id: wg-easy
    name: wg-easy
    summary: ok
    description: ok
    template_name: wg-easy
    template_version: "1.0.0"
    compose_template: x/compose.yaml
    env_template: x/env.template
    placeholders: []
    supported_versions: { docker: ">=20.10", compose: ">=2.0" }
    ports:
      - service: media
        container: 50000
        host: 50000
        host_range: "50000-50100"
    image_pins: []
    pangolin_guidance: {}
    risk_classification: [complex]
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := catalog.LoadCatalogBytes(t.Context(), []byte(tc.yaml))
			require.Error(t, err)
			assert.True(t, errors.Is(err, catalog.ErrCatalogInvalid),
				"want wrapped ErrCatalogInvalid; got %v", err)
		})
	}
}

func TestLoadCatalogBytes_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := catalog.LoadCatalogBytes(ctx, []byte(validCatalogYAML))
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

// TestLoadCatalogBytes_RejectsTopLevelList exercises the
// json.Unmarshal(jsonBytes, &rawMap) failure in step 1 of
// LoadCatalogBytes. The path was previously cataloged as
// "unreachable Category A" — top-level non-object YAML payloads
// (list, scalar) do reach this branch: yaml.Unmarshal into `any`
// succeeds (intermediate is a slice), json.Marshal succeeds (produces
// a JSON array), and json.Unmarshal of "[...]" into a `map[string]any`
// fails because a JSON array cannot decode into a Go map. The
// resulting error must wrap ErrCatalogInvalid via the "json
// normalize" sub-wrap.
func TestLoadCatalogBytes_RejectsTopLevelList(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
	}{
		{"top_level_list", "- one\n- two\n"},
		{"top_level_scalar", "42\n"},
		{"top_level_string", "just-a-string\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := catalog.LoadCatalogBytes(t.Context(), []byte(tc.yaml))
			require.Error(t, err)
			assert.True(t, errors.Is(err, catalog.ErrCatalogInvalid),
				"top-level non-object YAML must wrap ErrCatalogInvalid; got %v", err)
		})
	}
}

// TestLoadCatalog_HonorsCanceledContext covers the ctx.Err early
// return in the LoadCatalog wrapper (separate from LoadCatalogBytes'
// own ctx.Err check). Pre-canceled context with any valid absolute
// path must surface context.Canceled before any file I/O.
func TestLoadCatalog_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validCatalogYAML), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := catalog.LoadCatalog(ctx, path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

// TestLoadCatalog_DirectoryPathReturnsReadError covers the os.ReadFile
// failure wrap via the directory-as-file trick. Mirrors the same shape
// as TestLoadConfig_DirectoryPathReturnsReadError in internal/state —
// same defense-in-depth rationale: callers distinguish "no catalog"
// from "broken catalog" (verification failure)
// from "I/O problem" (permission hint), and a regression to the
// wrap layer must fail loud.
func TestLoadCatalog_DirectoryPathReturnsReadError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := catalog.LoadCatalog(t.Context(), dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading")
	assert.False(t, errors.Is(err, os.ErrNotExist),
		"directory-as-file error must not wrap os.ErrNotExist; got %v", err)
	assert.False(t, errors.Is(err, catalog.ErrCatalogInvalid),
		"directory-as-file error must not wrap ErrCatalogInvalid (read never reached parsing); got %v", err)
}

func TestLoadCatalog_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	_, err := catalog.LoadCatalog(t.Context(), "relative/catalog.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestLoadCatalog_RejectsEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := catalog.LoadCatalog(t.Context(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestLoadCatalog_MissingFileReturnsNotExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.yaml")

	_, err := catalog.LoadCatalog(t.Context(), path)
	require.Error(t, err)
	// from "broken catalog" via this errors.Is split, so the
	// missing-file path must NOT wrap ErrCatalogInvalid.
	assert.True(t, errors.Is(err, os.ErrNotExist))
	assert.False(t, errors.Is(err, catalog.ErrCatalogInvalid))
}

// TestLoadCatalog_AcceptsValidFile covers exit criterion
// line 392 ("catalog/schema.json accepts a hand-crafted valid
// catalog.yaml fixture") through the on-disk path.
func TestLoadCatalog_AcceptsValidFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validCatalogYAML), 0o600))

	cat, err := catalog.LoadCatalog(t.Context(), path)
	require.NoError(t, err)
	require.NotNil(t, cat)
	assert.Equal(t, 1, cat.SchemaVersion)
	assert.Equal(t, "stable", cat.Channel)
}

func TestLoadCatalog_RejectsInvalidFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`schema_version: 999
channel: stable
generated_at: "2026-05-19T09:14:33Z"
apps: []
`), 0o600))

	_, err := catalog.LoadCatalog(t.Context(), path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, catalog.ErrCatalogInvalid))
}
