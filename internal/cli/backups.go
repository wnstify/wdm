package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// This file holds the `apps backups` subgroup and its two leaf bodies —
// `apps backups list <app-id>` and `apps backups restore <app-id> <snapshot>`
// (PRD §7 "Backups", §21) — as thin callers of the engine's backup surface
// ([engine.Engine.ListBackups] / [engine.Engine.RestoreBackup]). Registration
// tests pin the command paths (`apps backups list`, `apps backups restore`),
// so the constructors keep their registered names.
// `apps backups` is a group command, mirroring the `apps` group (apps.go):
// Args:NoArgs plus a Runnable RunE that prints help — the golang-spf13-cobra
// canonical group pattern. Without Args:NoArgs, `apps backups foo` would
// silently exit 0; without a Runnable RunE, `apps backups --help` would skip
// the Usage and Flags sections.
// Restore is a config restore: every
// user-facing string says "config restore" and states what IS restored (wdm
// config files) and what is NOT (app data, databases, volumes), never the
// alternate undo vocabulary the invariant forbids. The boundary text and the
// recreate next-action are relayed from the engine's
// [types.RestoreBackupResult] verbatim — the canonical copy the failed-update
// auto-restore shares — never paraphrased.

// backupsListPayload is the inner shape of the wdm.v1 envelope emitted by
// `wdm apps backups list <app-id> --json` (PRD §32). The slice lives under the
// stable "backups" key, not at the top of envelope.data, because PRD §32
// mandates an object, and the keyed shape leaves room for sibling fields (a
// stack-path echo, scan warnings) without breaking parsers. This mirrors
// [appsListPayload] and [catalogListPayload] so every list view shares one
// envelope shape.
// A nil [engine.Engine.ListBackups] result is normalized to an empty slice so
// the wire contract emits "backups": [] for a stack that never backed up.
type backupsListPayload struct {
	Backups []types.BackupInfo `json:"backups"`
}

// newAppsBackupsCmd builds the `apps backups` subgroup and registers its
// `list` and `restore` leaves. The factory flows down to each leaf so it is
// wired inside RunE following the install/status precedent.
func newAppsBackupsCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	backups := &cobra.Command{
		Use:           "backups",
		Short:         "List and restore a managed stack's config backups",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	backups.AddCommand(newAppsBackupsListCmd(newEngine))
	backups.AddCommand(newAppsBackupsRestoreCmd(newEngine))
	return backups
}

// newAppsBackupsListCmd builds the `apps backups list <app-id>` leaf (PRD §7,
// §21). It calls [engine.Engine.ListBackups] through the injected factory — a
// read-only listing that takes neither a [types.Confirmer] nor a
// [types.ProgressFn] — and emits one of two forms based on the root's --json
// persistent flag:
//   - Plain mode: one snapshot per line, newest first (the engine's order), as
//     "<snapshot_id>\t<operation>\t<created_at>\t<N file(s)>" — tab-separated
//     with the free-text file-count summary last. The snapshot id, operation,
//     and RFC3339 timestamp are engine-shaped tab- and newline-free fields, so
//     the leading columns stay parseable by cut(1)/awk(1). A stack with no
//     backups emits no output and exits 0.
//   - JSON mode: the wdm.v1 envelope wraps an object whose "backups" key holds
//     the slice (PRD §32 forbids a top-level array). A nil result is
//     normalized to an empty slice.
//
// Read-only browse: no lock, no Docker, no [types.Confirmer], no
// [types.ProgressFn]. The engine factory is invoked inside RunE, and only
// there, so `wdm apps backups list --help` never reaches [engine.New] (PRD §14
// self-update smoke-check invariant).
// Exit codes (mapped from the engine's returned error by cmd/wdm's
// exitCodeFor, PRD §27):
//   - 0: success, including a managed stack that has never backed up (the
//     empty-list case the engine returns as a non-nil empty slice).
//   - 2 ([types.ErrCodeUsageValidation]): an empty app id, an unmanaged
//     directory, or an uninstalled app — the managed-only refusals the engine
//     raises before walking any backup directory.
//   - 4 ([types.ErrCodeRuntimeLockHeld]): another wdm operation holds the
//     stack's lock; the non-blocking shared-flock manifest read refuses fast
//     rather than stalling behind the writer.
//   - 1 ([types.ErrCodeGeneric]): the backup directory could not be listed (a
//     symlinked or non-directory backup root, or a stat/read failure).
//
// Exit codes 3 (verification), 5 (Docker unavailable), 6 (permission denied),
// and 7 (user canceled) are NOT reachable: the listing constructs no Docker
// client, performs no signature verification, takes no Confirmer, and raises
// no permission-typed error — its only typed errors are usage-validation (2),
// runtime-lock-held (4), and generic (1) above (verified against
// internal/core/backups.go's ListBackups path).
func newAppsBackupsListCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <app-id>",
		Short: "List config-backup snapshots for a managed stack",
		Long: `List shows the config-backup snapshots wdm has taken for a managed
app, newest first, one per line as snapshot id, operation, creation
time, and the number of config files captured.

Each snapshot is a config-only backup — compose, .env, lock file, and
the stack's managed config files — never app data, databases, or
volumes. A stack with no backups exits 0 with no output. Use
'wdm apps backups restore <app-id> <snapshot-id>' to restore one.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps backups list: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			backups, err := eng.ListBackups(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			if useJSON {
				if backups == nil {
					backups = []types.BackupInfo{}
				}
				return EmitJSON(cmd.OutOrStdout(), backupsListPayload{Backups: backups})
			}
			return writeBackupsList(cmd.OutOrStdout(), backups)
		},
	}

	return cmd
}

// newAppsBackupsRestoreCmd builds the `apps backups restore <app-id>
// <snapshot>` leaf (PRD §20:495, §21:539). It calls
// [engine.Engine.RestoreBackup] through the injected factory to restore a
// managed stack's config files from a snapshot — config files ONLY (compose,
// .env, lock file, managed config), never app data, databases, uploaded files,
// or Docker volumes. This is a config restore, the only undo vocabulary
// the invariant's wording invariant permits. The running containers keep the
// OLD config until the recreate next-action runs; this command
// never recreates them. It then renders [types.RestoreBackupResult] in one of
// two forms based on the root's --json persistent flag:
//   - Plain mode: a config-restore finish screen on stdout — a headline naming
//     the snapshot, the restored config files, the engine's verbatim
//     config-restore boundary notice (config files restored; app data,
//     databases, volumes not), the recreate next action, and
//     the post-restore status state. The engine's progress lines stream to
//     stderr.
//   - JSON mode: the full result wrapped in the wdm.v1 envelope on stdout, and
//     nothing else (PRD §32). Progress is suppressed.
//
// The flag set is minimal, mirroring `apps remove` / `apps restart`. The flags
// are --yes (a safe-confirmation bypass), --stack-path (the engine's
// fail-closed cross-check), and the inherited --json:
//   - --yes: accept the SAFE config-restore confirmation without prompting
//     The "restore_config" confirmation rewrites
//     wdm-managed config files only and destroys no data, so --yes accepts it.
//     Restore never produces a database-risk warning, so acceptDBRisk is wired
//     false (that flag lives only on `apps update`); the shared confirmer keeps
//     the database-risk path fail-closed regardless.
//   - --stack-path: pinned onto [types.RestoreBackupRequest.StackPath]. The
//     engine resolves the stack by app id and verifies a supplied stack path
//     against it (a mismatch refuses fail-closed with
//     [types.ErrCodeUsageValidation] before any restore), so this flag is a
//     guard, not an alternate resolution path.
//
// The confirmation surface is satisfied by the engine's "restore_config"
// payload (app, stack path, compose project, snapshot identity, the config
// files to be rewritten, the config-restore boundary, and the
// runtime-keeps-old-config consequence); this leaf relays it to stderr through
// the shared [cliConfirmer] prompt path rather than re-assembling it.
// Exit codes (mapped from the engine's typed errors by cmd/wdm's exitCodeFor,
// via errors.As on *types.Error):
//   - 0: the config restore succeeded. A needs_attention result (the
//     post-restore status verification found issues, or the still-running
//     containers carry the old config) still exits 0 — the engine returns a
//     non-nil *RestoreBackupResult, mirroring the restart/remove posture: the
//     exit code reports whether the restore completed, not the subject's
//     resulting health.
//   - 2 ([types.ErrCodeUsageValidation]): an empty app id, an empty or unknown
//     snapshot id, a traversal-shaped snapshot id, an unmanaged directory, an
//     uninstalled app, a --stack-path that does not match the resolved managed
//     stack, a corrupt manifest missing its compose project, or a nil
//     confirmer.
//   - 4 ([types.ErrCodeRuntimeLockHeld]): another wdm process holds the global
//     runtime lock, or the per-stack lock is busy.
//   - 7 ([types.ErrCodeUserCanceled]): the safe config-restore confirmation
//     was declined — an explicit "N" at the prompt, or no TTY and no --yes.
//   - 1 ([types.ErrCodeGeneric]): a generic failure, e.g. the config-restore
//     write failing, or a Docker client construction fault.
//
// Exit codes 3 (verification), 5 (Docker unavailable), and 6 (permission
// denied) are NOT reachable: the restore performs no signature verification,
// and the only Docker contact is the POST-restore status verification, which
// carves out context cancellation only and fuses every other failure (a
// daemon-down inspect included) into a needs_attention result at exit 0 rather
// than propagating [types.ErrCodeDockerUnavailable] — the restore has already
// rewritten the files, so the verify pass never fails the operation (verified
// against internal/core/backups.go's verifyRestoreStatus).
// The engine factory is invoked inside RunE, and only there, so
// `wdm apps backups restore --help` never reaches [engine.New] (PRD §14
// self-update smoke-check invariant, mirrored from `apps list`).
func newAppsBackupsRestoreCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		assumeYes bool
		stackPath string
	)

	cmd := &cobra.Command{
		Use:   "restore <app-id> <snapshot>",
		Short: "Restore config files for a managed stack from a snapshot",
		Long: `Restore rewrites a managed app's config files from a backup
snapshot: compose, .env, lock file, and the stack's managed config
files. This is a config restore: it restores config files ONLY and does
NOT restore app data, databases, uploaded files, or Docker volumes.

The running containers keep the old config until you recreate them.
After a restore, run 'wdm apps update <app-id>' to recreate the
containers and apply the restored config.

--yes accepts the safe config-restore confirmation without prompting.
Use --stack-path to assert which managed stack path is being restored;
it is verified against the app's resolved stack and refuses on a
mismatch.`,
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps backups restore: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			req := types.RestoreBackupRequest{
				AppID:      args[0],
				SnapshotID: args[1],
				StackPath:  stackPath,
			}

			// "restore_config" is a SAFE confirmation that --yes accepts (it
			// rewrites wdm-managed config files only and destroys no data,
			// confirmation, so acceptDBRisk is wired false.
			confirmer := newCLIConfirmer(cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), assumeYes, false)

			// JSON mode suppresses progress so the single envelope is the only
			// thing on stdout (PRD §32). Plain mode sends progress to stderr so
			// stdout carries just the finish screen.
			var onProgress types.ProgressFn
			if !useJSON {
				onProgress = stderrProgress(cmd.ErrOrStderr())
			}

			result, err := eng.RestoreBackup(cmd.Context(), req, onProgress, confirmer)
			if err != nil {
				return err
			}

			if useJSON {
				return EmitJSON(cmd.OutOrStdout(), result)
			}
			return writeRestoreFinish(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().BoolVar(&assumeYes, "yes", false, "accept the safe config-restore confirmation without prompting")
	cmd.Flags().StringVar(&stackPath, "stack-path", "", "assert the managed stack path being restored (verified against the app)")

	return cmd
}

// writeBackupsList renders the plain-mode `apps backups list` block to w
// (stdout): one snapshot per line, in the engine's newest-first order, as
// "<snapshot_id>\t<operation>\t<created_at>\t<N file(s)>". The fields are
// tab-separated and free of table-art so cut(1)/awk(1) parse the leading
// columns; the creation time is RFC3339 in UTC, and the file-count summary is
// the free-text last field. An empty slice writes nothing (exit 0).
func writeBackupsList(w io.Writer, backups []types.BackupInfo) error {
	for _, backup := range backups {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%d file(s)\n",
			backup.SnapshotID,
			backup.Operation,
			backup.CreatedAt.UTC().Format(time.RFC3339),
			len(backup.Files),
		); err != nil {
			return fmt.Errorf("apps backups list: writing output: %w", err)
		}
	}
	return nil
}

// writeRestoreFinish renders the config-restore finish screen for a completed
// restore to w (stdout), in order of importance: the config-restore headline
// naming the snapshot, the restored config files, the engine's verbatim
// boundary notice (what was and was not restored), the recreate next action
// and the
// post-restore status state. The layout is line-oriented and free of table-art
// so cut(1) and awk(1) stay usable, mirroring writeRestartFinish.
// This is a config restore (the invariant's wording invariant — the copy says
// "config restore", never the alternate undo vocabulary). BoundaryNotice and
// NextAction are relayed from the engine's [types.RestoreBackupResult] verbatim
// — the canonical copy the failed-update auto-restore shares —
// never paraphrased, so the wording invariant holds wherever this renders.
// The "restored" headline is gated on the post-restore status: a
// needs-attention result (the verification found issues) renders a neutral
// headline that defers to the status block below rather than asserting a clean
// restore — the same gate writeRemoveFinish and writeRestartFinish apply.
func writeRestoreFinish(w io.Writer, result *types.RestoreBackupResult) error {
	var b strings.Builder

	if result.Status != nil && result.Status.NeedsAttention {
		fmt.Fprintf(&b, "%s config restored from snapshot %s; see the status below for services that need attention.\n",
			result.AppID, result.SnapshotID)
	} else {
		fmt.Fprintf(&b, "%s config restored from snapshot %s.\n", result.AppID, result.SnapshotID)
	}

	if len(result.RestoredFiles) > 0 {
		b.WriteString("\nRestored config files:\n")
		for _, file := range result.RestoredFiles {
			fmt.Fprintf(&b, "  - %s\n", file)
		}
	}

	// Relay the engine's canonical boundary text verbatim: what
	// is restored (config files) and what is not (app data, databases,
	// volumes). Never paraphrased here.
	if result.BoundaryNotice != "" {
		fmt.Fprintf(&b, "\n%s\n", result.BoundaryNotice)
	}

	// The recreate next action is the the invariant correctness contract: the
	// running containers keep the old config until the user recreates them.
	// Surface it prominently and verbatim from the engine.
	if result.NextAction != "" {
		fmt.Fprintf(&b, "\nNext: %s\n", result.NextAction)
	}

	if result.Status != nil {
		fmt.Fprintf(&b, "\nStatus: %s\n", result.Status.State)
		if result.Status.Message != "" {
			fmt.Fprintf(&b, "  %s\n", result.Status.Message)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("apps backups restore: writing finish screen: %w", err)
	}
	return nil
}
