package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newAppsDeleteCmd builds the `apps delete <app-id>` leaf — the destructive
// deletion flow PRD §19:444-455 mandates be SEPARATE from the safe
// `apps remove`. It calls [engine.Engine.DeleteApp] through the
// injected factory to PERMANENTLY delete a managed stack: the engine stops and
// removes the stack's containers via `docker compose down` (NEVER -v per
// §19:453), then deletes everything wdm wrote for the app — the rendered
// Compose file, the `.env`, the `.wdm.lock` manifest, the `.wdm-backups/`
// config snapshots, and the stack directory itself — and reports what
// survives. Named Docker volumes and on-disk data are NEVER deleted
// (§19:453-455); the app's wdm-created Docker networks ARE removed
// best-effort (a network that cannot be removed is reported with the manual
// `docker network rm` command and never aborts the deletion; reinstall
// recreates them).
// This leaf is thin: it collects the flags, maps them verbatim
// onto [types.DeleteRequest], and renders [types.DeleteResult]. It carries ZERO
// deletion business logic — the engine owns the typed-name equality
// re-verification (§19:451), the path-containment refusal (§19:452), and the
// no-volume-deletion contract (§19:453). The CLI never pre-validates the typed
// name; an absent or wrong --confirm-name surfaces as the engine's
// usage-validation refusal.
// The result renders in one of two forms based on the root's --json persistent
// flag:
//   - Plain mode: the destructive-deletion finish screen on stdout — that the
//     app was permanently deleted, the deleted paths, the remaining named
//     volumes as a bulleted list per the confirmation rules (with the honest empty-state
//     line, since the listing is opportunistic), and the remaining networks
//     (only when non-empty). The engine's progress lines stream to stderr.
//   - JSON mode: the full result wrapped in the wdm.v1 envelope on stdout, and
//     nothing else (PRD §32). Progress is suppressed.
//
// The flag set is minimal and maximally safe:
//   - --confirm-name: the typed-back app name proving stronger intent
//     (§19:451), mapped VERBATIM onto [types.DeleteRequest.ConfirmationName].
//     The engine re-verifies it equals the app id and refuses on mismatch
//     before any deletion. The stronger confirmation IS typing the exact app
//     id.
//   - --stack-path: pinned onto [types.DeleteRequest.StackPath]. The engine
//     resolves the stack by app id and verifies a supplied stack path against
//     it (a mismatch refuses fail-closed before any Docker call), so this flag
//     is a guard, not an alternate resolution path (the apps remove precedent).
//
// There is NO --yes, and that is load-bearing. The destructive
// "delete_destructive" confirmation is NOT a safe confirmation, so
// structurally exclude it. With no --yes flag registered, a `--yes` invocation
// fails flag parsing at exit 2 before RunE runs — the same structural proof
// apps remove uses for -v. The shared confirmer is wired with assumeYes
// hardwired false so even future flag drift cannot auto-accept the deletion: a
// non-TTY or non-affirmative answer declines fail-closed and the engine maps it
// to [types.ErrCodeUserCanceled]. There is likewise NO --volumes, -v, --force,
// or --purge, and no shorthand letters: [types.DeleteRequest.DeleteNamedVolumes]
// has no flag and stays false (the engine hard-refuses a true value in v1).
// The confirmation surface (§19:449-455) is satisfied by the engine's
// "delete_destructive" payload: the permanence warning, the file/directory
// list with the `.wdm-backups/` snapshot count, and the kept named volumes.
// This leaf relays it to stderr through the shared [cliConfirmer] prompt path
// (y/N, No by default) rather than re-assembling it.
// Exit codes (mapped from the engine's typed errors by cmd/wdm's exitCodeFor,
// via errors.As on *types.Error):
//   - 0: deletion succeeded. [types.DeleteResult] carries no status field, so
//     there is no needs-attention exit-0 nuance — a returned result is a
//     completed deletion.
//   - 2 ([types.ErrCodeUsageValidation]): an empty app id, a --confirm-name
//     that does not equal the app id (including an omitted one), an unmanaged
//     directory, an uninstalled app, a --stack-path that does not match the
//     resolved managed stack, a corrupt manifest missing its compose project,
//     a nil confirmer, or a stack path that resolves outside the managed stack
//     base (§19:452). Also every flag-parse failure — an unknown --yes, -v,
//     --volumes, or --force — which fails before RunE runs.
//   - 4 ([types.ErrCodeRuntimeLockHeld]): another wdm process holds the global
//     runtime lock, or the per-stack lock is busy.
//   - 5 ([types.ErrCodeDockerUnavailable]): the Docker daemon is unreachable.
//     This IS reachable on delete (as on remove): the planning named-volume
//     listing runs `docker volume ls` BEFORE any deletion and propagates a
//     daemon-down error unchanged, and `docker compose down` can surface the
//     same code.
//   - 7 ([types.ErrCodeUserCanceled]): the destructive-deletion confirmation
//     was declined — an explicit "N" at the prompt, or no TTY (there is no
//     --yes). Zero trace: nothing was downed and nothing was deleted.
//   - 1 ([types.ErrCodeGeneric]): a generic failure, e.g. os.RemoveAll failing
//     to delete the stack files, or a corrupt/unreadable manifest surfacing as
//     wrapped stale state.
//
// The engine factory is invoked inside RunE, and only there, so
// `wdm apps delete --help` never reaches [engine.New] (PRD §14 self-update
// smoke-check invariant, mirrored from `apps list`).
func newAppsDeleteCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		confirmName string
		stackPath   string
	)

	cmd := &cobra.Command{
		Use:   "delete <app-id>",
		Short: "Permanently delete a managed stack's files and directory",
		Long: `Delete PERMANENTLY removes a managed app. It stops and removes the
stack's containers with docker compose down, then deletes the rendered
Compose file, the .env, the .wdm.lock manifest, the .wdm-backups
snapshots, and the stack directory itself. This is NOT the safe remove
and it cannot be undone (PRD §19).

Named Docker volumes and on-disk data are never deleted; the app's
wdm-created Docker networks are removed best-effort (a network that
cannot be removed is reported with the manual docker network rm command
and never aborts the deletion; reinstall recreates them). The result
reports what was removed and what survives. There is intentionally no
flag to delete volumes or data.

To confirm the deletion you must type the exact app id with
--confirm-name (typing the app id is the stronger confirmation). The
deletion is also gated by an interactive y/N prompt; without a terminal
to prompt on, the deletion is declined. Use --stack-path to assert which
managed stack path is being deleted; it is verified against the app's
resolved stack and refuses on a mismatch.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps delete: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			// ConfirmationName is mapped verbatim — the engine re-verifies it
			// equals AppID (§19:451). DeleteNamedVolumes has no
			// flag and stays false; the engine hard-refuses a true value in v1
			// (§19:453).
			req := types.DeleteRequest{
				AppID:            args[0],
				StackPath:        stackPath,
				ConfirmationName: confirmName,
			}

			// The destructive "delete_destructive" confirmation is NOT safe:
			// assumeYes is hardwired false (there is no --yes flag) so it can
			// never be auto-accepted, and acceptDBRisk is false because delete
			// produces no database-risk warning. The shared confirmer prompts
			// y/N on a TTY and declines fail-closed without one.
			confirmer := newCLIConfirmer(cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), false, false)

			// JSON mode suppresses progress so the single envelope is the only
			// thing on stdout (PRD §32). Plain mode sends progress to stderr so
			// stdout carries just the finish screen.
			var onProgress types.ProgressFn
			if !useJSON {
				onProgress = stderrProgress(cmd.ErrOrStderr())
			}

			result, err := eng.DeleteApp(cmd.Context(), req, onProgress, confirmer)
			if err != nil {
				return err
			}

			if useJSON {
				return EmitJSON(cmd.OutOrStdout(), result)
			}
			return writeDeleteFinish(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().StringVar(&confirmName, "confirm-name", "", "type the exact app id to confirm the permanent deletion (required, §19)")
	cmd.Flags().StringVar(&stackPath, "stack-path", "", "assert the managed stack path being deleted (verified against the app)")

	return cmd
}

// writeDeleteFinish renders the PRD §19 destructive-deletion finish screen for
// a completed deletion to w (stdout): that the app was permanently deleted,
// then the deleted paths, the remaining named volumes as a bulleted list, the
// wdm-created networks removed, and a manual `docker network rm` hint for any
// that could not be removed (only when non-empty). The layout is line-oriented
// and free of table-art so cut(1) and awk(1) stay usable, mirroring
// writeRemoveFinish.
// Empty-list honesty (mirroring writeRemoveFinish): the remaining named-volume
// listing is opportunistic — a daemon-down or transient inspect failure yields
// an EMPTY list, not a fault, so an empty RemainingNamedVolumes does not prove
// zero volumes survived. This screen does NOT claim "0 volumes remain" on an
// empty list; it states the volumes could not be enumerated.
func writeDeleteFinish(w io.Writer, result *types.DeleteResult) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s was permanently deleted\n", result.AppID)
	b.WriteString("The stack's containers were stopped and removed, and its files and directory were deleted.\n")

	if len(result.DeletedPaths) > 0 {
		b.WriteString("\nDeleted:\n")
		for _, path := range result.DeletedPaths {
			fmt.Fprintf(&b, "  - %s\n", path)
		}
	}

	b.WriteString("\nNamed volumes (not deleted):\n")
	if len(result.RemainingNamedVolumes) > 0 {
		for _, volume := range result.RemainingNamedVolumes {
			fmt.Fprintf(&b, "  - %s\n", volume)
		}
	} else {
		b.WriteString("  none reported (Docker inspection data may be unavailable)\n")
	}

	if len(result.RemovedNetworks) > 0 {
		b.WriteString("\nNetworks removed:\n")
		for _, network := range result.RemovedNetworks {
			fmt.Fprintf(&b, "  - %s\n", network)
		}
	}

	writeDeleteRetainedNetworks(&b, result)

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("apps delete: writing finish screen: %w", err)
	}
	return nil
}

// writeDeleteRetainedNetworks appends a warning section naming the wdm-created
// networks that could not be removed during deletion and the exact
// `docker network rm <name>` command to finish the cleanup manually. Network
// removal is best-effort and never aborts the deletion, so a retained network
// is reported as a follow-up action, mirroring writeUninstallRetainedNetworks.
func writeDeleteRetainedNetworks(b *strings.Builder, result *types.DeleteResult) {
	if len(result.RetainedNetworks) == 0 {
		return
	}
	b.WriteString("\nWARNING: some wdm-created networks could not be removed. Remove them manually:\n")
	for _, network := range result.RetainedNetworks {
		fmt.Fprintf(b, "    docker network rm %s\n", network.Name)
	}
}
