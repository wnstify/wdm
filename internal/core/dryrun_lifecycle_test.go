package core_test

// No-real-Docker lifecycle proof for every curated stable-catalog app (PRD
// §15). Install is exercised end-to-end against MOCKED Docker with
// all filesystem state in tempdirs, and update is exercised in literal
// types.UpdateRequest.DryRun mode.
// # What this file proves that the rest of the suite does not
// Every other internal/core test drives the SYNTHETIC appFixture catalog
// and synthetic one-line templates. The c34/c35 update tests
// (update_test.go: TestUpdate_DryRunUpToDateIsNoUpdateOutcome,
// TestUpdate_DryRunGroupsRiskPerCatalogClass,
// TestUpdate_DryRunRecordsOldToNewImageReferences) already pin the DryRun
// outcome shape, but only over appFixture and a HAND-WRITTEN.wdm.lock —
// never over the real stable catalog's curated apps nor over a manifest
// the real Install path actually committed.
// This file closes that gap: it drives the REAL stable catalog
// (catalog/stable/catalog.yaml) and the REAL curated templates
// (templates/<app>/...) from the source tree through the real engine
// APIs — eng.Install then eng.Update(DryRun) — with mocked Docker and a
// tempdir stack. It proves end-to-end that
//   - each curated app's real templates + catalog metadata survive the
//     full plan → render → write → manifest path into a tempdir stack
//     (real.env at 0o600, real label-injected docker-compose.yml, real
//     .wdm.lock with app identity, image pins, recommended resources, and
//     the install-kind last_successful_operation); and
//   - a subsequent Update(DryRun) against the unchanged catalog reads
//     that real manifest and reports up-to-date — a no-op check with no
//     confirmer consulted and zero file mutations (byte-compared) and no
//     backup; while
//   - a programmatically tag-bumped catalog variant reports the
//     transition with the per-service old → new references and the
//     catalog risk classifications copied verbatim, still with zero
//     mutations and no confirmer (DryRun never consults it per c34/c35).
// # Determinism: the fixed-host-port problem
// The real catalog pins fixed host ports (e.g. uptime-kuma 3008, freshrss
// 8088, jellyfin 8096, n8n 5678) and install does a REAL net.Listen pre-check
// on <host-ip>:<host-port> (install.go checkPortAvailable — there is no test
// seam for it). On a busy CI host those ports could be taken, making the
// install pass or fail on port luck. The host port is the only
// environment-sensitive input that can flip the install between pass and
// fail (UID/GID are environment-derived too but never refuse), and the catalog
// ports[].host field is metadata the installer probes (the Compose
// template hardcodes the bind), so realCatalogFSForApp rewrites each app's
// LOCALHOST ports[].host to an OS-allocated free ephemeral port
// (freeLocalTCPPort) — exactly what appFixture(appID, port) already does
// across this suite — before building the catalog FS. PUBLIC ports keep their
// catalog host port: the public-bind scan matches them against the template's
// hardcoded public bind, so rewriting them would fail the scan (see
// rewriteHostPorts). Every other catalog field, every template byte, and the
// whole plan → render → write → deploy → manifest path stays real; only the
// localhost probed integers change, so the pre-check binds a guaranteed-free
// port and the mocked post-deploy inspect reports every bound port back so the
// install-time port match is satisfied.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

const (
	// realCatalogPath is the real stable catalog in the source tree,
	// relative to this test file (internal/core sits two levels below
	// the repo root). The golden test (internal/render/golden_test.go)
	// loads the same file the same way.
	realCatalogPath = "../../catalog/stable/catalog.yaml"

	// realCatalogChannel is the channel directory the engine reads
	// (<channel>/catalog.yaml); it matches the engine's default
	// settings.CatalogChannel ("stable", engine.go:287).
	realCatalogChannel = "stable"

	// fakeSecretValue is the deterministic stand-in returned by the
	// stubbed secret generator so install never touches real entropy.
	// The obviously-synthetic marker keeps the assertion failure output
	// readable and could never be mistaken for a real credential.
	fakeSecretValue = "TESTONLY-FAKE-SECRET-0000000000000000000000"

	// fixedTestTimezone pins the TZ placeholder so install never falls
	// back to a host timezone lookup (which would be non-deterministic
	// across CI hosts without /etc/timezone or /etc/localtime).
	fixedTestTimezone = "Etc/UTC"
)

// TestRealCatalogInstall_ThenDryRunUpdateReportsUpToDate is the no-real-Docker
// lifecycle proof per curated app: a real Install against the real catalog
// commits the expected on-disk state into a tempdir stack, and a subsequent
// Update(DryRun) against the unchanged catalog reads that manifest and reports
// up-to-date as a pure no-op — no confirmer consulted, zero file mutations
// (byte-compared before/after), and no backup created. The no-op DryRun
// contract mirrors update_test.go's
// TestUpdate_DryRunUpToDateIsNoUpdateOutcome but over the REAL install output).
func TestRealCatalogInstall_ThenDryRunUpdateReportsUpToDate(t *testing.T) {
	t.Parallel()

	for _, app := range loadRealStableCatalogApps(t) {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			eng, stackPath, hostPort := installRealCuratedApp(t, app)

			// --- On-disk state the install committed. ---
			assertInstalledStackOnDisk(t, app, stackPath, hostPort)

			// --- Snapshot the stack before the DryRun so we can prove the
			//     check stage mutates nothing on disk. ---
			before := snapshotStackDir(t, stackPath)

			confirmer := &fakeConfirmer{}
			var steps []string
			var messages []string
			res, err := eng.Update(
				t.Context(),
				types.UpdateRequest{AppID: app.AppID, DryRun: true},
				func(step string, _ float64, message string) {
					steps = append(steps, step)
					messages = append(messages, message)
				},
				confirmer,
			)
			require.NoError(t, err)
			require.NotNil(t, res)

			// The manifest mirrors the catalog, so the check is a no-op:
			// equal versions, no changed services, no risk grouping.
			assert.Equal(t, app.AppID, res.AppID)
			assert.Equal(t, app.TemplateVersion, res.PreviousTemplateVersion)
			assert.Equal(t, app.TemplateVersion, res.NewTemplateVersion)
			assert.Empty(t, res.UpdatedServices, "an up-to-date stack changes no services")
			assert.Empty(t, res.RiskClassifications, "a no-op check has no candidate update to risk-group")
			assert.Empty(t, res.BackupPath, "a check takes no backup")
			assert.Nil(t, res.Status, "a check deploys nothing, so there is no status")
			assert.Contains(t, strings.Join(messages, "\n"),
				"already up to date at template version "+app.TemplateVersion)
			for _, step := range steps {
				assert.Equal(t, types.StepUpdatePlanning, step,
					"the check stage must emit only planning step events")
			}

			// DryRun never consults the confirmer (PRD §20; c34/c35).
			assert.Empty(t, confirmer.calls, "DryRun must never consult the confirmer")

			// The check stage mutates nothing on disk and creates no backup.
			assertStackDirUnchanged(t, stackPath, before)
			assert.NoDirExists(t, filepath.Join(stackPath, state.BackupDirName),
				"a DryRun check must not create a backup directory")
		})
	}
}

// TestRealCatalogDryRunUpdate_ReportsAvailableUpdate proves the update-available
// DryRun over the real install output: after a real Install, a DryRun against a
// programmatically tag-bumped variant of the real catalog (one service tag +
// template_version advanced, built from the real catalog — not a forked second
// fixture) reports the transition with the per-service old → new image
// references in UpdatedServices and on the planning stream, and the catalog risk
// classifications copied verbatim, while still consulting no confirmer and
// mutating no file (byte-compared). DryRun never consults the confirmer even when
// an update — including a database-risk one — is available (;
// c34/c35).
func TestRealCatalogDryRunUpdate_ReportsAvailableUpdate(t *testing.T) {
	t.Parallel()

	for _, app := range loadRealStableCatalogApps(t) {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			// Install against the real (current) catalog so the manifest
			// reflects the real install output. The installing engine is
			// not reused — the DryRun runs on a second engine pointed at the
			// bumped catalog but sharing this stack.
			_, stackPath, hostPort := installRealCuratedApp(t, app)
			before := snapshotStackDir(t, stackPath)

			// Build a bumped catalog variant from the real catalog: advance
			// the first service's tag and the template version for this app.
			bumpedFS, bumpedTag, bumpedVersion, bumpedService := bumpedCatalogFSForApp(t, app, hostPort)

			// A second engine sharing the same stack base dir reads the
			// already-committed manifest but advertises the bumped catalog.
			bumpedEng, bumpedFake := newEngineSharingStack(t, stackPath, core.WithCatalog(bumpedFS))

			confirmer := &fakeConfirmer{}
			var steps []string
			var messages []string
			res, err := bumpedEng.Update(
				t.Context(),
				types.UpdateRequest{AppID: app.AppID, DryRun: true},
				func(step string, _ float64, message string) {
					steps = append(steps, step)
					messages = append(messages, message)
				},
				confirmer,
			)
			require.NoError(t, err)
			require.NotNil(t, res)

			// The transition is reported: old (real) version → bumped version.
			assert.Equal(t, app.TemplateVersion, res.PreviousTemplateVersion)
			assert.Equal(t, bumpedVersion, res.NewTemplateVersion)
			assert.Equal(t, []string{bumpedService}, res.UpdatedServices,
				"exactly the bumped service must appear in the changed-services set")

			// Risk classifications are carried verbatim in catalog order.
			assert.Equal(t, app.RiskClassification, res.RiskClassifications,
				"the candidate update carries the catalog risk_classification array verbatim")

			// The planning stream names the bumped service's old → new image
			// references (PRD §20 tag visibility).
			joined := strings.Join(messages, "\n")
			oldRef := imageRefForService(app.ImagePins, bumpedService)
			newRef := oldRef // same image, advanced tag
			if idx := strings.LastIndex(newRef, ":"); idx >= 0 {
				newRef = newRef[:idx] + ":" + bumpedTag
			}
			assert.Contains(t, joined, fmt.Sprintf("service %s: %s -> %s", bumpedService, oldRef, newRef),
				"the planning stream must carry the bumped service old -> new image references")
			assert.Contains(t, joined, "update available", "the summary event must name the outcome")
			for _, step := range steps {
				assert.Equal(t, types.StepUpdatePlanning, step,
					"a DryRun check must emit only planning step events")
			}

			// DryRun consults no confirmer, runs no docker command, and
			// mutates nothing, even with an available (possibly
			// database-risk) update.
			assert.Empty(t, confirmer.calls, "DryRun must never consult the confirmer")
			assert.Zero(t, bumpedFake.calls, "a DryRun check runs no docker command")
			assert.Empty(t, res.BackupPath, "a DryRun check takes no backup")
			assert.Nil(t, res.Status, "a DryRun check deploys nothing")
			assertStackDirUnchanged(t, stackPath, before)
			assert.NoDirExists(t, filepath.Join(stackPath, state.BackupDirName),
				"a DryRun check must not create a backup directory")
		})
	}
}

// installRealCuratedApp drives a full mocked-Docker install of one real
// curated app into a tempdir stack and returns the engine (reusable for a
// same-catalog DryRun), the stack path, and the free host port the install
// bound. It supplies every input the real catalog demands deterministically:
// a stubbed host-resource probe and secret generator, a pinned TZ, a domain,
// and real existing directories for any path placeholders.
func installRealCuratedApp(t *testing.T, app catalog.App) (eng *core.Engine, stackPath string, hostPort int) {
	t.Helper()

	catalogFS, hostPort := realCatalogFSForApp(t, app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return fakeSecretValue, nil
	})
	// argon2id-encoded secrets (e.g. vaultwarden's ADMIN_TOKEN) go through a
	// separate credential seam: install generates a one-time plaintext and
	// persists only the $$-escaped PHC. Pin it so the rendered .env is
	// deterministic across hosts.
	core.SetInstallArgon2idGeneratorForTest(eng, func() (plaintext, phc string, err error) {
		return argon2idFixturePlaintext, argon2idFixturePHC, nil
	})
	// Mocked Docker never binds a real socket, so the real net.Listen probe
	// only adds host-port flakiness — public ports keep their catalog host
	// port (the localhost-port rewrite cannot make them ephemeral, see
	// rewriteHostPorts), so two parallel installs would race the same fixed
	// port. A no-op probe keeps the lifecycle deterministic; the install-time
	// port match is still proven by the mocked post-deploy inspect.
	core.SetInstallPortProbeForTest(eng, func(context.Context, types.PortBinding) error {
		return nil
	})

	stackPath = filepath.Join(filepath.Dir(stateDir), "stacks", app.AppID)

	fake := &fakeDockerClient{}
	fake.runFn = happyInstallRunFn(t, app, hostPort)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	req := types.InstallRequest{
		AppID:             app.AppID,
		Domain:            "app.test.example",
		PlaceholderValues: requiredPlaceholderValues(t, app, stackPath),
	}
	res, err := eng.Install(t.Context(), req, nil, &fakeConfirmer{})
	require.NoError(t, err, "real install of %s must succeed against mocked Docker", app.AppID)
	require.NotNil(t, res)

	// Pin the cleanly-running happy path so a silently broken mock (e.g.
	// an invocation type rename routing the container list to the default
	// empty-success arm) cannot degrade the install to needs_attention
	// without failing the test.
	require.NotNil(t, res.Status, "install must report post-deploy status")
	assert.Equal(t, "running", res.Status.State, "the mocked deploy must report cleanly running")
	assert.False(t, res.Status.NeedsAttention, "a happy mocked install must not need attention")

	return eng, stackPath, hostPort
}

// assertInstalledStackOnDisk asserts the on-disk artifacts the install
// committed for one curated app: the .env at mode 0o600
// carrying resolved values, the label-injected docker-compose.yml, and the
// .wdm.lock manifest with app identity, image pins, recommended resources,
// and the install-kind last_successful_operation.
func assertInstalledStackOnDisk(t *testing.T, app catalog.App, stackPath string, hostPort int) {
	t.Helper()

	// .env exists at 0o600 (PRD §11 secret-file mode) with resolved values.
	envPath := filepath.Join(stackPath, ".env")
	require.FileExists(t, envPath)
	assert.Equal(t, os.FileMode(0o600), fileModePerm(t, envPath),
		".env must be written with mode 0o600")
	envBytes, err := os.ReadFile(envPath)
	require.NoError(t, err)
	envText := string(envBytes)
	assert.NotContains(t, envText, "{{", "the rendered .env must carry no unresolved template directives")
	for _, ph := range app.Placeholders {
		if ph.Type != "secret" {
			continue
		}
		if ph.Encoding == "argon2id" {
			// argon2id secrets persist only the one-way PHC hash, $$-escaped so
			// Compose --env-file interpolation reconstructs the canonical PHC.
			// The plaintext is surfaced once and never lands in.env.
			escapedPHC := strings.ReplaceAll(argon2idFixturePHC, "$", "$$")
			assert.Contains(t, envText, ph.Name+"="+escapedPHC,
				"argon2id secret %q must render its $$-escaped PHC into .env", ph.Name)
			assert.NotContains(t, envText, argon2idFixturePlaintext,
				"argon2id secret %q must never persist its plaintext in .env", ph.Name)
			continue
		}
		// Other generated secrets land verbatim in .env. Every secret placeholder
		// is persisted there — including the ones also rendered into config
		// artifacts — so wdm can recover regenerable:false values and redact them
		// on update.
		assert.Contains(t, envText, ph.Name+"="+fakeSecretValue,
			"secret %q must render its generated value into .env", ph.Name)
	}

	// docker-compose.yml carries the mandatory wdm.managed / wdm.app labels
	// on every service.
	composePath := filepath.Join(stackPath, "docker-compose.yml")
	require.FileExists(t, composePath)
	composeBytes, err := os.ReadFile(composePath)
	require.NoError(t, err)
	services := composeServiceLabels(t, composeBytes)
	require.NotEmpty(t, services, "rendered compose for %s declares no services", app.AppID)
	for serviceName, labels := range services {
		assert.Equal(t, "true", labels["wdm.managed"],
			"service %q must carry wdm.managed=true", serviceName)
		assert.Equal(t, app.AppID, labels["wdm.app"],
			"service %q must carry wdm.app=%s", serviceName, app.AppID)
	}

	// .wdm.lock parses with the full install identity (PRD §9/§30).
	lock, err := state.ReadStackLock(t.Context(), filepath.Join(stackPath, ".wdm.lock"))
	require.NoError(t, err)
	assert.Equal(t, 1, lock.SchemaVersion)
	assert.Equal(t, app.AppID, lock.AppID)
	assert.Equal(t, app.TemplateName, lock.TemplateName)
	assert.Equal(t, app.TemplateVersion, lock.TemplateVersion)
	assert.Equal(t, realCatalogChannel, lock.CatalogChannel)
	assert.Equal(t, "wdm-"+app.AppID, lock.ComposeProject)
	expectedLocalPorts := make([]int, 0, len(app.Ports))
	for _, binding := range plannedHostBindings(app, hostPort) {
		expectedLocalPorts = append(expectedLocalPorts, binding.hostPort)
	}
	assert.Equal(t, expectedLocalPorts, lock.LocalPorts,
		"the manifest records every host port the install bound, in catalog order")

	// Image pins mirror the catalog, one per catalog pin, sorted by neither
	// side — assert as an unordered set so a renderer reorder cannot flake.
	assert.ElementsMatch(t, catalogImagePinSet(app), lockImagePinSet(lock),
		"the manifest image pins must mirror the catalog pins")

	// Generated secret fields mirror the catalog's secret placeholders.
	assert.ElementsMatch(t, catalogSecretNames(app), lock.GeneratedFields,
		"generated fields must list every catalog secret placeholder")

	require.NotNil(t, lock.LastSuccessfulOperation)
	assert.Equal(t, "install", lock.LastSuccessfulOperation.Kind)
	assert.Equal(t, "dev", lock.LastSuccessfulOperation.WDMVersion)

	// Every curated app declares resources, so the manifest records the
	// recommended totals.
	require.NotNil(t, lock.RecommendedResources,
		"every curated app declares resources, so recommended totals must be recorded")
	assert.Positive(t, lock.RecommendedResources.MemoryBytes,
		"recommended memory total must be the sum of the catalog recommended bands")
	assert.Positive(t, lock.RecommendedResources.CPUs,
		"recommended cpu total must be the sum of the catalog recommended bands")
}

// loadRealStableCatalogApps loads and validates the real stable catalog from
// the source tree and returns its apps, failing the test on any error so a
// malformed catalog cannot silently skip coverage. The nineteen curated apps
// must all be present.
func loadRealStableCatalogApps(t *testing.T) []catalog.App {
	t.Helper()

	abs, err := filepath.Abs(realCatalogPath)
	require.NoError(t, err)
	cat, err := catalog.LoadCatalog(context.Background(), abs)
	require.NoError(t, err, "load real stable catalog")
	require.NotNil(t, cat)
	require.Len(t, cat.Apps, 19, "stable catalog must carry the nineteen curated apps")

	return cat.Apps
}

// realCatalogFSForApp builds an fs.FS the engine can install from: it carries
// the real catalog (with this app's host ports rewritten to a free ephemeral
// port for determinism — see the file doc comment) under <channel>/catalog.yaml
// plus this app's real templates from the source tree. It returns the FS and the
// free host port the rewrite chose.
func realCatalogFSForApp(t *testing.T, app catalog.App) (catalogFS fstest.MapFS, hostPort int) {
	t.Helper()

	abs, err := filepath.Abs(realCatalogPath)
	require.NoError(t, err)
	cat, err := catalog.LoadCatalog(context.Background(), abs)
	require.NoError(t, err)

	hostPort = freeLocalTCPPort(t)
	mutated := rewriteHostPorts(cat, app.AppID, hostPort)

	return catalogFSWithRealTemplates(t, mutated, app), hostPort
}

// bumpedCatalogFSForApp builds a catalog FS from the real catalog with one
// service's image tag and the app's template_version advanced, so a DryRun
// against it reports an available update. It re-applies the same free-port
// rewrite the original install used (the check ignores ports; the re-apply
// keeps the bumped catalog identical to the installed one except the
// tag/version bump, so the diff under test is exact). It returns the FS plus
// the bumped tag, bumped version, and the service whose tag advanced.
func bumpedCatalogFSForApp(
	t *testing.T,
	app catalog.App,
	hostPort int,
) (catalogFS fstest.MapFS, bumpedTag, bumpedVersion, bumpedService string) {
	t.Helper()

	abs, err := filepath.Abs(realCatalogPath)
	require.NoError(t, err)
	cat, err := catalog.LoadCatalog(context.Background(), abs)
	require.NoError(t, err)

	mutated := rewriteHostPorts(cat, app.AppID, hostPort)

	bumpedVersion = "2099.01.01"
	for i := range mutated.Apps {
		if mutated.Apps[i].AppID != app.AppID {
			continue
		}
		require.NotEmpty(t, mutated.Apps[i].ImagePins, "curated app %s must declare image pins", app.AppID)
		mutated.Apps[i].TemplateVersion = bumpedVersion
		bumpedService = mutated.Apps[i].ImagePins[0].Service
		bumpedTag = bumpImageTag(mutated.Apps[i].ImagePins[0].Tag)
		mutated.Apps[i].ImagePins[0].Tag = bumpedTag
	}
	require.NotEmpty(t, bumpedService, "the bumped app must be present in the catalog")

	return catalogFSWithRealTemplates(t, mutated, app), bumpedTag, bumpedVersion, bumpedService
}

// catalogFSWithRealTemplates renders cat to YAML under <channel>/catalog.yaml
// and adds this app's real templates from the source tree (compose, env, and
// any additional_files sources), mirroring catalogFixtureFSWithFiles' MapFS
// shape but sourced from real files. The engine reads both the catalog and the
// templates from the same FS (install.go installCatalogFS), so they must live
// together here.
func catalogFSWithRealTemplates(t *testing.T, cat *catalog.Catalog, app catalog.App) fstest.MapFS {
	t.Helper()

	raw, err := yaml.Marshal(cat)
	require.NoError(t, err)

	catalogFS := fstest.MapFS{
		path.Join(realCatalogChannel, "catalog.yaml"): &fstest.MapFile{Data: raw},
		app.ComposeTemplate:                           &fstest.MapFile{Data: readRepoFile(t, app.ComposeTemplate)},
		app.EnvTemplate:                               &fstest.MapFile{Data: readRepoFile(t, app.EnvTemplate)},
	}
	templateDir := path.Dir(app.ComposeTemplate)
	for _, file := range app.AdditionalFiles {
		src := path.Join(templateDir, file.Src)
		catalogFS[src] = &fstest.MapFile{Data: readRepoFile(t, src)}
	}
	for _, artifact := range app.ConfigGeneration {
		src := path.Join(templateDir, artifact.Template)
		catalogFS[src] = &fstest.MapFile{Data: readRepoFile(t, src)}
	}

	return catalogFS
}

// rewriteHostPorts returns a deep-ish copy of cat with the named app's
// LOCALHOST host ports rewritten to a free ephemeral port for determinism, and
// its PUBLIC host ports left untouched. Only the slices the rewrite touches are
// copied; the rest is shared, which is safe because the result is immediately
// marshaled to YAML and never mutated again.
//
// Public ports are deliberately NOT rewritten: the compose template hardcodes
// the public bind at its catalog host port (e.g. 22000), and the public-bind
// scan (install.go verifyPublicBindsMatchCatalog) matches the catalog's
// public:true declarations against the rendered template by (protocol, host
// port). Rewriting a public port here would desynchronize the two and fail the
// scan. Localhost ports never enter that scan, so rewriting them to a
// guaranteed-free port is safe and keeps the install-time availability probe
// deterministic. Apps with more than one localhost port (e.g. wg-adguard's two
// web UIs) get consecutive ports hostPort, hostPort+1, … so the plan never
// collides two localhost binds on one host port; localhostHostPort applies the
// same offset on the expectation side.
func rewriteHostPorts(cat *catalog.Catalog, appID string, hostPort int) *catalog.Catalog {
	clone := *cat
	clone.Apps = slices.Clone(cat.Apps)
	for i := range clone.Apps {
		if clone.Apps[i].AppID != appID {
			continue
		}
		ports := slices.Clone(clone.Apps[i].Ports)
		localhostIndex := 0
		for j := range ports {
			if ports[j].Public {
				continue
			}
			ports[j].Host = hostPort + localhostIndex
			localhostIndex++
		}
		clone.Apps[i].Ports = ports
		clone.Apps[i].ImagePins = slices.Clone(clone.Apps[i].ImagePins)
	}
	return &clone
}

// localhostHostPort returns the rewritten host port rewriteHostPorts assigned to
// the localhostIndex-th localhost port of an app: hostPort for the first,
// hostPort+1 for the second, and so on.
func localhostHostPort(hostPort, localhostIndex int) int {
	return hostPort + localhostIndex
}

// plannedHostBinding is one host-port binding the install is expected to plan
// for an app, after the localhost-port rewrite. It mirrors the (container,
// host, protocol) tuple the mocked container inspect must publish and the
// (host) port the manifest must record.
type plannedHostBinding struct {
	service       string
	containerPort int
	hostPort      int
	protocol      string
	public        bool
}

// plannedHostBindings returns the host bindings the install plans for app after
// rewriteHostPorts: the localhost ports map to consecutive ports hostPort,
// hostPort+1, … in ports[] order, each public port keeps its catalog host port,
// and the catalog protocol default (tcp) is applied. A public range port
// (host_range/container_range, e.g. stoat's livekit 50000-50100/udp) expands to
// one binding per port in the span, mirroring install.go's rangePortBindings, so
// the manifest port expectation and the mocked container publish both cover the
// full range. The order matches the catalog's ports[] order, which is the order
// install.go planPorts preserves.
func plannedHostBindings(app catalog.App, hostPort int) []plannedHostBinding {
	bindings := make([]plannedHostBinding, 0, len(app.Ports))
	localhostIndex := 0
	for _, port := range app.Ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		// A range port keeps its catalog host span (ranges are public-only in
		// the curated set) and expands port-for-port from the low ends,
		// mirroring install.go's rangePortBindings.
		if port.HostRange != "" || port.ContainerRange != "" {
			hostLo, hostHi := fixturePortRange(port.HostRange)
			containerLo, _ := fixturePortRange(port.ContainerRange)
			for offset := 0; hostLo+offset <= hostHi; offset++ {
				bindings = append(bindings, plannedHostBinding{
					service:       port.Service,
					containerPort: containerLo + offset,
					hostPort:      hostLo + offset,
					protocol:      protocol,
					public:        port.Public,
				})
			}
			continue
		}
		host := port.Host
		if !port.Public {
			host = localhostHostPort(hostPort, localhostIndex)
			localhostIndex++
		}
		bindings = append(bindings, plannedHostBinding{
			service:       port.Service,
			containerPort: port.Container,
			hostPort:      host,
			protocol:      protocol,
			public:        port.Public,
		})
	}
	return bindings
}

// fixturePortRange parses an inclusive "<lo>-<hi>" span into its bounds. The
// schema already enforces the shape, so a malformed span here is a fixture bug;
// the helper tolerates it by returning zero bounds (yielding an empty range)
// rather than failing, keeping it dependency-free of *testing.T.
func fixturePortRange(span string) (lo, hi int) {
	loStr, hiStr, ok := strings.Cut(span, "-")
	if !ok {
		return 0, -1
	}
	lo, _ = strconv.Atoi(loStr)
	hi, _ = strconv.Atoi(hiStr)
	return lo, hi
}

// requiredPlaceholderValues returns the deterministic PlaceholderValues the real
// catalog demands for app: a pinned TZ for any timezone placeholder, and real
// existing absolute directories (outside the stack) for any required path
// placeholder. Secret placeholders are generated by the stubbed generator and
// domain placeholders flow from InstallRequest.Domain, so neither appears here.
func requiredPlaceholderValues(t *testing.T, app catalog.App, stackPath string) map[string]string {
	t.Helper()

	values := map[string]string{}
	for _, ph := range app.Placeholders {
		switch ph.Type {
		case "timezone":
			values[ph.Name] = fixedTestTimezone
		case "path":
			// Path placeholders must be absolute, exist, and live outside
			// the stack dir (install.go resolvePathPlaceholder). t.TempDir
			// satisfies all three.
			dir := filepath.Join(t.TempDir(), strings.ToLower(ph.Name))
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NotContains(t, dir, stackPath, "path placeholder must live outside the stack")
			values[ph.Name] = dir
		}
	}
	return values
}

// happyInstallRunFn scripts the mocked Docker calls for a clean install,
// dispatching on the invocation TYPE (not call number) so it is robust to each
// app's differing service/network/image-pin counts. Compose-config validate,
// network inspect+create, up, and image-digest inspect all succeed; the
// post-deploy status path is fed one running managed container per rendered
// service so the install reports cleanly running (each service publishes
// exactly the host bindings the plan assigned it so the install-time port match
// is satisfied).
func happyInstallRunFn(t *testing.T, app catalog.App, hostPort int) func(int, docker.Invocation) (docker.CommandResult, error) {
	t.Helper()

	services := serviceOrderForApp(app)

	bindingsByService := map[string][]plannedHostBinding{}
	for _, binding := range plannedHostBindings(app, hostPort) {
		bindingsByService[binding.service] = append(bindingsByService[binding.service], binding)
	}

	var inspectIdx int
	return func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
		switch fmt.Sprintf("%T", inv) {
		case "docker.networkInspectInvocation":
			// Report the network absent so EnsureNetwork takes its create
			// path (the created network's internal flag is whatever the
			// catalog declares; create always succeeds below).
			return missingNetworkResult("wdm-net")
		case "docker.projectContainerListInvocation":
			// One container id per rendered service; ids are inspected in
			// order below.
			var b strings.Builder
			for i := range services {
				fmt.Fprintf(&b, "%012d\n", i+1)
			}
			return docker.CommandResult{Stdout: b.String()}, nil
		case "docker.containerInspectInvocation":
			// Return the next service's inspect output in list order. Each
			// service publishes exactly the host bindings the plan assigned it
			// (an app may publish several ports on one service — e.g. a GUI
			// plus a public sync port); a service that owns none publishes an
			// empty port map so the inspect stays honest.
			require.Less(t, inspectIdx, len(services), "more inspect calls than services")
			service := services[inspectIdx]
			inspectIdx++
			return docker.CommandResult{
				Stdout: managedInspectStdout(t, "wdm-"+service+"-1", service, app.AppID, bindingsByService[service]),
			}, nil
		default:
			// composeConfig, networkCreate, composeUp, imageDigest all
			// succeed with empty output (empty digest is not a fault).
			return docker.CommandResult{}, nil
		}
	}
}

// managedInspectStdout produces a single managed container's `docker inspect`
// output in the shape internal/docker parses, publishing exactly the given
// planned bindings (a service may own several ports — e.g. a localhost GUI plus
// a public sync port). A service with no bindings reports an empty port map so
// non-port services in a multi-service app stay honest. The fuse port-match
// keys on (protocol, host port) only, but the inspect carries the real
// container port and host IP so the fabricated output stays faithful to a real
// `docker inspect`.
func managedInspectStdout(t *testing.T, name, service, appID string, bindings []plannedHostBinding) string {
	t.Helper()

	labels, err := json.Marshal(map[string]string{
		"com.docker.compose.service": service,
		"wdm.managed":                "true",
		"wdm.app":                    appID,
	})
	require.NoError(t, err)

	portMap := map[string][]map[string]string{}
	for _, binding := range bindings {
		hostIP := "127.0.0.1"
		if binding.public {
			hostIP = "0.0.0.0"
		}
		key := fmt.Sprintf("%d/%s", binding.containerPort, binding.protocol)
		portMap[key] = append(portMap[key], map[string]string{
			"HostIp":   hostIP,
			"HostPort": strconv.Itoa(binding.hostPort),
		})
	}
	portsJSON, err := json.Marshal(portMap)
	require.NoError(t, err)

	return fmt.Sprintf("%q\n%s\n\"running\"\ntrue\nfalse\n0\n\"\"\n%s\n", "/"+name, labels, portsJSON)
}

// readRepoFile reads a catalog-FS-relative path (e.g.
// "templates/n8n/.env.tmpl") from the source tree, which lays the catalog FS
// content out at the repo root.
func readRepoFile(t *testing.T, rel string) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	require.NoErrorf(t, err, "read source-tree file %s", rel)
	return raw
}

// newEngineSharingStack builds a second engine whose managed-stack base is the
// parent of stackPath, so it reads the stack a prior engine installed there
// while advertising the catalog supplied via extra. newTestEngine gives it
// fresh state/data dirs, so this engine has its OWN runtime.lock file (the
// lock is a per-state-dir flock, internal/state/runtime_lock.go — engines
// with distinct state dirs share no exclusion at all). That is harmless
// here: the calls are strictly sequential, and the only shared state is the
// per-stack .wdm.lock, which the DryRun check reads through its own
// non-blocking shared flock. The returned fake is wired so an accidental
// Docker call hits the hermetic fake instead of a real daemon, and so the
// caller can pin the check stage's zero-Docker posture.
func newEngineSharingStack(t *testing.T, stackPath string, extra ...core.Option) (*core.Engine, *fakeDockerClient) {
	t.Helper()

	eng, _ := newTestEngine(t, append([]core.Option{core.WithStackBaseDir(filepath.Dir(stackPath))}, extra...)...)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))
	return eng, fake
}

// snapshotStackDir reads every regular file under root into a path → bytes map
// so a later assertStackDirUnchanged can prove a DryRun mutated nothing.
func snapshotStackDir(t *testing.T, root string) map[string][]byte {
	t.Helper()

	snapshot := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		snapshot[p] = data
		return nil
	})
	require.NoError(t, err)
	return snapshot
}

// assertStackDirUnchanged proves the stack directory is byte-identical to a
// prior snapshot: same file set, same bytes, no additions or deletions.
func assertStackDirUnchanged(t *testing.T, root string, before map[string][]byte) {
	t.Helper()

	after := snapshotStackDir(t, root)
	require.Equal(t, len(before), len(after), "DryRun must not add or remove files in the stack dir")
	for p, want := range before {
		got, ok := after[p]
		require.Truef(t, ok, "DryRun removed %s", p)
		assert.Equalf(t, want, got, "DryRun mutated %s", p)
	}
}

// composeServiceLabels parses a rendered Compose document and returns each
// service's label map, reading labels from either the mapping form or the
// "key=value" sequence form. It reads the committed bytes independently of the
// renderer so a dropped-label regression cannot hide behind the renderer.
func composeServiceLabels(t *testing.T, composeBytes []byte) map[string]map[string]string {
	t.Helper()

	var doc struct {
		Services map[string]struct {
			Labels yaml.Node `yaml:"labels"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(composeBytes, &doc), "parse rendered compose")

	out := make(map[string]map[string]string, len(doc.Services))
	for name, svc := range doc.Services {
		labels := map[string]string{}
		switch svc.Labels.Kind {
		case 0:
			// No labels node.
		case yaml.MappingNode:
			require.NoError(t, svc.Labels.Decode(&labels))
		case yaml.SequenceNode:
			var pairs []string
			require.NoError(t, svc.Labels.Decode(&pairs))
			for _, pair := range pairs {
				key, value, _ := strings.Cut(pair, "=")
				labels[key] = value
			}
		default:
			t.Fatalf("unexpected labels node kind %d for service %q", svc.Labels.Kind, name)
		}
		out[name] = labels
	}
	return out
}

// serviceOrderForApp returns the rendered service names for app in a
// deterministic order, sourced from the catalog image pins (one pin per
// service). The order only governs which mocked inspect output answers which
// list id; correctness does not depend on it.
func serviceOrderForApp(app catalog.App) []string {
	services := make([]string, 0, len(app.ImagePins))
	for _, pin := range app.ImagePins {
		services = append(services, pin.Service)
	}
	sort.Strings(services)
	return services
}

// catalogImagePinSet projects the catalog image pins into comparable strings.
func catalogImagePinSet(app catalog.App) []string {
	out := make([]string, 0, len(app.ImagePins))
	for _, pin := range app.ImagePins {
		out = append(out, fmt.Sprintf("%s|%s|%s", pin.Service, pin.Image, pin.Tag))
	}
	return out
}

// lockImagePinSet projects the manifest image pins into comparable strings
// (service|image|tag), ignoring the opportunistic digest which is mocked-empty.
func lockImagePinSet(lock *state.StackLock) []string {
	out := make([]string, 0, len(lock.ImagePins))
	for _, pin := range lock.ImagePins {
		out = append(out, fmt.Sprintf("%s|%s|%s", pin.Service, pin.Image, pin.Tag))
	}
	return out
}

// catalogSecretNames returns the names of every secret-typed placeholder.
func catalogSecretNames(app catalog.App) []string {
	var out []string
	for _, ph := range app.Placeholders {
		if ph.Type == "secret" {
			out = append(out, ph.Name)
		}
	}
	return out
}

// imageRefForService returns the image:tag reference for the named service.
func imageRefForService(pins []catalog.ImagePin, service string) string {
	for _, pin := range pins {
		if pin.Service == service {
			return pin.Image + ":" + pin.Tag
		}
	}
	return ""
}

// bumpImageTag returns a tag guaranteed to differ from the input so a DryRun
// sees a change. A fixed sentinel suffices — the check compares image[:tag]
// equality, not version ordering.
func bumpImageTag(tag string) string {
	return tag + "-wdmtest9"
}
