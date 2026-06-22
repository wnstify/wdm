package render

import (
	"bytes"
	"fmt"
	"io/fs"
	"sort"
	"text/template"

	"gopkg.in/yaml.v3"
)

// RenderLabels renders [Input.ComposeTemplate] against
// [Input.Values] and injects the mandatory wdm.managed="true" and
// wdm.app=<AppID> labels into every Compose service per
// /. [Input.AppID] carries the stable catalog slug
// (e.g. "uptime-kuma") so the label matches the stack subdirectory
// name and Compose project suffix wdm uses elsewhere (PRD §9, §17).
// After injection it walks the services again and wraps
// [ErrServiceMissingLabel] if any service still lacks either label —
// render-time refusal, not a run-time warning; internal/core does
// NOT re-run the check.
// Structural validation runs before the template is parsed (as in
// [RenderEnv]):
//   - [ValidatePlaceholders] checks [Input.Placeholders] for empty
//     names, invalid types, and duplicates.
//   - [ValidateResolution] checks [Input.Values] against the
//     declared set (every Required placeholder present, no extra
//     keys).
//
// Failures wrap: [ErrComposeTemplateParse] (parse),
// [ErrComposeTemplateExecute] (execute), [ErrComposeYAMLParse]
// (yaml.v3 parse of the rendered bytes), [ErrComposeYAMLEncode]
// (yaml.v3 encode of the injected tree), [ErrComposeServicesMissing]
// (top-level not a mapping, no services key, services or a service
// value not a mapping), and [ErrServiceMissingLabel] (post-injection
// validation).
// Error messages may name placeholder keys, Compose service names,
// or text/template / yaml.v3 line/column diagnostics but never echo
// resolved values from [Input.Values] — treats every
// resolved value as potentially sensitive — and never include the
// rendered byte stream itself.
// Determinism: gopkg.in/yaml.v3 via yaml.Node preserves key order
// across the parse-mutate-emit cycle (Content slices, no Go-map
// iteration). Services are walked in YAML source order; a service
// with no labels key gets a new mapping appended at the end, and the
// wdm.managed and wdm.app entries are overwritten in place if
// present or appended if absent. The encoder uses a fixed 2-space
// indent, so repeated renders of the same Input are byte-identical.
// The render boundary stays pure: no filesystem I/O, no internal/* sibling
// imports. The internal-render-pure depguard rule enforces this;
// gopkg.in/yaml.v3 is the lone third-party allowance.
// Populates [RenderedStack.ComposeBytes] and
// [RenderedStack.ServiceLabels]; also verifies
// [Input.AdditionalFiles] mounts against service volumes and renders
// those sidecar artifacts into [RenderedStack.AdditionalFiles] when
// present. When [Input.ConfigGeneration] is supplied it likewise
// traversal-checks each [ConfigArtifact.Dest], verifies the declared
// mounts against service volumes, and renders the artifacts into
// [RenderedStack.ConfigArtifacts]. [RenderedStack.EnvBytes] stays zero per
// the [RenderedStack] godoc staging contract.
//
//nolint:revive // "RenderLabels" intentionally mirrors the Render* family. The verb-noun pairing parallels Validate* (ValidatePlaceholders, ValidateResolution); renaming to "Labels" would lose verb intent and collide with the labels noun used by Compose.
func RenderLabels(input Input) (RenderedStack, error) {
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

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, input.Values); err != nil {
		return RenderedStack{}, fmt.Errorf("%w: %w", ErrComposeTemplateExecute, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(rendered.Bytes(), &doc); err != nil {
		return RenderedStack{}, fmt.Errorf("%w: %w", ErrComposeYAMLParse, err)
	}

	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return RenderedStack{}, fmt.Errorf("render: compose top-level value is not a mapping: %w", ErrComposeServicesMissing)
	}

	services := findMapValue(root, "services")
	if services == nil {
		return RenderedStack{}, fmt.Errorf("render: compose top-level mapping has no services key: %w", ErrComposeServicesMissing)
	}
	if services.Kind != yaml.MappingNode {
		return RenderedStack{}, fmt.Errorf("render: compose top-level services value is not a mapping: %w", ErrComposeServicesMissing)
	}

	labelsByService := make(map[string]map[string]string, len(services.Content)/2)
	for i := 0; i+1 < len(services.Content); i += 2 {
		keyNode := services.Content[i]
		valNode := services.Content[i+1]
		serviceName := keyNode.Value
		if valNode.Kind != yaml.MappingNode {
			return RenderedStack{}, fmt.Errorf("render: compose service %q value is not a mapping: %w", serviceName, ErrComposeServicesMissing)
		}
		injectLabel(valNode, "wdm.managed", "true")
		injectLabel(valNode, "wdm.app", input.AppID)
		labelsByService[serviceName] = collectLabels(valNode)
	}

	for i := 0; i+1 < len(services.Content); i += 2 {
		serviceName := services.Content[i].Value
		labels := labelsByService[serviceName]
		if labels["wdm.managed"] != "true" {
			return RenderedStack{}, fmt.Errorf("render: compose service %q lacks wdm.managed label after injection: %w", serviceName, ErrServiceMissingLabel)
		}
		if labels["wdm.app"] != input.AppID {
			return RenderedStack{}, fmt.Errorf("render: compose service %q lacks wdm.app label after injection: %w", serviceName, ErrServiceMissingLabel)
		}
	}
	if err := verifyAdditionalFileMounts(services, input.AdditionalFiles); err != nil {
		return RenderedStack{}, err
	}
	additionalFiles, err := renderAdditionalFiles(input)
	if err != nil {
		return RenderedStack{}, err
	}

	configArtifacts, err := renderConfigGeneration(services, input)
	if err != nil {
		return RenderedStack{}, err
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return RenderedStack{}, fmt.Errorf("%w: %w", ErrComposeYAMLEncode, err)
	}
	if err := enc.Close(); err != nil {
		return RenderedStack{}, fmt.Errorf("%w: %w", ErrComposeYAMLEncode, err)
	}

	return RenderedStack{
		ComposeBytes:    out.Bytes(),
		AdditionalFiles: additionalFiles,
		ConfigArtifacts: configArtifacts,
		ServiceLabels:   labelsByService,
		VolumeMounts:    sortedVolumeMounts(services),
	}, nil
}

// sortedVolumeMounts returns the service volume mount specs from
// collectVolumeMounts as the sorted, deduplicated slice surfaced via
// [RenderedStack.VolumeMounts]. Returns nil when no service declares
// volumes.
func sortedVolumeMounts(services *yaml.Node) []string {
	mountSet := collectVolumeMounts(services)
	if len(mountSet) == 0 {
		return nil
	}
	mounts := make([]string, 0, len(mountSet))
	for mount := range mountSet {
		mounts = append(mounts, mount)
	}
	sort.Strings(mounts)
	return mounts
}

func verifyAdditionalFileMounts(services *yaml.Node, files []AdditionalFile) error {
	if len(files) == 0 {
		return nil
	}
	mounts := collectVolumeMounts(services)
	for _, file := range files {
		if file.Mount == "" {
			continue
		}
		if _, ok := mounts[file.Mount]; !ok {
			return fmt.Errorf(
				"render: additional file %q declares mount %q absent from compose volumes: %w",
				file.Dest,
				file.Mount,
				ErrAdditionalFileMountMissing,
			)
		}
	}
	return nil
}

func collectVolumeMounts(services *yaml.Node) map[string]struct{} {
	mounts := map[string]struct{}{}
	if services == nil || services.Kind != yaml.MappingNode {
		return mounts
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		service := services.Content[i+1]
		if service.Kind != yaml.MappingNode {
			continue
		}
		volumes := findMapValue(service, "volumes")
		if volumes == nil || volumes.Kind != yaml.SequenceNode {
			continue
		}
		for _, volume := range volumes.Content {
			if volume.Kind == yaml.ScalarNode {
				mounts[volume.Value] = struct{}{}
			}
		}
	}
	return mounts
}

func renderAdditionalFiles(input Input) ([]RenderedFile, error) {
	if len(input.AdditionalFiles) == 0 {
		return nil, nil
	}
	rendered := make([]RenderedFile, 0, len(input.AdditionalFiles))
	for _, file := range input.AdditionalFiles {
		tmpl, err := template.New("additional-file").Option("missingkey=error").Parse(file.Template)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrAdditionalFileTemplateParse, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, input.Values); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrAdditionalFileTemplateExecute, err)
		}
		rendered = append(rendered, RenderedFile{
			Dest:  file.Dest,
			Mode:  file.Mode,
			Bytes: buf.Bytes(),
		})
	}
	return rendered, nil
}

// renderConfigGeneration handles the [Input.ConfigGeneration] path in
// fail-closed order: traversal-check every dest first (cheapest, and a
// PRD §12/§13 safety gate), then verify declared mounts against the
// already-parsed services node, then render the artifacts. Returns nil
// for an empty slice so [RenderedStack.ConfigArtifacts] stays nil and
// curated apps without config_generation are unaffected.
func renderConfigGeneration(services *yaml.Node, input Input) ([]RenderedFile, error) {
	if err := validateConfigArtifactDests(input.ConfigGeneration); err != nil {
		return nil, err
	}
	if err := verifyConfigArtifactMounts(services, input.ConfigGeneration); err != nil {
		return nil, err
	}
	return renderConfigArtifacts(input)
}

// configArtifactDestSafe reports whether dest is a safe stack-relative
// file path. fs.ValidPath already rejects an empty path, an absolute or
// leading-slash path, any "." or ".." path element, and a trailing
// slash; the extra dest != "." then rejects the stack directory itself.
// This is an independent render-boundary traversal gate, not a superset
// of the catalog schema: the schema applies its own character-class and
// ".." checks upstream, while render fails closed on the path SHAPE so a
// schema gap can never let a write escape the stack directory (PRD
// §12/§13). It is a shape gate only — defense against wdm-reserved
// destination names and cross-artifact collisions belongs to the
// install/update writer, not here.
func configArtifactDestSafe(dest string) bool {
	return fs.ValidPath(dest) && dest != "."
}

// validateConfigArtifactDests traversal-checks every config artifact
// dest in slice order and returns on the first unsafe one, wrapping
// [ErrConfigArtifactDestUnsafe]. Naming the dest is safe: it is catalog
// metadata, not a resolved value from [Input.Values].
func validateConfigArtifactDests(arts []ConfigArtifact) error {
	for _, art := range arts {
		if !configArtifactDestSafe(art.Dest) {
			return fmt.Errorf(
				"render: config artifact dest %q is not a safe stack-relative path: %w",
				art.Dest,
				ErrConfigArtifactDestUnsafe,
			)
		}
	}
	return nil
}

func verifyConfigArtifactMounts(services *yaml.Node, arts []ConfigArtifact) error {
	if len(arts) == 0 {
		return nil
	}
	mounts := collectVolumeMounts(services)
	for _, art := range arts {
		if art.Mount == "" {
			continue
		}
		if _, ok := mounts[art.Mount]; !ok {
			return fmt.Errorf(
				"render: config artifact %q declares mount %q absent from compose volumes: %w",
				art.Dest,
				art.Mount,
				ErrConfigArtifactMountMissing,
			)
		}
	}
	return nil
}

func renderConfigArtifacts(input Input) ([]RenderedFile, error) {
	if len(input.ConfigGeneration) == 0 {
		return nil, nil
	}
	rendered := make([]RenderedFile, 0, len(input.ConfigGeneration))
	for _, art := range input.ConfigGeneration {
		tmpl, err := template.New("config-artifact").Option("missingkey=error").Parse(art.Template)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrConfigArtifactTemplateParse, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, input.Values); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrConfigArtifactTemplateExecute, err)
		}
		rendered = append(rendered, RenderedFile{
			Dest:  art.Dest,
			Mode:  art.Mode,
			Bytes: buf.Bytes(),
		})
	}
	return rendered, nil
}

// documentRoot returns the inner content node of a yaml.v3
// DocumentNode (its first Content entry is the document's top-level
// value). Returns nil for a nil or empty document, and the node
// itself for a non-document node.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	return doc
}

// findMapValue returns the value node paired with key in a yaml.v3
// MappingNode (whose Content slice interleaves key, value). Returns
// nil if not found, or if m is nil or not a MappingNode.
func findMapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// injectLabel writes the key="value" entry into the service's
// labels: mapping. If labels is absent, a new mapping is appended to
// the service. If labels exists in a non-mapping form (sequence,
// scalar), it is left alone — the post-injection walk in
// [RenderLabels] surfaces it as [ErrServiceMissingLabel] keyed by
// service name. An existing entry for key is overwritten in place to
// preserve YAML ordering; a missing entry is appended. The value
// scalar is tagged "!!str" and styled DoubleQuotedStyle so the
// encoder emits it quoted, matching the curated v1 template
// convention.
func injectLabel(service *yaml.Node, key, value string) {
	labels := findMapValue(service, "labels")
	if labels == nil {
		labelsKey := &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!str",
			Value: "labels",
		}
		labelsVal := &yaml.Node{
			Kind: yaml.MappingNode,
			Tag:  "!!map",
		}
		service.Content = append(service.Content, labelsKey, labelsVal)
		labels = labelsVal
	}
	if labels.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(labels.Content); i += 2 {
		if labels.Content[i].Kind == yaml.ScalarNode && labels.Content[i].Value == key {
			labels.Content[i+1].Kind = yaml.ScalarNode
			labels.Content[i+1].Tag = "!!str"
			labels.Content[i+1].Value = value
			labels.Content[i+1].Style = yaml.DoubleQuotedStyle
			return
		}
	}
	labels.Content = append(labels.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle},
	)
}

// collectLabels returns a service's labels mapping as a
// string-to-string snapshot for post-injection validation and
// [RenderedStack.ServiceLabels]. Returns nil if labels is absent or
// not a yaml.v3 MappingNode, in which case the post-injection walk in
// [RenderLabels] surfaces the service as [ErrServiceMissingLabel].
func collectLabels(service *yaml.Node) map[string]string {
	labels := findMapValue(service, "labels")
	if labels == nil || labels.Kind != yaml.MappingNode {
		return nil
	}
	m := make(map[string]string, len(labels.Content)/2)
	for i := 0; i+1 < len(labels.Content); i += 2 {
		if labels.Content[i].Kind != yaml.ScalarNode {
			continue
		}
		m[labels.Content[i].Value] = labels.Content[i+1].Value
	}
	return m
}
