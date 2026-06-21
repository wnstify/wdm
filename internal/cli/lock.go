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

// This file holds the top-level `lock` group and its two leaf bodies —
// `lock status` and `lock clear` (PRD §26 global runtime.lock, §18 condition 8
// stale-lock recovery) — as thin callers of the engine's runtime-lock surface
// ([engine.Engine.RuntimeLockStatus] / [engine.Engine.ClearStaleRuntimeLock]).
// Registration tests pin the command paths (`lock status`, `lock clear`),
// so the constructors keep their registered names.
// `lock status` reports the runtime.lock state (read-only, no acquisition) and
// `lock clear` clears it ONLY when provably stale (a dead holder PID or held
// beyond the staleness window; a live lock is refused per the invariant). The
// clear flow is a state-changing recovery, so it wires a [types.Confirmer]; the
// safe-vs-destructive gating is engine-side per the invariant. Neither leaf
// takes a positional argument — the global runtime.lock is singular — so both
// register Args:NoArgs.
// `lock` is a group command, mirroring the `apps` group (apps.go): Args:NoArgs
// plus a Runnable RunE that prints help — the golang-spf13-cobra canonical
// group pattern.

// newLockCmd builds the top-level `lock` group and registers its `status`
// and `clear` leaves. The factory flows down to each leaf so it is wired
// inside RunE following the install/status precedent.
func newLockCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	lock := &cobra.Command{
		Use:           "lock",
		Short:         "Inspect and recover the global runtime lock",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	lock.AddCommand(newLockStatusCmd(newEngine))
	lock.AddCommand(newLockClearCmd(newEngine))
	return lock
}

// newLockStatusCmd builds the `lock status` leaf (PRD §26, §18 condition 8). It
// calls [engine.Engine.RuntimeLockStatus] through the injected factory — a
// strictly read-only probe that takes neither a [types.Confirmer] nor a
// [types.ProgressFn] and never acquires, creates, or deletes the lock — and
// renders [types.RuntimeLockStatus] in one of two forms based on the root's
// --json persistent flag:
//   - Plain mode: a scannable, line-oriented lock-state block on stdout — the
//     exists/held/stale booleans, then the holder lines (pid, command, alive,
//     started-at, wdm version) only when a holder is recorded. The fields are
//     tab- and newline-free so cut(1)/awk(1) stay usable.
//   - JSON mode: the RuntimeLockStatus wrapped DIRECTLY in the wdm.v1 envelope
//     on stdout and nothing else (PRD §32). It marshals to a JSON object, so it
//     is the envelope.data object directly.
//
// Read-only browse: no lock, no Docker, no [types.Confirmer], no
// [types.ProgressFn]. The engine factory is invoked inside RunE, and only
// there, so `wdm lock status --help` never reaches [engine.New] (PRD §14
// self-update smoke-check invariant, mirrored from `apps list`).
// Exit codes (mapped from the engine's returned error by cmd/wdm's
// exitCodeFor, PRD §27):
//   - 0: success, including a probe of a held, stale, free-leftover, or
//     entirely absent lock — every lock state is a successful read the engine
//     returns as a non-nil *RuntimeLockStatus with a nil error.
//   - 1 ([types.ErrCodeGeneric]): the lock could not be inspected — a
//     stat/flock probe failure the engine wraps with ErrCodeGeneric.
//   - 2 ([types.ErrCodeUsageValidation]): a flag-parse failure (an unknown
//     flag, or a positional arg the NoArgs validator rejects), which fails
//     before RunE runs and routes through cmd/wdm's default arm.
//
// Exit codes 3 (verification), 4 (runtime lock held), 5 (Docker unavailable), 6
// (permission denied), and 7 (user canceled) are NOT reachable: the probe
// acquires no exclusive lock (its momentary shared flock is non-blocking and
// never surfaces ErrCodeRuntimeLockHeld), constructs no Docker client, performs
// no signature verification, takes no Confirmer, and raises no permission-typed
// error — its only typed error is the generic probe fault above (verified
// against internal/core/runtimelock.go's RuntimeLockStatus path).
func newLockStatusCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the global runtime lock state",
		Long: `Status reports the global runtime lock's state without acquiring,
creating, or deleting it: whether the lock file exists, whether a process
currently holds it, and whether wdm classifies it as stale (a dead holder
or a lock held beyond the staleness window). When a holder is recorded,
its pid, command, liveness, acquisition time, and wdm version are shown.

A stale lock can be cleared with 'wdm lock clear'.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("lock status: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			status, err := eng.RuntimeLockStatus(cmd.Context())
			if err != nil {
				return err
			}

			return emitResult(cmd, useJSON, status, writeLockStatus)
		},
	}

	return cmd
}

// newLockClearCmd builds the `lock clear` leaf (PRD §26:689 safe-recovery
// prompt). It calls [engine.Engine.ClearStaleRuntimeLock] through the injected
// factory to recover a wedged global runtime lock — but ONLY when the engine
// classifies it provably stale (a dead holder PID or a lock held beyond the
// staleness window; the invariant, forbidden to weaken). A live within-age lock
// is NEVER clearable: the engine refuses it with [types.ErrCodeRuntimeLockHeld]
// before consulting the confirmer. It then renders the post-clear
// [types.RuntimeLockStatus] in one of two forms based on the root's --json
// persistent flag:
//   - Plain mode: an honest generic headline on stdout — the lock is now clear
//     — followed by the post-clear status block (the same renderer `lock
//     status` uses). The engine does NOT expose the free-vs-stale outcome
//     across the frozen return type, so the copy never claims a stale-operation
//     recovery. This method takes no [types.ProgressFn]; the confirmer prompt
//     writes to stderr.
//   - JSON mode: the post-clear RuntimeLockStatus wrapped DIRECTLY in the
//     wdm.v1 envelope on stdout and nothing else (PRD §32) — the same
//     direct-wrap shape as `lock status`.
//
// The flag set is minimal, mirroring `apps remove` / `apps restart`. The only
// flags are --yes (a safe-confirmation bypass) and the inherited --json — there
// is no positional argument (the global runtime lock is singular) and no
// --stack-path (the lock is not per-stack):
//   - --yes: accept the SAFE stale-lock recovery confirmation without prompting
//     The "clear_stale_lock" confirmation is safe —
//     clearing a stale lock is a recovery, not a destructive action — so --yes
//     accepts it. Clearing never produces a database-risk warning, so
//     acceptDBRisk is wired false (that flag lives only on `apps update`); the
//     shared confirmer keeps the database-risk path fail-closed regardless.
//
// The confirmation surface is satisfied by the engine's "clear_stale_lock"
// payload (the lock path, holder identity, held duration, why the lock is
// classified stale, and the recovery consequence); this leaf relays it to
// stderr through the shared [cliConfirmer] prompt path (y/N, No by default)
// rather than re-assembling it. A free leftover (an unheld file) and a missing
// lock file are benign no-ops the engine tidies WITHOUT a prompt.
// Exit codes (mapped from the engine's typed errors by cmd/wdm's exitCodeFor,
// via errors.As on *types.Error):
//   - 0: the lock was cleared, including the benign tidy paths — a free
//     leftover (an unheld file) and an entirely absent lock both succeed as
//     no-op clears the engine returns as a non-nil *RuntimeLockStatus.
//   - 4 ([types.ErrCodeRuntimeLockHeld]): the lock is held by a LIVE,
//     within-age holder and so is not clearable, OR the state
//     writer could not re-verify the lock as stale during the clear (the holder
//     may have become active or its metadata may be unreadable). Both refuse
//     with this code.
//   - 7 ([types.ErrCodeUserCanceled]): the safe stale-lock recovery
//     confirmation was declined — an explicit "N" at the prompt, or no TTY and
//     no --yes. The lock file is left untouched.
//   - 1 ([types.ErrCodeGeneric]): the lock could not be inspected (the pre- or
//     post-clear probe failing) or the clear's file removal failed.
//   - 2 ([types.ErrCodeUsageValidation]): a flag-parse failure (an unknown
//     flag, or a positional arg the NoArgs validator rejects), which fails
//     before RunE runs and routes through cmd/wdm's default arm.
//
// Exit codes 3 (verification), 5 (Docker unavailable), and 6 (permission
// denied) are NOT reachable: the recovery performs no signature verification
// and no Docker contact, and raises no permission-typed error. The engine's
// nil-confirmer ErrCodeUsageValidation refusal is also unreachable here — this
// leaf ALWAYS passes a non-nil shared confirmer — so the only
// ErrCodeUsageValidation source is the flag parser above (verified against
// internal/core/runtimelock.go's ClearStaleRuntimeLock path).
// The engine factory is invoked inside RunE, and only there, so
// `wdm lock clear --help` never reaches [engine.New] (PRD §14 self-update
// smoke-check invariant, mirrored from `apps list`).
func newLockClearCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var assumeYes bool

	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Clear a stale global runtime lock",
		Long: `Clear recovers a wedged global runtime lock by removing it — but
only when wdm classifies it as provably stale (a dead holder process, or a
lock held beyond the staleness window). A lock held by a live, within-age
operation is never cleared; stop that process and retry instead.

A free leftover lock (the file present but unheld) and an absent lock are
tidied without a recovery prompt — nothing is wedged.

--yes accepts the safe stale-lock recovery confirmation without prompting.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("lock clear: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			// "clear_stale_lock" is a SAFE confirmation that --yes accepts
			// (clearing a stale lock is a recovery, not a destructive action,
			// confirmation, so acceptDBRisk is wired false.
			confirmer := newCLIConfirmer(cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), assumeYes, false)

			status, err := eng.ClearStaleRuntimeLock(cmd.Context(), confirmer)
			if err != nil {
				return err
			}

			return emitResult(cmd, useJSON, status, writeLockClearFinish)
		},
	}

	cmd.Flags().BoolVar(&assumeYes, "yes", false, "accept the safe stale-lock recovery confirmation without prompting")

	return cmd
}

// writeLockStatus renders the plain-mode runtime-lock state block for
// `lock status` to w (stdout). The layout is line-oriented and free of
// table-art so cut(1) and awk(1) stay usable: the exists/held/stale booleans
// first, then a holder block (pid, command, alive, started-at, wdm version)
// only when a holder is recorded. An absent or free-leftover lock shows only
// the booleans.
func writeLockStatus(w io.Writer, status *types.RuntimeLockStatus) error {
	var b strings.Builder

	fmt.Fprintf(&b, "exists\t%t\n", status.Exists)
	fmt.Fprintf(&b, "held\t%t\n", status.Held)
	fmt.Fprintf(&b, "stale\t%t\n", status.Stale)

	// The probe populates holder metadata only when a holder is recorded
	// (a held lock or a stale dead-holder file), so a non-zero pid is the
	// signal that the holder block is meaningful. An absent or free-leftover
	// lock has no recorded pid and shows only the booleans above.
	if status.HolderPID != 0 {
		b.WriteString("\nHolder:\n")
		fmt.Fprintf(&b, "  pid\t%d\n", status.HolderPID)
		if status.HolderCommand != "" {
			fmt.Fprintf(&b, "  command\t%s\n", status.HolderCommand)
		}
		fmt.Fprintf(&b, "  alive\t%t\n", status.HolderAlive)
		if status.StartedAt != nil {
			fmt.Fprintf(&b, "  started_at\t%s\n", status.StartedAt.UTC().Format(time.RFC3339))
		}
		if status.WDMVersion != "" {
			fmt.Fprintf(&b, "  wdm_version\t%s\n", status.WDMVersion)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("lock status: writing status: %w", err)
	}
	return nil
}

// writeLockClearFinish renders the plain-mode finish screen for a completed
// `lock clear` to w (stdout): an honest generic headline that the runtime lock
// is now clear, then the post-clear status block (the same renderer `lock
// status` uses). The headline never claims a stale-operation recovery — the
// engine does not expose the free-vs-stale outcome across its frozen return
// type, so the only honest statement is that the lock path is now free.
func writeLockClearFinish(w io.Writer, status *types.RuntimeLockStatus) error {
	if _, err := io.WriteString(w, "The global runtime lock was cleared; the path is now free.\n\n"); err != nil {
		return fmt.Errorf("lock clear: writing finish screen: %w", err)
	}
	return writeLockStatus(w, status)
}
