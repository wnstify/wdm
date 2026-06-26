package core

import (
	"context"
	"testing"
	"time"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

// ResolveDockerSocketSourceForTest exposes the rootless socket-source
// resolver (issue #134) so its env/uid behavior can be exercised directly.
func ResolveDockerSocketSourceForTest() string {
	return resolveDockerSocketSource()
}

// SetRootlessDaemonClientFactoryForTest overrides the daemon-mode probe's
// Docker client (PRD §11, issue #135) so the refusal runs against canned
// `docker info` output without shelling out. It returns a restore func.
func SetRootlessDaemonClientFactoryForTest(factory func() (docker.Client, error)) func() {
	prev := rootlessDaemonClientFactory
	rootlessDaemonClientFactory = factory
	return func() { rootlessDaemonClientFactory = prev }
}

// RecoverOrphanedStackForTest exposes the unexported orphan-recovery path so
// black-box tests can exercise the real removal/refusal seam directly.
func (e *Engine) RecoverOrphanedStackForTest(
	ctx context.Context,
	client docker.Client,
	stackPath string,
	composeProject string,
) error {
	return e.recoverOrphanedStack(ctx, client, stackPath, composeProject)
}

// SwapCoreUpdateCheckNowForTest swaps the package-private coreUpdateCheckNow
// clock seam so the daily launch-check gate methods can be exercised with a
// pinned local wall-clock time. The previous clock is restored on cleanup.
func SwapCoreUpdateCheckNowForTest(t *testing.T, now func() time.Time) {
	t.Helper()

	prev := coreUpdateCheckNow
	coreUpdateCheckNow = now
	t.Cleanup(func() {
		coreUpdateCheckNow = prev
	})
}

// InstallPlanSnapshotForTest is a test-only projection of the install plan.
type InstallPlanSnapshotForTest struct {
	StackPath       string
	ComposeProject  string
	ResolvedValues  map[string]string
	GeneratedFields []string
	LocalPorts      []types.PortBinding
}

// InstallRenderSnapshotForTest is a test-only projection of a rendered install plan.
type InstallRenderSnapshotForTest struct {
	StackPath       string
	ComposeProject  string
	ResolvedValues  map[string]string
	GeneratedFields []string
	LocalPorts      []types.PortBinding
	ComposeBytes    []byte
	EnvBytes        []byte
	AdditionalFiles []render.RenderedFile
	ConfigArtifacts []render.RenderedFile
	ServiceLabels   map[string]map[string]string
	Guidance        *types.PostInstallGuidance
	Frozen          bool
}

// TimezoneLookupDepsForTest is a test seam for timezone resolution.
type TimezoneLookupDepsForTest = timezoneLookupDeps

// SetInstallHostResourceProbeForTest is a temporary test seam.
func SetInstallHostResourceProbeForTest(e *Engine, probe func() (system.HostResources, error)) {
	e.detectHostResources = probe
}

// SetInstallSecretGeneratorForTest is a temporary test seam.
func SetInstallSecretGeneratorForTest(e *Engine, generate func(security.Encoding) (string, error)) {
	e.generateSecret = generate
}

// SetInstallArgon2idGeneratorForTest is a temporary test seam. It pins the
// argon2id one-time credential pair (plaintext, PHC) so install tests can
// assert on a deterministic surfaced plaintext and persisted hash.
func SetInstallArgon2idGeneratorForTest(e *Engine, generate func() (plaintext, phc string, err error)) {
	e.generateArgon2idCredential = generate
}

// SetInstallDockerClientFactoryForTest is a temporary test seam.
func SetInstallDockerClientFactoryForTest(e *Engine, factory func(security.Redactor) (docker.Client, error)) {
	e.newDockerClient = factory
}

// SetInstallPortProbeForTest is a temporary test seam. It replaces the
// real net.Listen port-availability probe so an install over a
// catalog-fixed public port (which the localhost-port rewrite cannot make
// ephemeral) stays deterministic on a busy host.
func SetInstallPortProbeForTest(e *Engine, probe func(context.Context, types.PortBinding) error) {
	e.probePort = probe
}

// PlanInstallForTest is a temporary test seam.
func PlanInstallForTest(
	e *Engine,
	ctx context.Context,
	req types.InstallRequest,
	host system.HostResources,
	onProgress types.ProgressFn,
) (*InstallPlanSnapshotForTest, error) {
	plan, err := e.planInstall(ctx, req, host, onProgress, defaultTimezoneLookupDeps)
	if err != nil {
		return nil, err
	}
	return snapshotInstallPlanForTest(plan), nil
}

// RenderInstallForTest is a temporary test seam.
func RenderInstallForTest(
	e *Engine,
	ctx context.Context,
	req types.InstallRequest,
	host system.HostResources,
	onProgress types.ProgressFn,
) (*InstallRenderSnapshotForTest, error) {
	plan, err := e.planInstall(ctx, req, host, onProgress, defaultTimezoneLookupDeps)
	if err != nil {
		return nil, err
	}
	if err := e.renderInstall(ctx, plan, onProgress); err != nil {
		return nil, err
	}
	return snapshotInstallRenderForTest(plan), nil
}

// DoubleFreezeInstallPlanForTest exercises the produce-then-freeze guard:
// it freezes a fresh plan once (must succeed) then again (must fail closed),
// returning both errors so the test can assert the second is non-nil.
func DoubleFreezeInstallPlanForTest() (first, second error) {
	plan := &installPlan{}
	return plan.freeze(), plan.freeze()
}

// WriteInstallFilesForTest is a temporary test seam for install artifact writes.
// It mirrors production semantics — refusal checks, fresh-directory
// creation, flock acquisition, atomic writes, and the fresh-install
// sad-path cleanup on write faults — releasing the held stack lock
// before returning so tests observe only the on-disk outcome.
func WriteInstallFilesForTest(
	ctx context.Context,
	stackPath string,
	rendered render.RenderedStack,
	onProgress types.ProgressFn,
) error {
	handle, err := writeInstallFiles(ctx, &installPlan{
		stackPath: stackPath,
		rendered:  rendered,
	}, onProgress)
	if err != nil {
		return err
	}
	return handle.Release()
}

// ResolveTimezoneForTest is a temporary test seam.
func ResolveTimezoneForTest(value string, stackPath string, deps TimezoneLookupDepsForTest) (string, error) {
	return resolveTimezone(value, stackPath, deps)
}

// UpdateBackupArtifactPathsForTest exposes the pre-update backup-path
// projection so the test can prove the snapshot covers both
// additional_files and config_generation destinations the rewrite may
// overwrite.
func UpdateBackupArtifactPathsForTest(app catalog.App) []string {
	return updateBackupArtifactPaths(app)
}

// VerifyImagePinsMatchTemplateForTest drives the catalog-pin /
// rendered-template image drift check directly so the matcher logic can
// be table-tested against the real stable catalog and constructed
// drifted compose bytes without rendering through the full install path.
func VerifyImagePinsMatchTemplateForTest(app catalog.App, composeBytes []byte) error {
	return verifyImagePinsMatchTemplate(security.NewActiveRedactor(nil), app, composeBytes)
}

// PortBindingsForTest drives the per-entry catalog→binding expansion and
// range validation directly. The catalog JSON schema is the first line of
// defense (it rejects out-of-pattern ranges and sub-1 ports), but
// internal/core must still fail closed on a malformed entry that slips
// through, so the validation is unit-tested here on constructed ports without
// schema-loading the fixture.
func PortBindingsForTest(port catalog.Port) ([]types.PortBinding, error) {
	return portBindings(port)
}

// VerifyPublicBindsMatchCatalogForTest drives the §11.1(a)(b) rendered-compose
// public-bind scan directly so the loopback-vs-public classification and the
// declaration/render parity invariant can be table-tested against constructed
// compose bytes without rendering through the full install path.
func VerifyPublicBindsMatchCatalogForTest(app catalog.App, composeBytes []byte) error {
	return verifyPublicBindsMatchCatalog(security.NewActiveRedactor(nil), app, composeBytes)
}

// VerifyContainerPrivilegeMatchCatalogForTest drives the §12.2 closed
// container-privilege allow-list scan directly so the catalog-declaration,
// universal-bound, and declaring-service parity arms can be table-tested
// against constructed compose bytes without rendering through the full install
// path.
func VerifyContainerPrivilegeMatchCatalogForTest(app catalog.App, composeBytes []byte) error {
	return verifyContainerPrivilegeMatchCatalog(security.NewActiveRedactor(nil), app, composeBytes)
}

// ContainerPrivilegeDisclosureLinesForTest exposes the §12.2 ELEVATED
// PRIVILEGE finish-screen block builder so its content and omit-when-empty
// behavior can be tested without driving a full install.
func ContainerPrivilegeDisclosureLinesForTest(app catalog.App) []string {
	return containerPrivilegeDisclosureLines(app)
}

// VerifySocketPolicyMatchCatalogForTest drives the PRD §12.1 socket-policy scan
// (catalog socket_proxy declaration validation + the universal direct-socket-mount
// refusal with the enabled-proxy exemption) directly so it can be table-tested
// against constructed compose bytes without rendering through the full install path.
func VerifySocketPolicyMatchCatalogForTest(app catalog.App, composeBytes []byte) error {
	return verifySocketPolicyMatchCatalog(security.NewActiveRedactor(nil), app, composeBytes)
}

// SocketProxyWarningLinesForTest exposes the PRD §12.1 DOCKER SOCKET ACCESS
// WARNING confirmation block builder so its read-vs-read-and-control content and
// omit-when-absent behavior can be tested without driving a full install.
func SocketProxyWarningLinesForTest(app catalog.App) []string {
	return socketProxyWarningLines(app)
}

// VerifyHostModuleMountMatchCatalogForTest drives the PRD §9/§12.2 host
// /lib/modules mount scan (the universal bound on undeclared services plus the
// declaring-service read-only parity check) directly so it can be table-tested
// against constructed compose bytes without rendering through the full install path.
func VerifyHostModuleMountMatchCatalogForTest(app catalog.App, composeBytes []byte) error {
	return verifyHostModuleMountMatchCatalog(security.NewActiveRedactor(nil), app, composeBytes)
}

// VerifyNetworkIPAMMatchCatalogForTest drives the PRD §9 network-IPAM scan (the
// catalog-declaration octet/subnet/service validation, the rendered parity
// check, and the universal bound on undeclared static IPs) directly so it can be
// table-tested against constructed compose bytes without rendering through the
// full install path.
func VerifyNetworkIPAMMatchCatalogForTest(app catalog.App, composeBytes []byte) error {
	return verifyNetworkIPAMMatchCatalog(security.NewActiveRedactor(nil), app, composeBytes)
}

// MarkStackNeedsAttentionAfterFailedRestoreForTest exercises the
// failed-restore needs-attention marker directly, including its torn-lock
// fallback arm (live.wdm.lock unparsable -> base the marker on the
// pre-update snapshot). That arm is not cheaply inducible through the
// public Update API, which always commits a parseable .wdm.lock before the
// sad path.
func MarkStackNeedsAttentionAfterFailedRestoreForTest(e *Engine, stackPath string, existing *state.StackLock) error {
	return e.markStackNeedsAttentionAfterFailedRestore(stackPath, existing)
}

// WriteNeedsAttentionMarkerForTest exercises the marker's row-27 in-place
// writer directly, including its nil-manifest best-effort guard.
func WriteNeedsAttentionMarkerForTest(lockPath string, lock *state.StackLock) error {
	return writeNeedsAttentionMarker(lockPath, lock)
}

// ClassifyPortBindErrorForTest exercises the port-bind error classifier
// directly, so the EACCES-vs-already-in-use split can be unit-tested
// against constructed wrapped syscall errors (a real sub-1024 bind is
// not portable across macOS/CI/root).
func ClassifyPortBindErrorForTest(hostPort int, err error) error {
	return classifyPortBindError(hostPort, err)
}

// ResolveDeleteTargetForTest exercises the §19:452 destructive-delete
// containment check directly so its escape / base-is-itself / happy arms
// can be table-tested without driving the full DeleteApp path (and
// independent of the resolution layer's own earlier symlinked-stack
// refusal, which catches the simplest direct-symlink case before this
// check ever runs). It NEVER deletes anything — it only decides whether a
// deletion is permitted and returns the symlink-resolved safe target.
func ResolveDeleteTargetForTest(stackBase, stackPath string) (string, error) {
	return resolveDeleteTarget(stackBase, stackPath)
}

// IsSuspiciouslyShallowPathForTest exercises the lexical shallow-path
// backstop directly. resolveDeleteTarget calls filepath.EvalSymlinks
// first (requiring the path to exist), so the shallow arm — a defense
// against a stack base that resolves near root — is unit-tested here on
// constructed absolute paths rather than through a brittle real /etc.
func IsSuspiciouslyShallowPathForTest(cleaned string) bool {
	return isSuspiciouslyShallowPath(cleaned)
}

func snapshotInstallPlanForTest(plan *installPlan) *InstallPlanSnapshotForTest {
	if plan == nil {
		return nil
	}
	resolved := make(map[string]string, len(plan.resolvedValues))
	for key, value := range plan.resolvedValues {
		resolved[key] = value
	}
	generated := append([]string(nil), plan.generatedFields...)
	localPorts := append([]types.PortBinding(nil), plan.localPorts...)
	return &InstallPlanSnapshotForTest{
		StackPath:       plan.stackPath,
		ComposeProject:  plan.composeProject,
		ResolvedValues:  resolved,
		GeneratedFields: generated,
		LocalPorts:      localPorts,
	}
}

func snapshotInstallRenderForTest(plan *installPlan) *InstallRenderSnapshotForTest {
	if plan == nil {
		return nil
	}
	base := snapshotInstallPlanForTest(plan)
	serviceLabels := make(map[string]map[string]string, len(plan.rendered.ServiceLabels))
	for service, labels := range plan.rendered.ServiceLabels {
		copied := make(map[string]string, len(labels))
		for key, value := range labels {
			copied[key] = value
		}
		serviceLabels[service] = copied
	}
	return &InstallRenderSnapshotForTest{
		StackPath:       base.StackPath,
		ComposeProject:  base.ComposeProject,
		ResolvedValues:  base.ResolvedValues,
		GeneratedFields: base.GeneratedFields,
		LocalPorts:      base.LocalPorts,
		ComposeBytes:    append([]byte(nil), plan.rendered.ComposeBytes...),
		EnvBytes:        append([]byte(nil), plan.rendered.EnvBytes...),
		AdditionalFiles: append([]render.RenderedFile(nil), plan.rendered.AdditionalFiles...),
		ConfigArtifacts: append([]render.RenderedFile(nil), plan.rendered.ConfigArtifacts...),
		ServiceLabels:   serviceLabels,
		Guidance:        plan.guidance,
		Frozen:          plan.frozen,
	}
}

// SelfUpdateTargetForTest is a test-only projection of the resolved
// self-update install target produced by ResolveSelfUpdateTargetForTest.
type SelfUpdateTargetForTest struct {
	Path string
	Dir  string
}

// ResolveSelfUpdateTargetForTest exposes the unexported writability
// gate (resolveSelfUpdateTarget) so the external core_test package can
// drive it through injected os.Executable / EvalSymlinks seams without
// depending on the test binary's own install location. It also keeps the
// gate referenced so the unused linter does not flag it.
func ResolveSelfUpdateTargetForTest(
	executablePath func() (string, error),
	resolveSymlinks func(string) (string, error),
) (SelfUpdateTargetForTest, error) {
	target, err := resolveSelfUpdateTarget(executablePath, resolveSymlinks)
	return SelfUpdateTargetForTest(target), err
}

// ManualInstallHintForTest exposes the manual-install hint string the gate
// attaches when the install target is not user-writable, so the test can
// assert the exact operator guidance.
const ManualInstallHintForTest = manualInstallHint

// DefaultRunVersionSmokeForTest exposes the production `wdm --version`
// smoke-exec seam (defaultRunVersionSmoke) so the external core_test package
// can prove the real argv-only exec against a test-created script — never the
// test runner — without driving it through the engine's injected seam.
func DefaultRunVersionSmokeForTest(ctx context.Context, binaryPath string) (string, error) {
	return defaultRunVersionSmoke(ctx, binaryPath)
}

// ReconfigureRewriteResultForTest is a test-only projection of the
// reconfigure resolve + in-place rewrite outcome: the validated resource
// values [buildReconfigurePlan] merged from the request and the on-disk
// .env, and the new .env bytes [rewriteResourceEnvLines] produced by
// editing only the targeted service's three resource-limit lines.
type ReconfigureRewriteResultForTest struct {
	Memory  string
	CPUs    string
	PIDs    int
	EnvFile []byte
}

// ReconfigureRewriteStackResultForTest is a test-only projection of the
// full in-place rewrite seam [Engine.rewriteReconfigureStack]: the secret
// literals the returned plan carries for the caller's redactor, and the
// rewritten .env bytes. It exists so a test can prove the rewrite seeds the
// redactor and that the catalog-vs-compose guards run against the on-disk
// compose.
type ReconfigureRewriteStackResultForTest struct {
	ReusedSecretValues []string
	GeneratedValues    []string
	EnvFile            []byte
}

// ReconfigureRewriteStackForTest drives the EXACT in-place rewrite the live
// `wdm resources` reconfigure runs: it resolves the plan with
// [buildReconfigurePlan] and then calls [Engine.rewriteReconfigureStack],
// which reads the on-disk .env and compose, edits only the targeted resource
// lines, seeds the redactor secret literals, and re-runs the install-arc
// catalog-vs-compose guards against the unchanged on-disk compose. It returns
// the secret literals the plan carries plus the rewritten .env, or the
// verification error a tampered on-disk compose triggers.
func (e *Engine) ReconfigureRewriteStackForTest(
	ctx context.Context,
	req types.ReconfigureRequest,
	app catalog.App,
	stackPath string,
	composeProject string,
) (ReconfigureRewriteStackResultForTest, error) {
	plan, err := buildReconfigurePlan(req, app, stackPath, composeProject)
	if err != nil {
		return ReconfigureRewriteStackResultForTest{}, err
	}
	rewrite, err := e.rewriteReconfigureStack(ctx, plan, nil)
	if err != nil {
		return ReconfigureRewriteStackResultForTest{}, err
	}
	return ReconfigureRewriteStackResultForTest{
		ReusedSecretValues: rewrite.reusedSecretValues,
		GeneratedValues:    rewrite.generatedValues,
		EnvFile:            rewrite.rendered.EnvBytes,
	}, nil
}

// ReconfigureResolveRewriteForTest drives the EXACT reconfigure resolve +
// in-place rewrite chain the live `wdm resources` reconfigure uses, without
// the runtime lock, backup, Docker, or manifest steps: it runs
// [buildReconfigurePlan] (catalog band + allow_override validation, the
// on-disk .env read via readServiceResourceValues, and the requested-vs-
// installed merge) and then [rewriteResourceEnvLines] over the stack's
// existing .env bytes with serviceKey(plan.service) and the merged values —
// the same two functions [Engine.rewriteReconfigureStack] calls. It exists
// so the catalog-wide reconfigure regression guard exercises the real path
// rather than a reimplementation. envBytes are the stack's current .env; the
// returned EnvFile is the rewritten output.
func ReconfigureResolveRewriteForTest(
	req types.ReconfigureRequest,
	app catalog.App,
	stackPath string,
	composeProject string,
	envBytes []byte,
) (ReconfigureRewriteResultForTest, error) {
	plan, err := buildReconfigurePlan(req, app, stackPath, composeProject)
	if err != nil {
		return ReconfigureRewriteResultForTest{}, err
	}
	newEnv, err := rewriteResourceEnvLines(envBytes, serviceKey(plan.service), plan.memory, plan.cpus, plan.pids)
	if err != nil {
		return ReconfigureRewriteResultForTest{}, err
	}
	return ReconfigureRewriteResultForTest{
		Memory:  plan.memory,
		CPUs:    plan.cpus,
		PIDs:    plan.pids,
		EnvFile: newEnv,
	}, nil
}
