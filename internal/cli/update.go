package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newAppsUpdateCmd builds the `apps update <app-id>` leaf (PRD §20, §32;
// injected factory and renders [types.UpdateResult] in one of two forms based
// on the root's --json persistent flag:
//   - Plain mode: the PRD §20 check block on stdout — the template version
//     transition, the changed services, and the catalog risk groups (safe /
//     major / database / complex). The engine's progress lines stream to
//     stderr carrying the per-service image references (old → new) for both
//     --dry-run planning and apply deployment. In apply mode a finish line
//     (new version, backup path, status state) follows the block.
//   - JSON mode: the full result wrapped in the wdm.v1 envelope on stdout, and
//     nothing else (PRD §32). Progress is suppressed.
//
// The default is an APPLY: the engine plans, backs up, rewrites the stack
// files, then confirms before pull + recreate — a decline restores the
// pre-update config snapshot (PRD §20 steps 6-11, §21). --dry-run stops after
// planning and returns the check result without consulting the confirmer or
// mutating anything ([types.UpdateRequest.DryRun]).
// Flags map onto [types.UpdateRequest] plus two authorization gates:
//   - --dry-run: plan and report only; no confirmation, no mutation.
//   - --yes: accept SAFE confirmations without prompting — here the recreate
//     confirmation that authorizes pull + force-recreate
//     #14). It NEVER accepts the database-risk warning.
//   - --accept-database-risk: the flag-only authorization for a database-risk
//     update (PRD §20 "must explicitly confirm").
//     A TTY "y" does NOT satisfy it in — see [cliConfirmer.Confirm].
//   - --target-version: pin the expected catalog template version
//     ([types.UpdateRequest.TargetTemplateVersion]). The engine refuses with
//     [types.ErrCodeUsageValidation] when the catalog offers a different
//     version, letting automation detect catalog drift instead of silently
//     updating. Empty (the default) accepts whatever the catalog offers.
//
// Exit codes (mapped from the engine's typed errors by cmd/wdm's exitCodeFor,
// via errors.As on *types.Error):
//   - 0: update applied, --dry-run check rendered, or no-op (already up to
//     date) — a no-op apply still runs the full backup + recreate pipeline and
//     exits 0.
//   - 2 ([types.ErrCodeUsageValidation]): unmanaged directory, uninstalled
//     app, or a --target-version the catalog does not offer.
//   - 4 ([types.ErrCodeRuntimeLockHeld]): another wdm process holds the
//     runtime lock, or the per-stack lock is busy.
//   - 7 ([types.ErrCodeUserCanceled]): a declined recreate confirmation, or a
//     database-risk warning declined for lack of --accept-database-risk.
//   - 1 ([types.ErrCodeGeneric]): an update that failed after file rewrite — a
//     deploy fault or an unreachable Docker daemon, since the restore boundary
//     re-codes post-exposure faults — and was rolled back to the pre-update
//     config snapshot, with the restored backup path in the error hint (PRD
//     §21).
//
// The engine factory is invoked inside RunE, and only there, so
// `wdm apps update --help` never reaches [engine.New] (PRD §14 self-update
// smoke-check invariant, mirrored from `apps list`).
func newAppsUpdateCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		dryRun        bool
		assumeYes     bool
		acceptDBRisk  bool
		targetVersion string
	)

	cmd := &cobra.Command{
		Use:   "update <app-id>",
		Short: "Update a managed stack to the current catalog version",
		Long: `Update re-renders a managed app from the catalog, groups the
candidate changes by risk, and confirms before anything is pulled
or recreated (PRD §20). A pre-update config backup is taken before
any byte changes; declining the confirmation restores it.

Use --dry-run to see the version transition, changed services, and
risk groups without applying anything.

--yes accepts only safe confirmations (the recreate). The
database-risk warning is never accepted by --yes; it requires
--accept-database-risk explicitly.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps update: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			req := types.UpdateRequest{
				AppID:                 args[0],
				TargetTemplateVersion: targetVersion,
				DryRun:                dryRun,
			}

			confirmer := newCLIConfirmer(cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), assumeYes, acceptDBRisk)

			// JSON mode suppresses progress so the single envelope is the
			// only thing on stdout (PRD §32). Plain mode sends progress to
			// stderr — for --dry-run the StepUpdatePlanning stream with the
			// per-service old → new image references (PRD §20 wants tag
			// changes on the check screen), for apply the full deploy stream
			// — so stdout carries just the check block plus finish line.
			var onProgress types.ProgressFn
			if !useJSON {
				onProgress = stderrProgress(cmd.ErrOrStderr())
			}

			result, err := eng.Update(cmd.Context(), req, onProgress, confirmer)
			if err != nil {
				return err
			}

			if useJSON {
				return EmitJSON(cmd.OutOrStdout(), result)
			}
			return writeUpdateResult(cmd.OutOrStdout(), result, dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "plan and report the update without applying it")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "accept safe confirmations without prompting (never the database-risk warning)")
	cmd.Flags().BoolVar(&acceptDBRisk, "accept-database-risk", false, "explicitly authorize a database-risk update (required for database-risk apps)")
	cmd.Flags().StringVar(&targetVersion, "target-version", "", "require this catalog template version (refuses if the catalog offers another)")

	return cmd
}

// writeUpdateResult renders the plain-mode PRD §20 update report to w
// (stdout). The block is the same for dry-run and apply: an availability
// header, the template version transition, the per-service image changes (old
// → new), and the catalog risk groups. In apply mode the engine has populated
// the backup path and post-update status, appended as a finish line; in
// dry-run they are unset and the block ends at the risk groups. The layout is
// line-oriented and free of table-art so cut(1) and awk(1) stay usable.
func writeUpdateResult(w io.Writer, result *types.UpdateResult, dryRun bool) error {
	var b strings.Builder

	available := updateIsAvailable(result)
	switch {
	case !available:
		fmt.Fprintf(&b, "%s\tup to date\t%s\n", result.AppID, result.NewTemplateVersion)
	case dryRun:
		fmt.Fprintf(&b, "%s\tupdate available\t%s -> %s\n",
			result.AppID, result.PreviousTemplateVersion, result.NewTemplateVersion)
	default:
		fmt.Fprintf(&b, "%s\tupdated\t%s -> %s\n",
			result.AppID, result.PreviousTemplateVersion, result.NewTemplateVersion)
	}

	if len(result.UpdatedServices) > 0 {
		b.WriteString("\nImage changes:\n")
		for _, svc := range result.UpdatedServices {
			fmt.Fprintf(&b, "  - %s\n", svc)
		}
	}

	if len(result.RiskClassifications) > 0 {
		fmt.Fprintf(&b, "\nRisk: %s\n", strings.Join(result.RiskClassifications, ", "))
	}

	if result.BackupPath != "" {
		fmt.Fprintf(&b, "\nConfig backup: %s\n", result.BackupPath)
	}

	if result.Status != nil {
		fmt.Fprintf(&b, "Status: %s\n", result.Status.State)
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("apps update: writing update report: %w", err)
	}
	return nil
}

// updateIsAvailable reports whether the check found an update to apply. It
// mirrors the engine's availability test (a changed template version OR at
// least one changed service): the engine leaves PreviousTemplateVersion ==
// NewTemplateVersion with no UpdatedServices for a no-op, so the CLI
// distinguishes "already up to date" from "update available" without a
// dedicated result field.
func updateIsAvailable(result *types.UpdateResult) bool {
	return result.PreviousTemplateVersion != result.NewTemplateVersion || len(result.UpdatedServices) > 0
}
