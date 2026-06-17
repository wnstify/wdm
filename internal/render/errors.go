package render

import "errors"

// Sentinel errors wrapped via fmt.Errorf with %w by this package.
// Detect with [errors.Is]. cmd/wdm wraps these via
// pkg/types.WrapError with pkg/types.ErrCodeUsageValidation at the
// engine boundary; user-facing copy is composed
// by cmd/wdm, not here.
// These cover the validation surface, env
// rendering, Compose rendering, label enforcement,
// and additional-files render and mount verification.
var (
	// ErrPlaceholderNameEmpty is returned (wrapped via %w) by
	// [ValidatePlaceholders] when a [Placeholder] carries an empty
	// [Placeholder.Name].
	ErrPlaceholderNameEmpty = errors.New("render: placeholder name must not be empty")

	// ErrPlaceholderNameDuplicate is returned (wrapped via %w) by
	// [ValidatePlaceholders] when two or more [Placeholder] values
	// share the same [Placeholder.Name].
	ErrPlaceholderNameDuplicate = errors.New("render: duplicate placeholder name")

	// ErrPlaceholderTypeInvalid is returned (wrapped via %w) by
	// [ValidatePlaceholders] when a [Placeholder] carries a
	// [Placeholder.Type] outside the closed enum declared in
	// types.go.
	ErrPlaceholderTypeInvalid = errors.New("render: placeholder type invalid")

	// ErrResolutionMissingPlaceholder is returned (wrapped via %w)
	// by [ValidateResolution] when a Required [Placeholder] has no
	// entry in the resolved value map.
	ErrResolutionMissingPlaceholder = errors.New("render: required placeholder missing from resolution")

	// ErrResolutionExtraKey is returned (wrapped via %w) by
	// [ValidateResolution] when the resolved value map contains a
	// key not declared in the placeholder set.
	ErrResolutionExtraKey = errors.New("render: resolution key not declared in placeholders")

	// ErrEnvTemplateParse is returned (wrapped via %w) by
	// [RenderEnv] when text/template fails to parse
	// [Input.EnvTemplate]. The wrapped cause carries text/template's
	// "template: <name>:<line>:<col>: <msg>" syntax diagnostic, which
	// names line/column from the template body and never echoes a
	// resolved value from [Input.Values].
	ErrEnvTemplateParse = errors.New("render: env template parse failed")

	// ErrEnvTemplateExecute is returned (wrapped via %w) by
	// [RenderEnv] when text/template Execute fails against
	// [Input.Values], most often a reference to a key absent from
	// Values: Option("missingkey=error") surfaces missing keys as
	// execute errors naming the key. The wrapped diagnostic names the
	// absent key but never echoes a resolved value — the
	// setup (no FuncMap, map[string]string data) has no error path
	// that includes a present value.
	ErrEnvTemplateExecute = errors.New("render: env template execute failed")

	// ErrComposeTemplateParse is returned (wrapped via %w) by
	// [RenderCompose] when text/template fails to parse
	// [Input.ComposeTemplate]. The wrapped cause carries text/
	// template's "template: <name>:<line>:<col>: <msg>" syntax
	// diagnostic, which names line/column from the template body and
	// never echoes a resolved value from [Input.Values].
	ErrComposeTemplateParse = errors.New("render: compose template parse failed")

	// ErrComposeTemplateExecute is returned (wrapped via %w) by
	// [RenderCompose] when text/template Execute fails against
	// [Input.Values], most often a reference to a key absent from
	// Values: Option("missingkey=error") surfaces missing keys as
	// execute errors naming the key. The wrapped diagnostic names the
	// absent key but never echoes a resolved value — the
	// setup (no FuncMap, map[string]string data) has no error path
	// that includes a present value.
	ErrComposeTemplateExecute = errors.New("render: compose template execute failed")

	// ErrComposeYAMLParse is returned (wrapped via %w) by
	// [RenderLabels] when gopkg.in/yaml.v3 fails to unmarshal the
	// rendered Compose bytes into a yaml.Node tree. The wrapped cause
	// carries yaml.v3's parser diagnostic naming line/column from the
	// rendered byte stream; it describes the offending token by
	// position and never echoes a resolved value from [Input.Values].
	ErrComposeYAMLParse = errors.New("render: compose yaml parse failed")

	// ErrComposeYAMLEncode is returned (wrapped via %w) by
	// [RenderLabels] when gopkg.in/yaml.v3 fails to marshal the
	// label-injected yaml.Node tree back to bytes. yaml.Marshal does
	// not fail on well-formed in-memory trees today; the sentinel
	// gives any future failure mode a stable identity rather than an
	// opaque error.
	ErrComposeYAMLEncode = errors.New("render: compose yaml encode failed")

	// ErrComposeServicesMissing is returned (wrapped via %w) by
	// [RenderLabels] when the rendered Compose document's top-level
	// value is not a mapping, the top-level mapping lacks a services
	// key, the services value is not a mapping, or a service value is
	// not a mapping. Label injection cannot recover from these, so
	// render refuses the document rather than emitting a partial
	// result.
	ErrComposeServicesMissing = errors.New("render: compose services mapping missing or malformed")

	// ErrServiceMissingLabel is returned (wrapped via %w) by
	// [RenderLabels] when the post-injection validation walk finds a
	// service still missing the mandatory wdm.managed="true" or
	// wdm.app=<AppID> label (e.g. its labels key existed in a
	// non-mapping form such as a Compose "key=value" sequence). The
	// error names the offending Compose service but never echoes a
	// resolved value from [Input.Values]. Render-time refusal per
	// core does NOT re-run the check.
	ErrServiceMissingLabel = errors.New("render: compose service missing required wdm label after injection")

	// ErrAdditionalFileMountMissing is returned (wrapped via %w) by
	// [RenderLabels] when an [AdditionalFile] declares a mount that
	// is absent from every Compose service's volumes list. Render
	// verifies catalog-declared mounts but never injects them.
	ErrAdditionalFileMountMissing = errors.New("render: additional file mount missing from compose volumes")

	// ErrAdditionalFileTemplateParse is returned (wrapped via %w)
	// when text/template fails to parse an [AdditionalFile.Template].
	ErrAdditionalFileTemplateParse = errors.New("render: additional file template parse failed")

	// ErrAdditionalFileTemplateExecute is returned (wrapped via %w)
	// when an [AdditionalFile.Template] references a missing key in
	// [Input.Values] or otherwise fails during execution.
	ErrAdditionalFileTemplateExecute = errors.New("render: additional file template execute failed")

	// ErrConfigArtifactDestUnsafe is returned (wrapped via %w) by
	// [RenderLabels] when a [ConfigArtifact.Dest] is absolute, empty,
	// the stack dir itself, or contains a parent-directory ("..")
	// traversal element. This guards the PRD §12/§13 "no writes
	// outside the stack directory" invariant at the render boundary —
	// defense in depth above the catalog schema's own traversal guard.
	// The error names the offending dest (catalog metadata, not a
	// resolved value) but never echoes [Input.Values].
	ErrConfigArtifactDestUnsafe = errors.New("render: config artifact dest escapes the stack directory")

	// ErrConfigArtifactMountMissing is returned (wrapped via %w) by
	// [RenderLabels] when a [ConfigArtifact] declares a mount that is
	// absent from every Compose service's volumes list. Render verifies
	// catalog-declared mounts but never injects them.
	ErrConfigArtifactMountMissing = errors.New("render: config artifact mount missing from compose volumes")

	// ErrConfigArtifactTemplateParse is returned (wrapped via %w) when
	// text/template fails to parse a [ConfigArtifact.Template].
	ErrConfigArtifactTemplateParse = errors.New("render: config artifact template parse failed")

	// ErrConfigArtifactTemplateExecute is returned (wrapped via %w)
	// when a [ConfigArtifact.Template] references a missing key in
	// [Input.Values] or otherwise fails during execution.
	ErrConfigArtifactTemplateExecute = errors.New("render: config artifact template execute failed")
)
