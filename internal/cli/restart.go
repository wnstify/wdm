package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newAppsRestartCmd builds the `apps restart <app-id>` leaf (PRD §18:416
// "Restart app", §18:425 restart-loop recovery; PRD §32). It calls
// [engine.Engine.Restart] through the injected factory to restart a managed
// stack in place: the engine runs `docker compose restart`, which stops and
// starts the SAME containers without re-reading the Compose file
// #46), so a restart never re-renders templates and never touches config files
// or backups. It then renders [types.RestartResult] in one of two forms based
// on the root's --json persistent flag:
//   - Plain mode: a restart-finish screen on stdout — a headline, the services
//     the restart touched, and the post-restart status state. The engine's
//     progress lines stream to stderr.
//   - JSON mode: the full result wrapped in the wdm.v1 envelope on stdout, and
//     nothing else (PRD §32). Progress is suppressed.
//
// The flag set is minimal, mirroring `apps remove`. Restart is whole-stack
// only in v1 ([types.RestartRequest] carries no Services field), so there is
// no per-service flag. The flags are --yes (a safe-confirmation bypass),
// --stack-path (the engine's fail-closed cross-check), and the inherited
// --json:
//   - --yes: accept the SAFE restart confirmation without prompting. It covers
//     restart, not any deletion or database-risk migration, so --yes accepts
//     it. Restart never produces the database-risk warning, so acceptDBRisk is
//     wired false (that flag lives only on `apps update`); the shared confirmer
//     keeps the database-risk path fail-closed regardless.
//   - --stack-path: pinned onto [types.RestartRequest.StackPath]. The engine
//     resolves the stack by app id and verifies a supplied stack path against
//     it (a mismatch refuses fail-closed with [types.ErrCodeUsageValidation]
//     before any Docker call), so this flag is a guard, not an alternate
//     resolution path.
//
// The confirmation surface is satisfied by the engine's "restart_safe" payload
// (app, stack path, compose project, the restart-in-place statement); this
// leaf relays it to stderr through the shared [cliConfirmer] prompt path rather
// than re-assembling it.
// Exit codes (mapped from the engine's typed errors by cmd/wdm's exitCodeFor,
// via errors.As on *types.Error):
//   - 0: the restart succeeded. A needs_attention result (a container that did
//     not come back cleanly, or a post-restart inspection that failed) still
//     exits 0 — the engine returns a non-nil *RestartResult, mirroring the
//     status/remove posture: the exit code reports whether the restart
//     completed, not the subject's resulting health.
//   - 2 ([types.ErrCodeUsageValidation]): an empty app id, an unmanaged
//     directory, an uninstalled app, a --stack-path that does not match the
//     resolved managed stack, a corrupt manifest missing its compose project,
//     or a nil confirmer.
//   - 4 ([types.ErrCodeRuntimeLockHeld]): another wdm process holds the global
//     runtime lock, or the per-stack lock is busy.
//   - 5 ([types.ErrCodeDockerUnavailable]): the Docker daemon is unreachable.
//     `docker compose restart` propagates internal/docker's typed daemon-down
//     code unchanged. (A daemon-down failure during the POST-restart status
//     verification does NOT surface this code: the restart already ran, so the
//     verify pass fuses the failure into a needs_attention result at exit 0.)
//   - 7 ([types.ErrCodeUserCanceled]): the safe restart confirmation was
//     declined — an explicit "N" at the prompt, or no TTY and no --yes.
//   - 1 ([types.ErrCodeGeneric]): a generic failure, e.g. a Docker client
//     construction fault.
//
// The engine factory is invoked inside RunE, and only there, so
// `wdm apps restart --help` never reaches [engine.New] (PRD §14 self-update
// smoke-check invariant, mirrored from `apps list`).
func newAppsRestartCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		assumeYes bool
		stackPath string
	)

	cmd := &cobra.Command{
		Use:   "restart <app-id>",
		Short: "Restart a managed stack's containers in place",
		Long: `Restart restarts a managed app in place: it runs docker compose
restart, which stops and starts the same containers without re-reading
the Compose file. It never re-renders templates and never touches your
.env, Compose file, lock file, backups, or app data (decision #46).

Restart affects the whole stack; there is no per-service option in this
version.

--yes accepts the safe restart confirmation without prompting. Use
--stack-path to assert which managed stack path is being restarted; it
is verified against the app's resolved stack and refuses on a
mismatch.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps restart: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			req := types.RestartRequest{
				AppID:     args[0],
				StackPath: stackPath,
			}

			// Restart never produces a database-risk confirmation, so
			// acceptDBRisk is wired false; "restart_safe" is a safe
			// confirmation that --yes accepts.
			confirmer, onProgress := stateChangeIO(cmd, assumeYes, false, useJSON)

			result, err := eng.Restart(cmd.Context(), req, onProgress, confirmer)
			if err != nil {
				return err
			}

			return emitResult(cmd, useJSON, result, writeRestartFinish)
		},
	}

	cmd.Flags().BoolVar(&assumeYes, "yes", false, "accept the safe restart confirmation without prompting")
	cmd.Flags().StringVar(&stackPath, "stack-path", "", "assert the managed stack path being restarted (verified against the app)")

	return cmd
}

// writeRestartFinish renders the restart-finish screen for a completed restart
// to w (stdout): that the stack was restarted, the services the restart
// touched, then the post-restart status state. The layout is line-oriented and
// free of table-art so cut(1) and awk(1) stay usable, mirroring
// writeRemoveFinish.
// The "restarted and running" headline is gated on the post-restart status:
// needs-attention means a container did not come back cleanly or the
// verification failed, so the headline stays neutral and defers to the status
// block below — the same gate writeRemoveFinish applies.
func writeRestartFinish(w io.Writer, result *types.RestartResult) error {
	var b strings.Builder

	if result.Status != nil && result.Status.NeedsAttention {
		fmt.Fprintf(&b, "%s was restarted; see the status below for services that need attention.\n", result.AppID)
	} else {
		fmt.Fprintf(&b, "%s was restarted and is running.\n", result.AppID)
	}

	if len(result.RestartedServices) > 0 {
		b.WriteString("\nRestarted services:\n")
		for _, service := range result.RestartedServices {
			fmt.Fprintf(&b, "  - %s\n", service)
		}
	}

	if result.Status != nil {
		fmt.Fprintf(&b, "\nStatus: %s\n", result.Status.State)
		if result.Status.Message != "" {
			fmt.Fprintf(&b, "  %s\n", result.Status.Message)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("apps restart: writing finish screen: %w", err)
	}
	return nil
}
