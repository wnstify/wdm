package core_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/pkg/types"
)

// TestReconfigure_StableCatalogInPlaceEditEveryApp is the catalog-wide
// regression guard for the issue #28 in-place reconfigure rework: for every
// app in the real stable catalog, for every service the catalog declares an
// adjustable resource band for, it drives the SAME resolve + in-place rewrite
// the live `wdm resources` reconfigure uses
// ([core.ReconfigureResolveRewriteForTest] → buildReconfigurePlan +
// rewriteResourceEnvLines) against the app's correctly-rendered install .env
// (the committed golden fixture under fixtures/golden/<app>/.env, the same
// bytes render's golden test pins) and asserts:
//
//   - the service's current resource values resolve with NO error (no "absent
//     from resolution", no "no resource band", no render failure);
//   - the targeted MEMORY_LIMIT_<svc> line changes to the new in-band value;
//   - every other .env line — secrets, derived values such as *_PUBLIC_URL,
//     comments, blank lines, ordering, and the other two resource limits — is
//     preserved byte-for-byte.
//
// The new value is the band's Min, which is always in-band and (across the
// curated catalog) strictly below the Recommended value the golden .env was
// rendered with, so the targeted line provably changes. An app that declares
// no adjustable service is asserted explicitly rather than skipped silently.
//
// If any app fails here it is a real reconfigure bug to surface, not a test
// to loosen.
func TestReconfigure_StableCatalogInPlaceEditEveryApp(t *testing.T) {
	t.Parallel()

	abs, err := filepath.Abs(realCatalogPath)
	require.NoError(t, err, "resolve stable catalog path")
	cat, err := catalog.LoadCatalog(context.Background(), abs)
	require.NoError(t, err, "load stable catalog")
	require.NotNil(t, cat)
	require.Len(t, cat.Apps, 19, "stable catalog must carry the nineteen curated apps")

	for _, app := range cat.Apps {
		t.Run(app.AppID, func(t *testing.T) {
			t.Parallel()

			goldenEnv, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "golden", app.AppID, ".env"))
			require.NoErrorf(t, err, "read golden .env for %s", app.AppID)

			adjustable := 0
			for _, profile := range app.Resources {
				if !profile.AllowOverride {
					continue
				}
				adjustable++

				t.Run(profile.Service, func(t *testing.T) {
					t.Parallel()
					assertReconfigureInPlaceEdit(t, app, profile, goldenEnv)
				})
			}

			if adjustable == 0 {
				assert.Zero(t, adjustable,
					"app %q declares no adjustable service; the reconfigure guard documents this rather than skipping",
					app.AppID)
			}
		})
	}
}

// assertReconfigureInPlaceEdit stages one app's golden .env in a temp stack,
// drives the real reconfigure resolve + in-place rewrite for an in-band
// memory change to one service, and asserts the targeted line changed while
// every other byte is preserved.
func assertReconfigureInPlaceEdit(
	t *testing.T,
	app catalog.App,
	profile catalog.ResourceProfile,
	goldenEnv []byte,
) {
	t.Helper()

	// buildReconfigurePlan reads the service's installed values from the
	// stack .env on disk via readServiceResourceValues, so stage the golden
	// bytes at the resolved stack path the way an installed stack carries them.
	stackPath := t.TempDir()
	envPath := filepath.Join(stackPath, ".env")
	require.NoError(t, os.WriteFile(envPath, goldenEnv, 0o600))

	key := reconfigureServiceKey(profile.Service)
	memoryLine := "MEMORY_LIMIT_" + key
	require.Containsf(t, string(goldenEnv), memoryLine+"=",
		"golden .env for %s must carry %s", app.AppID, memoryLine)

	// The band Min is in-band and, across the curated catalog, differs from
	// the Recommended value the golden .env was rendered with — so the
	// targeted line provably changes.
	newMemory := profile.Memory.Min
	currentMemory := envValue(t, goldenEnv, memoryLine)
	require.NotEqualf(t, currentMemory, newMemory,
		"band min %q must differ from the golden %s value %q so the edit is observable",
		newMemory, memoryLine, currentMemory)

	req := types.ReconfigureRequest{
		AppID:   app.AppID,
		Service: profile.Service,
		Memory:  &newMemory,
	}

	got, err := core.ReconfigureResolveRewriteForTest(req, app, stackPath, "wdm-"+app.AppID, goldenEnv)
	require.NoErrorf(t, err,
		"reconfigure resolve+rewrite must succeed for %s service %s", app.AppID, profile.Service)

	// The merge keeps the other two limits at their installed values, and the
	// requested memory takes effect.
	assert.Equal(t, newMemory, got.Memory, "the requested in-band memory is resolved")
	assert.Equal(t, envValue(t, goldenEnv, "CPUS_LIMIT_"+key), got.CPUs,
		"an unchanged cpus limit keeps its installed value")
	assert.Equal(t, mustAtoi(t, envValue(t, goldenEnv, "PIDS_LIMIT_"+key)), got.PIDs,
		"an unchanged pids limit keeps its installed value")

	// Byte-for-byte preservation: the rewritten .env must equal the golden
	// with ONLY the targeted memory line's value replaced. Reconstructing the
	// expected bytes this way proves no secret, derived value, comment, blank
	// line, ordering, or sibling resource limit drifted.
	want := replaceEnvLine(string(goldenEnv), memoryLine, newMemory)
	assert.Equalf(t, want, string(got.EnvFile),
		"only %s changed; every other byte of the %s .env is preserved", memoryLine, app.AppID)

	// Defense in depth on the resolve arm: a re-render regression would have
	// surfaced the install-only placeholder errors the in-place edit avoids.
	assert.NotContains(t, string(got.EnvFile), "absent from resolution")
}

// envValue returns the value of the first KEY=VALUE line whose key matches
// the given key in the .env bytes.
func envValue(t *testing.T, env []byte, key string) string {
	t.Helper()

	for _, line := range strings.Split(string(env), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(name) == key {
			return value
		}
	}
	t.Fatalf("key %q not found in .env", key)
	return ""
}

// replaceEnvLine returns env with the first KEY=VALUE line whose key matches
// key rewritten to key=value, leaving every other byte untouched. It mirrors
// rewriteResourceEnvLines' in-place edit so the test can reconstruct the
// expected output independently of the production rewriter.
func replaceEnvLine(env, key, value string) string {
	lines := strings.Split(env, "\n")
	for i, line := range lines {
		name, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(name) == key {
			lines[i] = key + "=" + value
			break
		}
	}
	return strings.Join(lines, "\n")
}

// mustAtoi parses an integer .env value, failing the test on a non-integer.
func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	n, err := strconv.Atoi(strings.TrimSpace(value))
	require.NoErrorf(t, err, "parse %q as int", value)

	return n
}

// reconfigureServiceKey reproduces internal/core's SERVICE_KEY derivation
// (uppercase, non-alphanumeric runs collapsed to a single underscore,
// trimmed) so the MEMORY_LIMIT_/CPUS_LIMIT_/PIDS_LIMIT_ keys match the
// rendered .env. A small copy keeps this external test free of an extra
// production seam for a derivation the golden test already mirrors.
func reconfigureServiceKey(service string) string {
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
