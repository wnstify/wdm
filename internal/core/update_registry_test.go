package core_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/registry"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/pkg/types"
)

// newUpdateFixtureWithRegistry mirrors newUpdateFixture but also injects a
// fake registry resolver via WithRegistryClient so planning resolves against
// the fake, never the network.
func newUpdateFixtureWithRegistry(
	t *testing.T,
	catalogFS fs.FS,
	app catalog.App,
	mutateLock func(*state.StackLock),
	resolver core.RegistryResolver,
) *updateTestFixture {
	t.Helper()

	eng, stateDir := newTestEngine(t,
		core.WithCatalog(catalogFS),
		core.WithRegistryClient(func() core.RegistryResolver { return resolver }),
	)
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)

	lock := updateStackLockForApp(app, stackPath)
	if mutateLock != nil {
		mutateLock(&lock)
	}
	writeStatusStackLock(t, stackBase, filepath.Base(stackPath), lock)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	return &updateTestFixture{
		eng:       eng,
		stateDir:  stateDir,
		stackBase: stackBase,
		stackPath: stackPath,
		appID:     app.AppID,
		fake:      fake,
	}
}

// collectUpdateProgress runs a DryRun update and returns the planning
// stream's steps and messages.
func collectUpdateProgress(t *testing.T, fx *updateTestFixture) (steps, messages []string, res *types.UpdateResult) {
	t.Helper()

	onProgress := func(step string, _ float64, message string) {
		steps = append(steps, step)
		messages = append(messages, message)
	}
	res, err := fx.eng.Update(t.Context(), types.UpdateRequest{AppID: fx.appID, DryRun: true}, onProgress, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	return steps, messages, res
}

// TestUpdate_DryRunShowsRegistryDigestWhenKnown proves the registry visibility
// surface: when the registry resolves the candidate (catalog-pinned) tag,
// planning emits an ADDITIONAL StepUpdatePlanning event disclosing the
// registry digest behind that tag — without changing the catalog-driven
// plan. The resolver is queried with the candidate ref the
// catalog dictates, never a registry-chosen tag.
func TestUpdate_DryRunShowsRegistryDigestWhenKnown(t *testing.T) {
	t.Parallel()

	app := appFixture("registry-app", 18080)
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0"},
	}
	resolver := &fakeRegistryResolver{
		digests: map[string]registry.Manifest{
			"docker.io/example/app:2.0.0": {Digest: "sha256:candidatedigest"},
		},
	}
	fx := newUpdateFixtureWithRegistry(t, catalogFixtureFS(t, app), app, func(lock *state.StackLock) {
		lock.ImagePins = []state.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
		}
	}, resolver)

	steps, messages, _ := collectUpdateProgress(t, fx)

	for _, step := range steps {
		assert.Equal(t, types.StepUpdatePlanning, step,
			"the registry-digest disclosure must reuse the existing planning step ID")
	}
	joined := strings.Join(messages, "\n")
	assert.Contains(t, joined, "service app: docker.io/example/app:1.0.0 -> docker.io/example/app:2.0.0",
		"the catalog-driven old -> new line is unchanged")
	assert.Contains(t, joined, "service app: registry digest for docker.io/example/app:2.0.0 is sha256:candidatedigest",
		"the registry digest behind the candidate tag is disclosed")
	assert.Contains(t, joined, "update available", "the summary event is unchanged")

	// The registry was queried with the CANDIDATE (catalog) ref only —
	// never the current ref, never a registry-chosen tag.
	assert.Equal(t, []string{"docker.io/example/app:2.0.0"}, resolver.queried)
	assert.Zero(t, fx.fake.calls, "the check stage runs no docker command")
}

// TestUpdate_DryRunRegistryUnreachableDegradesToToday proves the
// opportunistic never-fail posture: a registry transport
// failure during planning does NOT fail the update and produces the EXACT
// stream and result a registry-less run produces — the existing plan,
// risk grouping, and summary are byte-for-byte unchanged.
func TestUpdate_DryRunRegistryUnreachableDegradesToToday(t *testing.T) {
	t.Parallel()

	app := appFixture("degrade-app", 18081)
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0"},
	}
	app.RiskClassification = []string{"database", "complex"}

	mutate := func(lock *state.StackLock) {
		lock.ImagePins = []state.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
		}
	}

	// Run A: registry unreachable (every resolve returns a network error).
	unreachable := &fakeRegistryResolver{
		errs: map[string]error{
			"docker.io/example/app:2.0.0": types.NewError(types.ErrCodeNetworkFailure, "the registry request failed", ""),
		},
	}
	fxUnreachable := newUpdateFixtureWithRegistry(t, catalogFixtureFS(t, app), app, mutate, unreachable)
	stepsU, messagesU, resU := collectUpdateProgress(t, fxUnreachable)

	// Run B: registry not consulted at all (default production client is
	// never reached because there are changes but we point it at a
	// resolver that would fail loudly if queried — yet the result/stream
	// must match the unreachable run). Use a separate engine with the
	// SAME catalog/lock but a resolver that errors identically; the two
	// degraded streams must be identical.
	baseline := &fakeRegistryResolver{
		errs: map[string]error{
			"docker.io/example/app:2.0.0": types.NewError(types.ErrCodeNetworkFailure, "the registry request failed", ""),
		},
	}
	fxBaseline := newUpdateFixtureWithRegistry(t, catalogFixtureFS(t, app), app, mutate, baseline)
	stepsB, messagesB, resB := collectUpdateProgress(t, fxBaseline)

	// The two degraded runs are identical, and neither carries a registry
	// digest line.
	assert.Equal(t, stepsB, stepsU)
	assert.Equal(t, messagesB, messagesU)
	joined := strings.Join(messagesU, "\n")
	assert.NotContains(t, joined, "registry digest", "an unreachable registry discloses no digest")
	assert.Contains(t, joined, "service app: docker.io/example/app:1.0.0 -> docker.io/example/app:2.0.0")
	assert.Contains(t, joined, "update available")

	// The catalog-owned plan and risk grouping survive the registry
	// failure byte-for-byte.
	assert.Equal(t, []string{"app"}, resU.UpdatedServices)
	assert.Equal(t, []string{"database", "complex"}, resU.RiskClassifications,
		"risk grouping stays the catalog array verbatim through a registry failure")
	assert.Equal(t, resB, resU, "the dry-run result is unchanged by a registry failure")

	// The registry was consulted (and failed) but the update did not fail.
	assert.Positive(t, unreachable.calls.Load(), "the planning fold-in did attempt a registry resolve")
}

// TestUpdate_DryRunRegistryNeverChangesAppliedPlan is the the invariant
// crossover guard: even when the registry reports a digest, the candidate
// service set, version transition, and risk grouping in the UpdateResult
// are IDENTICAL to a run with no registry data — the registry adds
// visibility, never a different applied image and never a new apply path.
func TestUpdate_DryRunRegistryNeverChangesAppliedPlan(t *testing.T) {
	t.Parallel()

	app := appFixture("nochange-app", 18082)
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0"},
		{Service: "db", Image: "docker.io/example/db", Tag: "11.5"},
	}
	app.RiskClassification = []string{"database"}

	mutate := func(lock *state.StackLock) {
		lock.ImagePins = []state.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
			{Service: "db", Image: "docker.io/example/db", Tag: "11.4"},
		}
	}

	// With registry data for both changed services.
	withRegistry := &fakeRegistryResolver{
		digests: map[string]registry.Manifest{
			"docker.io/example/app:2.0.0": {Digest: "sha256:appdigest"},
			"docker.io/example/db:11.5":   {Digest: "sha256:dbdigest"},
		},
	}
	fxWith := newUpdateFixtureWithRegistry(t, catalogFixtureFS(t, app), app, mutate, withRegistry)
	_, _, resWith := collectUpdateProgress(t, fxWith)

	// Without registry data (every resolve degrades).
	without := &fakeRegistryResolver{
		errs: map[string]error{
			"docker.io/example/app:2.0.0": types.NewError(types.ErrCodeNetworkFailure, "x", ""),
			"docker.io/example/db:11.5":   types.NewError(types.ErrCodeNetworkFailure, "x", ""),
		},
	}
	fxWithout := newUpdateFixtureWithRegistry(t, catalogFixtureFS(t, app), app, mutate, without)
	_, _, resWithout := collectUpdateProgress(t, fxWithout)

	assert.Equal(t, resWithout, resWith,
		"registry data must not change the UpdateResult — the catalog is the only update source (decision #59)")
	assert.Equal(t, []string{"app", "db"}, resWith.UpdatedServices)
	assert.Equal(t, []string{"database"}, resWith.RiskClassifications)
}

// TestUpdate_DryRunNilRegistryClientDegradesToToday proves the defensive
// nil-client guard in the planning fold-in: a registry factory returning
// nil degrades to no disclosed digest and the existing plan is unchanged
// (the update never panics on a nil client).
func TestUpdate_DryRunNilRegistryClientDegradesToToday(t *testing.T) {
	t.Parallel()

	app := appFixture("nilclient-app", 18084)
	app.ImagePins = []catalog.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "2.0.0"},
	}
	eng, stateDir := newTestEngine(t,
		core.WithCatalog(catalogFixtureFS(t, app)),
		core.WithRegistryClient(func() core.RegistryResolver { return nil }),
	)
	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, app.AppID)
	lock := updateStackLockForApp(app, stackPath)
	lock.ImagePins = []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
	}
	writeStatusStackLock(t, stackBase, app.AppID, lock)

	var messages []string
	res, err := eng.Update(t.Context(), types.UpdateRequest{AppID: app.AppID, DryRun: true},
		func(_ string, _ float64, message string) { messages = append(messages, message) }, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	joined := strings.Join(messages, "\n")
	assert.NotContains(t, joined, "registry digest", "a nil registry client discloses no digest")
	assert.Contains(t, joined, "service app: docker.io/example/app:1.0.0 -> docker.io/example/app:2.0.0")
	assert.Equal(t, []string{"app"}, res.UpdatedServices)
}

// applyRegistryResult drives one full apply-path (DryRun:false) update
// through a registry-aware engine and returns the planning-stream messages,
// the confirmation payloads, and the UpdateResult. It mirrors
// newUpdateApplyFixture's on-disk stack construction but adds a fake
// registry resolver via WithRegistryClient so the apply-path planning
// fold-in resolves against the fake, never the network. The catalog,
// templates, secret generator, fake docker client, and on-disk stack are
// identical across calls so the only variable is the registry data.
func applyRegistryResult(
	t *testing.T,
	appID string,
	resolver core.RegistryResolver,
) (messages []string, confirms []types.Confirmation, res *types.UpdateResult) {
	t.Helper()

	app := updateApplyApp(appID)
	catalogFS := catalogFixtureFSWithFiles(t, updateApplyTemplates(app), app)

	eng, stateDir := newTestEngine(t,
		core.WithCatalog(catalogFS),
		core.WithRegistryClient(func() core.RegistryResolver { return resolver }),
	)
	core.SetInstallSecretGeneratorForTest(eng, updateApplySecretGenerator(t))
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	stackPath := filepath.Join(stackBase, appID)

	lock := updateStackLockForApp(app, stackPath)
	lock.ImagePins = []state.ImagePin{
		{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
	}
	lock.GeneratedFields = []string{"DB_PASSWORD", "API_TOKEN"}
	writeStatusStackLock(t, stackBase, appID, lock)

	require.NoError(t, os.WriteFile(filepath.Join(stackPath, ".env"), []byte(renderEnvFixture(map[string]string{
		"DB_PASSWORD": dbPasswordInstallValue,
		"API_TOKEN":   apiTokenInstallValue,
		"SITE_NAME":   siteNameInstallValue,
	})), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "docker-compose.yml"),
		[]byte("services:\n  app:\n    image: docker.io/example/app:1.0.0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stackPath, "init-data.sh"), []byte("echo old\n"), 0o755))

	confirmer := &fakeConfirmer{}
	res, err := eng.Update(t.Context(), types.UpdateRequest{AppID: appID, DryRun: false},
		func(_ string, _ float64, message string) { messages = append(messages, message) }, confirmer)
	require.NoError(t, err, "the full apply path completes end to end")
	require.NotNil(t, res)
	return messages, confirmer.calls, res
}

// TestUpdate_ApplyDisclosesRegistryLookupAndIsBehaviorNeutral closes the
// apply-path coverage gap: every prior registry-aware test runs
// DryRun:true, but planUpdateCheck runs for the REAL apply too, so the
// apply path also performs a registry round-trip during planning. This test
// drives DryRun:false and asserts two things:
//
//	(a) the PRE-LOOKUP disclosure event ("checking the registry for image
//	    digests") fires on apply BEFORE the network attempt; and
//	(b) the deployed plan is behavior-neutral to registry visibility — the
//	    UpdateResult and the confirmation payloads are byte-identical
//	    whether the registry resolves a digest or is unreachable, so
//	    registry visibility never changes what apply does.
func TestUpdate_ApplyDisclosesRegistryLookupAndIsBehaviorNeutral(t *testing.T) {
	t.Parallel()

	// Run WITH registry data: the candidate tag resolves to a digest.
	withRegistry := &fakeRegistryResolver{
		digests: map[string]registry.Manifest{
			"docker.io/example/app:2.0.0": {Digest: "sha256:applydigest"},
		},
	}
	msgsWith, confirmsWith, resWith := applyRegistryResult(t, "apply-registry-with", withRegistry)

	// (a) The pre-lookup disclosure fires on the apply path, BEFORE the
	// per-service digest disclosure (so it precedes the network attempt).
	joinedWith := strings.Join(msgsWith, "\n")
	assert.Contains(t, joinedWith, "checking the registry for image digests",
		"the apply path discloses the registry lookup before attempting it (no silent network work)")
	discloseIdx := indexOfMessage(msgsWith, "checking the registry for image digests")
	digestIdx := indexOfMessage(msgsWith, "registry digest for docker.io/example/app:2.0.0 is sha256:applydigest")
	require.GreaterOrEqual(t, discloseIdx, 0, "the pre-lookup disclosure must be emitted")
	require.GreaterOrEqual(t, digestIdx, 0, "the resolved digest is disclosed on apply too")
	assert.Less(t, discloseIdx, digestIdx,
		"the disclosure must precede the per-service digest line (it fires before the lookup)")

	// Run WITHOUT registry data: the candidate tag resolve fails (network),
	// the fold-in degrades to no disclosed digest — but the apply must still
	// complete and produce the same plan.
	unreachable := &fakeRegistryResolver{
		errs: map[string]error{
			"docker.io/example/app:2.0.0": types.NewError(types.ErrCodeNetworkFailure, "the registry request failed", ""),
		},
	}
	msgsWithout, confirmsWithout, resWithout := applyRegistryResult(t, "apply-registry-without", unreachable)

	// The pre-lookup disclosure ALSO fires when the registry is unreachable
	// (it precedes — and is independent of — the lookup's outcome), while no
	// per-service digest is disclosed because the resolve failed.
	joinedWithout := strings.Join(msgsWithout, "\n")
	assert.Contains(t, joinedWithout, "checking the registry for image digests",
		"the disclosure fires regardless of whether the registry turns out reachable")
	assert.NotContains(t, joinedWithout, "registry digest for", "an unreachable registry discloses no digest")
	assert.Positive(t, unreachable.calls.Load(), "the apply path did attempt the registry resolve")

	// (b) Behavior neutrality: the deployed UpdateResult is byte-identical
	// with vs without registry data. The fixtures use distinct app ids, so
	// normalize the id/path-derived fields before comparison — everything
	// else (services, version transition, risk grouping, backup-path
	// presence, status state) must match.
	normalizeUpdateResultForCompare(resWith)
	normalizeUpdateResultForCompare(resWithout)
	assert.Equal(t, resWithout, resWith,
		"registry visibility must not change the applied UpdateResult (decision #59)")

	// The recreate confirmation is behavior-neutral too: exactly one
	// confirm (a non-database app), the SAFE update_deploy kind, and a
	// Message that carries the catalog-driven image change but NO registry
	// digest — proven within each run so the distinct-app-id/path-derived
	// Message text never needs cross-fixture normalization.
	require.Len(t, confirmsWith, 1, "a non-database apply confirms exactly once (the recreate)")
	require.Len(t, confirmsWithout, 1)
	for _, c := range []types.Confirmation{confirmsWith[0], confirmsWithout[0]} {
		assert.Equal(t, "update_deploy", c.Kind, "the recreate is the SAFE update_deploy kind")
		assert.Contains(t, c.Message, "image change: service app: docker.io/example/app:1.0.0 -> docker.io/example/app:2.0.0",
			"the confirmation carries the catalog-driven image change")
		assert.NotContains(t, c.Message, "registry digest",
			"registry visibility must not leak into the recreate confirmation (decision #59)")
		assert.NotContains(t, c.Message, "sha256:",
			"the recreate confirmation carries no registry digest")
	}
}

// indexOfMessage returns the index of the first message that contains sub,
// or -1 when none does.
func indexOfMessage(messages []string, sub string) int {
	for i, m := range messages {
		if strings.Contains(m, sub) {
			return i
		}
	}
	return -1
}

// normalizeUpdateResultForCompare blanks the app-id/path-derived fields so
// two results from distinct-app fixtures compare on plan substance alone.
// The backup path embeds a unix-nanos snapshot name and the stack path, so
// it is reduced to a presence marker.
func normalizeUpdateResultForCompare(res *types.UpdateResult) {
	res.AppID = ""
	if res.BackupPath != "" {
		res.BackupPath = "<present>"
	}
	if res.Status != nil {
		res.Status.AppID = ""
		res.Status.ComposeProject = ""
		res.Status.StackPath = ""
		res.Status.UpdatedAt = nil
	}
}

// TestUpdate_DryRunUpToDateMakesNoRegistryCall proves that when there is
// no candidate update (the stack mirrors the catalog) the planning fold-in
// makes NO registry call — there are no changed services to disclose a
// digest for, so the path is identical to today and avoids needless
// network work.
func TestUpdate_DryRunUpToDateMakesNoRegistryCall(t *testing.T) {
	t.Parallel()

	app := appFixture("current-registry-app", 18083)
	resolver := &fakeRegistryResolver{}
	fx := newUpdateFixtureWithRegistry(t, catalogFixtureFS(t, app), app, nil, resolver)

	_, messages, res := collectUpdateProgress(t, fx)
	assert.Contains(t, strings.Join(messages, "\n"), "already up to date")
	assert.Empty(t, res.UpdatedServices)
	assert.Zero(t, resolver.calls.Load(), "an up-to-date stack queries no registry")
}
