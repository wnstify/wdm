package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// This file holds the top-level `self-update` group and its two leaf bodies —
// `self-update check` and `self-update apply` (PRD §14 binary self-update,
// §22/§23 verified release flow) — as thin callers of the engine's binary
// self-update surface ([engine.Engine.CheckSelfUpdate] /
// [engine.Engine.ApplySelfUpdate]).
// Group form: a
// top-level `self-update` group with `wdm self-update check` (read-only,
// mirroring Status — no runtime.lock, no [types.Confirmer], no
// [types.ProgressFn]) and `wdm self-update apply` (state-changing, mirroring
// Update — runtime.lock + [types.ProgressFn] + [types.Confirmer], the
// self_update confirmation kind). `self-update apply` verifies the downloaded
// binary, stages it, replaces atomically where practical, retains the previous
// binary as wdm.previous, runs the `wdm --version` smoke check, and restores
// the previous binary on failure (PRD §14). Neither leaf takes a
// positional argument — the binary is singular — so both register Args:NoArgs.
// `self-update` is a group command, mirroring the `apps` and `lock` groups:
// Args:NoArgs plus a Runnable RunE that prints help — the golang-spf13-cobra
// canonical group pattern. Without Args:NoArgs an unknown subcommand would
// silently exit 0; without a Runnable RunE, `self-update --help` would skip the
// Usage and Flags sections. Registration tests pin the registered command
// paths.
// Both leaves build the engine inside RunE, and only there, so
// `self-update check --help` / `self-update apply --help` never reach
// [engine.New] (PRD §14 self-update smoke-check invariant: a malformed
// config.toml must never break the --version / --help paths the self-update
// relies on). NO sudo and NO privilege flags are wired anywhere: when the
// executable lives where wdm cannot write, the engine refuses with a typed
// error naming the manual install path and this leaf returns it;
// cmd/wdm prints it to stderr (PRD §11, §14).

// newSelfUpdateCmd builds the top-level `self-update` group and registers
// its `check` and `apply` leaves. The factory flows down to each leaf so it
// is wired inside RunE following the install/status precedent.
func newSelfUpdateCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	selfUpdate := &cobra.Command{
		Use:           "self-update",
		Short:         "Check for and apply verified wdm binary updates",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	selfUpdate.AddCommand(newSelfUpdateCheckCmd(newEngine))
	selfUpdate.AddCommand(newSelfUpdateApplyCmd(newEngine))
	return selfUpdate
}

// newSelfUpdateCheckCmd builds the `self-update check` leaf (PRD §14, §22 — the
// read-only "is a newer verified wdm binary available?" probe). It calls
// [engine.Engine.CheckSelfUpdate] through the injected factory — a read-only
// probe that takes neither a [types.Confirmer] nor a [types.ProgressFn] and
// acquires no runtime.lock — and emits one of two forms based on the root's
// --json persistent flag:
//   - Plain mode: a scannable status block on stdout — the running binary
//     version, the latest published release version, whether an update is
//     available (yes/no), whether the latest release verified (yes/no), and any
//     operator-guidance notes the engine attached (for example that a dev build
//     is not offered a self-update). Line-oriented and free of table-art so
//     cut(1) and awk(1) stay usable.
//   - JSON mode: the [types.SelfUpdateStatus] wrapped DIRECTLY in the wdm.v1
//     envelope on stdout and nothing else (PRD §32). It marshals to a JSON
//     object, so it is the envelope.data object directly.
//
// The check tracks the single published release line, so [types.SelfUpdateQuery]
// is empty and the leaf carries NO flags — there is no --channel to invent.
// Read-only probe: no runtime.lock, no
// [types.Confirmer], no [types.ProgressFn]. The engine factory is invoked
// inside RunE, and only there, so `wdm self-update check --help` never reaches
// [engine.New] (PRD §14 self-update smoke-check invariant).
// Exit codes (mapped from the returned error by cmd/wdm's exitCodeFor, PRD
// §27):
//   - 0: success — including a dev build, an up-to-date binary, and a verified
//     update being available (the check only reports; it never applies).
//   - 3 ([types.ErrCodeVerificationFailed]): the latest release candidate
//     failed checksum/signature/attestation verification — fail closed rather
//     than reporting an unverified "update available" (PRD §22, §23, decision
//     #55/#64).
//   - 8 ([types.ErrCodeNetworkFailure]): a transport/auth/rate-limit fault
//     contacting the release endpoint or downloading the candidate
//     #64).
//   - 1 ([types.ErrCodeGeneric]): an operational fault, e.g. the ephemeral
//     verification staging directory could not be created.
//
// Exit code 2 is reachable only via a bare context-cancel mapping: a context
// cancellation surfaces as a bare context error (not a *types.Error), which
// cmd/wdm's exitCodeFor default arm maps to exit 2. The check has no argument or
// validation path that produces a typed exit-2 error. Exit codes 4
// (runtime-lock held), 5 (Docker unavailable), 6 (permission denied), and 7
// (user canceled) are NOT reachable: the check acquires no lock, constructs no
// Docker client, and takes no Confirmer (verified against
// internal/core/self_update.go's CheckSelfUpdate path).
func newSelfUpdateCheckCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check whether a newer verified wdm binary is available",
		Long: `Check reports the running wdm binary version against the latest
published release and whether a newer verified candidate is available.

It is read-only: it contacts the release endpoint, downloads and verifies
the latest candidate into an ephemeral staging directory, and reports the
result — it never replaces the binary. Use 'wdm self-update apply' to
install a verified update.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("self-update check: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			status, err := eng.CheckSelfUpdate(cmd.Context(), types.SelfUpdateQuery{})
			if err != nil {
				return err
			}

			return emitResult(cmd, useJSON, status, writeSelfUpdateStatus)
		},
	}

	return cmd
}

// newSelfUpdateApplyCmd builds the `self-update apply` leaf (PRD §14, §22, §23 —
// the state-changing verify/stage/replace/smoke/rollback path). It calls
// [engine.Engine.ApplySelfUpdate] through the injected factory and renders
// [types.SelfUpdateResult] in one of two forms based on the root's --json
// persistent flag:
//   - Plain mode: a finish block on stdout — the version transition (previous →
//     applied), whether the binary was replaced (yes/no), whether the
//     post-replace `wdm --version` smoke check passed (ok/failed), whether a
//     failed smoke check rolled the binary back (yes/no), the engine's
//     user-readable message, and the retained wdm.previous path when present. A
//     FAILED smoke check that rolled back is reported clearly (the rolled-back
//     line plus the message) and never hidden as success — the engine also
//     returns a non-nil error on that path, so the command exits nonzero. The
//     engine's progress lines stream to stderr.
//   - JSON mode: the [types.SelfUpdateResult] wrapped DIRECTLY in the wdm.v1
//     envelope on stdout, and nothing else (PRD §32). Progress is suppressed.
//
// The flag set is minimal and carries NO privilege escalation:
//   - --yes: accept the SAFE "self_update" confirmation without prompting
//     Replacing the binary while keeping a rollback
//     copy is safe, so --yes accepts it; self-update never produces a
//     database-risk warning, so acceptDBRisk is wired false (that flag lives
//     only on `apps update`); the shared confirmer keeps the database-risk path
//     fail-closed regardless.
//   - --target-version: pinned onto [types.SelfUpdateRequest.TargetVersion].
//     The engine re-verifies the downloaded candidate against this version
//     before any replacement and refuses with [types.ErrCodeUsageValidation]
//     when the latest verified release differs — so a check/apply TOCTOU never
//     silently installs an unexpected version. Empty (the default) accepts
//     whatever the latest verified release is.
//
// There is NO --force, --sudo, or privilege flag of any kind. When the
// executable lives where wdm cannot write, the engine refuses with a typed
// error whose hint names the user-writable manual install path; this
// leaf returns that error untouched (cmd/wdm prints it to stderr) and NEVER
// shells out or attempts elevation (PRD §11, §14).
// The confirmation surface is satisfied by the engine's "self_update" payload
// (the version transition, the exec path being replaced, and the wdm.previous
// retention); this leaf relays it to stderr through the shared [cliConfirmer]
// prompt path rather than re-assembling it.
// Exit codes (mapped from the engine's typed errors by cmd/wdm's exitCodeFor,
// via errors.As on *types.Error):
//   - 0: the self-update applied and its smoke check passed.
//   - 2 ([types.ErrCodeUsageValidation]): the install location is not
//     user-writable / does not exist / cannot be written (the
//     writability gate, with the manual-install hint), a --target-version the
//     latest verified release does not match, or a nil confirmer.
//   - 3 ([types.ErrCodeVerificationFailed]): the downloaded candidate failed
//     checksum/signature/attestation verification — fail closed before any
//     replacement (PRD §22, §23).
//   - 4 ([types.ErrCodeRuntimeLockHeld]): another wdm operation holds the
//     global runtime lock.
//   - 7 ([types.ErrCodeUserCanceled]): the safe self_update confirmation was
//     declined — an explicit "N" at the prompt, or no TTY and no --yes.
//   - 8 ([types.ErrCodeNetworkFailure]): a transport/auth/rate-limit fault
//     contacting the release endpoint or downloading the candidate.
//   - 1 ([types.ErrCodeGeneric]): an operational replace fault (the
//     wdm.previous copy, chmod, or rename failing), the staging directory
//     creation failing, OR a successful update whose post-replace smoke check
//     FAILED and was rolled back: the engine returns a non-nil
//     *SelfUpdateResult with RolledBack=true alongside this error, and the
//     finish block reports the rollback even on the error path.
//
// Exit codes 5 (Docker unavailable) and 6 (permission denied) are NOT
// reachable: the self-update constructs no Docker client, and the
// not-writable refusal maps to usage-validation (exit 2), not permission denied
// — wdm never elevates, so it surfaces an EACCES install location as a usage
// error with a manual-install next action rather than a permission fault
// (verified against internal/core/self_update_target.go's requireWritableDir).
// The engine factory is invoked inside RunE, and only there, so
// `wdm self-update apply --help` never reaches [engine.New] (PRD §14
// self-update smoke-check invariant, mirrored from `apps list`).
func newSelfUpdateApplyCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		assumeYes     bool
		targetVersion string
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Download, verify, and install a newer wdm binary",
		Long: `Apply downloads and verifies a newer wdm release binary, replaces
the running binary atomically, and keeps the previous binary as
wdm.previous for rollback (PRD §14). Verification runs before any
replacement; a post-replace 'wdm --version' smoke check must pass or the
previous binary is restored automatically.

wdm never uses sudo. If the binary lives where wdm cannot write, apply
refuses and points you at a user-writable install path — it never
attempts privilege escalation.

--yes accepts the safe self-update confirmation without prompting. Use
--target-version to require a specific release; apply refuses if the
latest verified release differs.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("self-update apply: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			req := types.SelfUpdateRequest{TargetVersion: targetVersion}

			// "self_update" is a SAFE confirmation that --yes accepts (it
			// replaces the binary while keeping a rollback copy, the confirmation rules);
			// self-update never produces a database-risk confirmation, so
			// acceptDBRisk is wired false.
			confirmer, onProgress := stateChangeIO(cmd, assumeYes, false, useJSON)

			result, err := eng.ApplySelfUpdate(cmd.Context(), req, onProgress, confirmer)
			if err != nil {
				// ApplySelfUpdate is the only engine method that returns a
				// non-nil result alongside a non-nil error: the rollback paths
				// A rolled-back apply is a
				// completed operation whose structured outcome the contract
				// requires reporting (especially the always-serialized
				// rolled_back / replaced / smoke_ok booleans for --json), so
				// surface it BEFORE returning the error and exiting nonzero.
				// This is the deliberate §32 exception: ordinary errors
				// (verification / network / writability / decline) return
				// (nil, err) and keep stdout empty, so the result != nil guard
				// preserves that contract. The result-render error is dropped so
				// a stdout write failure can never mask the engine error.
				if result != nil {
					if useJSON {
						_ = EmitJSON(cmd.OutOrStdout(), result) //nolint:errcheck // the engine error is the primary error; a stdout-write failure must never mask it (decision #61)
					} else {
						_ = writeSelfUpdateFinish(cmd.OutOrStdout(), result) //nolint:errcheck // the engine error is the primary error; a stdout-write failure must never mask it (decision #61)
					}
				}
				return err
			}

			return emitResult(cmd, useJSON, result, writeSelfUpdateFinish)
		},
	}

	cmd.Flags().BoolVar(&assumeYes, "yes", false, "accept the safe self-update confirmation without prompting")
	cmd.Flags().StringVar(&targetVersion, "target-version", "", "require this release version (refuses if the latest verified release differs)")

	return cmd
}

// writeSelfUpdateStatus renders the plain-mode `self-update check` status block
// to w (stdout). The layout is line-oriented and free of table-art so cut(1)
// and awk(1) stay usable: a current/latest version pair, the update-available
// and verified flags as yes/no, and any operator-guidance notes the engine
// attached (for example the dev-build note).
func writeSelfUpdateStatus(w io.Writer, status *types.SelfUpdateStatus) error {
	var b strings.Builder

	fmt.Fprintf(&b, "current version\t%s\n", displayValue(status.CurrentVersion))
	fmt.Fprintf(&b, "latest version\t%s\n", displayValue(status.LatestVersion))
	fmt.Fprintf(&b, "update available\t%s\n", yesNo(status.UpdateAvailable))
	fmt.Fprintf(&b, "verified\t%s\n", yesNo(status.Verified))

	if len(status.Notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, note := range status.Notes {
			fmt.Fprintf(&b, "  %s\n", note)
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("self-update check: writing status: %w", err)
	}
	return nil
}

// writeSelfUpdateFinish renders the plain-mode `self-update apply` finish block
// to w (stdout): the version transition, the replaced / smoke-check /
// rolled-back outcomes as yes/no, the engine's user-readable message, and the
// retained wdm.previous path when present. A failed smoke check that rolled
// back is rendered exactly as the engine reports it (RolledBack=true,
// SmokeOK=false, plus the message) so the rollback is never hidden as success.
// The apply RunE calls this on BOTH the success path and the rollback error
// path: on rollback the engine returns a non-nil *SelfUpdateResult alongside a
// non-nil error, and the leaf renders this block before
// returning the error so cmd/wdm still exits nonzero. That is the deliberate
// PRD §32 exception — a rolled-back apply is a completed operation whose
// structured outcome the contract requires reporting, distinct from an ordinary
// error (verification / network / writability / decline), which returns no
// result and keeps stdout empty.
func writeSelfUpdateFinish(w io.Writer, result *types.SelfUpdateResult) error {
	var b strings.Builder

	fmt.Fprintf(&b, "wdm self-update\t%s -> %s\n",
		displayValue(result.PreviousVersion), displayValue(result.AppliedVersion))
	fmt.Fprintf(&b, "replaced\t%s\n", yesNo(result.Replaced))
	fmt.Fprintf(&b, "smoke check\t%s\n", okFailed(result.SmokeOK))
	fmt.Fprintf(&b, "rolled back\t%s\n", yesNo(result.RolledBack))

	if result.Message != "" {
		fmt.Fprintf(&b, "\n%s\n", result.Message)
	}

	if result.PreviousBinaryPath != "" {
		fmt.Fprintf(&b, "previous binary kept at\t%s\n", result.PreviousBinaryPath)
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("self-update apply: writing finish block: %w", err)
	}
	return nil
}

// displayValue maps an empty version (or any unset string field) to a
// readable placeholder so a status or finish line never shows a blank value.
func displayValue(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}

// okFailed renders the smoke-check bool as ok/failed so a failed smoke check
// reads unambiguously in the finish block.
func okFailed(value bool) string {
	if value {
		return "ok"
	}
	return "failed"
}
