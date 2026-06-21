package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/logging"
	"github.com/wnstify/wdm/internal/registry"
	"github.com/wnstify/wdm/internal/release"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

// ErrClosed is returned by [Engine] methods invoked after
// [Engine.Close]. The engine.Engine contract requires methods to fail
// promptly once the receiver is closed: once Close releases flock
// handles and open log files, further work would be unsafe. Today
// Close holds no such resources (see [Engine.Close]).
var ErrClosed = errors.New("core: engine closed")

// Engine is the [pkg/engine.Engine] implementation. All paths held on
// the receiver are absolute and resolved at construction time; the
// engine does no XDG resolution or "~/" expansion per call.
// Concurrency: every method is safe for concurrent use. The only
// mutable field is the [atomic.Bool] closed flag, set by Close and
// read by every method's entry check.
type Engine struct {
	// settings is the resolved user configuration loaded from
	// configPath at construction time. Returned by [Engine.Settings]
	// as a defensive copy. Never nil: [New] populates PRD §34
	// defaults when config.toml is missing.
	settings *types.Settings

	// configPath is the absolute path to config.toml: the file settings
	// was loaded from, and where UpdateSettings writes back.
	configPath string

	// stackBase is the absolute path the scanner walks. Resolved
	// from WithStackBaseDir if set, otherwise from
	// settings.BaseStackPath with leading "~/" expanded.
	stackBase string

	// stateDir is the absolute path to ~/.local/state/wdm. Hosts the
	// global runtime.lock the state-changing methods acquire.
	stateDir string

	// dataDir is the absolute path to ~/.local/share/wdm. Roots the
	// catalog channel directories the install path reads from its
	// catalogs/ subdirectory when no catalog FS is injected.
	dataDir string

	// logger is the engine's structured logger, used by [Engine.List]
	// to surface scan warnings (PRD §26). Never nil: without WithLogger,
	// [New] builds a redaction-wrapped logger over [os.Stderr] via
	// [logging.New] and [security.NewActiveRedactor] so every record
	// passes through the active redactor before the sink (PRD §11, §24).
	// A caller-supplied logger is used as-is:
	// the caller already chose the handler chain.
	logger *slog.Logger

	// catalog is the optional catalog filesystem supplied via
	// [WithCatalog], which the install path reads in preference to the
	// dataDir catalogs/ tree. Stored as [fs.FS] so tests can inject a
	// fixture FS via [pkg/engine.WithCatalog] without touching real
	// channels (PRD §22).
	catalog fs.FS

	// version is the wdm binary version reported to runtime.lock
	// by state-changing engine methods (PRD §26, exit
	// criterion 396). From [WithVersion] when supplied, else "dev"
	// in [New] to match cmd/wdm's local-Makefile default. Never empty
	// on a constructed Engine.
	version string

	// closed is set true by [Engine.Close] so subsequent calls fail
	// with [ErrClosed]. [atomic.Bool] gives lock-free reads on every
	// method's entry check.
	closed atomic.Bool

	// logFile is the open latest.log handle the default logger writes to
	// when [New] opened the PRD §24 file sink. Nil when a logger was
	// supplied via [WithLogger] or when the sink failed to open and the
	// engine fell back to the surface writer. [Engine.Close] closes it.
	logFile io.Closer

	// detectHostResources probes host CPU/memory for install planning.
	// Default is [system.DetectHostResources]; tests may replace it.
	detectHostResources func() (system.HostResources, error)

	// generateSecret creates install-time secret placeholder values.
	// Default is [security.GenerateSecret]; tests may replace it.
	generateSecret func(security.Encoding) (string, error)

	// generateArgon2idCredential mints an argon2id one-time credential
	// pair: the random plaintext surfaced once and the PHC hash persisted
	// in .env. A separate seam from [generateSecret] because it returns a
	// pair, not a single encoded value. Default is
	// [security.GenerateArgon2idCredential]; tests may replace it.
	generateArgon2idCredential func() (plaintext, phc string, err error)

	// newDockerClient constructs the Docker client the install path uses
	// (Compose config validation, catalog network pre-creation, compose
	// up -d deployment, image-digest capture, post-deploy container
	// inspection). The factory receives the per-operation redactor
	// carrying the generated install secrets so Docker stderr is scrubbed
	// before it reaches errors or logs. Default
	// is [defaultDockerClientFactory]; tests may replace it with a fake
	// [docker.Client] factory.
	newDockerClient func(security.Redactor) (docker.Client, error)

	// probePort verifies a single planned host binding is free, both at
	// planning time and on the pre-deploy re-check. Default is
	// [checkPortAvailable] (a real net.Listen); tests may replace it so a
	// catalog-fixed public port — which a localhost-port rewrite cannot make
	// ephemeral — does not flake on a busy host.
	probePort func(context.Context, types.PortBinding) error

	// releaseDeps is the catalog-update network+verify seam.
	// newReleaseClient builds the trusted GitHub release metadata
	// client CheckCatalogUpdate / ApplyCatalogUpdate use to resolve the
	// latest release (network failures -> exit 8). verifyCatalogBundle
	// downloads and verifies the catalog asset set fail-closed before any
	// write. Defaulted in
	// [New] to the production constructors ([defaultReleaseDeps]) that
	// source the trust anchors inside internal/release so internal/core
	// stays free of the sigstore-go verifier tree; tests
	// inject an offline fake via [WithReleaseDeps].
	releaseDeps releaseDeps

	// selfUpdateDeps is the binary self-update execution seam: the
	// filesystem and process-exec touchpoints behind the verified
	// binary replacement, the wdm.previous retention, and the post-replace
	// version smoke check. It mirrors the [releaseDeps] single-struct seam
	// precedent so a test fixture replaces every touchpoint in lockstep and
	// NO test ever resolves the real running executable, stages over the
	// real binary, or execs the real test runner. Defaulted in [New] to the
	// production functions ([defaultSelfUpdateDeps]); the release-metadata
	// client is reused from [releaseDeps.newReleaseClient] rather than
	// duplicated here. Tests inject the seam via [WithSelfUpdateDeps].
	selfUpdateDeps selfUpdateDeps

	// newRegistryClient builds the Go-native registry metadata client the
	// read-only image-update check ([Engine.CheckImageUpdates]) and the
	// opportunistic registry-digest visibility folded into [Engine.Update]
	// planning use to resolve a catalog-pinned tag to its canonical
	// registry digest. A factory, not
	// a stored client, so a per-call client carries the caller's context
	// deadline cleanly, mirroring [releaseDeps.newReleaseClient]. The client
	// is anonymous — public metadata only, no credentials — and
	// contacts the registry through Go HTTP code, NEVER `docker manifest
	// inspect` or any Docker. Defaulted in [New] to
	// [defaultRegistryClient]; tests inject an httptest-backed or offline
	// fake via [WithRegistryClient] so NO test touches a real registry.
	newRegistryClient func() RegistryResolver
}

// RegistryResolver is the minimal registry seam internal/core depends on:
// it resolves a tag reference to its canonical manifest digest over the Go-native
// registry client. The production *[registry.Client] satisfies it; tests
// inject a fake through [WithRegistryClient] so no test touches a real
// registry. It is exported so external test packages can supply that fake,
// mirroring the exported types the [releaseDeps] and [selfUpdateDeps]
// seams expose. It is deliberately narrow: the visibility surface needs
// only the digest behind the catalog-pinned tag, never a registry-chosen
// tag.
type RegistryResolver interface {
	// ResolveDigest resolves ref's tag to its canonical manifest digest.
	// A transport/auth/rate-limit fault is [types.ErrCodeNetworkFailure]
	// (exit 8); a malformed ref is [types.ErrCodeUsageValidation] (exit
	// 2). It performs no trust verification.
	ResolveDigest(ctx context.Context, ref string) (registry.Manifest, error)
}

// releaseDeps bundles the catalog-update network and verification seams
// into one injectable surface. It mirrors the single-field
// [newDockerClient] seam precedent: a struct of pure factory/verify
// functions, defaulted in [New], overridable as a unit via
// [WithReleaseDeps] so a test fixture replaces both halves in lockstep and
// no test ever touches real GitHub or the live Sigstore root.
type releaseDeps struct {
	// newReleaseClient builds the trusted release metadata client. It is a
	// factory (not a stored client) so a per-call client carries the
	// caller's context deadline cleanly. Default: [defaultReleaseClient].
	newReleaseClient func() (*release.Client, error)

	// verifyCatalogBundle downloads and verifies the catalog asset set for
	// the resolved release metadata, returning the verified bundle bytes
	// and provenance with NO filesystem write. The supplied
	// client is the same one newReleaseClient produced so downloads hit the
	// same endpoint the metadata named. Default:
	// [release.VerifyCatalogBundleProduction].
	verifyCatalogBundle func(context.Context, *release.Client, *release.Metadata) (*release.VerifiedCatalogBundle, error)
}

// selfUpdateDeps bundles the binary self-update execution touchpoints into
// one injectable surface, mirroring the [releaseDeps]
// precedent. Every field is a seam so the apply path's filesystem and
// process-exec side effects run against test fixtures: a test never
// resolves the real running executable, never stages over the real binary,
// and never execs the real test runner. The download+verify half is NOT
// duplicated here — it reuses [release.StageCandidateProduction] through the
// stageCandidate field and [releaseDeps.newReleaseClient] for the metadata
// client.
type selfUpdateDeps struct {
	// executablePath resolves the running executable's path, feeding the
	// [os.Executable]; tests return a fake path inside a t.TempDir.
	executablePath func() (string, error)

	// resolveSymlinks resolves symlinks in the executable path so the gate
	// judges the real file's directory (PRD §12/§13). Default:
	// [filepath.EvalSymlinks]; tests pass an identity or a controlled map.
	resolveSymlinks func(string) (string, error)

	// stageCandidate downloads and verifies the candidate binary fail-closed
	// into stagingDir, returning the verified staged binary. Default:
	// [release.StageCandidateProduction] (which sources the trust anchors
	// internal/core must not touch, the invariant); tests inject an offline
	// fake-release-backed verify+stage. A verification fault is exit 3, a
	// transport fault exit 8 — both propagated unchanged.
	stageCandidate func(context.Context, *release.Client, *release.Metadata, string) (*release.StagedCandidate, error)

	// runVersionSmoke execs `<binaryPath> --version` (argv only, NO shell —
	// PRD §12) and returns the trimmed reported version, or an error on a
	// non-zero exit or exec failure. Default: [defaultRunVersionSmoke] (a
	// bounded [exec.CommandContext]); tests return a controlled version/exit
	// so the real test binary never runs.
	runVersionSmoke func(ctx context.Context, binaryPath string) (string, error)
}

// The compile-time confirmation that *Engine satisfies the
// pkg/engine.Engine contract lives in pkg/engine/new.go, not here, so
// internal/core no longer imports pkg/engine: the bridge wires
// pkg/engine.New through to core.New and checks the assertion at the
// bridge site.

// New constructs an [*Engine] from the supplied options. The returned
// engine is ready for every lifecycle call: List, Settings,
// UpdateSettings, Install, Status, Logs, Update, Remove, Restart,
// ValidateConfig, ListBackups, RestoreBackup, AvailableApps,
// AvailableApp, DeleteApp, RuntimeLockStatus, and ClearStaleRuntimeLock
// are live.
// Construction is eager: config.toml is loaded and validated
// immediately so callers get the [types.ErrConfigInvalid] signal at
// startup rather than on first use (PRD §29, §34). When config.toml is
// absent, PRD §34 defaults are used silently so a first-launch user
// without a config can still run "wdm apps list".
// XDG path resolution follows the spec strictly: relative $XDG_*_HOME
// values are ignored (security posture against PATH-style injection per
// the XDG Base Directory Specification). Leading "~/" in user-supplied
// paths is expanded against [os.UserHomeDir]; the expansion site is
// centralized in internal/core.
// Errors are wrapped with the "core.New:" prefix so callers can tell
// construction failures from later operation failures even after the
// error has propagated through several layers.
func New(opts ...Option) (*Engine, error) {
	cfg := &config{}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("core.New: %w", err)
		}
	}

	if cfg.version == "" {
		cfg.version = "dev"
	}

	if err := resolveDirs(cfg); err != nil {
		return nil, fmt.Errorf("core.New: %w", err)
	}

	var logFile io.Closer
	if cfg.logger == nil {
		logger, closer, err := buildDefaultLogger(cfg.stateDir, cfg.fallbackLog)
		if err != nil {
			return nil, fmt.Errorf("core.New: %w", err)
		}
		cfg.logger = logger
		logFile = closer
	}

	// Release the log handle if construction fails before the Engine owns it.
	constructed := false
	defer func() {
		if !constructed && logFile != nil {
			_ = logFile.Close() //nolint:errcheck // best-effort unwind on a failure path; the construction error is what the caller needs.
		}
	}()

	settings, err := loadConfigOrDefaults(cfg.configPath)
	if err != nil {
		return nil, fmt.Errorf("core.New: %w", err)
	}

	stackBase, err := resolveStackBase(cfg.stackBaseDir, settings)
	if err != nil {
		return nil, fmt.Errorf("core.New: %w", err)
	}

	constructed = true
	return &Engine{
		settings:                   settings,
		configPath:                 cfg.configPath,
		stackBase:                  stackBase,
		stateDir:                   cfg.stateDir,
		dataDir:                    cfg.dataDir,
		logger:                     cfg.logger,
		logFile:                    logFile,
		catalog:                    cfg.catalog,
		version:                    cfg.version,
		detectHostResources:        system.DetectHostResources,
		generateSecret:             security.GenerateSecret,
		generateArgon2idCredential: security.GenerateArgon2idCredential,
		newDockerClient:            defaultDockerClientFactory,
		probePort:                  checkPortAvailable,
		releaseDeps:                resolveReleaseDeps(cfg.releaseDeps),
		selfUpdateDeps:             resolveSelfUpdateDeps(cfg.selfUpdateDeps),
		newRegistryClient:          resolveRegistryClient(cfg.newRegistryClient),
	}, nil
}

// buildDefaultLogger constructs the engine's default redaction-wrapped
// logger when no [WithLogger] was supplied. The normal path is the PRD §24
// file sink at <stateDir>/logs/latest.log: [logging.OpenLogFile] creates the
// owner-only tree, archives the prior session, and prunes per the retention
// policy, returning an open handle the engine owns and closes.
// It fails soft: if the sink cannot be opened (permissions, a file where the
// logs dir is expected, a tampered symlink), it degrades to fallback — the
// surface writer from [WithFallbackLogWriter], defaulting to [os.Stderr] —
// so a logging fault never blocks a wdm operation (PRD §24 "wdm must always
// write a normal log"). The returned closer is nil on the fallback path,
// since the engine does not own the fallback writer.
func buildDefaultLogger(stateDir string, fallback io.Writer) (*slog.Logger, io.Closer, error) {
	if fallback == nil {
		fallback = os.Stderr
	}

	writer := fallback
	var closer io.Closer
	if f, err := logging.OpenLogFile(filepath.Join(stateDir, "logs")); err == nil {
		writer = f
		closer = f
	}

	logger, err := logging.New(
		logging.WithWriter(writer),
		logging.WithLevel(slog.LevelInfo),
		logging.WithRedactor(security.NewActiveRedactor(nil)),
	)
	if err != nil {
		if closer != nil {
			//nolint:errcheck // unwinding a half-built logger; the construction error below is what the caller needs.
			_ = closer.Close()
		}
		return nil, nil, err
	}
	return logger, closer, nil
}

// resolveRegistryClient picks the registry-client factory: the
// [WithRegistryClient] override when set, otherwise the production
// [defaultRegistryClient]. Mirrors [resolveReleaseDeps]' fill-the-default
// posture so a nil override never leaves a nil factory for the
// image-update paths to panic on.
func resolveRegistryClient(override func() RegistryResolver) func() RegistryResolver {
	if override != nil {
		return override
	}
	return defaultRegistryClient
}

// defaultRegistryClient is the production registry-client seam: an
// anonymous, public-only Go-native registry metadata client
// ([registry.NewClient]) over a bounded, redirect-capped HTTP client. It
// stores no credentials and contacts the registry through Go
// HTTP code, NEVER `docker manifest inspect` or any Docker
// #58). Every transport/auth/rate-limit fault it returns is a typed
// [types.ErrCodeNetworkFailure] (exit 8), keeping the network/verification
// exit-code split intact.
func defaultRegistryClient() RegistryResolver {
	return registry.NewClient()
}

// resolveSelfUpdateDeps fills any unset field of the binary self-update
// execution seam with its production default so [WithSelfUpdateDeps] may
// override only the fields a given test cares about without leaving a nil
// function for the apply path to panic on. The production defaults route
// the download+verify half through internal/release.
func resolveSelfUpdateDeps(override selfUpdateDeps) selfUpdateDeps {
	deps := override
	if deps.executablePath == nil {
		deps.executablePath = os.Executable
	}
	if deps.resolveSymlinks == nil {
		deps.resolveSymlinks = filepath.EvalSymlinks
	}
	if deps.stageCandidate == nil {
		deps.stageCandidate = release.StageCandidateProduction
	}
	if deps.runVersionSmoke == nil {
		deps.runVersionSmoke = defaultRunVersionSmoke
	}
	return deps
}

// selfUpdateSmokeTimeout bounds the post-replacement `wdm --version` smoke
// check so a hung new binary cannot stall the apply indefinitely; the
// derived context kills the child when it fires ([exec.CommandContext]
// SIGKILL semantics). Applied on top of the caller's context, which still
// wins when shorter.
const selfUpdateSmokeTimeout = 30 * time.Second

// defaultRunVersionSmoke is the production smoke-exec seam: it runs the
// freshly-installed binary with the single `--version` argument through
// [exec.CommandContext] — argv only, NEVER a shell (PRD §12) — under a
// bounded context, and returns the trimmed stdout (the bare version string
// per the cmd/wdm version template). A non-zero exit or any exec failure is
// returned as an error so the caller rolls back. stdout goes
// to a small bounded buffer; stderr is discarded so no Docker- or
// daemon-shaped content can flow into the result.
func defaultRunVersionSmoke(ctx context.Context, binaryPath string) (string, error) {
	smokeCtx, cancel := context.WithTimeout(ctx, selfUpdateSmokeTimeout)
	defer cancel()

	cmd := exec.CommandContext(smokeCtx, binaryPath, "--version")
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// resolveReleaseDeps fills any unset half of the catalog-update seam with
// its production default so [WithReleaseDeps] may override only one half
// (or omit it entirely) without leaving a nil factory for the
// catalog-update methods to panic on. Both defaults source the trust
// anchors inside internal/release.
func resolveReleaseDeps(override releaseDeps) releaseDeps {
	deps := override
	if deps.newReleaseClient == nil {
		deps.newReleaseClient = defaultReleaseClient
	}
	if deps.verifyCatalogBundle == nil {
		deps.verifyCatalogBundle = release.VerifyCatalogBundleProduction
	}
	return deps
}

// defaultReleaseClient is the production release-metadata client seam: an
// anonymous GitHub client pinned to the [release.DefaultTrustPolicy]
// source repository. Every fault it later returns is a typed
// network failure (exit 8), keeping the network/verification exit-code
// split intact.
func defaultReleaseClient() (*release.Client, error) {
	return release.NewClient(release.DefaultTrustPolicy())
}

// defaultDockerClientFactory is the production docker-client seam used
// by [Engine.Install]: argv-only execution through [docker.New] with
// the per-operation redactor wired via [docker.WithRedactor] so
// generated secrets never reach a sink through Docker stderr.
func defaultDockerClientFactory(redactor security.Redactor) (docker.Client, error) {
	return docker.New(docker.WithRedactor(redactor))
}

// resolveDirs fills any unset path field on cfg with its XDG default.
// Set paths must be absolute; relative values from WithConfigPath /
// WithStateDir / WithDataDir are rejected here rather than at use time so
// misuse fails fast at construction.
func resolveDirs(cfg *config) error {
	if cfg.configPath == "" {
		path, err := defaultConfigPath()
		if err != nil {
			return err
		}
		cfg.configPath = path
	} else if !filepath.IsAbs(cfg.configPath) {
		return fmt.Errorf("WithConfigPath requires absolute path, got %q", cfg.configPath)
	}

	if cfg.stateDir == "" {
		base, err := xdgDir("XDG_STATE_HOME", ".local/state")
		if err != nil {
			return err
		}
		cfg.stateDir = filepath.Join(base, "wdm")
	} else if !filepath.IsAbs(cfg.stateDir) {
		return fmt.Errorf("WithStateDir requires absolute path, got %q", cfg.stateDir)
	}

	if cfg.dataDir == "" {
		base, err := xdgDir("XDG_DATA_HOME", ".local/share")
		if err != nil {
			return err
		}
		cfg.dataDir = filepath.Join(base, "wdm")
	} else if !filepath.IsAbs(cfg.dataDir) {
		return fmt.Errorf("WithDataDir requires absolute path, got %q", cfg.dataDir)
	}

	if cfg.stackBaseDir != "" && !filepath.IsAbs(cfg.stackBaseDir) {
		return fmt.Errorf("WithStackBaseDir requires absolute path, got %q", cfg.stackBaseDir)
	}
	return nil
}

// resolveStackBase picks the effective stack base: the WithStackBaseDir
// override when set, otherwise the loaded settings.BaseStackPath with
// leading "~/" expanded. Either way the result MUST be absolute — the
// scanner refuses relative paths at its API boundary.
func resolveStackBase(override string, settings *types.Settings) (string, error) {
	if override != "" {
		return override, nil
	}
	expanded, err := expandHome(settings.BaseStackPath)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("stack base must resolve to an absolute path, got %q", expanded)
	}
	return expanded, nil
}

// loadConfigOrDefaults reads configPath via [state.LoadConfig]. A
// missing file (wrapped [os.ErrNotExist]) maps to PRD §34 defaults so a
// first-launch user without a config can still run List. Any other
// failure — parse, schema validation, EACCES — propagates to the caller.
// A wrapped [types.ErrConfigInvalid] surfaces distinctly so cmd/wdm can
// map it to exit code 2.
func loadConfigOrDefaults(configPath string) (*types.Settings, error) {
	settings, err := state.LoadConfig(context.Background(), configPath)
	if err == nil {
		return settings, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return defaultSettings(), nil
	}
	return nil, err
}

// defaultSettings returns the fallback [*types.Settings] matching the
// example in and PRD §34. Used when
// config.toml is absent; the returned struct is freshly allocated so
// callers may mutate it without affecting subsequent calls.
func defaultSettings() *types.Settings {
	return &types.Settings{
		SchemaVersion:         1,
		BaseStackPath:         "~/docker",
		Timezone:              "",
		DefaultDockerNetwork:  "wdm_default",
		CatalogChannel:        "stable",
		UpdateCheckPreference: "daily-on-launch",
	}
}

// defaultConfigPath returns $XDG_CONFIG_HOME/wdm/config.toml or
// ~/.config/wdm/config.toml when XDG_CONFIG_HOME is unset (or
// set to a relative path, per the XDG spec). PRD §34,
// "On-disk layout".
func defaultConfigPath() (string, error) {
	base, err := xdgDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "wdm", "config.toml"), nil
}

// List returns one [types.AppInfo] per managed stack under the
// configured stack base directory (PRD §9). Corrupt.wdm.lock files are
// logged as WARN-level slog entries via the engine's logger and excluded
// from the result; directories without a .wdm.lock are silently ignored.
// See for the full contract.
// The returned slice is freshly allocated on every call: the
// engine.Engine interface guarantees defensive-copy semantics so callers
// may retain or mutate the result without affecting subsequent calls.
// [state.ScanStacks] already provides this guarantee one layer below; no
// re-copy is needed.
// Returns [ErrClosed] when called after [Engine.Close].
func (e *Engine) List(ctx context.Context) ([]types.AppInfo, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.List: %w", err)
	}

	result, err := state.ScanStacks(ctx, e.stackBase)
	if err != nil {
		return nil, fmt.Errorf("core.List: %w", err)
	}

	for _, w := range result.Warnings {
		e.logger.WarnContext(ctx, "core: stack scan warning",
			slog.String("path", w.Path),
			slog.String("cause", w.Cause.Error()),
		)
	}

	return result.Apps, nil
}

// Settings returns the resolved user settings loaded at [New] time
// (PRD §29, §34). The returned pointer references a fresh copy of the
// engine's cached settings, so callers may mutate the result without
// affecting subsequent Settings calls or the engine's state.
// [types.Settings] is a value type of strings and ints, so a pointer
// dereference and re-take is an effective deep copy without
// field-by-field handling.
// Returns [ErrClosed] when called after [Engine.Close].
func (e *Engine) Settings(ctx context.Context) (*types.Settings, error) {
	if e.isClosed() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("core.Settings: %w", err)
	}

	s := *e.settings
	return &s, nil
}

// Close releases the engine's held resources. When [New] opened the
// PRD §24 file sink (no [WithLogger] supplied and the sink opened), Close
// closes the latest.log handle; a caller-supplied logger and the fallback
// writer are not owned and so not closed.
// Close is idempotent: subsequent calls are no-ops, and the file is closed
// at most once. After Close, every other [Engine] method returns
// [ErrClosed].
func (e *Engine) Close() error {
	if e.closed.Swap(true) {
		return nil
	}
	if e.logFile != nil {
		return e.logFile.Close()
	}
	return nil
}

// isClosed reports whether [Engine.Close] has been called. This is
// the hot path for every Engine method's entry check; [atomic.Bool]
// keeps it lock-free.
func (e *Engine) isClosed() bool {
	return e.closed.Load()
}
