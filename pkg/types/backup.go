package types

import "time"

// BackupInfo describes one config-backup snapshot of a managed stack,
// returned by Engine.ListBackups (PRD §7 "Backups", §21). A snapshot
// captures config files only — compose, .env, .wdm.lock, and declared
// additional files — never app data or volumes.
type BackupInfo struct {
	// SnapshotID is the snapshot directory basename, of the form
	// "<unix-nanos>-<operation>" (for example "1717000000000000000-update").
	SnapshotID string `json:"snapshot_id"`

	// Operation is the lifecycle operation that created the snapshot
	// ("update", "install", "migration",...).
	Operation string `json:"operation"`

	// CreatedAt is when the snapshot was taken.
	CreatedAt time.Time `json:"created_at"`

	// Path is the absolute snapshot directory path.
	Path string `json:"path"`

	// Files lists the config filenames captured in the snapshot.
	Files []string `json:"files,omitempty"`
}

// RestoreBackupRequest carries the inputs required by
// Engine.RestoreBackup (PRD §20:495, §21:539). The restore touches
// config files ONLY — this is a config restore, never a rollback of app
// data or databases.
type RestoreBackupRequest struct {
	// AppID identifies the managed stack to restore into.
	AppID string `json:"app_id"`

	// StackPath is an optional fail-closed cross-check: when set it must
	// match the AppID-resolved managed stack or the engine refuses
	// before any restore, mirroring RemoveRequest's guard.
	StackPath string `json:"stack_path,omitempty"`

	// SnapshotID is the BackupInfo.SnapshotID basename to restore from.
	SnapshotID string `json:"snapshot_id"`
}

// RestoreBackupResult summarizes a completed config restore (PRD
// §20:495, §21:539). The restore rewrites config files but does NOT
// apply them to the running containers, because docker compose restart
// does not re-read the Compose file; NextAction therefore names the
// recreate path, never plain restart. All wording is
// "config restore", never "rollback".
type RestoreBackupResult struct {
	// AppID is the app whose config was restored.
	AppID string `json:"app_id"`

	// SnapshotID is the snapshot that was restored from.
	SnapshotID string `json:"snapshot_id"`

	// RestoredFiles lists the top-level config filenames captured in the
	// restored snapshot — the set written back for every curated layout
	// today. The restore walks the snapshot recursively, so a nested file
	// is written back without appearing here.
	RestoredFiles []string `json:"restored_files,omitempty"`

	// BoundaryNotice states the config-only guarantee: config files were
	// restored; app data, databases, and volumes were not (PRD §21). It
	// echoes the canonical notice from the shared restore path so the UI
	// need not re-type it.
	BoundaryNotice string `json:"boundary_notice"`

	// NextAction tells the caller how to apply the restored config: the
	// recreate path, never plain restart, since a restart would not
	// re-read the restored Compose file. The engine track
	// owns the exact wording; the contract is that it names the recreate
	// path.
	NextAction string `json:"next_action,omitempty"`

	// Status is the post-restore runtime status snapshot. It reflects
	// the still-running containers, which keep the old config until the
	// recreate next-action runs.
	Status *AppStatus `json:"status,omitempty"`
}
