package core_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/pkg/types"
)

// TestRealCatalog_NonSecretPlaceholdersPersistedInEnv is the permanent guard
// for the issue #98/#100 placeholder class: the update rewrite precheck
// (internal/core/update_apply.go resolveUpdatePlaceholder) re-resolves every
// declared NON-SECRET placeholder from the existing .env and fails closed when
// the key is absent. So every declared non-secret placeholder MUST be persisted
// as its own NAME= key in the app's rendered .env — even a `type: domain` input
// the template only consumes inside a derived expression (DOMAIN=https://NAME).
//
// This mirrors the precheck predicate faithfully — declared placeholder, not
// secret-typed — with no per-app allowlist, so it catches the next unpersisted
// placeholder at CI time instead of at a user's `wdm apps update`. A rendered
// `.env.tmpl` line `NAME={{ .NAME }}` becomes `NAME=value`, so a source-level
// `NAME=` key line is exactly the persistence the precheck reads back.
func TestRealCatalog_NonSecretPlaceholdersPersistedInEnv(t *testing.T) {
	t.Parallel()

	for _, app := range loadRealStableCatalogApps(t) {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			keys := envTemplateKeys(t, app)
			for _, ph := range app.Placeholders {
				// The precheck only re-resolves non-secret placeholders from the
				// existing .env; secret placeholders take the generate/reuse arm.
				if ph.Type == "secret" {
					continue
				}
				assert.Containsf(t, keys, ph.Name,
					"non-secret placeholder %q must be persisted as its own %s= key "+
						"in %s, or `wdm apps update %s` fails closed in the rewrite precheck",
					ph.Name, ph.Name, app.EnvTemplate, app.AppID)
			}
		})
	}
}

// envTemplateKeys returns the set of NAME= assignment keys declared in the
// app's real .env.tmpl (left-hand sides of non-comment KEY=VALUE lines).
func envTemplateKeys(t *testing.T, app catalog.App) map[string]struct{} {
	t.Helper()

	keys := make(map[string]struct{})
	sc := bufio.NewScanner(strings.NewReader(string(readRepoFile(t, app.EnvTemplate))))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.IndexByte(line, '='); i > 0 {
			keys[strings.TrimSpace(line[:i])] = struct{}{}
		}
	}
	require.NoError(t, sc.Err())
	return keys
}

// TestUpdateApply_DomainPlaceholderPersisted is the issue #100 regression (the
// issue #98 vaultwarden fix generalized): baserow, serpbear, and docuseal each
// declare a non-secret `type: domain` placeholder their .env.tmpl consumes only
// inside a derived URL/host line, so before the template fix the rendered .env
// omitted the placeholder's own key and a real update refused with
// "placeholder \"<APP>_DOMAIN\" is absent from the existing .env". This drives
// each app through the REAL catalog + templates: a real install, then a real
// (non-DryRun) Update whose rewrite stage re-resolves the placeholder from the
// existing .env. vaultwarden is covered by
// TestUpdateApply_VaultwardenDomainPlaceholderPersisted.
func TestUpdateApply_DomainPlaceholderPersisted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		appID         string
		placeholder   string
		derivedSuffix string // a fragment of the derived line that must survive
	}{
		{"baserow", "BASEROW_DOMAIN", "BASEROW_PUBLIC_URL=https://app.test.example"},
		{"serpbear", "SERPBEAR_DOMAIN", "NEXT_PUBLIC_APP_URL=https://app.test.example"},
		{"docuseal", "DOCUSEAL_DOMAIN", "DOCUSEAL_HOST=app.test.example"},
	}

	apps := make(map[string]catalog.App)
	for _, a := range loadRealStableCatalogApps(t) {
		apps[a.AppID] = a
	}

	for _, tc := range cases {
		t.Run(tc.appID, func(t *testing.T) {
			t.Parallel()

			app, ok := apps[tc.appID]
			require.Truef(t, ok, "stable catalog must carry %s", tc.appID)

			eng, stackPath, hostPort := installRealCuratedApp(t, app)

			// The redeploy reuses the install Docker factory, so give it a fresh
			// run-fn whose per-service inspect counter is not exhausted.
			updateFake := &fakeDockerClient{}
			updateFake.runFn = happyInstallRunFn(t, app, hostPort)
			core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(updateFake))

			expectedKey := tc.placeholder + "=app.test.example"
			envBytes, err := os.ReadFile(filepath.Join(stackPath, ".env"))
			require.NoError(t, err)
			require.Containsf(t, string(envBytes), expectedKey,
				"the rendered .env must persist %s as its own key", tc.placeholder)

			// A real (non-DryRun) Update against the unchanged catalog re-resolves
			// every declared placeholder from the existing .env — the seam the bug
			// lived in. It must no longer refuse on %s_DOMAIN.
			res, err := eng.Update(t.Context(), types.UpdateRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
			require.NoErrorf(t, err, "the update rewrite must resolve %s from the existing .env", tc.placeholder)
			require.NotNil(t, res)

			envAfter, err := os.ReadFile(filepath.Join(stackPath, ".env"))
			require.NoError(t, err)
			assert.Containsf(t, string(envAfter), expectedKey,
				"the rewrite must reuse the persisted %s value", tc.placeholder)
			assert.Containsf(t, string(envAfter), tc.derivedSuffix,
				"the derived line must survive the rewrite")
		})
	}
}
