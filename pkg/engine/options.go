package engine

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
)

// config is the resolved set of construction-time overrides that [New]
// assembles from the supplied [Option] values. Each With* function below
// targets exactly one field. Defaults (XDG paths, ~/docker, the on-disk
// catalog FS) are resolved by internal/core at construction time, not
// here: resolving them reads the environment and filesystem, which is out
// of scope for pkg/engine.
type config struct {
	stateDir     string
	dataDir      string
	stackBaseDir string
	configPath   string
	logger       *slog.Logger
	fallbackLog  io.Writer
	catalog      fs.FS
	version      string
}

// Option mutates an engine config during [New] (functional options).
// The target [config] type is unexported on purpose: third-party code
// configures the engine only through the With* constructors in this file.
// The error return lets an Option reject bad input at construction time
// rather than at runtime. No option fails today, but the signature leaves
// room for per-option validation without breaking callers.
type Option func(*config) error

// WithStateDir overrides the runtime state directory (default:
// $XDG_STATE_HOME/wdm, falling back to ~/.local/state/wdm).
// PRD §9 and §26 — this directory houses runtime.lock and the log
// retention tree.
func WithStateDir(path string) Option {
	return func(c *config) error {
		c.stateDir = path
		return nil
	}
}

// WithDataDir overrides the data directory (default:
// $XDG_DATA_HOME/wdm, falling back to ~/.local/share/wdm).
// PRD §22 — this directory houses verified catalog channels.
func WithDataDir(path string) Option {
	return func(c *config) error {
		c.dataDir = path
		return nil
	}
}

// WithStackBaseDir overrides the directory under which managed stacks
// are written (default: ~/docker — PRD §9).
func WithStackBaseDir(path string) Option {
	return func(c *config) error {
		c.stackBaseDir = path
		return nil
	}
}

// WithConfigPath overrides the on-disk config.toml location (default:
// $XDG_CONFIG_HOME/wdm/config.toml — PRD §34).
func WithConfigPath(path string) Option {
	return func(c *config) error {
		c.configPath = path
		return nil
	}
}

// WithLogger supplies a configured [*slog.Logger]. When unset,
// internal/core builds a default sink under the resolved state dir
// (PRD §24).
func WithLogger(l *slog.Logger) Option {
	return func(c *config) error {
		c.logger = l
		return nil
	}
}

// WithFallbackLogWriter sets the writer the default logger degrades to when
// the PRD §24 file sink cannot be opened. It also encodes the calling
// surface: cmd/wdm passes [os.Stderr] from a CLI invocation so a degraded
// log stays visible, and [io.Discard] from the TUI so a logging fault never
// corrupts the Bubble Tea display (PRD §24, §28). When unset, the engine
// defaults to [os.Stderr]. Ignored when [WithLogger] supplies a logger.
func WithFallbackLogWriter(w io.Writer) Option {
	return func(c *config) error {
		c.fallbackLog = w
		return nil
	}
}

// WithCatalog overrides the catalog filesystem (default: [os.DirFS]
// rooted at the resolved data dir). Primarily used by tests to inject
// fixture catalogs without touching real channels (PRD §22).
func WithCatalog(catalogFS fs.FS) Option {
	return func(c *config) error {
		c.catalog = catalogFS
		return nil
	}
}

// WithVersion sets the wdm binary version that state-changing engine
// methods record in the runtime.lock metadata (PRD §26; exit
// criterion 396). cmd/wdm passes the build-time ldflag value so the
// on-disk runtime.lock identifies the holder process across
// Mac/VM/release boundaries.
// WithVersion("") is rejected at construction time so a release-pipeline
// typo cannot produce a runtime.lock with an empty wdm_version. When
// WithVersion is omitted, the underlying engine defaults to "dev",
// matching cmd/wdm's local-Makefile default for development builds.
// This is the only Option that maps to engine runtime state rather than
// a directory path: the runtime.lock acquire/release needs the metadata,
// and the install/update flows reuse the same field.
func WithVersion(version string) Option {
	return func(c *config) error {
		if version == "" {
			return errors.New("WithVersion requires non-empty version string")
		}
		c.version = version
		return nil
	}
}
