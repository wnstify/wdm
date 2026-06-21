package render_test

import (
	"errors"
	"testing"

	"github.com/wnstify/wdm/internal/render"
	"gopkg.in/yaml.v3"
)

// labelTemplateNoLabels mirrors the shape of a Compose template
// whose service does NOT carry a labels: mapping; [RenderLabels]
// must create the labels mapping and inject both required entries.
const labelTemplateNoLabels = `name: wdm-test
services:
  app:
    image: nginx:1.27
`

// labelTemplateUnrelatedLabels carries a service-level labels:
// mapping with a single unrelated entry; [RenderLabels] must
// preserve that entry while appending both required wdm labels.
const labelTemplateUnrelatedLabels = `name: wdm-test
services:
  app:
    image: nginx:1.27
    labels:
      io.docker.compose.example: "preserved"
`

// labelTemplateWrongValues carries the two required wdm labels
// with WRONG values; [RenderLabels] must overwrite them in place,
// preserving their position in the labels: mapping.
const labelTemplateWrongValues = `name: wdm-test
services:
  app:
    image: nginx:1.27
    labels:
      wdm.managed: "false"
      wdm.app: "wrong-app"
      io.docker.compose.example: "preserved"
`

// labelTemplateMultiService exercises the per-service walk: two
// services with different starting label shapes.
const labelTemplateMultiService = `name: wdm-test
services:
  app:
    image: nginx:1.27
  db:
    image: mariadb:11
    labels:
      io.docker.compose.example: "preserved"
`

// labelTemplateSequenceLabels carries a labels entry in the legacy
// Compose sequence form ("key=value" strings). [RenderLabels]'s
// injectLabel helper leaves the structure alone, so the post-
// injection validation walk surfaces the service via
// [ErrServiceMissingLabel] (the sentinel's documented trigger
// path).
const labelTemplateSequenceLabels = `name: wdm-test
services:
  app:
    image: nginx:1.27
    labels:
      - "wdm.managed=true"
      - "wdm.app=test-app"
`

func TestRenderLabels_InjectsLabelsIntoServicesWithNoLabelsKey(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: labelTemplateNoLabels,
		AppID:           "test-app",
	}

	stack, err := render.RenderLabels(input)
	if err != nil {
		t.Fatalf("RenderLabels returned unexpected error: %v", err)
	}

	got := stack.ServiceLabels["app"]
	if got["wdm.managed"] != "true" {
		t.Errorf(`ServiceLabels["app"]["wdm.managed"] = %q, want "true"`, got["wdm.managed"])
	}
	if got["wdm.app"] != "test-app" {
		t.Errorf(`ServiceLabels["app"]["wdm.app"] = %q, want "test-app"`, got["wdm.app"])
	}

	// Re-parse the encoded YAML to confirm the labels mapping is
	// present in the on-disk shape, not just in the
	// ServiceLabels snapshot.
	var parsed struct {
		Services map[string]struct {
			Labels map[string]string `yaml:"labels"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(stack.ComposeBytes, &parsed); err != nil {
		t.Fatalf("re-parsing ComposeBytes failed: %v", err)
	}
	if parsed.Services["app"].Labels["wdm.managed"] != "true" {
		t.Errorf(`re-parsed labels["wdm.managed"] = %q, want "true"`, parsed.Services["app"].Labels["wdm.managed"])
	}
	if parsed.Services["app"].Labels["wdm.app"] != "test-app" {
		t.Errorf(`re-parsed labels["wdm.app"] = %q, want "test-app"`, parsed.Services["app"].Labels["wdm.app"])
	}
}

func TestRenderLabels_PreservesExistingUnrelatedLabels(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: labelTemplateUnrelatedLabels,
		AppID:           "test-app",
	}

	stack, err := render.RenderLabels(input)
	if err != nil {
		t.Fatalf("RenderLabels returned unexpected error: %v", err)
	}

	got := stack.ServiceLabels["app"]
	if got["io.docker.compose.example"] != "preserved" {
		t.Errorf(`ServiceLabels["app"]["io.docker.compose.example"] = %q, want "preserved"`, got["io.docker.compose.example"])
	}
	if got["wdm.managed"] != "true" {
		t.Errorf(`ServiceLabels["app"]["wdm.managed"] = %q, want "true"`, got["wdm.managed"])
	}
	if got["wdm.app"] != "test-app" {
		t.Errorf(`ServiceLabels["app"]["wdm.app"] = %q, want "test-app"`, got["wdm.app"])
	}

	if !contains(string(stack.ComposeBytes), "io.docker.compose.example") {
		t.Errorf("ComposeBytes lost the unrelated label key:\n%s", stack.ComposeBytes)
	}
}

func TestRenderLabels_OverwritesWrongExistingLabelValues(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: labelTemplateWrongValues,
		AppID:           "correct-app",
	}

	stack, err := render.RenderLabels(input)
	if err != nil {
		t.Fatalf("RenderLabels returned unexpected error: %v", err)
	}

	got := stack.ServiceLabels["app"]
	if got["wdm.managed"] != "true" {
		t.Errorf(`ServiceLabels["app"]["wdm.managed"] = %q, want "true"`, got["wdm.managed"])
	}
	if got["wdm.app"] != "correct-app" {
		t.Errorf(`ServiceLabels["app"]["wdm.app"] = %q, want "correct-app"`, got["wdm.app"])
	}
	if got["io.docker.compose.example"] != "preserved" {
		t.Errorf(`ServiceLabels["app"]["io.docker.compose.example"] = %q, want "preserved"`, got["io.docker.compose.example"])
	}

	// The wrong values must NOT appear in the re-emitted YAML.
	rendered := string(stack.ComposeBytes)
	if contains(rendered, `"false"`) {
		t.Errorf("ComposeBytes still contains the wrong wdm.managed value:\n%s", rendered)
	}
	if contains(rendered, "wrong-app") {
		t.Errorf("ComposeBytes still contains the wrong wdm.app value:\n%s", rendered)
	}
}

func TestRenderLabels_MultiServiceInjection(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: labelTemplateMultiService,
		AppID:           "multi-app",
	}

	stack, err := render.RenderLabels(input)
	if err != nil {
		t.Fatalf("RenderLabels returned unexpected error: %v", err)
	}

	if got := stack.ServiceLabels["app"]; got["wdm.managed"] != "true" || got["wdm.app"] != "multi-app" {
		t.Errorf(`ServiceLabels["app"] = %v, want both wdm labels present`, got)
	}
	if got := stack.ServiceLabels["db"]; got["wdm.managed"] != "true" || got["wdm.app"] != "multi-app" {
		t.Errorf(`ServiceLabels["db"] = %v, want both wdm labels present`, got)
	}
	if got := stack.ServiceLabels["db"]; got["io.docker.compose.example"] != "preserved" {
		t.Errorf(`ServiceLabels["db"]["io.docker.compose.example"] = %q, want "preserved"`, got["io.docker.compose.example"])
	}
	if len(stack.ServiceLabels) != 2 {
		t.Errorf("ServiceLabels has %d entries, want 2", len(stack.ServiceLabels))
	}
}

func TestRenderLabels_CollectsSortedVolumeMounts(t *testing.T) {
	t.Parallel()

	const composeWithVolumes = `name: wdm-test
services:
  app:
    image: nginx:1.27
    volumes:
      - ./data:/app/data
      - named-volume:/var/lib/app
  db:
    image: mariadb:11
    volumes:
      - ./db:/var/lib/mysql
      - ./data:/app/data
`
	input := render.Input{
		ComposeTemplate: composeWithVolumes,
		AppID:           "test-app",
	}

	stack, err := render.RenderLabels(input)
	if err != nil {
		t.Fatalf("RenderLabels returned unexpected error: %v", err)
	}

	want := []string{"./data:/app/data", "./db:/var/lib/mysql", "named-volume:/var/lib/app"}
	if len(stack.VolumeMounts) != len(want) {
		t.Fatalf("VolumeMounts = %v, want %v", stack.VolumeMounts, want)
	}
	for i, mount := range want {
		if stack.VolumeMounts[i] != mount {
			t.Errorf("VolumeMounts[%d] = %q, want %q", i, stack.VolumeMounts[i], mount)
		}
	}
}

func TestRenderLabels_NoServiceVolumesYieldsNilVolumeMounts(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: labelTemplateNoLabels,
		AppID:           "test-app",
	}

	stack, err := render.RenderLabels(input)
	if err != nil {
		t.Fatalf("RenderLabels returned unexpected error: %v", err)
	}
	if stack.VolumeMounts != nil {
		t.Errorf("VolumeMounts = %v, want nil for a volume-free template", stack.VolumeMounts)
	}
}

func TestRenderLabels_TopLevelServicesKeyMissingFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: "name: lonely\nversion: \"3\"\n",
		AppID:           "test-app",
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrComposeServicesMissing")
	}
	if !errors.Is(err, render.ErrComposeServicesMissing) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrComposeServicesMissing)", err)
	}
}

func TestRenderLabels_TopLevelNotMappingFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		// Top-level is a sequence, not a mapping.
		ComposeTemplate: "- one\n- two\n",
		AppID:           "test-app",
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrComposeServicesMissing")
	}
	if !errors.Is(err, render.ErrComposeServicesMissing) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrComposeServicesMissing)", err)
	}
}

func TestRenderLabels_ServicesNotMappingFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		// services is a sequence, not a mapping.
		ComposeTemplate: "name: wdm-test\nservices:\n  - one\n  - two\n",
		AppID:           "test-app",
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrComposeServicesMissing")
	}
	if !errors.Is(err, render.ErrComposeServicesMissing) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrComposeServicesMissing)", err)
	}
}

func TestRenderLabels_ServiceValueNotMappingFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		// app value is a scalar, not a mapping.
		ComposeTemplate: "name: wdm-test\nservices:\n  app: scalar-not-mapping\n",
		AppID:           "test-app",
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrComposeServicesMissing")
	}
	if !errors.Is(err, render.ErrComposeServicesMissing) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrComposeServicesMissing)", err)
	}
}

func TestRenderLabels_MalformedYAMLFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		// Unclosed flow sequence — text/template parses fine,
		// yaml.v3 rejects the rendered bytes.
		ComposeTemplate: "services: [\n  unclosed\n",
		AppID:           "test-app",
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrComposeYAMLParse")
	}
	if !errors.Is(err, render.ErrComposeYAMLParse) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrComposeYAMLParse)", err)
	}
}

func TestRenderLabels_MalformedTemplateFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		// {{ unclosed action — text/template parse rejects this.
		ComposeTemplate: "services:\n  app:\n    image: nginx:{{ .UNCLOSED\n",
		AppID:           "test-app",
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrComposeTemplateParse")
	}
	if !errors.Is(err, render.ErrComposeTemplateParse) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrComposeTemplateParse)", err)
	}
}

func TestRenderLabels_MissingKeyFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: "services:\n  app:\n    image: nginx:{{ .UNDECLARED_IN_VALUES }}\n",
		AppID:           "test-app",
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrComposeTemplateExecute")
	}
	if !errors.Is(err, render.ErrComposeTemplateExecute) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrComposeTemplateExecute)", err)
	}
}

func TestRenderLabels_InvalidPlaceholderFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: labelTemplateNoLabels,
		AppID:           "test-app",
		Placeholders: []render.Placeholder{
			{Name: "X", Type: render.Type("bogus"), Required: true},
		},
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrPlaceholderTypeInvalid")
	}
	if !errors.Is(err, render.ErrPlaceholderTypeInvalid) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrPlaceholderTypeInvalid)", err)
	}
}

func TestRenderLabels_ExtraResolutionKeyFails(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: labelTemplateNoLabels,
		AppID:           "test-app",
		Placeholders:    nil,
		Values: map[string]string{
			"GHOST": "stray",
		},
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrResolutionExtraKey")
	}
	if !errors.Is(err, render.ErrResolutionExtraKey) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrResolutionExtraKey)", err)
	}
}

func TestRenderLabels_LabelsInSequenceFormFailsPostInjection(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: labelTemplateSequenceLabels,
		AppID:           "test-app",
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrServiceMissingLabel")
	}
	if !errors.Is(err, render.ErrServiceMissingLabel) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrServiceMissingLabel)", err)
	}
}

// TestRenderLabels_DeterministicAcrossRenders pins commit
// 7's deterministic-output guarantee: repeated renders of the same
// Input produce byte-identical [RenderedStack.ComposeBytes]. yaml.v3
// via yaml.Node iterates Content slices (no Go-map iteration); the
// encoder uses a fixed 2-space indent set by RenderLabels — so the
// output cannot drift between runs.
func TestRenderLabels_DeterministicAcrossRenders(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: labelTemplateMultiService,
		AppID:           "deterministic",
	}

	first, err := render.RenderLabels(input)
	if err != nil {
		t.Fatalf("first RenderLabels returned unexpected error: %v", err)
	}
	second, err := render.RenderLabels(input)
	if err != nil {
		t.Fatalf("second RenderLabels returned unexpected error: %v", err)
	}

	if string(first.ComposeBytes) != string(second.ComposeBytes) {
		t.Errorf("RenderLabels output is not deterministic between runs:\n first:\n%s\n second:\n%s", first.ComposeBytes, second.ComposeBytes)
	}
}

func TestRenderLabels_OnlyComposeBytesAndServiceLabelsPopulated(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: labelTemplateNoLabels,
		AppID:           "test-app",
	}

	stack, err := render.RenderLabels(input)
	if err != nil {
		t.Fatalf("RenderLabels returned unexpected error: %v", err)
	}

	if len(stack.ComposeBytes) == 0 {
		t.Error("ComposeBytes is empty; want rendered content")
	}
	if len(stack.ServiceLabels) == 0 {
		t.Error("ServiceLabels is empty; want at least one service")
	}
	if stack.EnvBytes != nil {
		t.Errorf("EnvBytes = %q, want nil (RenderEnv populates this; RenderLabels must not)", stack.EnvBytes)
	}
	if stack.AdditionalFiles != nil {
		t.Errorf("AdditionalFiles = %v, want nil (input declares no sidecar files)", stack.AdditionalFiles)
	}
}

func TestRenderLabels_RendersAdditionalFilesAndVerifiesMounts(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: `services:
  postgres:
    image: postgres:18.4
    volumes:
      - ./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro
`,
		AppID: "n8n",
		Placeholders: []render.Placeholder{
			{Name: "POSTGRES_PASSWORD", Type: render.TypeSecret, Required: true},
		},
		Values: map[string]string{
			"POSTGRES_PASSWORD": "generated-secret",
		},
		AdditionalFiles: []render.AdditionalFile{
			{
				Dest:     "init-data.sh",
				Mode:     "0755",
				Mount:    "./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro",
				Template: "password={{ .POSTGRES_PASSWORD }}\n",
			},
		},
	}

	stack, err := render.RenderLabels(input)
	if err != nil {
		t.Fatalf("RenderLabels returned unexpected error: %v", err)
	}
	if len(stack.AdditionalFiles) != 1 {
		t.Fatalf("AdditionalFiles length = %d, want 1", len(stack.AdditionalFiles))
	}
	if stack.AdditionalFiles[0].Dest != "init-data.sh" {
		t.Errorf("AdditionalFiles[0].Dest = %q, want init-data.sh", stack.AdditionalFiles[0].Dest)
	}
	if stack.AdditionalFiles[0].Mode != "0755" {
		t.Errorf("AdditionalFiles[0].Mode = %q, want 0755", stack.AdditionalFiles[0].Mode)
	}
	if got := string(stack.AdditionalFiles[0].Bytes); got != "password=generated-secret\n" {
		t.Errorf("AdditionalFiles[0].Bytes = %q, want rendered password", got)
	}
}

func TestRenderLabels_RejectsAdditionalFileMountDrift(t *testing.T) {
	t.Parallel()

	input := render.Input{
		ComposeTemplate: `services:
  postgres:
    image: postgres:18.4
    volumes:
      - ./different.sh:/docker-entrypoint-initdb.d/init-data.sh:ro
`,
		AppID: "n8n",
		AdditionalFiles: []render.AdditionalFile{
			{
				Dest:     "init-data.sh",
				Mode:     "0755",
				Mount:    "./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro",
				Template: "echo ok\n",
			},
		},
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want mount drift error")
	}
	if !errors.Is(err, render.ErrAdditionalFileMountMissing) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrAdditionalFileMountMissing)", err)
	}
}

// TestRenderLabels_NeverLeaksValuesInErrors guards the invariant
// from scope: error text must not echo resolved
// values from [Input.Values]. The test plants a sentinel value
// under a DECLARED key and forces an execute-time missing-key
// error via a separate UNDECLARED variable; the error must name
// the missing key but must not echo the planted value.
func TestRenderLabels_NeverLeaksValuesInErrors(t *testing.T) {
	t.Parallel()

	const secret = "ULTRA_SECRET_VALUE_THIS_STRING_MUST_NEVER_APPEAR_IN_LABELS_ERROR_TEXT"

	input := render.Input{
		ComposeTemplate: "services:\n  app:\n    image: nginx\n    x-api-key: \"{{ .API_KEY }}\"\n    x-miss: \"{{ .UNDECLARED_IN_VALUES }}\"\n",
		AppID:           "test-app",
		Placeholders: []render.Placeholder{
			{Name: "API_KEY", Type: render.TypeSecret, Required: true},
		},
		Values: map[string]string{
			"API_KEY": secret,
		},
	}

	_, err := render.RenderLabels(input)
	if err == nil {
		t.Fatal("RenderLabels returned nil, want error wrapping ErrComposeTemplateExecute")
	}
	if !errors.Is(err, render.ErrComposeTemplateExecute) {
		t.Errorf("RenderLabels returned %v, want errors.Is(_, ErrComposeTemplateExecute)", err)
	}
	if contains(err.Error(), secret) {
		t.Errorf("RenderLabels error message leaked the secret value:\n  err:    %q\n  secret: %q", err.Error(), secret)
	}
}
