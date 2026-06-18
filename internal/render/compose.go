package render

import (
	"bytes"
	"fmt"
	"text/template"
)

// RenderCompose renders [Input.ComposeTemplate] against
// [Input.Values] via text/template with Option("missingkey=error"),
// so a reference to a key absent from Values surfaces as
// [ErrComposeTemplateExecute] rather than "<no value>". No FuncMap
// is registered: curated v1 Compose templates primarily keep
// shell-style ${VAR} references for Compose's runtime --env-file
// substitution, with only simple value substitutions such as built-in
// UID/GID when a service must render host-user ownership directly.
// Shell-style references pass through unchanged.
// Structural validation runs before the template is parsed (as in
// [RenderEnv]):
//   - [ValidatePlaceholders] checks [Input.Placeholders] for empty
//     names, invalid types, and duplicates.
//   - [ValidateResolution] checks [Input.Values] against the declared
//     set (every Required placeholder present, no extra keys).
//
// Validator errors are returned directly; their sentinels stay
// reachable via [errors.Is]. Parse failures wrap
// [ErrComposeTemplateParse]; execute failures wrap
// [ErrComposeTemplateExecute].
// Error messages may name placeholder keys (e.g. text/template's
// missing-key diagnostic) but never echo resolved values from
// [Input.Values] — treats every resolved value as
// potentially sensitive. Secrets route through.env via ${VAR}
// but render cannot assume any Values entry is
// non-sensitive: any {{.Identifier }} reference could carry a
// secret.
// Determinism: text/template Execute is deterministic for the same
// input values, and on an action-free body emits the source bytes
// contiguously. RenderCompose performs no map iteration or YAML
// serialization, so output stays stable across Go versions and
// architectures for a fixed input.
// label enforcement and mount verification land in [RenderLabels].
// The render boundary stays pure per / operational
// protocol step 4: no filesystem I/O, no internal/* sibling imports
// (the internal-render-pure depguard rule enforces).
// Populates only [RenderedStack.ComposeBytes]; the other fields
// ([RenderedStack.EnvBytes], [RenderedStack.AdditionalFiles],
// [RenderedStack.LockManifest], [RenderedStack.ServiceLabels]) stay
// zero per the [RenderedStack] godoc staging contract.
//
//nolint:revive // "RenderCompose" intentionally mirrors the Render* family. The verb-noun pairing parallels Validate* (ValidatePlaceholders, ValidateResolution); renaming to "Compose" would lose verb intent and collide with the docker/compose noun.
func RenderCompose(input Input) (RenderedStack, error) {
	if err := ValidatePlaceholders(input.Placeholders); err != nil {
		return RenderedStack{}, err
	}
	if err := ValidateResolution(input.Placeholders, input.Values); err != nil {
		return RenderedStack{}, err
	}

	tmpl, err := template.New("compose").Option("missingkey=error").Parse(input.ComposeTemplate)
	if err != nil {
		return RenderedStack{}, fmt.Errorf("%w: %w", ErrComposeTemplateParse, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, input.Values); err != nil {
		return RenderedStack{}, fmt.Errorf("%w: %w", ErrComposeTemplateExecute, err)
	}

	return RenderedStack{ComposeBytes: buf.Bytes()}, nil
}
