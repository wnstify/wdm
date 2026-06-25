// Command wdm is the entry point for the Webnestify Docker Manager
// (PRD §1, §29).
// Engine construction is deferred until a path needs it: the interactive
// no-argument TUI entry or a CLI leaf RunE handler. The factory wraps
// [engine.New] with [engine.WithVersion] so the build-time ldflag value
// reaches runtime.lock metadata (PRD §26);
// the engine resolves all other state (XDG paths, the default slog logger,
// the on-disk catalog FS) from the environment. `wdm --version`,
// `wdm --help`, `wdm apps --help`, and leaf help paths never reach
// engine.New, so PRD §14's self-update smoke check still gets a --version
// exit 0 even when the installed config.toml is malformed.
// The PRD §11 root/sudo refusal runs first — before Cobra parsing, before
// the factory is handed off — because PRD §11 gates on invocation context,
// not the requested action: `sudo wdm --version` must exit 6, not print
// the version.
// PRD §11's rootless-daemon refusal runs inside the engine factories via
// [engine.RequireRootlessDaemon], so every real command and the TUI entry
// refuse a rootful or indeterminate Docker daemon before the engine is built,
// while --version/--help never reach the factory and so never contact Docker.
// This is the only package permitted to call [os.Exit]; every other layer
// returns typed errors that [exitCodeFor] maps to PRD §27 exit codes. The
// depguard "cmd-wires-app" rule enforces the import allow list from here
// (.golangci.yml).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/wnstify/wdm/internal/cli"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/internal/tui"
	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// version is the build-time version string, set via
// `-ldflags "-X main.version=..."`. Local Makefile builds report "dev".
// PRD §14's self-update smoke check requires it to match the downloaded
// release exactly, so the release pipeline is the canonical
// setter; ships the "dev" default.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "wdm: %v\n", err)
		os.Exit(exitCodeFor(err))
	}
}

// debugRequested reports whether --debug appears in the raw args. The engine
// is constructed before Cobra parses flags, so the debug level is sourced
// from a pre-scan here rather than the parsed flag (PRD §24).
// Both the bare --debug and the --debug=<bool> spellings are detected here;
// Cobra still validates the flag for --help and rejects typos.
func debugRequested(args []string) bool {
	for _, a := range args {
		if a == "--debug" {
			return true
		}
		if v, ok := strings.CutPrefix(a, "--debug="); ok {
			if enabled, err := strconv.ParseBool(v); err == nil && enabled {
				return true
			}
		}
	}
	return false
}

type runOptions struct {
	args        []string
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	refuse      func() error
	stdinIsTTY  func() bool
	stdoutIsTTY func() bool
	// newEngine constructs the engine for the interactive TUI entry. Its
	// production form routes the default-logger fallback to [io.Discard]
	// so a log-sink failure never corrupts the Bubble Tea display
	// (PRD §24, §28).
	newEngine func() (engine.Engine, error)
	// newCLIEngine constructs the engine for CLI leaf commands. Its
	// production form routes the fallback to [os.Stderr] so a degraded log
	// stays visible. When nil, [normalizeRunOptions] mirrors newEngine so
	// existing tests that inject only newEngine keep working.
	newCLIEngine    func() (engine.Engine, error)
	runTUI          func(context.Context, engine.Engine) error
	runStartupError func(context.Context, error) error
}

func run(args []string) error {
	return runWithOptions(defaultRunOptions(args))
}

func defaultRunOptions(args []string) runOptions {
	debug := debugRequested(args)
	return runOptions{
		args:        args,
		stdin:       os.Stdin,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		refuse:      system.RefuseRootOrSudo,
		stdinIsTTY:  func() bool { return fileIsTerminal(os.Stdin) },
		stdoutIsTTY: func() bool { return fileIsTerminal(os.Stdout) },
		newEngine: func() (engine.Engine, error) {
			if err := engine.RequireRootlessDaemon(context.Background()); err != nil {
				return nil, err
			}
			return engine.New(
				engine.WithVersion(version),
				engine.WithFallbackLogWriter(io.Discard),
				engine.WithDebug(debug),
			)
		},
		newCLIEngine: func() (engine.Engine, error) {
			if err := engine.RequireRootlessDaemon(context.Background()); err != nil {
				return nil, err
			}
			return engine.New(
				engine.WithVersion(version),
				engine.WithFallbackLogWriter(os.Stderr),
				engine.WithDebug(debug),
			)
		},
		runTUI:          runTUI,
		runStartupError: runStartupError,
	}
}

// runWithOptions holds the control flow extracted from [main] so the
// returned error drives both the stderr message and the exit code. Only
// [main] calls [os.Exit]; run returns typed errors per PRD §27.
// Order is load-bearing: the PRD §11 root/sudo refusal runs BEFORE Cobra
// parsing and the TUI entry decision, so `sudo wdm --version` exits 6
// rather than printing the version and exiting 0.
// With no arguments, an interactive terminal enters the TUI; a non-TTY
// no-argument invocation takes the Cobra root path and prints usage rather
// than starting an interactive program on a pipe.
func runWithOptions(opts runOptions) error {
	opts = normalizeRunOptions(opts)
	if err := opts.refuse(); err != nil {
		return err
	}

	// Record the engine each factory builds so a failure can show its log
	// path (PRD §24 failure UX) without re-deriving the path and risking
	// drift from the sink the engine actually opened.
	var built engine.Engine
	record := func(factory func() (engine.Engine, error)) func() (engine.Engine, error) {
		return func() (engine.Engine, error) {
			eng, err := factory()
			if eng != nil {
				built = eng
			}
			return eng, err
		}
	}

	err := dispatchRun(opts, record(opts.newEngine), record(opts.newCLIEngine))
	if err != nil && built != nil {
		if path := built.LogPath(); path != "" {
			//nolint:errcheck // best-effort failure hint to stderr; the returned err drives the exit code regardless.
			fmt.Fprintf(opts.stderr, "wdm: see %s; review the log before sharing it publicly (e.g. on GitHub)\n", path)
		}
	}
	return err
}

// dispatchRun routes a normalized invocation to the TUI entry or the Cobra
// root, taking the (engine-recording) factories so the caller can surface
// the log path on failure.
func dispatchRun(
	opts runOptions,
	newEngine func() (engine.Engine, error),
	newCLIEngine func() (engine.Engine, error),
) error {
	if len(opts.args) == 0 && opts.stdinIsTTY() && opts.stdoutIsTTY() {
		eng, err := newEngine()
		if err != nil {
			return opts.runStartupError(context.Background(), err)
		}
		return opts.runTUI(context.Background(), eng)
	}

	root := cli.NewRootCmd(version, newCLIEngine)
	root.SetArgs(opts.args)
	root.SetIn(opts.stdin)
	root.SetOut(opts.stdout)
	root.SetErr(opts.stderr)
	return root.Execute()
}

func normalizeRunOptions(opts runOptions) runOptions {
	if opts.args == nil {
		opts.args = []string{}
	}
	if opts.stdin == nil {
		opts.stdin = os.Stdin
	}
	if opts.stdout == nil {
		opts.stdout = os.Stdout
	}
	if opts.stderr == nil {
		opts.stderr = os.Stderr
	}
	if opts.refuse == nil {
		opts.refuse = system.RefuseRootOrSudo
	}
	if opts.stdinIsTTY == nil {
		opts.stdinIsTTY = func() bool { return fileIsTerminal(os.Stdin) }
	}
	if opts.stdoutIsTTY == nil {
		opts.stdoutIsTTY = func() bool { return fileIsTerminal(os.Stdout) }
	}
	if opts.newEngine == nil {
		opts.newEngine = func() (engine.Engine, error) {
			return engine.New(
				engine.WithVersion(version),
				engine.WithFallbackLogWriter(io.Discard),
			)
		}
	}
	if opts.newCLIEngine == nil {
		// Mirror newEngine so tests that inject only newEngine still drive
		// the CLI path; production sets a distinct os.Stderr-fallback CLI
		// factory in defaultRunOptions.
		opts.newCLIEngine = opts.newEngine
	}
	if opts.runTUI == nil {
		opts.runTUI = runTUI
	}
	if opts.runStartupError == nil {
		opts.runStartupError = runStartupError
	}
	return opts
}

func runTUI(ctx context.Context, eng engine.Engine) error {
	return tui.Run(ctx, eng)
}

func runStartupError(ctx context.Context, err error) error {
	return tui.RunStartupError(ctx, err)
}

func fileIsTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// exitCodeFor maps a non-nil error from [run] to a PRD §27 exit code:
//   - system.ErrRunningAsRoot / ErrRunningWithSudo →
//   - types.ErrConfigInvalid → (config schema failure, reachable
//     from any subcommand that loads the engine)
//   - any *types.Error → its embedded Code, extracted via errors.As so
//     subcommands' wrapped errors route correctly
//   - default →
//
// The default is (usage/validation) rather than (generic)
// because the dominant unclassified error source is Cobra's
// flag/subcommand parser, and those are usage errors by Unix convention.
// Subcommands returning non-usage failures (engine, IO) MUST wrap with
// [types.WrapError] so the typed branch catches them; the default then
// covers only genuine usage errors.
func exitCodeFor(err error) int {
	var typeErr *types.Error
	switch {
	case errors.Is(err, system.ErrRunningAsRoot),
		errors.Is(err, system.ErrRunningWithSudo):
		return int(types.ErrCodePermissionDenied)
	case errors.Is(err, types.ErrConfigInvalid):
		return int(types.ErrCodeUsageValidation)
	case errors.As(err, &typeErr):
		return int(typeErr.Code)
	default:
		return int(types.ErrCodeUsageValidation)
	}
}
