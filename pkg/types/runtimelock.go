package types

import "time"

// RuntimeLockStatus is the UI-facing projection of the global
// runtime.lock state, returned by Engine.RuntimeLockStatus and
// Engine.ClearStaleRuntimeLock (PRD §26, §18 condition 8). It mirrors
// the read-only state probe but lives in pkg/types so the engine can
// return it across the facade: a UI layer must never import the state
// package (PRD §29). The engine derives Stale from its staleness policy
// (dead holder PID OR held beyond the staleness window) — a UI never
// computes it, and ClearStaleRuntimeLock acts only when Stale is true
type RuntimeLockStatus struct {
	// Exists reports whether the runtime.lock file is present on disk. A
	// released lock leaves its file behind by design, so existence alone
	// carries no held-or-stale signal.
	Exists bool `json:"exists"`

	// Held reports whether another process currently holds the exclusive
	// flock — i.e. a state-changing operation appears to be in flight.
	Held bool `json:"held"`

	// Stale reports whether the engine's staleness policy classifies the
	// lock as recoverable (a dead holder PID or held beyond the
	// staleness window). ClearStaleRuntimeLock clears only when this is
	// true; a live lock is refused.
	Stale bool `json:"stale"`

	// HolderPID is the PID recorded in the lock file when readable.
	HolderPID int `json:"holder_pid,omitempty"`

	// HolderCommand is the operation name the holder recorded (for
	// example "install").
	HolderCommand string `json:"holder_command,omitempty"`

	// HolderAlive reports whether the holder PID refers to a running
	// process (signal-0 probe), meaningful only when Held is true. It
	// always serializes (no omitempty): a recorded holder whose PID is
	// dead is exactly the stale-recovery signal (PRD §26, §18 condition
	// 8), and omitempty would drop holder_alive:false just when it
	// carries that signal. Always serializing also keeps the lock-state
	// booleans consistent (Exists/Held/Stale always serialize) and lets
	// consumers distinguish a dead holder (holder_alive:false beside the
	// holder fields) from no recorded holder (false with those fields
	// absent).
	HolderAlive bool `json:"holder_alive"`

	// StartedAt is the holder's acquisition timestamp when readable.
	StartedAt *time.Time `json:"started_at,omitempty"`

	// WDMVersion is the wdm version string the holder recorded.
	WDMVersion string `json:"wdm_version,omitempty"`
}
