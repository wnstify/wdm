// Package state owns wdm's local persistent state: runtime
// locking, config loading, per-stack lock files, and the stack scanner.
//   - — generic OS-level flock helpers ([LockExclusive],
//     [TryLockExclusive], [Unlock]) and the runtime.lock manager
//     ([AcquireRuntimeLock], [RuntimeLockHandle]) that PRD §26
//     requires around every state-changing operation.
//   - — the TOML config loader ([LoadConfig],
//     [LoadConfigBytes]) that reads ~/.config/wdm/config.toml,
//     validates it against config/schema.json (draft 2020-12), and
//     returns a populated pkg/types.Settings or wraps
//     pkg/types.ErrConfigInvalid (PRD §34, "Config & catalog
//     schemas").
//   - — the managed stack scanner ([ScanStacks],
//     [ScanResult], [ScanWarning]) and the .wdm.lock parser
//     ([ReadStackLock], [StackLock], [ImagePin]) required by
//     Engine.List (PRD §9, §26). Subdirectories without
//     .wdm.lock are silently ignored; subdirectories with
//     corrupt locks become ScanResult.Warnings rather than fatal
//     errors.
//   - of — the per-stack held-lock API
//     ([AcquireStackLock], [StackLockHandle]) that keeps
//     flock(LOCK_EX) across long-running install/update/remove work
//     and persists manifest updates through the held fd using the
//     truncate/seek/write/fsync pattern.
//   - [ReadStackEnv] — the existing .env reader that parses KEY=VALUE lines
//     for update-time regenerable=false secret reuse without mutating disk.
//
// Locking primitives are reusable: the .wdm.lock reader and held-handle
// writer both call the same [LockExclusive] helper (blocking variant,
// mirroring PRD §26's read-modify-write protocol).
// Locking protocol:
//  1. Open(path, O_RDWR|O_CREAT, 0o600)
//  2. flock(fd, LOCK_EX) — LOCK_NB for runtime.lock, blocking for .wdm.lock
//  3. Read current JSON — best-effort for runtime.lock (diagnostic only);
//     authoritative for .wdm.lock readers and held-handle RMW
//  4. Mutate in-memory
//  5. Truncate + write back; fsync
//  6. Release fd — held handles unlock first; close is the safety net
//
// tmp+rename is forbidden for flock-backed lock files (.wdm.lock and
// runtime.lock): rename detaches flock from the original inode,
// defeating cross-process exclusion. That
// prohibition is lock-file specific; non-lock artifacts may compose
// through [WriteFileAtomic].
// Import boundary: internal/state may import other internal/* siblings
// (notably internal/security for path safety) plus the standard library and
// narrowly scoped third-party libraries needed for config decoding —
// github.com/BurntSushi/toml (TOML parser) and
// github.com/santhosh-tekuri/jsonschema/v6 (JSON Schema validator).
// It MUST NOT depend on pkg/engine, internal/tui, internal/cli, or
// internal/core. cmd/wdm owns the translation of
// [ErrRuntimeLockHeld] into pkg/types.WrapError with
// pkg/types.ErrCodeRuntimeLockHeld for exit code 4,
// and of pkg/types.ErrConfigInvalid into pkg/types.ErrCodeUsageValidation
// for exit code 2.
// Public surface roll-call:
//   - [LockExclusive], [TryLockExclusive], [Unlock] — flock wrappers
//   - [RuntimeLockInfo] — on-disk runtime.lock JSON shape (PRD §26)
//   - [RuntimeLockMetadata] — caller-supplied input to AcquireRuntimeLock
//   - [RuntimeLockHandle] — owns the held flock; Release unlocks and closes the fd
//   - [AcquireRuntimeLock] — the PRD §26 entry gate
//   - [ErrRuntimeLockHeld] — sentinel for contention; match with errors.Is
//   - [LockHeldError] — typed error carrying the holder's metadata; extract with errors.As
//   - [LoadConfig] — read, parse, and validate ~/.config/wdm/config.toml
//   - [LoadConfigBytes] — same as LoadConfig but from an in-memory payload
//   - [StackLock] — on-disk JSON shape of <stack>/.wdm.lock (PRD §9)
//   - [ImagePin] — single service-to-image binding inside StackLock
//   - [ReadStackLock] — open, flock(LOCK_EX), read, and parse a .wdm.lock
//   - [StackLockHandle] — held flock over a stack .wdm.lock fd
//   - [AcquireStackLock] — open/create, flock(LOCK_EX), parse, hold for RMW
//   - [ScanResult] — Apps + Warnings produced by ScanStacks
//   - [ScanWarning] — typed entry for a directory whose .wdm.lock failed to parse
//   - [ScanStacks] — walk the stack base, return one types.AppInfo per parseable lock
//   - [GeneratedDirMode] — mode used for auto-created parent directories
//   - [SyncDirectory] — open + fsync + close helper for directory durability
//   - [WriteFileAtomic] — tmp-file write + fsync + rename + parent-directory fsync
//   - [BackupDirName] — well-known stack-local backup directory name
//   - [CreateConfigBackup] — pre-operation config snapshot creator under BackupDirName
//   - [BackupRetentionLimit] — hard cap for retained config snapshots per stack
//   - [PruneConfigBackups] — retention pass preserving the pinned successful snapshot
//   - [ConfigRestoreBoundaryNotice] — user-facing config-restore boundary text
//   - [RestoreConfigBackup] — config-file-only snapshot restore helper
//   - [ConfigBackupSnapshot] — one enumerated backup snapshot directory
//   - [ListConfigBackups] — lock-free newest-first config snapshot lister
//   - [ReadStackEnv] — read-only KEY=VALUE parser for <stack>/.env reuse
//   - [WithMigrationLogger] — option supplying the logger the .wdm.lock
//     migration framework records to (PRD §30; see migration.go)
//   - [ExtractTarGzToDir] — byte-level SINK that extracts a verified
//     gzip-tar catalog bundle into a fresh contained directory with
//     hostile-member-name rejection, bounded sizes, and full-tree
//     rollback on failure
//   - [CopyTree] — contained recursive tree copy with mode
//     normalization and rollback, used to materialize the active
//     catalog templates from an immutable snapshot
//   - [RemoveContainedTree] — the single sanctioned os.RemoveAll site
//     for the catalog-storage rollback path (internal/catalog routes
//     its destructive removal through here, holding no os.RemoveAll of
//     its own)
//   - [ErrBundleExtraction] — sentinel for extraction/copy failures;
//     match with errors.Is (wraps security path-escape sentinels)
//
// The.wdm.lock schema migration framework (PRD §30) lives in
// migration.go and is wired into [AcquireStackLock]: when a state-changing
// op opens a managed stack whose schema_version is OLDER than the current
// [stackLockSchemaVersion], the held flock lets the open path back up the
// config, apply the explicit registered migration chain in memory, and
// persist the migrated manifest through the held fd — failing closed with a
// [pkg/types.ErrCodeMigrationFailure] error that leaves the stack untouched
// on any error. The read-only readers never migrate. runtime.lock is a
// DISTINCT file with its own schema and never passes through this framework
// Platform: this package builds on unix (Linux + Darwin for dev
// builds). PRD §2 ships Linux amd64 only; Darwin support exists
// for Mac local development.
package state
