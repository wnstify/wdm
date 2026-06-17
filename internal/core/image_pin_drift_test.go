package core_test

// Image-pin / template drift verification (interstitial D). The catalog
// image_pins drive wdm's update diffing (diffUpdateServicePins compares the
// stack manifest's pins against the catalog's pins to decide what is an
// "update"); the template's literal `image:` line drives what `docker
// compose up` actually pulls and deploys. If the two drift — a maintainer
// bumps the template image without the pin, or bumps the pin without the
// template — wdm reports updates that do not match what deploys, a silent
// correctness lie. verifyImagePinsMatchTemplate makes that drift
// structurally impossible by refusing install and update before any Docker
// contact.
// This file proves three things:
//   - the matcher passes clean on the real stable catalog's four curated
//     apps (zero false positives — the working-tree catalog as it sits,
//     including the parked n8n edits);
//   - drift in either direction (template-ahead, pin-ahead, renamed image,
//     pin naming an absent service) refuses with a verification-failed
//     typed error naming the app, service, pinned image, and template
//     image; and
//   - the refusal fires end-to-end on BOTH the install render arc
//     (RenderInstallForTest) and the update re-render arc (eng.Update),
//     before any Docker invocation.

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

// driftFixtureApp returns a two-service synthetic catalog app whose pins
// match the driftFixtureCompose image lines, so individual tests can drift
// exactly one axis and prove the others stay clean.
func driftFixtureApp() catalog.App {
	app := appFixture("drift-app", 19090)
	app.ImagePins = []catalog.ImagePin{
		{Service: "web", Image: "docker.io/example/web", Tag: "1.2.3"},
		{Service: "db", Image: "docker.io/example/db", Tag: "4.5.6"},
	}
	return app
}

// driftFixtureCompose is a rendered-shaped compose carrying both services'
// image lines matching driftFixtureApp's pins. Tests mutate one line to
// induce drift.
const driftFixtureCompose = `services:
  web:
    image: docker.io/example/web:1.2.3
  db:
    image: docker.io/example/db:4.5.6
`

func TestImagePinDrift_RealStableCatalogPassesClean(t *testing.T) {
	t.Parallel()

	// The matcher works on the RENDERED compose (post label injection), but
	// the curated templates carry literal image lines, so rendering through
	// RenderLabels preserves them. The real-catalog lifecycle tests already
	// prove the integrated install path; here we assert the matcher itself
	// is a no-op against every curated app's real template image lines so a
	// false positive in the matcher cannot hide behind an unrelated install
	// failure.
	for _, app := range loadRealStableCatalogApps(t) {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			composeBytes := readRepoFile(t, app.ComposeTemplate)
			require.NoError(t,
				core.VerifyImagePinsMatchTemplateForTest(app, composeBytes),
				"the real stable catalog pins must match the curated template image lines for %s",
				app.AppID,
			)
		})
	}
}

func TestImagePinDrift_MatcherTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		compose     string
		wantErr     bool
		wantContent []string
	}{
		{
			name:    "matching pins and template pass",
			compose: driftFixtureCompose,
			wantErr: false,
		},
		{
			name: "template tag ahead of pin refuses",
			compose: `services:
  web:
    image: docker.io/example/web:9.9.9
  db:
    image: docker.io/example/db:4.5.6
`,
			wantErr: true,
			// The pin (1.2.3) and the deployed template image (9.9.9) are
			// both named so a maintainer sees exactly which side drifted.
			wantContent: []string{
				"drift-app",
				"web",
				"docker.io/example/web:1.2.3",
				"docker.io/example/web:9.9.9",
			},
		},
		{
			name: "second service drift is anchored per service",
			compose: `services:
  web:
    image: docker.io/example/web:1.2.3
  db:
    image: docker.io/example/db:7.0.0
`,
			wantErr: true,
			wantContent: []string{
				"db",
				"docker.io/example/db:4.5.6",
				"docker.io/example/db:7.0.0",
			},
		},
		{
			name: "renamed image refuses even at the same tag",
			compose: `services:
  web:
    image: docker.io/elsewhere/web:1.2.3
  db:
    image: docker.io/example/db:4.5.6
`,
			wantErr: true,
			wantContent: []string{
				"docker.io/example/web:1.2.3",
				"docker.io/elsewhere/web:1.2.3",
			},
		},
		{
			name: "pin naming an absent service refuses",
			compose: `services:
  web:
    image: docker.io/example/web:1.2.3
`,
			wantErr: true,
			wantContent: []string{
				"db",
				"no such service",
			},
		},
		{
			name:    "malformed compose refuses fail-closed",
			compose: "services: [this is not a mapping",
			wantErr: true,
			wantContent: []string{
				"drift-app",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := core.VerifyImagePinsMatchTemplateForTest(driftFixtureApp(), []byte(tc.compose))
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assertVerificationFailed(t, err)
			for _, want := range tc.wantContent {
				assert.Contains(t, err.Error(), want,
					"drift error must name %q so the maintainer can fix it", want)
			}
		})
	}
}

func TestImagePinDrift_PinTagAheadOfTemplateRefuses(t *testing.T) {
	t.Parallel()

	// The pin-ahead direction (catalog bumped, template stale) is the more
	// dangerous lie: wdm would report the app up-to-date against the new
	// pin while still deploying the old image. Prove it refuses too.
	app := driftFixtureApp()
	app.ImagePins[0].Tag = "2.0.0"

	err := core.VerifyImagePinsMatchTemplateForTest(app, []byte(driftFixtureCompose))
	require.Error(t, err)
	assertVerificationFailed(t, err)
	assert.Contains(t, err.Error(), "docker.io/example/web:2.0.0", "the ahead pin is named")
	assert.Contains(t, err.Error(), "docker.io/example/web:1.2.3", "the stale template image is named")
}

func TestImagePinDrift_InstallArcRefusesDriftedTemplate(t *testing.T) {
	t.Parallel()

	app := driftFixtureApp()
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/drift-install/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/drift-install/.env.tmpl"

	// The template deploys web at a tag the pin does not name.
	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/drift-install/docker-compose.yml.tmpl": `services:
  web:
    image: docker.io/example/web:8.8.8
  db:
    image: docker.io/example/db:4.5.6
`,
		"templates/drift-install/.env.tmpl": "",
	}, app)

	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))

	_, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.Error(t, err)
	assertVerificationFailed(t, err)
	assert.Contains(t, err.Error(), "web")
	assert.Contains(t, err.Error(), "docker.io/example/web:1.2.3", "the pin is named")
	assert.Contains(t, err.Error(), "docker.io/example/web:8.8.8", "the deployed template image is named")
}

func TestImagePinDrift_UpdateArcRefusesBeforeAnyDockerContact(t *testing.T) {
	t.Parallel()

	// Poison the candidate template's image line so the update re-render
	// produces an image the catalog pin (2.0.0) does not name. The check
	// fires at the shared render-verification chokepoint, so the update
	// refuses before any Docker invocation — same posture as the secret
	// leak refusal.
	fx := newUpdateApplyFixture(t, updateApplyApp("drift-update-app"), false, func(templates map[string]string) {
		templates["templates/drift-update-app/docker-compose.yml.tmpl"] = `services:
  app:
    image: docker.io/example/app:6.6.6
    volumes:
      - ./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro
    environment:
      SITE_NAME: ${SITE_NAME}
`
	}, nil)

	composeBefore, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)

	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assertVerificationFailed(t, err)
	assert.Contains(t, err.Error(), "app")
	assert.Contains(t, err.Error(), "docker.io/example/app:2.0.0", "the catalog pin is named")
	assert.Contains(t, err.Error(), "docker.io/example/app:6.6.6", "the rewritten template image is named")

	// Refused before any Docker contact and before the on-disk compose was
	// left rewritten (the c38 restore boundary covers the rewrite-then-fail
	// path; here the failure leaves the original compose in place).
	assert.Zero(t, fx.fake.calls, "the update must refuse before any Docker invocation")
	composeAfter, err := os.ReadFile(fx.composePath)
	require.NoError(t, err)
	assert.Equal(t, string(composeBefore), string(composeAfter),
		"a refused drifted update must leave the original compose bytes unchanged")
}
