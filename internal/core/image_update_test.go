package core_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/registry"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// fakeRegistryResolver is the test double for core.RegistryResolver: it
// answers ResolveDigest from a per-reference table (or a per-reference
// error) and records the refs it was asked to resolve so tests can assert
// the EXACT registry traffic — no test touches a real registry ("Test
// through seams"). An unexpected ref fails the call loudly.
type fakeRegistryResolver struct {
	digests map[string]registry.Manifest
	errs    map[string]error
	calls   atomic.Int64
	queried []string
}

func (f *fakeRegistryResolver) ResolveDigest(_ context.Context, ref string) (registry.Manifest, error) {
	f.calls.Add(1)
	f.queried = append(f.queried, ref)
	if err, ok := f.errs[ref]; ok {
		return registry.Manifest{}, err
	}
	if m, ok := f.digests[ref]; ok {
		return m, nil
	}
	return registry.Manifest{}, errors.New("fakeRegistryResolver: unexpected ref " + ref)
}

// imageUpdateFixture seeds a managed stack with the given image pins and
// wires the fake registry resolver into the engine via WithRegistryClient
// so CheckImageUpdates resolves against the fake, never the network.
type imageUpdateFixture struct {
	eng       *core.Engine
	stackBase string
	appID     string
	registry  *fakeRegistryResolver
}

func newImageUpdateFixture(t *testing.T, appID string, pins []state.ImagePin, resolver *fakeRegistryResolver) *imageUpdateFixture {
	t.Helper()

	eng, stateDir := newTestEngine(t, core.WithRegistryClient(func() core.RegistryResolver {
		return resolver
	}))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, appID)

	lock := state.StackLock{
		SchemaVersion:   1,
		AppID:           appID,
		TemplateName:    appID,
		TemplateVersion: "2026-06-01",
		CatalogChannel:  "stable",
		CatalogVersion:  "2026.06.01",
		StackPath:       stackPath,
		ComposeProject:  "wdm-" + appID,
		ImagePins:       pins,
		LastSuccessfulOperation: &types.Operation{
			Kind:       "install",
			At:         time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC),
			WDMVersion: "0.1.0",
		},
	}
	writeStatusStackLock(t, stackBase, appID, lock)

	return &imageUpdateFixture{eng: eng, stackBase: stackBase, appID: appID, registry: resolver}
}

// snapshotDir returns a stable map of relative path -> bytes for every
// regular file under dir so a test can prove CheckImageUpdates wrote
// nothing (the read-only no-mutation contract).
func snapshotStackFiles(t *testing.T, dir string) map[string]string {
	t.Helper()

	snapshot := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		snapshot[rel] = string(data)
		return nil
	})
	require.NoError(t, err)
	return snapshot
}

// TestCheckImageUpdates_CandidateFound proves the happy path: each pinned
// service's tag is resolved to its registry digest, the changed digest is
// marked UpdateAvailable, and the unchanged digest is not — all without a
// real network call (the fake resolver answers).
func TestCheckImageUpdates_CandidateFound(t *testing.T) {
	t.Parallel()

	resolver := &fakeRegistryResolver{
		digests: map[string]registry.Manifest{
			"docker.io/example/app:1.0.0": {Digest: "sha256:newapp", MediaType: "application/vnd.oci.image.manifest.v1+json"},
			"docker.io/example/db:11.4":   {Digest: "sha256:samedb"},
		},
	}
	fx := newImageUpdateFixture(t, "candidate-app", []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0", Digest: "sha256:oldapp"},
		{Service: "db", Image: "docker.io/example/db", Tag: "11.4", Digest: "sha256:samedb"},
	}, resolver)

	report, err := fx.eng.CheckImageUpdates(t.Context(), types.ImageUpdateQuery{AppID: fx.appID})
	require.NoError(t, err)
	require.NotNil(t, report)

	assert.Equal(t, fx.appID, report.AppID)
	assert.False(t, report.CheckedAt.IsZero(), "CheckedAt must be stamped")
	require.Len(t, report.Candidates, 2)

	app := report.Candidates[0]
	assert.Equal(t, "app", app.Service)
	assert.Equal(t, "docker.io/example/app", app.Image)
	assert.Equal(t, "1.0.0", app.CurrentTag)
	assert.Equal(t, "1.0.0", app.LatestTag, "the catalog-pinned tag is the only tag surfaced (decision #59)")
	assert.Equal(t, "sha256:oldapp", app.CurrentDigest)
	assert.Equal(t, "sha256:newapp", app.LatestDigest)
	assert.True(t, app.UpdateAvailable, "a differing registry digest behind the pinned tag is an update")

	db := report.Candidates[1]
	assert.Equal(t, "db", db.Service)
	assert.Equal(t, "sha256:samedb", db.CurrentDigest)
	assert.Equal(t, "sha256:samedb", db.LatestDigest)
	assert.False(t, db.UpdateAvailable, "a matching registry digest is not an update")

	assert.Equal(t, int64(2), resolver.calls.Load(), "exactly one resolve per tagged pin")
}

// TestCheckImageUpdates_UpToDate proves a stack whose recorded digests all
// match the registry reports no available updates.
func TestCheckImageUpdates_UpToDate(t *testing.T) {
	t.Parallel()

	resolver := &fakeRegistryResolver{
		digests: map[string]registry.Manifest{
			"docker.io/example/app:2.0.0": {Digest: "sha256:current"},
		},
	}
	fx := newImageUpdateFixture(t, "uptodate-app", []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0", Digest: "sha256:current"},
	}, resolver)

	report, err := fx.eng.CheckImageUpdates(t.Context(), types.ImageUpdateQuery{AppID: fx.appID})
	require.NoError(t, err)
	require.Len(t, report.Candidates, 1)
	assert.False(t, report.Candidates[0].UpdateAvailable)
	assert.Equal(t, "sha256:current", report.Candidates[0].LatestDigest)
}

// TestCheckImageUpdates_PerServiceRegistryInfoUnavailable proves the
// per-service "registry info unavailable" state represented without a new
// field: a digest-only pin (no tag) has no tag to
// resolve, so its candidate carries the current digest with empty latest
// fields and UpdateAvailable=false and NO registry call is made for it,
// while a sibling tagged pin still resolves normally.
func TestCheckImageUpdates_PerServiceRegistryInfoUnavailable(t *testing.T) {
	t.Parallel()

	resolver := &fakeRegistryResolver{
		digests: map[string]registry.Manifest{
			"docker.io/example/app:1.2.3": {Digest: "sha256:resolved"},
		},
	}
	fx := newImageUpdateFixture(t, "mixed-app", []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "1.2.3", Digest: "sha256:old"},
		{Service: "sidecar", Image: "docker.io/example/sidecar", Tag: "", Digest: "sha256:pinneddigest"},
		{Service: "", Image: "docker.io/example/ghost", Tag: "9.9.9"},
	}, resolver)

	report, err := fx.eng.CheckImageUpdates(t.Context(), types.ImageUpdateQuery{AppID: fx.appID})
	require.NoError(t, err)
	require.Len(t, report.Candidates, 2, "the empty-service pin is skipped")

	app := report.Candidates[0]
	assert.Equal(t, "app", app.Service)
	assert.Equal(t, "sha256:resolved", app.LatestDigest)

	sidecar := report.Candidates[1]
	assert.Equal(t, "sidecar", sidecar.Service)
	assert.Equal(t, "sha256:pinneddigest", sidecar.CurrentDigest)
	assert.Empty(t, sidecar.LatestTag, "a digest-only pin discloses no registry tag")
	assert.Empty(t, sidecar.LatestDigest, "a digest-only pin discloses no registry digest")
	assert.False(t, sidecar.UpdateAvailable)

	assert.Equal(t, int64(1), resolver.calls.Load(), "a digest-only pin makes no registry call")
	assert.Equal(t, []string{"docker.io/example/app:1.2.3"}, resolver.queried)
}

// TestCheckImageUpdates_EmptyCurrentDigestIsNotAnUpdate proves a never-
// recorded current digest
// surfaces the registry digest for visibility but does NOT report an
// update — a missing baseline cannot masquerade as a pending update.
func TestCheckImageUpdates_EmptyCurrentDigestIsNotAnUpdate(t *testing.T) {
	t.Parallel()

	resolver := &fakeRegistryResolver{
		digests: map[string]registry.Manifest{
			"docker.io/example/app:3.0.0": {Digest: "sha256:fromregistry"},
		},
	}
	fx := newImageUpdateFixture(t, "nodigest-app", []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "3.0.0"},
	}, resolver)

	report, err := fx.eng.CheckImageUpdates(t.Context(), types.ImageUpdateQuery{AppID: fx.appID})
	require.NoError(t, err)
	require.Len(t, report.Candidates, 1)
	assert.Equal(t, "sha256:fromregistry", report.Candidates[0].LatestDigest, "the registry digest is still shown")
	assert.Empty(t, report.Candidates[0].CurrentDigest)
	assert.False(t, report.Candidates[0].UpdateAvailable, "no recorded baseline cannot be a pending update")
}

// TestCheckImageUpdates_TransportFailureIsExit8 proves the network/
// verification split: a registry transport failure on the
// EXPLICIT check propagates the registry client's typed
// ErrCodeNetworkFailure (exit 8) — never an exit-3 verification error —
// and writes nothing.
func TestCheckImageUpdates_TransportFailureIsExit8(t *testing.T) {
	t.Parallel()

	netErr := types.NewError(types.ErrCodeNetworkFailure, "the registry request failed", "check the network connection and try again")
	resolver := &fakeRegistryResolver{
		errs: map[string]error{
			"docker.io/example/app:1.0.0": netErr,
		},
	}
	fx := newImageUpdateFixture(t, "netfail-app", []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0", Digest: "sha256:old"},
	}, resolver)

	before := snapshotStackFiles(t, filepath.Join(fx.stackBase, fx.appID))

	report, err := fx.eng.CheckImageUpdates(t.Context(), types.ImageUpdateQuery{AppID: fx.appID})
	require.Error(t, err)
	assert.Nil(t, report)

	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeNetworkFailure, typed.Code, "a transport failure is exit 8")
	assert.NotEqual(t, types.ErrCodeVerificationFailed, typed.Code, "never an exit-3 verification error (decision #64)")

	after := snapshotStackFiles(t, filepath.Join(fx.stackBase, fx.appID))
	assert.Equal(t, before, after, "the read-only check must mutate no stack file on a network failure")
}

// TestCheckImageUpdates_MalformedRefIsExit2 proves a malformed image
// reference surfaces the registry client's usage-validation code (exit 2),
// distinct from a network failure.
func TestCheckImageUpdates_MalformedRefIsExit2(t *testing.T) {
	t.Parallel()

	usageErr := types.NewError(types.ErrCodeUsageValidation, "image reference is invalid", "")
	resolver := &fakeRegistryResolver{
		errs: map[string]error{
			"docker.io/example/app:bad": usageErr,
		},
	}
	fx := newImageUpdateFixture(t, "badref-app", []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "bad"},
	}, resolver)

	report, err := fx.eng.CheckImageUpdates(t.Context(), types.ImageUpdateQuery{AppID: fx.appID})
	require.Error(t, err)
	assert.Nil(t, report)
	var typed *types.Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, types.ErrCodeUsageValidation, typed.Code)
}

// TestCheckImageUpdates_RefusesEmptyMissingAndUnmanaged proves the
// managed-only refusals fire BEFORE any registry call (PRD §10): an empty
// app id, an uninstalled app, and an unmanaged directory all refuse with
// usage validation and zero registry traffic.
func TestCheckImageUpdates_RefusesEmptyMissingAndUnmanaged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		appID        string
		setup        func(t *testing.T, stackBase string)
		wantContains string
	}{
		{
			name:         "empty app id",
			appID:        "",
			wantContains: "app id is required",
		},
		{
			name:         "uninstalled app",
			appID:        "ghost",
			wantContains: "app is not installed",
		},
		{
			name:  "unmanaged directory",
			appID: "handrolled",
			setup: func(t *testing.T, stackBase string) {
				t.Helper()
				dir := filepath.Join(stackBase, "handrolled")
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))
			},
			wantContains: "not managed by wdm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resolver := &fakeRegistryResolver{}
			eng, stateDir := newTestEngine(t, core.WithRegistryClient(func() core.RegistryResolver { return resolver }))
			stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
			if tt.setup != nil {
				tt.setup(t, stackBase)
			}

			report, err := eng.CheckImageUpdates(t.Context(), types.ImageUpdateQuery{AppID: tt.appID})
			require.Error(t, err)
			assert.Nil(t, report)
			var typed *types.Error
			require.ErrorAs(t, err, &typed)
			assert.Equal(t, types.ErrCodeUsageValidation, typed.Code)
			assert.Contains(t, err.Error(), tt.wantContains)
			assert.Zero(t, resolver.calls.Load(), "refusals must precede any registry call")
		})
	}
}

// TestCheckImageUpdates_NilRegistryClientIsTypedNotPanic proves the
// explicit-check nil-client guard: the explicit check is NON-opportunistic
// so a registry factory returning nil cannot silently
// degrade like the Update-planning fold-in does — it would otherwise
// dereference the nil interface in resolveImageUpdateCandidates and panic
// (the no-panic-in-internal/core invariant). Instead the check fails closed
// with a typed ErrCodeGeneric (exit 1) and never panics. (The sibling
// TestUpdate_DryRunNilRegistryClientDegradesToToday proves the fold-in's
// nil tolerance; this closes the asymmetry on the explicit path.)
func TestCheckImageUpdates_NilRegistryClientIsTypedNotPanic(t *testing.T) {
	t.Parallel()

	// A managed stack with a tagged pin so the path reaches the registry
	// client (a digest-only or empty-pin stack would never call ResolveDigest).
	eng, stateDir := newTestEngine(t, core.WithRegistryClient(func() core.RegistryResolver { return nil }))
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, "nilclient-check-app")
	lock := state.StackLock{
		SchemaVersion:   1,
		AppID:           "nilclient-check-app",
		TemplateName:    "nilclient-check-app",
		TemplateVersion: "2026-06-01",
		CatalogChannel:  "stable",
		CatalogVersion:  "2026.06.01",
		StackPath:       stackPath,
		ComposeProject:  "wdm-nilclient-check-app",
		ImagePins: []state.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0", Digest: "sha256:old"},
		},
		LastSuccessfulOperation: &types.Operation{
			Kind:       "install",
			At:         time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC),
			WDMVersion: "0.1.0",
		},
	}
	writeStatusStackLock(t, stackBase, "nilclient-check-app", lock)

	// The call must return a typed error, NOT panic (require.NotPanics
	// makes the no-panic invariant the load-bearing assertion).
	var (
		report *types.ImageUpdateReport
		err    error
	)
	require.NotPanics(t, func() {
		report, err = eng.CheckImageUpdates(t.Context(), types.ImageUpdateQuery{AppID: "nilclient-check-app"})
	}, "a nil registry client must not panic the explicit check")
	require.Error(t, err)
	assert.Nil(t, report)
	assert.True(t, types.IsCode(err, types.ErrCodeGeneric),
		"a nil registry client on the explicit check fails closed with ErrCodeGeneric (exit 1)")
}

// TestCheckImageUpdates_ClosedEngine pins the ErrClosed contract.
func TestCheckImageUpdates_ClosedEngine(t *testing.T) {
	t.Parallel()

	resolver := &fakeRegistryResolver{}
	eng, _ := newTestEngine(t, core.WithRegistryClient(func() core.RegistryResolver { return resolver }))
	require.NoError(t, eng.Close())

	report, err := eng.CheckImageUpdates(t.Context(), types.ImageUpdateQuery{AppID: "x"})
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, report)
	assert.Zero(t, resolver.calls.Load())
}

// TestCheckImageUpdates_CanceledContext proves a canceled context refuses
// before any registry call.
func TestCheckImageUpdates_CanceledContext(t *testing.T) {
	t.Parallel()

	resolver := &fakeRegistryResolver{}
	eng, _ := newTestEngine(t, core.WithRegistryClient(func() core.RegistryResolver { return resolver }))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	report, err := eng.CheckImageUpdates(ctx, types.ImageUpdateQuery{AppID: "x"})
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, report)
	assert.Zero(t, resolver.calls.Load())
}

// TestImageUpdate_ProductionPathHasNoDockerManifestInspect is the
// the invariant guard: a Go-native registry check NEVER shells out to
// `docker manifest inspect` or any docker registry command. It parses
// every non-_test.go production file under internal/core and internal/
// registry and asserts no string literal contains "docker manifest" or
// "manifest inspect" (the comment in image_update.go that NAMES the
// forbidden command is a comment, not a string literal, so it is invisible
// to this AST scan — the scan inspects string literals only).
func TestImageUpdate_ProductionPathHasNoDockerManifestInspect(t *testing.T) {
	t.Parallel()

	wd, err := os.Getwd()
	require.NoError(t, err)
	roots := []string{wd, filepath.Join(filepath.Dir(wd), "registry")}

	for _, root := range roots {
		entries, readErr := os.ReadDir(root)
		require.NoError(t, readErr)
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(root, name)
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			require.NoError(t, parseErr)
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value := strings.ToLower(lit.Value)
				assert.NotContains(t, value, "docker manifest", "production file %s must not embed a docker manifest command", path)
				assert.NotContains(t, value, "manifest inspect", "production file %s must not embed a manifest inspect command", path)
				return true
			})
		}
	}
}
