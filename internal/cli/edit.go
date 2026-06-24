package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// launchEditor runs the resolved editor argv with the process's stdio
// inherited so the user edits interactively. It is a package var so tests
// override it and never spawn a real editor; production wires the os/exec
// path. argv[0] is the binary, argv[1:] its flags plus the target path, all
// typed (never a shell string) so editor values with metacharacters stay
// literal arguments (no shell interpretation).
var launchEditor = func(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv is typed (no shell); the editor is the user's own $VISUAL/$EDITOR
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// newEditCmd builds the top-level `edit <app-id> --compose|--env` command.
// It resolves the user-owned overlay path through the engine (creating it
// idempotently), opens it in the user's editor ($VISUAL → $EDITOR → nano),
// then runs a warn-but-allow stack validation. Exactly one of --compose or
// --env is required.
//
// --print-path prints the resolved overlay path and exits 0 BEFORE any TTY
// check, so scripts and headless callers stay supported. Without it, a
// non-terminal stdin/stdout fails with guidance rather than silently doing
// nothing — a non-interactive caller has no editor to drive.
//
// --compose prints a one-line security warning before launching because a
// compose override can re-add dropped capabilities, expose ports on
// 0.0.0.0, or break wdm tracking if it removes the wdm.managed labels or
// the project name (PRD §29, §37). The .env overlay carries no such risk,
// so no warning there.
//
// --env first offers a migration on a pre-feature stack: if the on-disk
// compose does not yet wire .env.user via env_file, RewireStack re-renders
// and restarts the stack so the overlay goes live (detect → confirm →
// rewire → restart, T8). A decline is warn-but-allow — the editor still
// opens; an already-wired stack is a silent no-op (no prompt).
func newEditCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		composeFlag bool
		envFlag     bool
		printPath   bool
	)

	cmd := &cobra.Command{
		Use:   "edit <app-id>",
		Short: "Edit a managed app's user-owned compose override or .env overlay",
		Long: `Edit opens a managed app's user-owned overlay in your editor.

Choose exactly one target:
  --compose  edit docker-compose.override.yml (merged over the wdm base)
  --env      edit .env.user (extra environment merged into every service)

wdm creates the overlay if it does not exist, opens it in your editor
($VISUAL, then $EDITOR, then nano), and validates the stack afterward —
a validation warning is reported but never fails the command.

--print-path prints the resolved overlay path and exits without opening an
editor, for scripting and headless use. Without --print-path an editor
needs an interactive terminal.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if composeFlag == envFlag {
				return types.NewError(
					types.ErrCodeUsageValidation,
					"choose exactly one of --compose or --env",
					"pass exactly one of --compose or --env to select the overlay to edit",
				)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			appID := args[0]
			ctx := cmd.Context()

			var path string
			if composeFlag {
				path, err = eng.EnsureUserOverride(ctx, appID)
			} else {
				path, err = eng.EnsureUserEnv(ctx, appID)
			}
			if err != nil {
				return err
			}

			// Scriptable, headless-safe path resolution comes BEFORE any TTY
			// gate so `--print-path` works in pipelines.
			if printPath {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), path); err != nil {
					return fmt.Errorf("edit: writing path: %w", err)
				}
				return nil
			}

			if !editTTYReadyFn() {
				return types.NewError(
					types.ErrCodeUsageValidation,
					"editing needs an interactive terminal; rerun in a terminal or use --print-path to edit the overlay yourself",
					"--print-path writes the resolved overlay path to stdout so a script or non-interactive caller can open it directly",
				)
			}

			if composeFlag {
				if _, err := io.WriteString(cmd.ErrOrStderr(),
					"warning: a compose override can re-add dropped capabilities, expose ports on 0.0.0.0, or break wdm tracking if it removes the wdm.managed labels or project name.\n"); err != nil {
					return fmt.Errorf("edit: writing warning: %w", err)
				}
			}

			// .env.user reaches the containers only if the on-disk compose wires
			// it via env_file. A stack installed before that overlay landed lacks
			// the line, so offer to re-render+restart (RewireStack) BEFORE opening
			// the editor. The compose override merges independently of env_file, so
			// this is the --env path only. A no-op (already wired) never prompts; a
			// decline is warn-but-allow — the edit still opens (PRD §3.40, T8).
			if envFlag {
				confirmer, _ := stateChangeIO(cmd, false, false, false)
				if rewired, _, rwErr := eng.RewireStack(ctx, appID, confirmer); rwErr != nil {
					if !types.IsCode(rwErr, types.ErrCodeUserCanceled) {
						return rwErr
					}
					if _, err := io.WriteString(cmd.ErrOrStderr(),
						"overlay not activated; run `wdm update "+appID+"` to activate .env.user later.\n"); err != nil {
						return fmt.Errorf("edit: writing rewire note: %w", err)
					}
				} else if rewired {
					if _, err := io.WriteString(cmd.ErrOrStderr(),
						"migrated this stack so .env.user is now active.\n"); err != nil {
						return fmt.Errorf("edit: writing rewire note: %w", err)
					}
				}
			}

			argv, err := engine.ResolveEditorArgv(os.Getenv("VISUAL"), os.Getenv("EDITOR"), path)
			if err != nil {
				return err
			}
			if err := launchEditor(argv); err != nil {
				return fmt.Errorf("edit: running editor: %w", err)
			}

			warnings, err := eng.ValidateStack(ctx, appID)
			return writeStackValidation(cmd.ErrOrStderr(), warnings, err)
		},
	}

	cmd.Flags().BoolVar(&composeFlag, "compose", false, "edit the docker-compose.override.yml overlay")
	cmd.Flags().BoolVar(&envFlag, "env", false, "edit the .env.user overlay")
	cmd.Flags().BoolVar(&printPath, "print-path", false, "print the resolved overlay path and exit without opening an editor")

	return cmd
}

// editTTYReadyFn reports whether both stdin and stdout are terminals, the
// precondition for launching an interactive editor. It is a package var so
// tests drive the no-TTY guard deterministically; production checks the real
// char-device state with the same heuristic as the confirmer's stdinIsTTY.
var editTTYReadyFn = func() bool {
	return fileIsCharDevice(os.Stdin) && fileIsCharDevice(os.Stdout)
}

func fileIsCharDevice(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// writeStackValidation renders the post-edit ValidateStack outcome to w
// (stderr) as warn-but-allow: warnings and a validation error are reported
// but never fail the command, so a user's in-progress override edit is not
// blocked. The validateErr is rendered (not returned) by design — only a
// write failure to w surfaces as the returned error.
func writeStackValidation(w io.Writer, warnings []string, validateErr error) error {
	var b strings.Builder

	if validateErr != nil {
		fmt.Fprintf(&b, "stack validation reported an issue (the edit was kept): %v\n", validateErr)
	} else {
		for _, warning := range warnings {
			fmt.Fprintf(&b, "stack validation warning: %s\n", warning)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("edit: writing validation output: %w", err)
	}
	return nil
}
