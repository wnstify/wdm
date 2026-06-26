package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"text/template"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/pkg/types"
)

func (e *Engine) renderInstall(
	ctx context.Context,
	plan *installPlan,
	onProgress types.ProgressFn,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(types.StepInstallRender, 25, "rendering install")
	}
	// Generate-then-bind is one inseparable step: the redactor is built from
	// the generated-secret set the same call mints, so it cannot be bound
	// before generation completes (issue #120 ordering invariant).
	redactor, err := plan.generateSecretsAndBindRedactor(e.generateSecret, e.generateArgon2idCredential)
	if err != nil {
		return err
	}

	input, err := e.installRenderInput(ctx, plan)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return redactedVerificationError(
			redactor,
			"install templates could not be loaded",
			"refresh the catalog and retry",
			err,
		)
	}
	envStack, err := render.RenderEnv(input)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"env template could not be rendered",
			"refresh the catalog and retry",
			err,
		)
	}
	composeStack, err := render.RenderLabels(input)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"compose template could not be rendered",
			"refresh the catalog and retry",
			err,
		)
	}

	plan.rendered = render.RenderedStack{
		ComposeBytes:    composeStack.ComposeBytes,
		EnvBytes:        envStack.EnvBytes,
		AdditionalFiles: composeStack.AdditionalFiles,
		ConfigArtifacts: composeStack.ConfigArtifacts,
		ServiceLabels:   composeStack.ServiceLabels,
		VolumeMounts:    composeStack.VolumeMounts,
	}

	// A PortOverrides remap only reaches the deployed binding by editing the
	// rendered compose (host ports are literal template ints, ADR 0004). Do it
	// before the §11.1 bind scans below so they validate the rewritten compose.
	if len(plan.portOverrides) > 0 {
		rewritten, err := rewriteComposeHostPorts(plan.rendered.ComposeBytes, plan.portOverrides)
		if err != nil {
			return redactedVerificationError(
				redactor,
				"remapped compose could not be produced",
				"retry without --port, or choose a different host port",
				err,
			)
		}
		plan.rendered.ComposeBytes = rewritten
	}

	guidance, err := buildInstallGuidance(plan)
	if err != nil {
		return redactedVerificationError(
			redactor,
			"install guidance could not be rendered",
			"refresh the catalog and retry",
			err,
		)
	}
	plan.guidance = guidance
	if err := verifyImagePinsMatchTemplate(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifyPublicBindsMatchCatalog(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifyContainerPrivilegeMatchCatalog(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifySocketPolicyMatchCatalog(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifyHostModuleMountMatchCatalog(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifyNetworkIPAMMatchCatalog(redactor, plan.app, plan.rendered.ComposeBytes); err != nil {
		return err
	}
	if err := verifyCompletedServicesMatchCatalog(plan.app, plan.rendered.ServiceLabels); err != nil {
		return err
	}
	// Generated secrets plus sensitive --set values are both forbidden from
	// non-secret artifacts; sensitive values fail closed against inline
	// rendering exactly as on the update and reconfigure paths. argon2id
	// one-time plaintexts are deliberately excluded here (they never enter
	// generatedValues), keeping the leak-check scope unchanged for every app.
	leakSecrets := append(slices.Clone(plan.generatedValues), sensitiveSetValues(plan)...)
	if err := verifyRenderedNonSecretArtifacts(redactor, leakSecrets, plan.rendered, guidance); err != nil {
		return err
	}
	// End of the producing region: secrets are minted, the redactor is
	// bound, the stack is rendered and leak-checked. Freeze so every later
	// install phase consumes a read-only plan (issue #120).
	return plan.freeze()
}

// buildInstallGuidance assembles the post-install guidance from the
// catalog's structured fields. The local
// target URL comes from local_target_url_template rendered against
// the resolved placeholder map, falling back to the first declared
// port as http://127.0.0.1:<port> per the confirmation rules. Pangolin guidance
// is omitted when the catalog entry carries no guidance content, so
// JSON consumers see the field dropped, not empty.
func buildInstallGuidance(plan *installPlan) (*types.PostInstallGuidance, error) {
	localTargetURL, err := renderInstallLocalTargetURL(plan)
	if err != nil {
		return nil, err
	}

	firstRunNotes := append([]string(nil), plan.app.FirstRunNotes...)
	firstRunNotes = append(firstRunNotes, containerPrivilegeDisclosureLines(plan.app)...)

	guidance := &types.PostInstallGuidance{
		LocalTargetURL: localTargetURL,
		FirstRunNotes:  firstRunNotes,
	}
	// One-time credential plaintexts ride on the guidance struct for the
	// human finish screen only. They are deliberately NOT fed to
	// guidanceText() (the non-secret leak check), so the plaintext is never
	// inspected against the rendered artifacts, and the json:"-" tag keeps
	// them out of every JSON envelope (PRD §24).
	if len(plan.shownCredentials) > 0 {
		guidance.GeneratedCredentials = append([]types.GeneratedCredential(nil), plan.shownCredentials...)
	}
	pangolin := plan.app.PangolinGuidance
	if pangolin.TargetURL != "" || pangolin.RecommendedSubdomain != "" || len(pangolin.Notes) > 0 {
		guidance.Pangolin = &types.PangolinGuidance{
			TargetURL:            pangolin.TargetURL,
			RecommendedSubdomain: pangolin.RecommendedSubdomain,
			Notes:                append([]string(nil), pangolin.Notes...),
		}
	}
	return guidance, nil
}

func renderInstallLocalTargetURL(plan *installPlan) (string, error) {
	if plan.app.LocalTargetURLTemplate == "" {
		if len(plan.localPorts) == 0 {
			return "", nil
		}
		return fmt.Sprintf("http://127.0.0.1:%d", plan.localPorts[0].HostPort), nil
	}

	tmpl, err := template.New("local-target-url").
		Option("missingkey=error").
		Parse(plan.app.LocalTargetURLTemplate)
	if err != nil {
		return "", fmt.Errorf("parse local_target_url_template: %w", err)
	}
	var rendered strings.Builder
	if err := tmpl.Execute(&rendered, plan.resolvedValues); err != nil {
		return "", fmt.Errorf("render local_target_url_template: %w", err)
	}
	return rendered.String(), nil
}

// guidanceText flattens the guidance strings for the non-secret
// artifact verification pass. Guidance reaches terminal output, JSON
// envelopes, and logs, so a generated secret must never appear in it.
func guidanceText(guidance *types.PostInstallGuidance) []byte {
	if guidance == nil {
		return nil
	}
	parts := []string{guidance.LocalTargetURL}
	parts = append(parts, guidance.FirstRunNotes...)
	if guidance.Pangolin != nil {
		parts = append(parts, guidance.Pangolin.TargetURL, guidance.Pangolin.RecommendedSubdomain)
		parts = append(parts, guidance.Pangolin.Notes...)
	}
	return []byte(strings.Join(parts, "\n"))
}

func (e *Engine) installRenderInput(ctx context.Context, plan *installPlan) (render.Input, error) {
	if err := ctx.Err(); err != nil {
		return render.Input{}, err
	}
	catalogFS := e.installCatalogFS()
	composeTemplate, err := readCatalogTemplate(catalogFS, plan.app.ComposeTemplate)
	if err != nil {
		return render.Input{}, err
	}
	envTemplate, err := readCatalogTemplate(catalogFS, plan.app.EnvTemplate)
	if err != nil {
		return render.Input{}, err
	}
	additionalFiles, err := readAdditionalFileTemplates(catalogFS, plan.app)
	if err != nil {
		return render.Input{}, err
	}
	configGeneration, err := readConfigGenerationTemplates(catalogFS, plan.app)
	if err != nil {
		return render.Input{}, err
	}

	values := make(map[string]string, len(plan.resolvedValues))
	for key, value := range plan.resolvedValues {
		values[key] = value
	}
	placeholders := append([]render.Placeholder(nil), plan.placeholders...)
	return render.Input{
		EnvTemplate:      string(envTemplate),
		ComposeTemplate:  string(composeTemplate),
		Placeholders:     placeholders,
		Values:           values,
		AppID:            plan.app.AppID,
		AdditionalFiles:  additionalFiles,
		ConfigGeneration: configGeneration,
	}, nil
}

func readCatalogTemplate(catalogFS fs.FS, name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, catalogVerificationError(
			"catalog template path is invalid",
			"refresh the catalog and retry",
			fmt.Errorf("template path %q is invalid", name),
		)
	}
	raw, err := fs.ReadFile(catalogFS, name)
	if err != nil {
		return nil, fmt.Errorf("read template %q: %w", name, err)
	}
	return raw, nil
}

func readAdditionalFileTemplates(catalogFS fs.FS, app catalog.App) ([]render.AdditionalFile, error) {
	if len(app.AdditionalFiles) == 0 {
		return nil, nil
	}
	templateDir := path.Dir(app.ComposeTemplate)
	files := make([]render.AdditionalFile, 0, len(app.AdditionalFiles))
	for _, file := range app.AdditionalFiles {
		if !fs.ValidPath(file.Src) {
			return nil, catalogVerificationError(
				"catalog additional file source path is invalid",
				"refresh the catalog and retry",
				fmt.Errorf("additional file source path %q is invalid", file.Src),
			)
		}
		raw, err := readCatalogTemplate(catalogFS, path.Join(templateDir, file.Src))
		if err != nil {
			return nil, err
		}
		files = append(files, render.AdditionalFile{
			Dest:     file.Dest,
			Mode:     file.Mode,
			Mount:    file.Mount,
			Template: string(raw),
		})
	}
	return files, nil
}

// readConfigGenerationTemplates reads each catalog config_generation
// template off the catalog FS and projects it into the render-local
// [render.ConfigArtifact] shape so [render.RenderLabels] can render the
// artifact, verify its declared mount, and traversal-check its dest. The
// template path is resolved relative to the app's template directory,
// mirroring readAdditionalFileTemplates; both kinds are catalog-declared
// templates rendered in memory before any disk write (PRD §17).
func readConfigGenerationTemplates(catalogFS fs.FS, app catalog.App) ([]render.ConfigArtifact, error) {
	if len(app.ConfigGeneration) == 0 {
		return nil, nil
	}
	templateDir := path.Dir(app.ComposeTemplate)
	artifacts := make([]render.ConfigArtifact, 0, len(app.ConfigGeneration))
	for _, artifact := range app.ConfigGeneration {
		if !fs.ValidPath(artifact.Template) {
			return nil, catalogVerificationError(
				"catalog config artifact source path is invalid",
				"refresh the catalog and retry",
				fmt.Errorf("config artifact source path %q is invalid", artifact.Template),
			)
		}
		raw, err := readCatalogTemplate(catalogFS, path.Join(templateDir, artifact.Template))
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, render.ConfigArtifact{
			Dest:     artifact.Dest,
			Mode:     artifact.Mode,
			Mount:    artifact.Mount,
			Template: string(raw),
		})
	}
	return artifacts, nil
}
