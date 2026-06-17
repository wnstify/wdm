package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newAppsRemoveCmd builds the `apps remove <app-id>` leaf (PRD §19, §32;
// injected factory to safely remove a managed stack: the engine stops and
// removes the stack's containers via `docker compose down` (NEVER -v per
// then renders [types.RemoveResult] in one of two forms based on the root's
// --json persistent flag:
//   - Plain mode: the PRD §19 safe-removal finish screen on stdout — that
//     containers were stopped and removed, what was kept (preserved paths, the
//     named volumes as a bulleted list, the remaining networks), and the
//     post-remove status state. The
//     engine's progress lines stream to stderr.
//   - JSON mode: the full result wrapped in the wdm.v1 envelope on stdout, and
//     nothing else (PRD §32). Progress is suppressed.
//
// The flag set is minimal by the "never deletes stack files or
// volumes" contract: "without destructive flags" is structural, so this
// command defines NO flag that could express volume or data deletion — no
// --volumes, -v, --purge, or --force. The flags are --yes (a safe-confirmation
// bypass), --stack-path (the engine's fail-closed cross-check), and the
// inherited --json:
//   - --yes: accept the SAFE remove confirmation without prompting. Remove
//     does not perform any deletion, so --yes accepts it. Remove never produces the
//     database-risk warning, so acceptDBRisk is wired false (that flag lives
//     only on `apps update`); the shared confirmer keeps the database-risk
//     path fail-closed regardless.
//   - --stack-path: pinned onto [types.RemoveRequest.StackPath]. The engine
//     resolves the stack by app id and verifies a supplied stack path against
//     it (a mismatch refuses fail-closed with [types.ErrCodeUsageValidation]
//     before any Docker call), so this flag is a guard, not an alternate
//     resolution path.
//
// The confirmation surface is satisfied by the engine's
// "remove_safe" payload: the app name, stack path, compose project, the
// containers-removed/files-kept statement, and the preserved paths / named
// volumes / networks. This leaf relays that payload to stderr through the
// shared [cliConfirmer] prompt path rather than re-assembling it, so ":shows
// app name and stack path before confirmation" holds wherever the prompt shows.
// Exit codes (mapped from the engine's typed errors by cmd/wdm's exitCodeFor,
// via errors.As on *types.Error):
//   - 0: removal succeeded. A needs_attention result (a container that
//     lingered after the down, or a post-remove inspection that failed) still
//     exits 0 — the engine returns a non-nil *RemoveResult, mirroring the
//     status/install posture: the exit code reports whether the removal
//     completed, not the subject's resulting health.
//   - 2 ([types.ErrCodeUsageValidation]): an empty app id, an unmanaged
//     directory, an uninstalled app, a --stack-path that does not match the
//     resolved managed stack, a corrupt manifest missing its compose project,
//     or a nil confirmer.
//   - 4 ([types.ErrCodeRuntimeLockHeld]): another wdm process holds the global
//     runtime lock, or the per-stack lock is busy.
//   - 5 ([types.ErrCodeDockerUnavailable]): the Docker daemon is unreachable.
//     Unlike `apps update`, this IS reachable on remove: the planning
//     named-volume listing runs `docker volume ls` BEFORE the commit point and
//     propagates a daemon-down error unchanged, and `docker compose down` can
//     surface the same code. (A daemon-down re-list AFTER the commit point
//     yields an empty list, not an error, so it never re-codes a durable
//     removal.)
//   - 7 ([types.ErrCodeUserCanceled]): the safe removal confirmation was
//     declined — an explicit "N" at the prompt, or no TTY and no --yes.
//   - 1 ([types.ErrCodeGeneric]): a generic failure, e.g. the manifest
//     commit-point write failing.
//
// The engine factory is invoked inside RunE, and only there, so
// `wdm apps remove --help` never reaches [engine.New] (PRD §14 self-update
// smoke-check invariant, mirrored from `apps list`).
func newAppsRemoveCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		assumeYes bool
		stackPath string
	)

	cmd := &cobra.Command{
		Use:   "remove <app-id>",
		Short: "Safely remove a managed stack, keeping files and volumes",
		Long: `Remove safely removes a managed app: it stops and removes the
stack's containers with docker compose down, then keeps every file
and every named volume on disk. Your .env, Compose file, lock file,
backups, app data, databases, and named volumes are preserved (PRD
§19).

This command intentionally has no flag to delete volumes or data. A
destructive deletion flow is out of scope here.

--yes accepts the safe removal confirmation without prompting. Use
--stack-path to assert which managed stack path is being removed; it
is verified against the app's resolved stack and refuses on a
mismatch.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps remove: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			req := types.RemoveRequest{
				AppID:     args[0],
				StackPath: stackPath,
			}

			// Remove never produces a database-risk confirmation, so
			// acceptDBRisk is wired false; "remove_safe" is a safe confirmation
			// that --yes accepts.
			confirmer := newCLIConfirmer(cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), assumeYes, false)

			// JSON mode suppresses progress so the single envelope is the only
			// thing on stdout (PRD §32). Plain mode sends progress to stderr so
			// stdout carries just the finish screen.
			var onProgress types.ProgressFn
			if !useJSON {
				onProgress = stderrProgress(cmd.ErrOrStderr())
			}

			result, err := eng.Remove(cmd.Context(), req, onProgress, confirmer)
			if err != nil {
				return err
			}

			if useJSON {
				return EmitJSON(cmd.OutOrStdout(), result)
			}
			return writeRemoveFinish(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().BoolVar(&assumeYes, "yes", false, "accept the safe removal confirmation without prompting")
	cmd.Flags().StringVar(&stackPath, "stack-path", "", "assert the managed stack path being removed (verified against the app)")

	return cmd
}

// writeRemoveFinish renders the PRD §19 safe-removal finish screen for a
// completed removal to w (stdout): that containers were stopped and removed,
// then what was kept — the preserved paths, the remaining named volumes as a
// bulleted list, and the remaining networks (, "so
// the user knows what wdm did not delete") — followed by the post-remove status
// state. The layout is line-oriented and free of table-art so cut(1) and
// awk(1) stay usable, mirroring writeInstallFinish.
// Empty-list honesty: the named-volume listing is opportunistic — a daemon-down
// or transient inspect failure yields an EMPTY list, not a fault
// so an empty RemainingNamedVolumes does not prove zero
// volumes survived. This screen does NOT claim "0 volumes preserved" on an
// empty list; it states the volumes could not be enumerated. The same neutral
// phrasing applies to networks, whose list is likewise opportunistic.
func writeRemoveFinish(w io.Writer, result *types.RemoveResult) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s is removed from %s\n", result.AppID, result.StackPath)
	// The stopped-and-removed assertion is gated on the post-remove status:
	// needs-attention means a container lingered after the down or the
	// inspection failed, so the headline stays neutral and defers to the
	// status block below.
	if result.Status != nil && result.Status.NeedsAttention {
		b.WriteString("Removal is recorded. Files and data were kept; see the status below for containers that may remain.\n")
	} else {
		b.WriteString("Containers were stopped and removed. Files and data were kept.\n")
	}

	if len(result.PreservedPaths) > 0 {
		b.WriteString("\nKept on disk:\n")
		for _, path := range result.PreservedPaths {
			fmt.Fprintf(&b, "  - %s\n", path)
		}
	}

	b.WriteString("\nNamed volumes:\n")
	if len(result.RemainingNamedVolumes) > 0 {
		for _, volume := range result.RemainingNamedVolumes {
			fmt.Fprintf(&b, "  - %s\n", volume)
		}
	} else {
		b.WriteString("  none reported (Docker inspection data may be unavailable)\n")
	}

	if len(result.RemainingNetworks) > 0 {
		b.WriteString("\nNetworks left in place:\n")
		for _, network := range result.RemainingNetworks {
			fmt.Fprintf(&b, "  - %s\n", network)
		}
	}

	if result.Status != nil {
		fmt.Fprintf(&b, "\nStatus: %s\n", result.Status.State)
		if result.Status.Message != "" {
			fmt.Fprintf(&b, "  %s\n", result.Status.Message)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("apps remove: writing finish screen: %w", err)
	}
	return nil
}
