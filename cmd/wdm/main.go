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

type runOptions struct {
	args            []string
	stdin           io.Reader
	stdout          io.Writer
	stderr          io.Writer
	refuse          func() error
	stdinIsTTY      func() bool
	stdoutIsTTY     func() bool
	newEngine       func() (engine.Engine, error)
	runTUI          func(context.Context, engine.Engine) error
	runStartupError func(context.Context, error) error
}

func run(args []string) error {
	return runWithOptions(defaultRunOptions(args))
}

func defaultRunOptions(args []string) runOptions {
	return runOptions{
		args:        args,
		stdin:       os.Stdin,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		refuse:      system.RefuseRootOrSudo,
		stdinIsTTY:  func() bool { return fileIsTerminal(os.Stdin) },
		stdoutIsTTY: func() bool { return fileIsTerminal(os.Stdout) },
		newEngine: func() (engine.Engine, error) {
			return engine.New(engine.WithVersion(version))
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

	if len(opts.args) == 0 && opts.stdinIsTTY() && opts.stdoutIsTTY() {
		eng, err := opts.newEngine()
		if err != nil {
			return opts.runStartupError(context.Background(), err)
		}
		return opts.runTUI(context.Background(), eng)
	}

	root := cli.NewRootCmd(version, opts.newEngine)
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
			return engine.New(engine.WithVersion(version))
		}
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
