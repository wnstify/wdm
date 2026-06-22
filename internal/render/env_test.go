package render_test

import (
	"errors"
	"testing"

	"github.com/wnstify/wdm/internal/render"
)

func TestRenderEnv_Success(t *testing.T) {
	t.Parallel()

	input := render.Input{
		EnvTemplate: "TZ={{ .TZ }}\nPORT={{ .PORT }}\nMEMORY_LIMIT_APP={{ .MEMORY_LIMIT_APP }}\n",
		Placeholders: []render.Placeholder{
			{Name: "TZ", Type: render.TypeTimezone, Required: true},
			{Name: "PORT", Type: render.TypePort, Required: true},
			{Name: "MEMORY_LIMIT_APP", Type: render.TypeString, Required: true},
		},
		Values: map[string]string{
			"TZ":               "Europe/Berlin",
			"PORT":             "8080",
			"MEMORY_LIMIT_APP": "512m",
		},
	}

	stack, err := render.RenderEnv(input)
	if err != nil {
		t.Fatalf("RenderEnv returned unexpected error: %v", err)
	}

	const want = "TZ=Europe/Berlin\nPORT=8080\nMEMORY_LIMIT_APP=512m\n"
	if got := string(stack.EnvBytes); got != want {
		t.Errorf("EnvBytes:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderEnv_OnlyEnvBytesPopulated(t *testing.T) {
	t.Parallel()

	input := render.Input{
		EnvTemplate:  "STATIC=value\n",
		Placeholders: nil,
		Values:       nil,
	}

	stack, err := render.RenderEnv(input)
	if err != nil {
		t.Fatalf("RenderEnv returned unexpected error: %v", err)
	}

	if len(stack.EnvBytes) == 0 {
		t.Error("EnvBytes is empty; want rendered content")
	}
	if stack.ComposeBytes != nil {
		t.Errorf("ComposeBytes = %q, want nil (later wiring populates this)", stack.ComposeBytes)
	}
	if stack.AdditionalFiles != nil {
		t.Errorf("AdditionalFiles = %v, want nil (RenderLabels handles sidecar files)", stack.AdditionalFiles)
	}
	if stack.ServiceLabels != nil {
		t.Errorf("ServiceLabels = %v, want nil (later wiring populates this)", stack.ServiceLabels)
	}
}

// TestRenderEnv_MissingKey forces a text/template execute failure by
// referencing an UNDECLARED variable from the template body — the
// validators do not see template bodies, so the failure surfaces at
// template Execute time as ErrEnvTemplateExecute. Option("missingkey=error")
// makes the miss fatal rather than emitting
// "<no value>").
func TestRenderEnv_MissingKey(t *testing.T) {
	t.Parallel()

	input := render.Input{
		EnvTemplate: "DECLARED={{ .DECLARED }}\nUNDECLARED={{ .UNDECLARED_IN_VALUES }}\n",
		Placeholders: []render.Placeholder{
			{Name: "DECLARED", Type: render.TypeString, Required: true},
		},
		Values: map[string]string{
			"DECLARED": "ok",
		},
	}

	_, err := render.RenderEnv(input)
	if err == nil {
		t.Fatal("RenderEnv returned nil, want error wrapping ErrEnvTemplateExecute")
	}
	if !errors.Is(err, render.ErrEnvTemplateExecute) {
		t.Errorf("RenderEnv returned %v, want errors.Is(_, ErrEnvTemplateExecute)", err)
	}
}

func TestRenderEnv_MalformedTemplate(t *testing.T) {
	t.Parallel()

	input := render.Input{
		// {{ unclosed action — text/template parse rejects this.
		EnvTemplate:  "BROKEN={{ .UNCLOSED\n",
		Placeholders: nil,
		Values:       nil,
	}

	_, err := render.RenderEnv(input)
	if err == nil {
		t.Fatal("RenderEnv returned nil, want error wrapping ErrEnvTemplateParse")
	}
	if !errors.Is(err, render.ErrEnvTemplateParse) {
		t.Errorf("RenderEnv returned %v, want errors.Is(_, ErrEnvTemplateParse)", err)
	}
}

// TestRenderEnv_ExtraResolutionKeyFails proves [RenderEnv] threads
// [ValidateResolution] before reaching the template engine — an
// extra key in [Input.Values] (not declared in Placeholders) is
// rejected with ErrResolutionExtraKey before any parse/execute runs.
func TestRenderEnv_ExtraResolutionKeyFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		EnvTemplate: "X={{ .X }}\n",
		Placeholders: []render.Placeholder{
			{Name: "X", Type: render.TypeString, Required: true},
		},
		Values: map[string]string{
			"X":     "value",
			"EXTRA": "stray",
		},
	}

	_, err := render.RenderEnv(input)
	if err == nil {
		t.Fatal("RenderEnv returned nil, want error wrapping ErrResolutionExtraKey")
	}
	if !errors.Is(err, render.ErrResolutionExtraKey) {
		t.Errorf("RenderEnv returned %v, want errors.Is(_, ErrResolutionExtraKey)", err)
	}
}

// TestRenderEnv_InvalidPlaceholderFails proves [RenderEnv] threads
// [ValidatePlaceholders] before reaching [ValidateResolution] or the
// template engine — an invalid placeholder type is rejected with
// ErrPlaceholderTypeInvalid first.
func TestRenderEnv_InvalidPlaceholderFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		EnvTemplate: "",
		Placeholders: []render.Placeholder{
			{Name: "X", Type: render.Type("bogus"), Required: true},
		},
		Values: nil,
	}

	_, err := render.RenderEnv(input)
	if err == nil {
		t.Fatal("RenderEnv returned nil, want error wrapping ErrPlaceholderTypeInvalid")
	}
	if !errors.Is(err, render.ErrPlaceholderTypeInvalid) {
		t.Errorf("RenderEnv returned %v, want errors.Is(_, ErrPlaceholderTypeInvalid)", err)
	}
}

// TestRenderEnv_NeverLeaksValuesInErrors guards the "error text does
// not leak secret values" invariant from
// scope. Values carries a known sentinel secret under a DECLARED
// name (so ValidateResolution accepts it) and the template
// references an UNDECLARED variable to force an execute-time
// missing-key error. The error message must name the missing key
// but must not echo the secret value.
func TestRenderEnv_NeverLeaksValuesInErrors(t *testing.T) {
	t.Parallel()

	const secret = "ULTRA_SECRET_VALUE_THIS_STRING_MUST_NEVER_APPEAR_IN_ERROR_TEXT"

	input := render.Input{
		EnvTemplate: "API_KEY={{ .API_KEY }}\nMISS={{ .UNDECLARED_IN_VALUES }}\n",
		Placeholders: []render.Placeholder{
			{Name: "API_KEY", Type: render.TypeSecret, Required: true},
		},
		Values: map[string]string{
			"API_KEY": secret,
		},
	}

	_, err := render.RenderEnv(input)
	if err == nil {
		t.Fatal("RenderEnv returned nil, want error wrapping ErrEnvTemplateExecute")
	}
	if !errors.Is(err, render.ErrEnvTemplateExecute) {
		t.Errorf("RenderEnv returned %v, want errors.Is(_, ErrEnvTemplateExecute)", err)
	}
	if contains(err.Error(), secret) {
		t.Errorf("RenderEnv error message leaked the secret value:\n  err:    %q\n  secret: %q", err.Error(), secret)
	}
}
