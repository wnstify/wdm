package render_test

import (
	"errors"
	"testing"

	"github.com/wnstify/wdm/internal/render"
)

// representativeComposeBody mirrors the common action-free Compose
// template shape: shell-style ${VAR} references (Compose's runtime
// --env-file substitution per the confirmation rules) and zero
// Go-template {{... }} actions. The byte-identity test asserts
// text/template passes this through unchanged.
const representativeComposeBody = `name: wdm-uptime-kuma
services:
  app:
    image: louislam/uptime-kuma:1
    container_name: wdm-uptime-kuma-app
    environment:
      MARIADB_ROOT_PASSWORD: ${MARIADB_ROOT_PASSWORD}
      MARIADB_PASSWORD: ${UPTIME_KUMA_DB_PASSWORD}
    labels:
      wdm.managed: "true"
      wdm.app: "uptime-kuma"
`

func TestRenderCompose_Success(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: "image: nginx:{{ .NGINX_VERSION }}\nport: \"{{ .PORT }}\"\n",
		Placeholders: []render.Placeholder{
			{Name: "NGINX_VERSION", Type: render.TypeString, Required: true},
			{Name: "PORT", Type: render.TypePort, Required: true},
		},
		Values: map[string]string{
			"NGINX_VERSION": "1.27",
			"PORT":          "8080",
		},
	}

	stack, err := render.RenderCompose(input)
	if err != nil {
		t.Fatalf("RenderCompose returned unexpected error: %v", err)
	}

	const want = "image: nginx:1.27\nport: \"8080\"\n"
	if got := string(stack.ComposeBytes); got != want {
		t.Errorf("ComposeBytes:\n got: %q\nwant: %q", got, want)
	}
}

// TestRenderCompose_NoActionTemplateByteIdentical pins the
// deterministic-output guarantee for action-free Compose bodies:
// text/template Execute emits the source bytes contiguously and
// the rendered output equals the input byte-for-byte. Acts as a
// regression guard for any future change that would make render
// non-deterministic on action-free bodies (a normalizer pass, a
// trailing-newline trim, etc.).
func TestRenderCompose_NoActionTemplateByteIdentical(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: representativeComposeBody,
		Placeholders:    nil,
		Values:          nil,
	}

	stack, err := render.RenderCompose(input)
	if err != nil {
		t.Fatalf("RenderCompose returned unexpected error: %v", err)
	}

	if got := string(stack.ComposeBytes); got != representativeComposeBody {
		t.Errorf("ComposeBytes does not match input byte-for-byte:\n got: %q\nwant: %q", got, representativeComposeBody)
	}
}

func TestRenderCompose_OnlyComposeBytesPopulated(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: representativeComposeBody,
		Placeholders:    nil,
		Values:          nil,
	}

	stack, err := render.RenderCompose(input)
	if err != nil {
		t.Fatalf("RenderCompose returned unexpected error: %v", err)
	}

	if len(stack.ComposeBytes) == 0 {
		t.Error("ComposeBytes is empty; want rendered content")
	}
	if stack.EnvBytes != nil {
		t.Errorf("EnvBytes = %q, want nil (RenderEnv populates this; RenderCompose must not)", stack.EnvBytes)
	}
	if stack.AdditionalFiles != nil {
		t.Errorf("AdditionalFiles = %v, want nil (RenderLabels handles sidecar files)", stack.AdditionalFiles)
	}
	if stack.LockManifest != nil {
		t.Errorf("LockManifest = %v, want nil (later wiring populates this)", stack.LockManifest)
	}
	if stack.ServiceLabels != nil {
		t.Errorf("ServiceLabels = %v, want nil (later wiring populates this)", stack.ServiceLabels)
	}
}

// TestRenderCompose_MissingKey forces a text/template execute
// failure by referencing an UNDECLARED variable from the template
// body — the validators do not see template bodies, so the failure
// surfaces at template Execute time as ErrComposeTemplateExecute
// (Option("missingkey=error") makes the miss fatal rather than
// emitting "<no value>").
func TestRenderCompose_MissingKey(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: "image: nginx:{{ .DECLARED }}\nport: \"{{ .UNDECLARED_IN_VALUES }}\"\n",
		Placeholders: []render.Placeholder{
			{Name: "DECLARED", Type: render.TypeString, Required: true},
		},
		Values: map[string]string{
			"DECLARED": "1.27",
		},
	}

	_, err := render.RenderCompose(input)
	if err == nil {
		t.Fatal("RenderCompose returned nil, want error wrapping ErrComposeTemplateExecute")
	}
	if !errors.Is(err, render.ErrComposeTemplateExecute) {
		t.Errorf("RenderCompose returned %v, want errors.Is(_, ErrComposeTemplateExecute)", err)
	}
}

func TestRenderCompose_MalformedTemplate(t *testing.T) {
	t.Parallel()

	input := render.Input{
		// {{ unclosed action — text/template parse rejects this.
		ComposeTemplate: "image: nginx:{{ .UNCLOSED\n",
		Placeholders:    nil,
		Values:          nil,
	}

	_, err := render.RenderCompose(input)
	if err == nil {
		t.Fatal("RenderCompose returned nil, want error wrapping ErrComposeTemplateParse")
	}
	if !errors.Is(err, render.ErrComposeTemplateParse) {
		t.Errorf("RenderCompose returned %v, want errors.Is(_, ErrComposeTemplateParse)", err)
	}
}

// TestRenderCompose_ExtraResolutionKeyFails proves [RenderCompose]
// threads [ValidateResolution] before reaching the template engine
// — an extra key in [Input.Values] (not declared in Placeholders)
// is rejected with ErrResolutionExtraKey before any parse/execute
// runs.
func TestRenderCompose_ExtraResolutionKeyFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: "image: nginx:{{ .X }}\n",
		Placeholders: []render.Placeholder{
			{Name: "X", Type: render.TypeString, Required: true},
		},
		Values: map[string]string{
			"X":     "1.27",
			"EXTRA": "stray",
		},
	}

	_, err := render.RenderCompose(input)
	if err == nil {
		t.Fatal("RenderCompose returned nil, want error wrapping ErrResolutionExtraKey")
	}
	if !errors.Is(err, render.ErrResolutionExtraKey) {
		t.Errorf("RenderCompose returned %v, want errors.Is(_, ErrResolutionExtraKey)", err)
	}
}

// TestRenderCompose_InvalidPlaceholderFails proves [RenderCompose]
// threads [ValidatePlaceholders] before reaching
// [ValidateResolution] or the template engine — an invalid
// placeholder type is rejected with ErrPlaceholderTypeInvalid
// first.
func TestRenderCompose_InvalidPlaceholderFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: "",
		Placeholders: []render.Placeholder{
			{Name: "X", Type: render.Type("bogus"), Required: true},
		},
		Values: nil,
	}

	_, err := render.RenderCompose(input)
	if err == nil {
		t.Fatal("RenderCompose returned nil, want error wrapping ErrPlaceholderTypeInvalid")
	}
	if !errors.Is(err, render.ErrPlaceholderTypeInvalid) {
		t.Errorf("RenderCompose returned %v, want errors.Is(_, ErrPlaceholderTypeInvalid)", err)
	}
}

// TestRenderCompose_NeverLeaksValuesInErrors guards the "error
// text does not leak resolved values" invariant from
// today route secrets through.env via ${VAR} per the confirmation rules,
// the render boundary cannot assume any Values entry is non-
// sensitive — a future {{.Identifier }} reference in a Compose
// template body could carry a secret. The test plants a known
// sentinel value under a DECLARED key and forces an execute-time
// missing-key error via a separate UNDECLARED variable; the
// error must name the missing key but must not echo the planted
// value.
func TestRenderCompose_NeverLeaksValuesInErrors(t *testing.T) {
	t.Parallel()

	const secret = "ULTRA_SECRET_VALUE_THIS_STRING_MUST_NEVER_APPEAR_IN_COMPOSE_ERROR_TEXT"

	input := render.Input{
		ComposeTemplate: "x-api-key: \"{{ .API_KEY }}\"\nmiss: \"{{ .UNDECLARED_IN_VALUES }}\"\n",
		Placeholders: []render.Placeholder{
			{Name: "API_KEY", Type: render.TypeSecret, Required: true},
		},
		Values: map[string]string{
			"API_KEY": secret,
		},
	}

	_, err := render.RenderCompose(input)
	if err == nil {
		t.Fatal("RenderCompose returned nil, want error wrapping ErrComposeTemplateExecute")
	}
	if !errors.Is(err, render.ErrComposeTemplateExecute) {
		t.Errorf("RenderCompose returned %v, want errors.Is(_, ErrComposeTemplateExecute)", err)
	}
	if contains(err.Error(), secret) {
		t.Errorf("RenderCompose error message leaked the secret value:\n  err:    %q\n  secret: %q", err.Error(), secret)
	}
}
