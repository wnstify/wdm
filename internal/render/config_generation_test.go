package render_test

// Tests for the config_generation render path (PRD §17): the renderer
// that turns catalog config_generation declarations into rendered
// artifacts. This path mirrors the additional_files machinery
// (in-memory template content, mount verification against the parsed
// Compose volumes) and adds an in-render dest traversal guard that
// fails closed above the catalog schema (PRD §12/§13).
//
// The synthetic-golden tests (meshcentral-shaped, stoat-shaped) pin the
// byte-exact rendered artifacts against committed goldens under
// testdata/config_generation/. Per the golden_test.go value-pinning
// philosophy every placeholder value is an obviously-fake pinned
// constant ("TESTONLY-FAKE-…") so no fixture looks like a real secret
// and a secret scan over the goldens stays clean.
//
// Regeneration:
//
//	go test ./internal/render -run TestConfigGenerationGolden -update
//
// rewrites the goldens from current render output. Always eyeball the
// diff before committing, then prove stability with a plain (no
// -update) run. update and assertGolden are declared in golden_test.go
// (same package); this file reuses them.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/render"
)

// configArtifactComposeMounted is a minimal Compose template whose
// single service declares the bind mount the config-artifact tests
// reference, so mount verification has a matching volume to find.
const configArtifactComposeMounted = `name: wdm-test
services:
  app:
    image: nginx:1.27
    volumes:
      - ./config.json:/app/config.json:ro
`

// TestRenderLabels_ConfigArtifactDestTraversal exercises the in-render
// dest guard through the public RenderLabels entry point: unsafe dests
// must fail closed (wrapping ErrConfigArtifactDestUnsafe), safe
// stack-relative dests must pass.
func TestRenderLabels_ConfigArtifactDestTraversal(t *testing.T) {
	t.Parallel()

	const composeNoMount = `name: wdm-test
services:
  app:
    image: nginx:1.27
`

	unsafe := []struct {
		name string
		dest string
	}{
		{"absolute", "/etc/passwd"},
		{"parent", "../escape"},
		{"embedded-parent", "a/../../b"},
		{"dotdot", ".."},
		{"empty", ""},
		{"current-dir", "."},
		{"trailing-slash", "foo/"},
	}
	for _, tc := range unsafe {
		t.Run("unsafe/"+tc.name, func(t *testing.T) {
			t.Parallel()

			input := render.Input{
				ComposeTemplate: composeNoMount,
				AppID:           "test-app",
				ConfigGeneration: []render.ConfigArtifact{
					{Dest: tc.dest, Mode: "0640", Template: "ok\n"},
				},
			}

			_, err := render.RenderLabels(input)
			require.Error(t, err, "dest %q must be rejected", tc.dest)
			assert.ErrorIs(t, err, render.ErrConfigArtifactDestUnsafe)
		})
	}

	safe := []string{"config.json", "conf/app.toml", "a/b/c.yml"}
	for _, dest := range safe {
		t.Run("safe/"+dest, func(t *testing.T) {
			t.Parallel()

			input := render.Input{
				ComposeTemplate: composeNoMount,
				AppID:           "test-app",
				ConfigGeneration: []render.ConfigArtifact{
					{Dest: dest, Mode: "0640", Template: "ok\n"},
				},
			}

			stack, err := render.RenderLabels(input)
			require.NoError(t, err, "dest %q must be accepted", dest)
			require.Len(t, stack.ConfigArtifacts, 1)
			assert.Equal(t, dest, stack.ConfigArtifacts[0].Dest)
		})
	}
}

// TestRenderLabels_ConfigArtifactReservedNamesPassRenderGuard pins the
// render/writer boundary contract: configArtifactDestSafe is a path-SHAPE
// gate only, so a dest that collides with a wdm-reserved filename
// (.env, docker-compose.yml, .wdm.lock) is accepted here. Rejecting those
// — and any cross-artifact dest collision — is the install/update
// writer's job, not render's. This test fails loudly if a future change
// moves reserved-name defense into render (the wrong layer) and lets it
// be silently dropped from the writer.
func TestRenderLabels_ConfigArtifactReservedNamesPassRenderGuard(t *testing.T) {
	t.Parallel()

	const composeNoMount = `name: wdm-test
services:
  app:
    image: nginx:1.27
`
	for _, dest := range []string{".env", "docker-compose.yml", ".wdm.lock"} {
		t.Run(dest, func(t *testing.T) {
			t.Parallel()

			input := render.Input{
				ComposeTemplate: composeNoMount,
				AppID:           "test-app",
				ConfigGeneration: []render.ConfigArtifact{
					{Dest: dest, Mode: "0640", Template: "x\n"},
				},
			}

			stack, err := render.RenderLabels(input)
			require.NoError(t, err, "render is a shape-only guard; reserved-name rejection is the writer's job")
			require.Len(t, stack.ConfigArtifacts, 1)
			assert.Equal(t, dest, stack.ConfigArtifacts[0].Dest)
		})
	}
}

func TestRenderLabels_ConfigArtifactMountMissing(t *testing.T) {
	t.Parallel()

	const composeOtherMount = `name: wdm-test
services:
  app:
    image: nginx:1.27
    volumes:
      - ./other.json:/app/other.json:ro
`

	input := render.Input{
		ComposeTemplate: composeOtherMount,
		AppID:           "test-app",
		ConfigGeneration: []render.ConfigArtifact{
			{
				Dest:     "config.json",
				Mode:     "0640",
				Mount:    "./config.json:/app/config.json:ro",
				Template: "{}\n",
			},
		},
	}

	_, err := render.RenderLabels(input)
	require.Error(t, err)
	assert.ErrorIs(t, err, render.ErrConfigArtifactMountMissing)
}

func TestRenderLabels_ConfigArtifactMountPresent(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: configArtifactComposeMounted,
		AppID:           "test-app",
		ConfigGeneration: []render.ConfigArtifact{
			{
				Dest:     "config.json",
				Mode:     "0640",
				Mount:    "./config.json:/app/config.json:ro",
				Template: "{}\n",
			},
		},
	}

	stack, err := render.RenderLabels(input)
	require.NoError(t, err)
	require.Len(t, stack.ConfigArtifacts, 1)
	assert.Equal(t, "{}\n", string(stack.ConfigArtifacts[0].Bytes))
}

// TestRenderLabels_ConfigArtifactMissingKeyRedacts forces an
// execute-time missing-key error and asserts the error names the
// missing key but never echoes a resolved value from Values — the
// sentinel secret must not leak into the error text.
func TestRenderLabels_ConfigArtifactMissingKeyRedacts(t *testing.T) {
	t.Parallel()

	const sentinel = "SUPER-SECRET-DO-NOT-LEAK"

	input := render.Input{
		ComposeTemplate: configArtifactComposeMounted,
		AppID:           "test-app",
		Placeholders: []render.Placeholder{
			{Name: "API_KEY", Type: render.TypeSecret, Required: true},
		},
		Values: map[string]string{
			"API_KEY": sentinel,
		},
		ConfigGeneration: []render.ConfigArtifact{
			{
				Dest:     "config.json",
				Mode:     "0640",
				Mount:    "./config.json:/app/config.json:ro",
				Template: "key={{ .UNDECLARED_IN_VALUES }}\n",
			},
		},
	}

	_, err := render.RenderLabels(input)
	require.Error(t, err)
	assert.ErrorIs(t, err, render.ErrConfigArtifactTemplateExecute)
	assert.Contains(t, err.Error(), "UNDECLARED_IN_VALUES", "error should name the missing key")
	assert.Falsef(t, strings.Contains(err.Error(), sentinel),
		"error leaked the secret value: %q", err.Error())
}

func TestRenderLabels_ConfigArtifactMalformedTemplate(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: configArtifactComposeMounted,
		AppID:           "test-app",
		ConfigGeneration: []render.ConfigArtifact{
			{
				Dest:     "config.json",
				Mode:     "0640",
				Mount:    "./config.json:/app/config.json:ro",
				Template: "key={{ .UNCLOSED\n",
			},
		},
	}

	_, err := render.RenderLabels(input)
	require.Error(t, err)
	assert.ErrorIs(t, err, render.ErrConfigArtifactTemplateParse)
}

// TestRenderLabels_ConfigArtifactDeterministic renders the same Input
// twice and asserts the rendered artifact bytes are byte-identical.
func TestRenderLabels_ConfigArtifactDeterministic(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: configArtifactComposeMounted,
		AppID:           "test-app",
		Placeholders: []render.Placeholder{
			{Name: "DOMAIN", Type: render.TypeDomain, Required: true},
			{Name: "TOKEN", Type: render.TypeSecret, Required: true},
		},
		Values: map[string]string{
			"DOMAIN": "app.test.example",
			"TOKEN":  "TESTONLY-FAKE-SECRET-TOKEN",
		},
		ConfigGeneration: []render.ConfigArtifact{
			{
				Dest:     "config.json",
				Mode:     "0640",
				Mount:    "./config.json:/app/config.json:ro",
				Template: "{\n  \"domain\": \"{{ .DOMAIN }}\",\n  \"token\": \"{{ .TOKEN }}\"\n}\n",
			},
		},
	}

	first, err := render.RenderLabels(input)
	require.NoError(t, err)
	second, err := render.RenderLabels(input)
	require.NoError(t, err)

	require.Len(t, first.ConfigArtifacts, 1)
	require.Len(t, second.ConfigArtifacts, 1)
	assert.Equal(t, string(first.ConfigArtifacts[0].Bytes), string(second.ConfigArtifacts[0].Bytes),
		"config artifact render is not deterministic between runs")
}

// TestRenderLabels_ConfigArtifactEmptyIsNoOp confirms an Input with no
// config_generation leaves ConfigArtifacts nil and ComposeBytes
// unchanged, so the curated apps are unaffected by this path.
func TestRenderLabels_ConfigArtifactEmptyIsNoOp(t *testing.T) {
	t.Parallel()

	base := render.Input{
		ComposeTemplate: configArtifactComposeMounted,
		AppID:           "test-app",
	}

	withNil := base
	withNil.ConfigGeneration = nil

	stack, err := render.RenderLabels(withNil)
	require.NoError(t, err)
	assert.Nil(t, stack.ConfigArtifacts, "nil ConfigGeneration must yield nil ConfigArtifacts")

	reference, err := render.RenderLabels(base)
	require.NoError(t, err)
	assert.Equal(t, string(reference.ComposeBytes), string(stack.ComposeBytes),
		"config_generation must not alter ComposeBytes")
}

// TestConfigGenerationGoldenMeshcentralShaped pins a single-artifact
// config.json that references a domain and a secret, mounted into a
// service whose synthetic Compose declares the matching volume.
func TestConfigGenerationGoldenMeshcentralShaped(t *testing.T) {
	t.Parallel()

	const compose = `name: wdm-meshcentral
services:
  meshcentral:
    image: ghcr.io/ylianst/meshcentral:1.1.21
    volumes:
      - ./config.json:/opt/meshcentral/meshcentral-data/config.json:ro
`
	const configTemplate = `{
  "settings": {
    "cert": "{{ .DOMAIN }}",
    "port": 4430
  },
  "domains": {
    "": {
      "title": "MeshCentral",
      "loginKey": "{{ .LOGIN_KEY }}"
    }
  }
}
`

	input := render.Input{
		ComposeTemplate: compose,
		AppID:           "meshcentral",
		Placeholders: []render.Placeholder{
			{Name: "DOMAIN", Type: render.TypeDomain, Required: true},
			{Name: "LOGIN_KEY", Type: render.TypeSecret, Required: true},
		},
		Values: map[string]string{
			"DOMAIN":    "mesh.test.example",
			"LOGIN_KEY": "TESTONLY-FAKE-SECRET-LOGIN-KEY-0000000000000000000000000000",
		},
		ConfigGeneration: []render.ConfigArtifact{
			{
				Dest:     "config.json",
				Mode:     "0640",
				Mount:    "./config.json:/opt/meshcentral/meshcentral-data/config.json:ro",
				Template: configTemplate,
			},
		},
	}

	stack, err := render.RenderLabels(input)
	require.NoError(t, err)
	require.Len(t, stack.ConfigArtifacts, 1)

	assertGolden(
		t,
		"testdata/config_generation/meshcentral-shaped/config.json",
		stack.ConfigArtifacts[0].Bytes,
	)
}

// TestConfigGenerationGoldenStoatShaped pins a multi-artifact app:
// several config files plus a split env file, each templated and mounted
// into the matching service volume.
func TestConfigGenerationGoldenStoatShaped(t *testing.T) {
	t.Parallel()

	const compose = `name: wdm-stoat
services:
  livekit:
    image: livekit/livekit-server:v1.8.0
    volumes:
      - ./livekit.yml:/etc/livekit.yml:ro
  api:
    image: ghcr.io/revoltchat/server:latest
    volumes:
      - ./Revolt.toml:/Revolt.toml:ro
      - ./api.env:/app/api.env:ro
  garage:
    image: dxflrs/garage:v1.0.1
    volumes:
      - ./garage.toml:/etc/garage.toml:ro
  caddy:
    image: caddy:2.8
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
`

	input := render.Input{
		ComposeTemplate: compose,
		AppID:           "stoat",
		Placeholders: []render.Placeholder{
			{Name: "DOMAIN", Type: render.TypeDomain, Required: true},
			{Name: "LIVEKIT_API_KEY", Type: render.TypeSecret, Required: true},
			{Name: "LIVEKIT_API_SECRET", Type: render.TypeSecret, Required: true},
			{Name: "REVOLT_API_SECRET", Type: render.TypeSecret, Required: true},
			{Name: "GARAGE_RPC_SECRET", Type: render.TypeSecret, Required: true},
			{Name: "GARAGE_ADMIN_TOKEN", Type: render.TypeSecret, Required: true},
		},
		Values: map[string]string{
			"DOMAIN":             "stoat.test.example",
			"LIVEKIT_API_KEY":    "TESTONLY-FAKE-LIVEKIT-API-KEY",
			"LIVEKIT_API_SECRET": "TESTONLY-FAKE-LIVEKIT-API-SECRET-0000000000000000000000000000",
			"REVOLT_API_SECRET":  "TESTONLY-FAKE-REVOLT-API-SECRET-0000000000000000000000000000",
			"GARAGE_RPC_SECRET":  "TESTONLY-FAKE-GARAGE-RPC-SECRET-0000000000000000000000000000",
			"GARAGE_ADMIN_TOKEN": "TESTONLY-FAKE-GARAGE-ADMIN-TOKEN-0000000000000000000000000000",
		},
		ConfigGeneration: []render.ConfigArtifact{
			{
				Dest:     "livekit.yml",
				Mode:     "0640",
				Mount:    "./livekit.yml:/etc/livekit.yml:ro",
				Template: "port: 7880\nkeys:\n  {{ .LIVEKIT_API_KEY }}: {{ .LIVEKIT_API_SECRET }}\n",
			},
			{
				Dest:     "Revolt.toml",
				Mode:     "0640",
				Mount:    "./Revolt.toml:/Revolt.toml:ro",
				Template: "[api]\nhost = \"https://{{ .DOMAIN }}\"\nsecret = \"{{ .REVOLT_API_SECRET }}\"\n",
			},
			{
				Dest:     "garage.toml",
				Mode:     "0640",
				Mount:    "./garage.toml:/etc/garage.toml:ro",
				Template: "rpc_secret = \"{{ .GARAGE_RPC_SECRET }}\"\n\n[admin]\nadmin_token = \"{{ .GARAGE_ADMIN_TOKEN }}\"\n",
			},
			{
				Dest:     "Caddyfile",
				Mode:     "0640",
				Mount:    "./Caddyfile:/etc/caddy/Caddyfile:ro",
				Template: "{{ .DOMAIN }} {\n  reverse_proxy api:8000\n}\n",
			},
			{
				Dest:     "api.env",
				Mode:     "0640",
				Mount:    "./api.env:/app/api.env:ro",
				Template: "REVOLT_PUBLIC_URL=https://{{ .DOMAIN }}\nREVOLT_API_SECRET={{ .REVOLT_API_SECRET }}\n",
			},
		},
	}

	stack, err := render.RenderLabels(input)
	require.NoError(t, err)
	require.Len(t, stack.ConfigArtifacts, 5)

	for _, art := range stack.ConfigArtifacts {
		assertGolden(
			t,
			"testdata/config_generation/stoat-shaped/"+art.Dest,
			art.Bytes,
		)
	}
}
