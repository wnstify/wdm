package render

import (
	"bytes"
	"fmt"
	"text/template"
)

// RenderEnv renders [Input.EnvTemplate] against [Input.Values] via
// text/template with Option("missingkey=error"), so a reference to a
// key absent from Values surfaces as [ErrEnvTemplateExecute] rather
// than "<no value>". No FuncMap is registered: the curated v1 env
// templates carry only bare `.Identifier` references.
// Structural validation runs before the template is parsed:
//   - [ValidatePlaceholders] checks [Input.Placeholders] for empty
//     names, invalid types, and duplicates.
//   - [ValidateResolution] checks [Input.Values] against the declared
//     set (every Required placeholder present, no extra keys).
//
// Validator errors are returned directly; their sentinels stay
// reachable via [errors.Is]. Parse failures wrap
// [ErrEnvTemplateParse]; execute failures wrap
// [ErrEnvTemplateExecute].
// Error messages may name placeholder keys (e.g. text/template's
// missing-key diagnostic) but never echo resolved values from
// [Input.Values] — treats every resolved value as
// potentially sensitive (some are crypto/rand secrets unredacted at
// this layer). The render boundary stays pure per /
// operational protocol step 4: no filesystem I/O, no internal/*
// sibling imports (the internal-render-pure depguard rule enforces).
// Populates only [RenderedStack.EnvBytes]; the other fields
// ([RenderedStack.ComposeBytes], [RenderedStack.AdditionalFiles],
// [RenderedStack.LockManifest], [RenderedStack.ServiceLabels]) stay
// zero per the [RenderedStack] godoc staging contract.
//
//nolint:revive // "RenderEnv" intentionally mirrors the Render* family. The verb-noun pairing parallels Validate* (ValidatePlaceholders, ValidateResolution); renaming to "Env" would lose verb intent and split the family.
func RenderEnv(input Input) (RenderedStack, error) {
	if err := ValidatePlaceholders(input.Placeholders); err != nil {
		return RenderedStack{}, err
	}
	if err := ValidateResolution(input.Placeholders, input.Values); err != nil {
		return RenderedStack{}, err
	}

	tmpl, err := template.New("env").Option("missingkey=error").Parse(input.EnvTemplate)
	if err != nil {
		return RenderedStack{}, fmt.Errorf("%w: %w", ErrEnvTemplateParse, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, input.Values); err != nil {
		return RenderedStack{}, fmt.Errorf("%w: %w", ErrEnvTemplateExecute, err)
	}

	return RenderedStack{EnvBytes: buf.Bytes()}, nil
}
