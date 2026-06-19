package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newAppsStopAllCmd builds the `apps stop-all` leaf (issue #27). It calls
// [engine.Engine.StopAll] through the injected factory to stop every
// managed stack at once: the engine runs docker compose stop against each
// stack, which stops the running containers without removing them, so
// containers, networks, and named volumes stay defined and all data is
// preserved (this is NOT docker compose down). It then renders
// [types.StopAllResult] in one of two forms based on the root's --json
// persistent flag:
//   - Plain mode: a finish screen on stdout listing the apps that stopped
//     and the apps that failed. The engine's progress lines stream to
//     stderr.
//   - JSON mode: the full result wrapped in the wdm.v1 envelope on stdout,
//     and nothing else (PRD §32). Progress is suppressed.
//
// StopAll is continue-on-error: every stack is attempted even if some
// fail, and the result partitions the managed set into stopped and
// failed. The command exits nonzero when any stack failed even though the
// engine returned no error, mirroring the partial-failure contract.
//
// The flag set is minimal. StopAll takes no app id (it is all-apps only)
// and no per-service option (it is whole-stack only). The only flag is
// --yes (a safe-confirmation bypass) plus the inherited --json:
//   - --yes: accept the SAFE stop confirmation without prompting. Stop
//     removes nothing, so --yes accepts it. acceptDBRisk is wired false;
//     stop never produces the database-risk warning.
//
// Exit codes (mapped from the engine's typed errors by cmd/wdm's
// exitCodeFor, via errors.As on *types.Error):
//   - 0: every managed stack stopped (including an empty managed set, a
//     clean no-op). An already-stopped stack counts as stopped.
//   - 1 ([types.ErrCodeGeneric]): a whole-operation failure (a Docker
//     client construction fault), OR at least one stack failed to stop
//     while the operation itself succeeded — the partial-failure exit.
//   - 2 ([types.ErrCodeUsageValidation]): a nil confirmer.
//   - 4 ([types.ErrCodeRuntimeLockHeld]): another wdm process holds the
//     global runtime lock.
//   - 7 ([types.ErrCodeUserCanceled]): the safe stop confirmation was
//     declined — an explicit "N" at the prompt, or no TTY and no --yes.
//
// The engine factory is invoked inside RunE, and only there, so
// `wdm apps stop-all --help` never reaches [engine.New] (PRD §14
// self-update smoke-check invariant, mirrored from `apps list`).
func newAppsStopAllCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var assumeYes bool

	cmd := &cobra.Command{
		Use:   "stop-all",
		Short: "Stop all managed apps at once",
		Long: `Stop-all stops every managed app at once: it runs docker compose
stop against each managed stack, which stops the running containers
without removing them. Containers, networks, and named volumes stay
defined and all data is preserved; this is NOT docker compose down.

Stop-all affects every managed app and the whole of each stack; there is
no per-app or per-service option.

It continues on error: every app is attempted even if some fail, and the
result lists the apps that stopped and the apps that failed. The command
exits nonzero when any app failed.

--yes accepts the safe stop confirmation without prompting.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps stop-all: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			// Stop never produces a database-risk confirmation, so
			// acceptDBRisk is wired false; "stop_all_safe" is a safe
			// confirmation that --yes accepts.
			confirmer := newCLIConfirmer(cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), assumeYes, false)

			// JSON mode suppresses progress so the single envelope is the only
			// thing on stdout (PRD §32). Plain mode sends progress to stderr so
			// stdout carries just the finish screen.
			var onProgress types.ProgressFn
			if !useJSON {
				onProgress = stderrProgress(cmd.ErrOrStderr())
			}

			result, err := eng.StopAll(cmd.Context(), types.StopAllRequest{}, onProgress, confirmer)
			if err != nil {
				return err
			}

			if useJSON {
				if err := EmitJSON(cmd.OutOrStdout(), result); err != nil {
					return err
				}
			} else if err := writeStopAllFinish(cmd.OutOrStdout(), result); err != nil {
				return err
			}

			// Partial failure: the engine returned no error but one or more
			// stacks failed to stop. Surface a generic error so the command
			// exits nonzero after the result has already been rendered.
			if len(result.Failed) > 0 {
				return types.NewError(
					types.ErrCodeGeneric,
					fmt.Sprintf("%d app(s) failed to stop", len(result.Failed)),
					"see the per-app detail above and re-run stop-all once the cause is fixed",
				)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&assumeYes, "yes", false, "accept the safe stop confirmation without prompting")

	return cmd
}

// writeStopAllFinish renders the stop-all finish screen for a completed
// batch to w (stdout): the apps that stopped, then the apps that failed
// with their per-app detail. The layout is line-oriented and free of
// table-art so cut(1) and awk(1) stay usable, mirroring
// writeRestartFinish.
func writeStopAllFinish(w io.Writer, result *types.StopAllResult) error {
	var b strings.Builder

	skipped := stopAllSkippedNote(result)
	switch {
	case len(result.Stopped) == 0 && len(result.Failed) == 0:
		b.WriteString("No running apps to stop." + skipped + "\n")
	case len(result.Failed) == 0:
		fmt.Fprintf(&b, "Stopped %d app(s).%s\n", len(result.Stopped), skipped)
	default:
		fmt.Fprintf(&b, "Stopped %d app(s); %d failed.%s\n", len(result.Stopped), len(result.Failed), skipped)
	}

	if len(result.Stopped) > 0 {
		b.WriteString("\nStopped:\n")
		for _, app := range result.Stopped {
			fmt.Fprintf(&b, "  - %s\n", app.AppID)
		}
	}

	if len(result.Failed) > 0 {
		b.WriteString("\nFailed:\n")
		for _, app := range result.Failed {
			fmt.Fprintf(&b, "  - %s: %s\n", app.AppID, app.Error)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("apps stop-all: writing finish screen: %w", err)
	}
	return nil
}

// stopAllSkippedNote returns a short parenthetical note about the managed
// apps that were already stopped and so skipped, or an empty string when
// none were skipped. It is appended to the finish-screen headline so the
// plain output explains why fewer apps were stopped than are installed.
func stopAllSkippedNote(result *types.StopAllResult) string {
	if len(result.AlreadyStopped) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d already stopped, skipped)", len(result.AlreadyStopped))
}
