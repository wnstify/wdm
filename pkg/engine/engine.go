package engine

import (
	"context"

	"github.com/wnstify/wdm/pkg/types"
)

// Engine is the stable, GUI-facing surface of wdm (PRD §29, §37).
// Every method takes [context.Context] first so callers can cancel
// long-running work — Ctrl+C in the TUI, modal cancel in the future GUI,
// and deadlines in tests.
// Distribution surfaces split into read-only Check* methods and
// state-changing Apply* methods. The read path mirrors Status (no
// runtime.lock, no Confirmer, no ProgressFn); the apply path mirrors
// Update (runtime.lock + ProgressFn + Confirmer). The registry image
// check has no Apply counterpart because app updates apply only through
// Update.
type Engine interface {
	// List returns one [types.AppInfo] per managed stack under the
	// configured base directory (default ~/docker — PRD §9) whose
	// .wdm.lock parses cleanly. A corrupt lock surfaces as a warning on
	// its entry, not as a fatal error.
	// Implementations MUST return a fresh slice on each call so callers
	// may mutate or retain the result without affecting later List calls
	// (defensive-copy semantics per golang-safety).
	List(ctx context.Context) ([]types.AppInfo, error)

	// Status reports the operational state of a single managed stack
	// identified by appID (PRD §18).
	Status(ctx context.Context, appID string) (*types.AppStatus, error)

	// Logs streams structured log lines for the requested stack via
	// onLine (PRD §24). Streaming stops when ctx is canceled, when the
	// upstream closes, or when the request specifies a finite tail and
	// no follow.
	Logs(ctx context.Context, req types.LogsRequest, onLine LogLineFn) error

	// Install creates and starts a new managed stack (PRD §17). onProgress
	// receives PRD §37 progress events; confirmer authorizes any
	// consequential prompt (port collision, overwrite). Both may be nil:
	// implementations must tolerate a nil onProgress, and must refuse to
	// proceed past a confirmation step when confirmer is nil.
	Install(ctx context.Context, req types.InstallRequest, onProgress ProgressFn, confirmer Confirmer) (*types.InstallResult, error)

	// Update re-renders templates, detects drift, and recreates services
	// (PRD §20). A pre-update config backup is captured before any
	// destructive step (PRD §21). The confirmer is consulted before
	// breaking changes proceed.
	Update(ctx context.Context, req types.UpdateRequest, onProgress ProgressFn, confirmer Confirmer) (*types.UpdateResult, error)

	// Remove performs a safe removal of a managed stack (PRD §19):
	// docker compose down without -v, leaving named volumes intact. The
	// confirmer is consulted before any irreversible step.
	Remove(ctx context.Context, req types.RemoveRequest, onProgress ProgressFn, confirmer Confirmer) (*types.RemoveResult, error)

	// Restart recreates a managed stack's containers in place — the
	// "Restart app" needs-attention next action (PRD §18:416, also the
	// §18:425 restart-loop recovery). ships plain restart
	// semantics: docker compose restart stops and starts the SAME
	// containers without re-reading the Compose file, so it never
	// re-renders templates and never touches config files or backups
	// Whole-stack only in v1; there is no per-service
	// field. As a state-changing op it holds the global runtime.lock and
	// the per-stack flock and consults confirmer before the recreate;
	// onProgress receives the step_restart_* events.
	Restart(ctx context.Context, req types.RestartRequest, onProgress ProgressFn, confirmer Confirmer) (*types.RestartResult, error)

	// ValidateConfig runs docker compose config --quiet against the
	// managed stack's on-disk Compose file and reports the outcome (PRD
	// §18:418 "Validate config", §18:427 compose-validation condition).
	// Read-only: it never acquires the runtime.lock, mirroring Status's
	// non-blocking shared-flock posture. A validation failure is NOT an
	// error: it returns a [types.ValidationResult] with Valid false and a
	// redactor-scrubbed Detail, just as Status returns a needs-attention
	// stack at exit 0. Raw compose-config stdout is
	// never surfaced because it interpolates.env secrets.
	ValidateConfig(ctx context.Context, appID string) (*types.ValidationResult, error)

	// ListBackups returns the config-backup snapshots for a managed
	// stack, newest first (PRD §7 "Backups", §21). Read-only: it never
	// acquires the runtime.lock. The returned slice is freshly allocated
	// on each call so callers may retain or mutate it without affecting
	// later calls (defensive-copy semantics per golang-safety, matching
	// List).
	ListBackups(ctx context.Context, appID string) ([]types.BackupInfo, error)

	// RestoreBackup restores config files from a snapshot — config files
	// ONLY (compose, .env, .wdm.lock, guidance), never app data or
	// volumes (PRD §20:495, §21:539). It uses the SAME internal path as
	// the failed-update automatic restore. As a
	// state-changing op ("restore" per PRD §26:686) it holds the global
	// runtime.lock and the per-stack flock and consults confirmer before
	// the file rewrite; onProgress receives the step_restore_* events.
	// Because docker compose restart does not re-read the Compose file,
	// this call does NOT apply the restored config to the running
	// containers: the result surfaces the recreate path as the next
	// action, never plain restart. This is a config
	// restore, never a rollback.
	RestoreBackup(ctx context.Context, req types.RestoreBackupRequest, onProgress ProgressFn, confirmer Confirmer) (*types.RestoreBackupResult, error)

	// AvailableApps returns the installable catalog entries for the
	// queried channel, projected into [types.CatalogApp] so the install
	// picker can populate without crossing the facade (PRD §7 "Install an
	// app", §8 first-run wizard step 3, §15 eligibility). Read-only and
	// local-filesystem only: no network call, no catalog download, no
	// signature verification — the §22 self-update surface is
	// The returned slice is freshly allocated on each call
	// (defensive-copy semantics, matching List).
	AvailableApps(ctx context.Context, req types.CatalogQuery) ([]types.CatalogApp, error)

	// AvailableApp returns the full detail projection for one catalog
	// entry — description, placeholders, ports, image pins, and resource
	// bands — for the app-detail and install-form screens (PRD §7, §8,
	// §15). Read-only and local-filesystem only, like AvailableApps
	AvailableApp(ctx context.Context, req types.CatalogAppQuery) (*types.CatalogApp, error)

	// DeleteApp permanently deletes a managed stack's files and directory
	// — the destructive deletion flow that PRD §19:444-455 mandates be
	// SEPARATE from the safe Remove. The engine re-verifies
	// req.ConfirmationName == AppID before any deletion,
	// refuses any path outside the managed stack root (§19:452), runs
	// docker compose down (NEVER -v) then deletes the files, and NEVER
	// deletes named volumes: req.DeleteNamedVolumes is hard-refused when
	// true in v1 (§19:453). As a state-changing op it holds the global
	// runtime.lock and the per-stack flock and consults confirmer with a
	// [types.ConfirmationKindDeleteDestructive] payload; onProgress
	// receives the step_delete_* events.
	DeleteApp(ctx context.Context, req types.DeleteRequest, onProgress ProgressFn, confirmer Confirmer) (*types.DeleteResult, error)

	// RuntimeLockStatus reports the current global runtime.lock state
	// (PRD §26, §18 condition 8). Read-only: it probes without acquiring,
	// creating, or deleting the lock, projecting [state.RuntimeLockProbe]
	// across the facade into [types.RuntimeLockStatus].
	RuntimeLockStatus(ctx context.Context) (*types.RuntimeLockStatus, error)

	// ClearStaleRuntimeLock removes the global runtime.lock ONLY when it
	// is provably stale — a dead holder PID or held beyond the staleness
	// window (PRD §26:689 "safe recovery prompt"). A live lock is NEVER
	// clearable: it is refused with [types.ErrCodeRuntimeLockHeld]
	// The engine owns the staleness policy; the UI only
	// renders the prompt. confirmer authorizes the recovery; the returned
	// status reflects the post-clear state.
	ClearStaleRuntimeLock(ctx context.Context, confirmer Confirmer) (*types.RuntimeLockStatus, error)

	// ---: trust and distribution surface ---
	// Each distribution surface splits into a read-only Check* and a
	// state-changing Apply*. The Check* methods mirror Status (no
	// runtime.lock, no Confirmer, no ProgressFn); the Apply* methods
	// mirror Update (runtime.lock + ProgressFn + Confirmer). Network
	// actions are explicit and typed: a registry/GitHub/catalog failure
	// maps to [types.ErrCodeNetworkFailure] (exit 8), a verification
	// failure to [types.ErrCodeVerificationFailed] (exit 3)
	// #64). No telemetry is ever emitted.

	// CheckCatalogUpdate reports whether a newer verified catalog exists
	// for the queried channel (PRD §22). Read-only: it contacts the
	// configured catalog endpoint, compares the local catalog version
	// against the latest available, and returns the change summary plus
	// verification state — without acquiring the runtime.lock or writing
	// anything. It never modifies
	// deployed apps.
	CheckCatalogUpdate(ctx context.Context, req types.CatalogUpdateQuery) (*types.CatalogUpdateStatus, error)

	// ApplyCatalogUpdate downloads, verifies, and installs a newer catalog
	// for the requested channel (PRD §22). Verification — checksum,
	// signature, and attestation — runs BEFORE any catalog file is written
	// the verified bytes
	// are then written atomically under the global runtime.lock. A
	// verified catalog older than the active one is refused as a signed
	// rollback. The apply NEVER modifies deployed apps or
	// per-stack .wdm.lock files. As a state-changing op it
	// consults confirmer with a [types.ConfirmationKindCatalogUpdate]
	// payload before the download/apply; onProgress receives the
	// step_catalog_update_* events.
	ApplyCatalogUpdate(ctx context.Context, req types.CatalogUpdateRequest, onProgress ProgressFn, confirmer Confirmer) (*types.CatalogUpdateResult, error)

	// CheckSelfUpdate reports whether a newer verified wdm binary release
	// exists (PRD §14). Read-only: it contacts the release endpoint and
	// reports the current binary version against the latest release and
	// whether a newer verified candidate exists — without acquiring the
	// runtime.lock or downloading/replacing anything (the invariant,
	// mirroring Status). It never performs a network check on the
	// --version / --help paths (PRD §14).
	CheckSelfUpdate(ctx context.Context, req types.SelfUpdateQuery) (*types.SelfUpdateStatus, error)

	// ApplySelfUpdate downloads and verifies a candidate wdm binary, then
	// replaces the running binary (PRD §14, §31). Verification runs BEFORE
	// any replacement; the candidate is staged, the binary
	// replaced atomically where practical, and the prior binary retained
	// as wdm.previous. The exact-version `wdm --version` smoke check runs
	// after replacement; on failure the previous binary is restored and
	// the result reports the rollback. As a state-changing
	// op it holds the global runtime.lock and consults confirmer with a
	// [types.ConfirmationKindSelfUpdate] payload before the download/
	// replace; onProgress receives the step_self_update_* events.
	ApplySelfUpdate(ctx context.Context, req types.SelfUpdateRequest, onProgress ProgressFn, confirmer Confirmer) (*types.SelfUpdateResult, error)

	// CheckImageUpdates reports registry-derived tag/digest candidates for
	// a managed app's service images (PRD §14, §20). Go-native and
	// read-only: it contacts the container registry through Go HTTP code —
	// never `docker manifest inspect` — and returns the
	// candidates that feed the EXISTING app-update planning surface,
	// without acquiring the runtime.lock or writing anything (the invariant,
	// mirroring Status). There is deliberately no apply counterpart: app
	// image updates apply only through [Engine.Update].
	CheckImageUpdates(ctx context.Context, req types.ImageUpdateQuery) (*types.ImageUpdateReport, error)

	// DailyLaunchCheckDue reports whether an automatic daily-on-launch
	// update check is currently due. It returns true only when the
	// user's UpdateCheckPreference is "daily-on-launch" AND no successful
	// check is recorded for the current local calendar day; "manual" and
	// "disabled" always return false. Read-only: it neither mutates state
	// nor performs network work, so the TUI can run it on launch without
	// blocking the first render. The launch-check flow gates the read-only
	// CheckCatalogUpdate/CheckSelfUpdate calls on this answer.
	DailyLaunchCheckDue(ctx context.Context) (bool, error)

	// RecordDailyLaunchCheck records that a daily-on-launch update check ran
	// by persisting the current instant as the last-check time.
	// Callers MUST invoke this ONLY after a check has SUCCEEDED: a failed or
	// offline check deliberately does not record, so the gate stays open and
	// the check retries on the next launch. It stores only a timestamp,
	// never secrets.
	RecordDailyLaunchCheck(ctx context.Context) error

	// Settings returns the resolved user settings loaded from
	// ~/.config/wdm/config.toml (PRD §34). PRD §29 requires the
	// engine to expose this so future GUI builds need not parse the
	// config file directly.
	Settings(ctx context.Context) (*types.Settings, error)

	// UpdateSettings persists s back to config.toml (PRD §29, §34). The
	// engine validates s before any byte is written — schema version, the
	// update-check-preference enum, the locked "stable" catalog channel,
	// base-stack-path safety, the Docker network name schema, and the
	// timezone — refusing invalid settings with a typed usage error, then
	// writes the config atomically. Settings writes never reconcile
	// deployed apps (PRD §34).
	UpdateSettings(ctx context.Context, s types.Settings) error

	// Close releases held resources (open log files, flock handles,
	// catalog file systems). After Close, every other method on the
	// receiver must return an error promptly.
	Close() error
}
