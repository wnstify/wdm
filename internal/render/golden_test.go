package render_test

// Generated-file golden tests for the curated apps in the stable
// catalog (PRD §15). They pin the
// byte-exact rendered artifacts that the install path commits to disk:
// the label-injected docker-compose.yml ([render.RenderLabels]), the
// literal-valued .env ([render.RenderEnv]), and every catalog-declared
// additional_file (n8n's init-data.sh). The goldens live under
// fixtures/golden/<app_id>/ per ("fixtures/ … rendered
// golden outputs").
// # What this test proves
//   - The real production render functions, fed the real curated
//     templates and the real stable-catalog metadata (placeholder
//     lists, types, encodings, resource bands, additional_files),
//     produce exactly the committed golden bytes. A future template,
//     catalog, or renderer edit that changes any rendered byte fails
//     this test until the author regenerates and eyeballs the diff.
//   - Label injection cannot be silently dropped:
//     beyond byte-equality, [TestGoldenLabelInjectionCannotBeDropped]
//     re-parses each golden Compose and asserts every services.* entry
//     carries wdm.managed: "true" and wdm.app: <app_id>.
// # Why internal/render (and why it may import internal/catalog here)
// internal/render owns the byte-output contract these goldens pin
// (RenderEnv / RenderLabels). The test is black-box (package
// render_test) like the rest of the package's tests. It reads the real
// catalog through internal/catalog purely to source the real
// placeholder/resource metadata instead of hand-copying it — and the
// .golangci.yml exclusions block exempts _test.go files from depguard,
// so this test-only import does not breach the internal-render-pure
// boundary that still binds production code. Rendering still flows
// through the real [render.RenderEnv] / [render.RenderLabels]; nothing
// here re-implements the renderer.
// # How the render.Input is built (faithful to internal/core)
// buildInput mirrors internal/core's install render wiring
// (internal/core/install.go: planPlaceholders → planResources →
// generateInstallSecrets → installRenderInput): every catalog placeholder is
// projected to a [render.Placeholder] verbatim (Name/Type/Required/
// Default/Regenerable), the built-in UID/GID vars and the per-service
// MEMORY_LIMIT_/CPUS_LIMIT_/PIDS_LIMIT_<SERVICE_KEY> resource vars are
// added as synthetic string placeholders, and the value map is filled
// to exactly that key set so [render.ValidateResolution] (every
// Required key present, no extra keys) passes the way it does in
// production.
// # Value-pinning policy (determinism)
// Every value is a pinned, obviously-fake constant — these bytes land
// in committed golden.env files, so secrets must never look real or
// trip a scanner, and NO real internal/security.GenerateSecret output
// is ever used:
//   - secret placeholders → "TESTONLY-FAKE-SECRET-<NAME>-0000…" (the
//     unmistakable marker proves the value is synthetic).
//   - timezone → "Etc/UTC"; domain → "n8n.test.example"; path
//     placeholders → fixed "/srv/test/<name>" stand-ins.
//   - string placeholders with a catalog Default → the catalog Default
//     itself (so the golden documents the default rendering).
//   - built-in UID/GID → 1000/1000.
//   - MEMORY_LIMIT_/CPUS_LIMIT_ → each service's catalog *recommended*
//     band value; PIDS_LIMIT_ → the catalog pids *default*. Pinning to
//     the recommended band (the value install selects on a host with
//     ample budget and no overrides) makes the goldens double as
//     documentation of the recommended rendering.
// # Regeneration
//	go test./internal/render -run TestGolden -update
// rewrites every golden from current render output. The -update run is
// NOT a rubber stamp: always eyeball the resulting diff before
// committing — a wrong template edit and its regenerated golden would
// otherwise agree with each other. The committed suite is generated
// with -update and then proven stable by a plain (no -update) run.

import (
	"context"
	"flag"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/render"
)

// update, when set via `-update`, rewrites the golden files from the
// current render output instead of comparing against them. See the
// file doc comment's regeneration note — always eyeball the diff.
var update = flag.Bool("update", false, "rewrite golden render outputs in fixtures/golden")

const (
	// repoRoot is the source-tree root relative to this test file
	// (internal/render sits two levels below it). Catalog template
	// paths (app.ComposeTemplate / app.EnvTemplate, e.g.
	// "templates/n8n/docker-compose.yml.tmpl") are catalog-FS-relative:
	// internal/core resolves them against the runtime catalog FS root
	// (~/.local/share/wdm/catalogs/<channel>), while the source tree
	// lays the same content out at the repo root, so the test joins
	// them against repoRoot via repoPath rather than reconstructing
	// the runtime FS shape.
	repoRoot    = "../.."
	catalogPath = repoRoot + "/catalog/stable/catalog.yaml"

	// goldenRoot is where the rendered artifacts are committed
	// One subdirectory per app_id.
	goldenRoot = repoRoot + "/fixtures/golden"

	// fakeSecretPrefix marks every test-pinned secret value so the
	// committed golden.env files can never be mistaken for real
	// credentials and a secret scan over the goldens stays clean. The
	// trailing run of zeros keeps the rendered value length-plausible
	// without carrying any entropy.
	fakeSecretPrefix = "TESTONLY-FAKE-SECRET-"
	fakeSecretSuffix = "-0000000000000000000000000000"

	// testUID / testGID pin the built-in.UID /.GID template vars
	// (internal/core resolves these from os.Getuid/os.Getgid; the
	// golden uses a fixed pair so the rendered PUID/PGID lines never
	// depend on the machine running the test).
	testUID = "1000"
	testGID = "1000"

	// testTimezone / testDomain pin the timezone and domain
	// placeholders. internal/core resolves an empty timezone to the
	// host zone and a domain from --domain; the golden uses fixed
	// stand-ins so the rendered TZ / N8N_HOST lines stay stable.
	testTimezone = "Etc/UTC"
	testDomain   = "n8n.test.example"
)

// TestGoldenRenderedArtifacts renders every curated v1 app through the
// real [render.RenderEnv] / [render.RenderLabels] and asserts the
// rendered .env, docker-compose.yml, and any additional_files match
// the committed goldens byte-for-byte. With -update it rewrites them.
func TestGoldenRenderedArtifacts(t *testing.T) {
	t.Parallel()

	cat := loadStableCatalog(t)

	// The twenty-one curated apps must all be present; a missing entry is a
	// catalog regression, not a skip.
	require.Len(t, cat.Apps, 21, "stable catalog must carry the twenty-one curated apps")

	for _, app := range cat.Apps {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			input := buildInput(t, app)

			envStack, err := render.RenderEnv(input)
			require.NoError(t, err, "RenderEnv(%s)", app.AppID)

			composeStack, err := render.RenderLabels(input)
			require.NoError(t, err, "RenderLabels(%s)", app.AppID)

			appGoldenDir := filepath.Join(goldenRoot, app.AppID)

			assertGolden(t, filepath.Join(appGoldenDir, ".env"), envStack.EnvBytes)
			assertGolden(t, filepath.Join(appGoldenDir, "docker-compose.yml"), composeStack.ComposeBytes)

			for _, file := range composeStack.AdditionalFiles {
				// file.Dest is the stack-relative destination (e.g.
				// "init-data.sh"); the golden mirrors that name under
				// the app dir so nested-path sidecars stay distinct.
				assertGolden(t, filepath.Join(appGoldenDir, filepath.FromSlash(file.Dest)), file.Bytes)
			}

			for _, artifact := range composeStack.ConfigArtifacts {
				// Config-generation artifacts (e.g. meshcentral's
				// config.json) are pinned the same way as additional
				// files: the golden mirrors the stack-relative dest under
				// the app dir.
				assertGolden(t, filepath.Join(appGoldenDir, filepath.FromSlash(artifact.Dest)), artifact.Bytes)
			}
		})
	}
}

func TestStoatLiveKitUsesRenderedInstallUser(t *testing.T) {
	t.Parallel()

	cat := loadStableCatalog(t)
	var stoat catalog.App
	for _, app := range cat.Apps {
		if app.AppID == "stoat" {
			stoat = app
			break
		}
	}
	require.NotEmpty(t, stoat.AppID, "stable catalog must carry stoat")

	input := buildInput(t, stoat)
	input.Values["UID"] = "1234"
	input.Values["GID"] = "2345"

	stack, err := render.RenderLabels(input)
	require.NoError(t, err)

	var doc struct {
		Services map[string]struct {
			User string `yaml:"user"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(stack.ComposeBytes, &doc))
	require.Contains(t, doc.Services, "livekit")
	assert.Equal(t, "1234:2345", doc.Services["livekit"].User)
}

// TestGoldenLabelInjectionCannotBeDropped re-parses each committed
// golden docker-compose.yml and asserts every services.* entry carries
// the injected wdm.managed="true" and wdm.app=<app_id> labels. This is
// the omission proof: byte-equality alone would let a
// future golden regeneration silently ship a Compose file with the
// labels stripped, so this test reads the goldens independently of the
// renderer and checks the labels are really there.
func TestGoldenLabelInjectionCannotBeDropped(t *testing.T) {
	if *update {
		// Nothing to verify while -update is rewriting the goldens —
		// TestGoldenRenderedArtifacts owns generation, and reading
		// half-written goldens here would race it.
		t.Skip("golden regeneration in progress (-update)")
	}
	t.Parallel()

	cat := loadStableCatalog(t)

	for _, app := range cat.Apps {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			golden, err := os.ReadFile(filepath.Join(goldenRoot, app.AppID, "docker-compose.yml"))
			require.NoError(t, err, "read golden compose for %s (run -update first?)", app.AppID)

			services := composeServices(t, golden)
			require.NotEmpty(t, services, "golden compose for %s declares no services", app.AppID)

			for serviceName, labels := range services {
				assert.Equal(
					t,
					"true",
					labels["wdm.managed"],
					"service %q in %s golden is missing wdm.managed=true", serviceName, app.AppID,
				)
				assert.Equal(
					t,
					app.AppID,
					labels["wdm.app"],
					"service %q in %s golden has wrong wdm.app label", serviceName, app.AppID,
				)
			}
		})
	}
}

// TestGoldenEveryServiceCarriesUserOverlay re-parses each committed
// golden docker-compose.yml and asserts every services.* entry lists
// the per-stack .env.user overlay in its env_file, and that nothing but
// .env may precede it. Compose applies later env_file entries over
// earlier ones, so .env.user appearing after a generated-secret file
// would let a user's overlay silently override a generated secret; the
// ordering arm is the machine check that keeps .env.user ahead of every
// secret file (only the literal-default .env is an allowed predecessor).
func TestGoldenEveryServiceCarriesUserOverlay(t *testing.T) {
	if *update {
		t.Skip("golden regeneration in progress (-update)")
	}
	t.Parallel()

	cat := loadStableCatalog(t)

	for _, app := range cat.Apps {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			golden, err := os.ReadFile(filepath.Join(goldenRoot, app.AppID, "docker-compose.yml"))
			require.NoError(t, err, "read golden compose for %s (run -update first?)", app.AppID)

			services := composeEnvFiles(t, golden)
			require.NotEmpty(t, services, "golden compose for %s declares no services", app.AppID)

			for serviceName, files := range services {
				idx := slices.Index(files, ".env.user")
				require.GreaterOrEqualf(
					t,
					idx,
					0,
					"service %q in %s golden is missing .env.user in env_file (got %v)", serviceName, app.AppID, files,
				)
				for _, before := range files[:idx] {
					assert.Equalf(
						t,
						".env",
						before,
						"service %q in %s golden lists %q before .env.user; only .env may precede the overlay", serviceName, app.AppID, before,
					)
				}
			}
		})
	}
}

// composeEnvFiles parses a rendered Compose document and returns each
// service's env_file entries in order, reading either the scalar form
// (env_file: file) or the sequence form (env_file: [a, b]) the same way
// decodeLabels handles labels. Used by the overlay enforcement proof to
// inspect the golden bytes independently of the renderer.
func composeEnvFiles(t *testing.T, composeBytes []byte) map[string][]string {
	t.Helper()

	var doc struct {
		Services map[string]struct {
			EnvFile yaml.Node `yaml:"env_file"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(composeBytes, &doc), "parse rendered compose")

	out := make(map[string][]string, len(doc.Services))
	for name, svc := range doc.Services {
		out[name] = decodeEnvFile(t, svc.EnvFile)
	}

	return out
}

// decodeEnvFile reads a Compose service env_file node in either
// supported form (scalar string or sequence of strings) into an ordered
// slice.
func decodeEnvFile(t *testing.T, node yaml.Node) []string {
	t.Helper()

	switch node.Kind {
	case 0:
		return nil
	case yaml.ScalarNode:
		var single string
		require.NoError(t, node.Decode(&single), "decode scalar-form env_file")
		return []string{single}
	case yaml.SequenceNode:
		var files []string
		require.NoError(t, node.Decode(&files), "decode sequence-form env_file")
		return files
	default:
		t.Fatalf("unexpected env_file node kind %d", node.Kind)
		return nil
	}
}

// loadStableCatalog loads and validates the real stable catalog from
// the source tree, failing the test on any error so a malformed
// catalog cannot silently skip the golden coverage.
func loadStableCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()

	cat, err := catalog.LoadCatalog(context.Background(), mustAbs(t, catalogPath))
	require.NoError(t, err, "load stable catalog")
	require.NotNil(t, cat)

	return cat
}

// buildInput assembles the [render.Input] for one app, mirroring
// internal/core's install render wiring: real templates read from the
// source tree, catalog placeholders projected verbatim, and the
// synthetic built-in / resource placeholders plus their pinned values
// added so [render.ValidateResolution] passes exactly as it does in
// production.
func buildInput(t *testing.T, app catalog.App) render.Input {
	t.Helper()

	// Template paths come from the catalog fields production reads
	// (install.go reads app.ComposeTemplate / app.EnvTemplate; nothing
	// guarantees the conventional <app_id>/ filenames), so a catalog
	// repointing a template cannot leave the goldens silently pinned
	// to the wrong file. The additional-files dir mirrors
	// readAdditionalFileTemplates' path.Dir(ComposeTemplate) join.
	composeTemplate := mustReadFile(t, repoPath(app.ComposeTemplate))
	envTemplate := mustReadFile(t, repoPath(app.EnvTemplate))
	templateDir := repoPath(path.Dir(app.ComposeTemplate))

	placeholders := make([]render.Placeholder, 0, len(app.Placeholders))
	values := map[string]string{}

	for _, ph := range app.Placeholders {
		placeholders = append(placeholders, render.Placeholder{
			Name:        ph.Name,
			Type:        render.Type(ph.Type),
			Required:    ph.Required,
			Default:     ph.Default,
			Regenerable: ph.Regenerable,
		})
		values[ph.Name] = pinnedValue(t, ph)
	}

	// Built-in template vars
	// and per-service resource limits are synthetic string
	// placeholders in internal/core; reproduce them so the value set
	// matches the .env.tmpl's references.
	addSynthetic(&placeholders, values, "UID", testUID)
	addSynthetic(&placeholders, values, "GID", testGID)

	for _, profile := range app.Resources {
		key := serviceKey(profile.Service)
		addSynthetic(&placeholders, values, "MEMORY_LIMIT_"+key, profile.Memory.Recommended)
		addSynthetic(&placeholders, values, "CPUS_LIMIT_"+key, profile.CPUs.Recommended)
		addSynthetic(&placeholders, values, "PIDS_LIMIT_"+key, strconv.Itoa(profile.PIDs.Default))
	}

	return render.Input{
		EnvTemplate:      string(envTemplate),
		ComposeTemplate:  string(composeTemplate),
		Placeholders:     placeholders,
		Values:           values,
		AppID:            app.AppID,
		AdditionalFiles:  additionalFiles(t, app, templateDir),
		ConfigGeneration: configGeneration(t, app, templateDir),
	}
}

// pinnedValue returns the deterministic test value for one catalog
// placeholder per the file doc comment's value-pinning policy.
func pinnedValue(t *testing.T, ph catalog.Placeholder) string {
	t.Helper()

	switch render.Type(ph.Type) {
	case render.TypeSecret:
		return fakeSecretPrefix + ph.Name + fakeSecretSuffix
	case render.TypeTimezone:
		return testTimezone
	case render.TypeDomain:
		return testDomain
	case render.TypePath:
		return "/srv/test/" + strings.ToLower(strings.TrimSuffix(ph.Name, "_PATH"))
	case render.TypeString:
		if ph.Default != nil {
			return defaultString(t, ph)
		}
		return "test-" + strings.ToLower(ph.Name)
	case render.TypeBool, render.TypePort:
		// No curated v1 app declares a bool or port placeholder, and a
		// pinned stand-in here could bake a value production refuses
		// (ports must be 1-65535; a required bool without a default is
		// refused) into a regenerated golden. Fail loudly so this arm
		// gets a real pinning policy when a curated app first needs it.
		t.Fatalf("placeholder %q: no golden pinning policy for type %q yet", ph.Name, ph.Type)
		return ""
	default:
		t.Fatalf("placeholder %q has unhandled type %q", ph.Name, ph.Type)
		return ""
	}
}

// defaultString renders a catalog placeholder Default (carried as any
// so a YAML scalar survives projection) into its.env string form. The
// curated v1 catalog only ships string defaults (CRON_MIN, UMASK), so
// a non-string default is a catalog drift the test refuses.
func defaultString(t *testing.T, ph catalog.Placeholder) string {
	t.Helper()

	s, ok := ph.Default.(string)
	require.Truef(t, ok, "placeholder %q default %v is not a string", ph.Name, ph.Default)

	return s
}

// additionalFiles projects the catalog additional_files for an app
// into [render.AdditionalFile] values with their template bytes read
// from the source tree, mirroring internal/core's
// readAdditionalFileTemplates (Src joined against the template dir).
func additionalFiles(t *testing.T, app catalog.App, templateDir string) []render.AdditionalFile {
	t.Helper()

	if len(app.AdditionalFiles) == 0 {
		return nil
	}

	files := make([]render.AdditionalFile, 0, len(app.AdditionalFiles))
	for _, file := range app.AdditionalFiles {
		body := mustReadFile(t, filepath.Join(templateDir, filepath.FromSlash(file.Src)))
		files = append(files, render.AdditionalFile{
			Dest:     file.Dest,
			Mode:     file.Mode,
			Mount:    file.Mount,
			Template: string(body),
		})
	}

	return files
}

// configGeneration projects the catalog config_generation entries for an
// app into [render.ConfigArtifact] values with their template bytes read
// from the source tree, mirroring internal/core's
// readConfigGenerationTemplates (Template joined against the template dir).
// The rendered artifacts are pinned as goldens alongside the .env and
// compose, so a curated config_generation app (meshcentral) double-checks
// its rendered config.json byte-for-byte.
func configGeneration(t *testing.T, app catalog.App, templateDir string) []render.ConfigArtifact {
	t.Helper()

	if len(app.ConfigGeneration) == 0 {
		return nil
	}

	artifacts := make([]render.ConfigArtifact, 0, len(app.ConfigGeneration))
	for _, artifact := range app.ConfigGeneration {
		body := mustReadFile(t, filepath.Join(templateDir, filepath.FromSlash(artifact.Template)))
		artifacts = append(artifacts, render.ConfigArtifact{
			Dest:     artifact.Dest,
			Mode:     artifact.Mode,
			Mount:    artifact.Mount,
			Template: string(body),
		})
	}

	return artifacts
}

// addSynthetic appends a synthetic required string placeholder and its
// value, matching internal/core's addSyntheticResolvedValue shape for
// built-in and resource-limit vars.
func addSynthetic(placeholders *[]render.Placeholder, values map[string]string, name, value string) {
	*placeholders = append(*placeholders, render.Placeholder{
		Name:     name,
		Type:     render.TypeString,
		Required: true,
	})
	values[name] = value
}

// serviceKey reproduces internal/core's SERVICE_KEY derivation
// (uppercase, non-alphanumeric runs collapsed to a single underscore,
// trimmed) so MEMORY_LIMIT_/CPUS_LIMIT_/PIDS_LIMIT_ keys match the
// .env.tmpl references. A small copy keeps this test free of an
// internal/core import (which depguard forbids render from importing in
// production and which would pull the orchestrator into a pure-render
// test).
func serviceKey(service string) string {
	var b strings.Builder

	lastUnderscore := false
	for _, r := range strings.ToUpper(service) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}

	return strings.Trim(b.String(), "_")
}

// composeServices parses a rendered Compose document and returns each
// service's label map (key → value), reading the labels from either
// the mapping form (labels: {k: v}) or the sequence form (labels:
// ["k=v"]). Used by the label-injection omission proof to inspect the
// golden bytes independently of the renderer.
func composeServices(t *testing.T, composeBytes []byte) map[string]map[string]string {
	t.Helper()

	var doc struct {
		Services map[string]struct {
			Labels yaml.Node `yaml:"labels"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(composeBytes, &doc), "parse rendered compose")

	out := make(map[string]map[string]string, len(doc.Services))
	for name, svc := range doc.Services {
		out[name] = decodeLabels(t, svc.Labels)
	}

	return out
}

// decodeLabels reads a Compose service labels node in either supported
// form (mapping or "key=value" sequence) into a flat map.
func decodeLabels(t *testing.T, node yaml.Node) map[string]string {
	t.Helper()

	labels := map[string]string{}

	switch node.Kind {
	case 0:
		// No labels node at all.
	case yaml.MappingNode:
		require.NoError(t, node.Decode(&labels), "decode mapping-form labels")
	case yaml.SequenceNode:
		var pairs []string
		require.NoError(t, node.Decode(&pairs), "decode sequence-form labels")
		for _, pair := range pairs {
			key, value, _ := strings.Cut(pair, "=")
			labels[key] = value
		}
	default:
		t.Fatalf("unexpected labels node kind %d", node.Kind)
	}

	return labels
}

// assertGolden compares got against the golden file at goldenFile, or
// rewrites the golden when -update is set. On mismatch it reports the
// golden path and the rendered string so the diff is readable in the
// failure output.
func assertGolden(t *testing.T, goldenFile string, got []byte) {
	t.Helper()

	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenFile), 0o755))
		require.NoError(t, os.WriteFile(goldenFile, got, 0o644), "write golden %s", goldenFile)
		return
	}

	want, err := os.ReadFile(goldenFile)
	require.NoErrorf(t, err, "read golden %s (run `go test ./internal/render -run TestGolden -update` to create it)", goldenFile)

	assert.Equalf(t, string(want), string(got), "rendered output for %s differs from golden; rerun with -update after eyeballing the diff", goldenFile)
}

// repoPath resolves a catalog-FS-relative path (e.g.
// "templates/n8n/docker-compose.yml.tmpl") against the source tree,
// which lays the catalog FS content out at the repo root.
func repoPath(rel string) string {
	return filepath.Join(repoRoot, filepath.FromSlash(rel))
}

// mustAbs resolves a repo-relative test path to an absolute one
// ([catalog.LoadCatalog] requires an absolute path).
func mustAbs(t *testing.T, rel string) string {
	t.Helper()

	abs, err := filepath.Abs(rel)
	require.NoError(t, err, "resolve %s", rel)

	return abs
}

// mustReadFile reads a source-tree file, failing the test on error.
func mustReadFile(t *testing.T, name string) []byte {
	t.Helper()

	raw, err := os.ReadFile(name)
	require.NoErrorf(t, err, "read %s", name)

	return raw
}
