package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newUninstallCmd builds the top-level `wdm uninstall` command (PRD §39,
// issue #29). It calls [engine.Engine.Uninstall] through the injected
// factory to tear down every managed stack and then remove wdm's own
// on-disk footprint, including the running binary. For each managed stack
// the engine runs docker compose down --rmi all (NEVER -v): containers and
// the stack's images are removed. After every app is down the wdm-created
// Docker networks are removed best-effort, but ALL named volumes and every
// ~/docker/<app>/ stack directory are KEPT — self-uninstall never deletes
// user data. It is wdm-managed scope only, never a system-wide prune.
//
// Uninstall is fail-closed: if any stack teardown fails it ABORTS before
// removing any footprint, leaving wdm installed and listing the failed
// stacks. The core NEVER calls os.Exit; this leaf owns the process exit.
// On a clean run the binary is already gone from disk and the command
// exits 0; on an abort it exits nonzero so scripts see the failure.
//
// The result renders in one of two forms based on the root's --json
// persistent flag:
//   - Plain mode: a finish screen on stdout listing the torn-down stacks
//     and the kept-data paths, or, on abort, the failed stacks with a
//     clear "wdm was NOT removed" message. The engine's progress lines
//     stream to stderr.
//   - JSON mode: the full result wrapped in the wdm.v1 envelope on stdout,
//     and nothing else (PRD §32). Progress is suppressed.
//
// The flag set is minimal. Uninstall takes no app id (it is all-apps only)
// and no per-service option (it is whole-stack only). The only flag is
// --yes plus the inherited --json:
//   - --yes: accept the destructive uninstall confirmation without
//     prompting. Without it, the shared confirmer prompts y/N on a TTY and
//     declines fail-closed without one. acceptDBRisk is wired false;
//     uninstall never produces the database-risk warning.
//
// Exit codes (mapped from the engine's typed errors by cmd/wdm's
// exitCodeFor, via errors.As on *types.Error):
//   - 0: full success — every managed stack torn down and the footprint
//     removed (an empty managed set is a clean no-op).
//   - 1 ([types.ErrCodeGeneric]): one or more stacks failed to tear down
//     while the engine itself returned no error — the fail-closed abort.
//     The footprint was left untouched and wdm stays installed. The
//     command surfaces a generic error so it exits nonzero AFTER the
//     result has been rendered.
//   - 2 ([types.ErrCodeUsageValidation]): a nil confirmer, or any flag-
//     parse failure (an unknown flag) which fails before RunE runs.
//   - 4 ([types.ErrCodeRuntimeLockHeld]): another wdm process holds the
//     global runtime lock.
//   - 5 ([types.ErrCodeDockerUnavailable]): the Docker daemon is
//     unreachable.
//   - 7 ([types.ErrCodeUserCanceled]): the destructive confirmation was
//     declined — an explicit "N" at the prompt, or no TTY and no --yes.
//
// The engine factory is invoked inside RunE, and only there, so
// `wdm uninstall --help` never reaches [engine.New] (PRD §14 self-update
// smoke-check invariant, mirrored from `apps stop-all`).
func newUninstallCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var assumeYes bool

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall wdm and tear down every managed app",
		Long: `Uninstall removes wdm itself and tears down every managed app. For
each managed stack it runs docker compose down --rmi all, which removes
the containers and the stack's images. After every app is down, the
wdm-created Docker networks are removed too, so Docker is left clean. ALL
named volumes and every ~/docker/<app>/ stack directory are KEPT; this is
NOT docker compose down -v and no user data is deleted.

Uninstall affects only wdm-managed apps and wdm's own footprint (the
config, data, and state directories and the wdm binary). It is never a
system-wide Docker prune.

It is fail-closed: if any stack fails to tear down it aborts before
removing anything, leaves wdm installed, and lists the stacks that
failed. The command exits nonzero on an abort.

--yes accepts the destructive uninstall confirmation without prompting.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("uninstall: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			// The destructive "uninstall_destructive" confirmation is gated
			// like delete: --yes accepts it, otherwise the shared confirmer
			// prompts y/N on a TTY and declines fail-closed without one.
			// acceptDBRisk is false; uninstall produces no database-risk
			// warning.
			confirmer := newCLIConfirmer(cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), assumeYes, false)

			// JSON mode suppresses progress so the single envelope is the only
			// thing on stdout (PRD §32). Plain mode sends progress to stderr so
			// stdout carries just the finish screen.
			var onProgress types.ProgressFn
			if !useJSON {
				onProgress = stderrProgress(cmd.ErrOrStderr())
			}

			result, err := eng.Uninstall(cmd.Context(), types.UninstallRequest{}, onProgress, confirmer)
			if err != nil {
				return err
			}

			if useJSON {
				if err := EmitJSON(cmd.OutOrStdout(), result); err != nil {
					return err
				}
			} else if err := writeUninstallFinish(cmd.OutOrStdout(), result); err != nil {
				return err
			}

			// Fail-closed abort: the engine returned no error but one or more
			// stacks failed to tear down, so the footprint was left in place
			// and wdm is still installed. Surface a generic error so the
			// command exits nonzero after the result has already been
			// rendered.
			if len(result.Failed) > 0 {
				return types.NewError(
					types.ErrCodeGeneric,
					fmt.Sprintf("%d app(s) failed to tear down; wdm was not removed", len(result.Failed)),
					"see the per-app detail above, fix the cause, and re-run uninstall",
				)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&assumeYes, "yes", false, "accept the destructive uninstall confirmation without prompting")

	return cmd
}

// writeUninstallFinish renders the uninstall finish screen for a completed
// run to w (stdout). On a clean run it reports that wdm was uninstalled,
// lists the torn-down stacks and the removed footprint, and lists the
// kept-data paths. On a fail-closed abort it reports that wdm was NOT
// removed, lists the stacks that failed with their per-stack detail, and
// names the stacks that were torn down before the abort. The layout is
// line-oriented and free of table-art so cut(1) and awk(1) stay usable,
// mirroring writeStopAllFinish.
func writeUninstallFinish(w io.Writer, result *types.UninstallResult) error {
	var b strings.Builder

	if len(result.Failed) > 0 {
		fmt.Fprintf(&b, "Uninstall aborted: %d app(s) failed to tear down. wdm was NOT removed.\n", len(result.Failed))

		b.WriteString("\nFailed:\n")
		for _, app := range result.Failed {
			fmt.Fprintf(&b, "  - %s: %s\n", app.AppID, app.Error)
		}

		if len(result.TornDown) > 0 {
			b.WriteString("\nTorn down before the abort:\n")
			for _, app := range result.TornDown {
				fmt.Fprintf(&b, "  - %s\n", app.AppID)
			}
		}

		writeUninstallKeptData(&b, result)

		if _, err := io.WriteString(w, b.String()); err != nil {
			return fmt.Errorf("uninstall: writing finish screen: %w", err)
		}
		return nil
	}

	b.WriteString("wdm was uninstalled.\n")
	b.WriteString("Every managed app was torn down (containers, images, and wdm-created networks removed); named volumes and stack data were kept.\n")

	if len(result.TornDown) > 0 {
		b.WriteString("\nTorn down:\n")
		for _, app := range result.TornDown {
			fmt.Fprintf(&b, "  - %s\n", app.AppID)
		}
	}

	if len(result.RemovedNetworks) > 0 {
		b.WriteString("\nNetworks removed:\n")
		for _, name := range result.RemovedNetworks {
			fmt.Fprintf(&b, "  - %s\n", name)
		}
	}

	if len(result.RemovedPaths) > 0 {
		b.WriteString("\nRemoved:\n")
		for _, path := range result.RemovedPaths {
			fmt.Fprintf(&b, "  - %s\n", path)
		}
	}

	writeUninstallRetainedNetworks(&b, result)
	writeUninstallKeptData(&b, result)

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("uninstall: writing finish screen: %w", err)
	}
	return nil
}

// writeUninstallRetainedNetworks appends a warning section naming the
// wdm-created networks that could not be removed and the exact
// `docker network rm <name>` command to finish the cleanup manually. Network
// cleanup is best-effort, so a retained network never fails the uninstall; this
// section just surfaces the manual follow-up.
func writeUninstallRetainedNetworks(b *strings.Builder, result *types.UninstallResult) {
	if len(result.RetainedNetworks) == 0 {
		return
	}
	b.WriteString("\nWARNING: some wdm-created networks could not be removed. Remove them manually:\n")
	for _, network := range result.RetainedNetworks {
		fmt.Fprintf(b, "  - %s (%s)\n", network.Name, network.Reason)
		fmt.Fprintf(b, "    docker network rm %s\n", network.Name)
	}
}

// writeUninstallKeptData appends the kept-data section to b. Named volumes
// and ~/docker/<app>/ stack directories are never deleted by uninstall, so
// the section always states the preservation guarantee; the per-path list
// follows when the engine reported any kept stack directories.
func writeUninstallKeptData(b *strings.Builder, result *types.UninstallResult) {
	b.WriteString("\nKept (never deleted by uninstall): named volumes and per-app stack data.\n")
	if len(result.KeptDataPaths) > 0 {
		for _, path := range result.KeptDataPaths {
			fmt.Fprintf(b, "  - %s\n", path)
		}
	}
}
