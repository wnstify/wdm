package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnstify/wdm/internal/release"
)

// config is the resolved set of construction-time overrides [New]
// assembles from the supplied [Option] values. Defaults are applied
// inside [New], not here, so the With* setters stay pure assignments.
// The struct is unexported: callers configure the engine exclusively
// through the With* constructors below.
type config struct {
	stateDir     string
	dataDir      string
	stackBaseDir string // overrides Settings.BaseStackPath when set
	configPath   string
	logger       *slog.Logger
	catalog      fs.FS       // populated by WithCatalog
	version      string      // populated by WithVersion; defaulted to "dev" in New
	releaseDeps  releaseDeps // populated by WithReleaseDeps; defaulted in New

	selfUpdateDeps selfUpdateDeps // populated by WithSelfUpdateDeps; defaulted in New

	newRegistryClient func() RegistryResolver // populated by WithRegistryClient; defaulted in New
}

// Option mutates a core config during [New] (functional options).
// Returning an error lets an Option reject bad input at construction
// time rather than at runtime. No option fails today, but the signature leaves
// room for per-option validation without breaking callers.
// The target [config] type is unexported: third-party code configures the
// engine exclusively through the With* constructors in this file.
type Option func(*config) error

// WithStateDir overrides the runtime state directory (default:
// $XDG_STATE_HOME/wdm, falling back to ~/.local/state/wdm). PRD §9, §26 —
// this directory houses runtime.lock and the log retention tree.
// State-changing operations acquire runtime.lock here.
func WithStateDir(path string) Option {
	return func(c *config) error {
		c.stateDir = path
		return nil
	}
}

// WithDataDir overrides the data directory (default: $XDG_DATA_HOME/wdm,
// falling back to ~/.local/share/wdm). PRD §22 — this directory houses
// verified catalog channels. The install path reads them from its
// catalogs/ subtree when no catalog FS is injected via [WithCatalog].
func WithDataDir(path string) Option {
	return func(c *config) error {
		c.dataDir = path
		return nil
	}
}

// WithStackBaseDir overrides the directory under which managed stacks are
// written. When unset, the engine uses the resolved
// types.Settings.BaseStackPath (default ~/docker per PRD §9) with any
// leading ~/ expanded against $HOME.
func WithStackBaseDir(path string) Option {
	return func(c *config) error {
		c.stackBaseDir = path
		return nil
	}
}

// WithConfigPath overrides the on-disk config.toml location (default:
// $XDG_CONFIG_HOME/wdm/config.toml — PRD §34). The supplied path MUST be
// absolute; relative paths are rejected in [New], not here, so the setter
// stays pure.
func WithConfigPath(path string) Option {
	return func(c *config) error {
		c.configPath = path
		return nil
	}
}

// WithCatalog overrides the catalog filesystem (default: [os.DirFS]
// rooted at the resolved data dir). Used mainly by tests to inject
// fixture catalogs without touching real channels (PRD §22).
// It mirrors [pkg/engine.WithCatalog] so the bridge in pkg/engine.New can
// forward the option through unchanged. When set, the install path
// resolves the catalog through this FS in preference to the dataDir
// catalogs/ tree.
func WithCatalog(catalogFS fs.FS) Option {
	return func(c *config) error {
		c.catalog = catalogFS
		return nil
	}
}

// WithLogger supplies a configured [*slog.Logger]. When unset, the engine
// builds a redaction-wrapped logger over [os.Stderr] via
// [internal/logging.New] and [internal/security.NewActiveRedactor] so
// every record passes through the active redactor before reaching the
// sink (PRD §11, §24). Tests SHOULD pass an
// explicit no-op logger via slog.New(slog.NewTextHandler(io.Discard,
// nil)) to keep test output clean.
// A caller-supplied logger is used as-is — the engine does not re-wrap it
// in the redacting handler, since the caller already chose the handler
// chain. Callers that need redaction with a custom sink must assemble the
// chain themselves via [internal/logging.New] with a
// [internal/security.Redactor].
// [Engine.List] uses the logger to surface scan warnings as WARN-level
// entries (PRD §26 "Detect stale locks where practical and show a safe
// recovery prompt").
func WithLogger(l *slog.Logger) Option {
	return func(c *config) error {
		c.logger = l
		return nil
	}
}

// WithVersion sets the wdm binary version state-changing engine methods
// report to runtime.lock (PRD §26). cmd/wdm
// passes the build-time ldflag value through [pkg/engine.WithVersion] so
// the on-disk runtime.lock metadata identifies the holder process across
// the Mac/VM/release boundaries.
// An empty value is rejected at construction time so a release-pipeline
// typo cannot land a runtime.lock with an empty wdm_version (the
// underlying [state.AcquireRuntimeLock] would also reject it, but failing
// here gives a clearer error chain).
// When WithVersion is not applied, [New] defaults the field to "dev",
// matching cmd/wdm's local-Makefile default.
func WithVersion(version string) Option {
	return func(c *config) error {
		if version == "" {
			return errors.New("WithVersion requires non-empty version string")
		}
		c.version = version
		return nil
	}
}

// WithReleaseDeps overrides the catalog-update network and verification
// seam. It is the test injection point: pass a factory
// returning a [release.Client] aimed at an httptest server and a verify
// function backed by an offline fake-release fixture so CheckCatalogUpdate
// / ApplyCatalogUpdate never touch real GitHub or the live Sigstore root.
// Either argument may be nil to keep that half's production default ([New]
// fills the gaps via resolveReleaseDeps): pass only the verify half to
// exercise verification while leaving the client default, or vice versa.
// Production never calls this — [New] wires the defaults.
// newReleaseClient builds the metadata client; verifyBundle downloads and
// verifies the catalog asset set for the resolved metadata, returning the
// verified bytes with no filesystem write.
func WithReleaseDeps(
	newReleaseClient func() (*release.Client, error),
	verifyBundle func(context.Context, *release.Client, *release.Metadata) (*release.VerifiedCatalogBundle, error),
) Option {
	return func(c *config) error {
		c.releaseDeps = releaseDeps{
			newReleaseClient:    newReleaseClient,
			verifyCatalogBundle: verifyBundle,
		}
		return nil
	}
}

// WithSelfUpdateDeps overrides the binary self-update execution seam
// It is the test injection point for the four filesystem /
// process-exec touchpoints the apply path depends on — resolving the
// running executable, resolving its symlinks, downloading+verifying the
// candidate into a staging dir, and running the post-replace
// `wdm --version` smoke check — so a test drives the verified replacement,
// the wdm.previous retention, and the rollback entirely against fixtures,
// never touching the real running executable or execing the real test
// runner.
// Any argument may be nil to keep that touchpoint's production default
// ([New] fills the gaps via resolveSelfUpdateDeps): a test that exercises
// only the rollback can override runSmoke alone and leave the rest
// defaulted. Production never calls this — [New] wires the defaults,
// sourcing the download+verify trust anchors inside internal/release
func WithSelfUpdateDeps(
	executablePath func() (string, error),
	resolveSymlinks func(string) (string, error),
	stageCandidate func(context.Context, *release.Client, *release.Metadata, string) (*release.StagedCandidate, error),
	runSmoke func(ctx context.Context, binaryPath string) (string, error),
) Option {
	return func(c *config) error {
		c.selfUpdateDeps = selfUpdateDeps{
			executablePath:  executablePath,
			resolveSymlinks: resolveSymlinks,
			stageCandidate:  stageCandidate,
			runVersionSmoke: runSmoke,
		}
		return nil
	}
}

// WithRegistryClient overrides the Go-native registry-client factory used
// by the read-only image-update check ([Engine.CheckImageUpdates]) and the
// opportunistic registry-digest visibility in [Engine.Update] planning
// It is the test injection point: pass a factory returning
// a fake [RegistryResolver] (or a [registry.NewClient] aimed at an
// httptest server via [registry.WithHTTPClient] / [registry.WithScheme])
// so no test touches a real registry.
// A nil factory keeps the production default ([New] fills the gap via
// resolveRegistryClient). Production never calls this — [New] wires the
// anonymous public-only default ([defaultRegistryClient]).
func WithRegistryClient(newClient func() RegistryResolver) Option {
	return func(c *config) error {
		c.newRegistryClient = newClient
		return nil
	}
}

// xdgDir returns $envVar when it holds an absolute path, otherwise
// $HOME/defaultRelative. The XDG Base Directory Specification mandates
// ignoring relative values in XDG_* variables — a security posture
// against PATH-style injection — so a relative value is treated as unset.
// It errors only when $HOME cannot be resolved; an absent XDG_* env var
// is the documented default path, never an error.
func xdgDir(envVar, defaultRelative string) (string, error) {
	if v := os.Getenv(envVar); v != "" && filepath.IsAbs(v) {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving $HOME: %w", err)
	}
	return filepath.Join(home, defaultRelative), nil
}

// expandHome resolves a leading "~/" or a bare "~" against
// [os.UserHomeDir]; all other paths pass through unchanged.
// "On-disk layout" anchors path expansion at the engine layer, and this
// helper is that expansion site. Settings loaded from config.toml carry
// "~/docker"-style values verbatim, and [New] expands them before
// populating the Engine fields.
func expandHome(path string) (string, error) {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding ~: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding ~/: %w", err)
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
