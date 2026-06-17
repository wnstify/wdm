//go:build unix

package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// runtimeLockSchemaVersion is the schema_version written into every
// runtime.lock by this package. Bump only when PRD §26 /
// changes the field set in a forward-incompatible way; callers MUST
// read the value back from [RuntimeLockInfo.SchemaVersion] rather
// than hard-coding it elsewhere.
const runtimeLockSchemaVersion = 1

// RuntimeLockInfo is the on-disk JSON shape of
// ~/.local/state/wdm/runtime.lock per PRD §26 and 's
// "runtime.lock" subsection. Field tags use snake_case so the file
// reads identically to the shape the spec mandates and to what
// downstream debugging (cat / jq) expects.
type RuntimeLockInfo struct {
	// SchemaVersion is the stable forward-compat marker; today
	// always [runtimeLockSchemaVersion] (= 1).
	SchemaVersion int `json:"schema_version"`

	// PID is the holder's process ID (os.Getpid at acquisition).
	// ProbeRuntimeLock uses it for signal-0 liveness probing per
	// PRD §26's "Detect stale locks where practical" clause.
	PID int `json:"pid"`

	// Command is the name of the operation that took the lock —
	// conventionally one of: "install", "update", "remove",
	// "restore", "catalog-update", "migrate" (PRD §26).
	Command string `json:"command"`

	// StartedAt is the UTC acquisition timestamp; encoded as
	// RFC3339 by encoding/json's default time.Time marshaling.
	StartedAt time.Time `json:"started_at"`

	// StartedTime is the holder process's kernel start-time
	// (the Linux /proc/<pid>/stat "starttime" field — clock ticks
	// since boot) captured at acquisition. It exists ONLY to
	// disambiguate PID reuse: a recycled PID names a different
	// process, whose start-time will not match this value, so
	// [ProbeRuntimeLock] can classify the original holder as gone
	// even though some live process now answers to the same PID
	// Additive field: zero (the json omitempty default) means the
	// holder's start-time was unavailable at acquisition — old lock
	// files predating this field, and non-Linux platforms with no
	// /proc, both leave it zero. The probe treats zero as "no
	// disambiguation signal" and falls back to the signal-0 liveness
	// answer (fail toward ALIVE). The value is opaque and meaningful
	// only relative to the running kernel's boot; it is NOT a
	// wall-clock time and MUST NOT be compared across reboots.
	StartedTime uint64 `json:"started_time,omitempty"`

	// WDMVersion is the build-time version string of the
	// holder (e.g. "0.1.0"), surfaced verbatim in the file.
	WDMVersion string `json:"wdm_version"`
}

// RuntimeLockMetadata is the caller-supplied input to
// [AcquireRuntimeLock]. PID and StartedAt are populated by the
// function (the holder is always this process at acquisition time);
// SchemaVersion is fixed at [runtimeLockSchemaVersion].
// Both fields below are REQUIRED — the function returns an error if
// either is empty so a half-populated runtime.lock never reaches disk.
type RuntimeLockMetadata struct {
	// Command names the operation taking the lock; mirrors
	// [RuntimeLockInfo.Command].
	Command string

	// WDMVersion is the build-time version string of the
	// process; mirrors [RuntimeLockInfo.WDMVersion].
	WDMVersion string
}

// RuntimeLockHandle owns an acquired runtime.lock. The held flock is
// released when [RuntimeLockHandle.Release] is called or — as the
// kernel safety net — when the process exits.
// A handle is NOT safe for concurrent use by multiple goroutines.
// Release is the only mutating method and is idempotent for
// double-call safety, but the package offers no synchronization
// beyond that: a single goroutine MUST own the acquire/release
// lifecycle.
type RuntimeLockHandle struct {
	path string
	file *os.File
	info RuntimeLockInfo
}

// Path returns the absolute path of the runtime.lock file backing h.
// Stable across the handle's lifetime; returns "" for a nil receiver.
func (h *RuntimeLockHandle) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// Info returns the [RuntimeLockInfo] this handle wrote on acquisition.
// Useful for log lines that surface the holder's PID without re-reading
// the file from disk. Returns the zero value for a nil receiver.
func (h *RuntimeLockHandle) Info() RuntimeLockInfo {
	if h == nil {
		return RuntimeLockInfo{}
	}
	return h.info
}

// Release explicitly unlocks and then closes the handle's underlying
// file descriptor. The explicit unlock makes the release observable
// before the fd teardown path, while close remains the kernel safety
// net PRD §26's "Always release on clean exit" depends on.
// Release is idempotent: calling it again after the first success is a
// no-op. The runtime.lock file itself is intentionally NOT removed —
// the next [AcquireRuntimeLock] call reuses the inode (preserving
// flock semantics on inode-based filesystems) and overwrites the JSON
// content with fresh holder metadata.
func (h *RuntimeLockHandle) Release() error {
	if h == nil || h.file == nil {
		return nil
	}
	unlockErr := Unlock(h.file)
	closeErr := h.file.Close()
	h.file = nil
	var releaseErr error
	if unlockErr != nil {
		releaseErr = errors.Join(
			releaseErr,
			fmt.Errorf("state.RuntimeLockHandle.Release: unlocking %q: %w", h.path, unlockErr),
		)
	}
	if closeErr != nil {
		releaseErr = errors.Join(
			releaseErr,
			fmt.Errorf("state.RuntimeLockHandle.Release: closing %q: %w", h.path, closeErr),
		)
	}
	return releaseErr
}

// ErrRuntimeLockHeld is the sentinel returned (wrapped in a
// [*LockHeldError]) by [AcquireRuntimeLock] when another wdm
// process is already holding the runtime lock. Detect with
// [errors.Is].
// cmd/wdm wraps this via pkg/types.WrapError with
// pkg/types.ErrCodeRuntimeLockHeld so the process exits 4 (PRD §27);
// the user-facing copy is composed by cmd/wdm, not here.
var ErrRuntimeLockHeld = errors.New("state: runtime lock held by another process")

// LockHeldError is the typed error returned by [AcquireRuntimeLock]
// when the runtime lock is held. Holder carries the on-disk metadata
// read from the lock file; when the file was empty or unparseable,
// Holder is the zero [RuntimeLockInfo] and the [Error] message
// degrades gracefully.
// Detection model:
//   - [errors.Is](err, [ErrRuntimeLockHeld]) — true for contention regardless
//     of whether the holder's metadata was readable.
//   - [errors.As](err, &lockHeld) — extracts the structured holder so
//     cmd/wdm can compose the PRD §26 hint ("Another wdm
//     operation is already running: update apps").
type LockHeldError struct {
	// Path is the absolute path of the contested runtime.lock.
	Path string

	// Holder is the on-disk metadata of the process currently
	// holding the lock. Zero value if the lock file was empty,
	// truncated mid-write, or contained invalid JSON.
	Holder RuntimeLockInfo
}

// Error implements the error interface. The format includes the
// holder's command when the on-disk JSON was readable; when it was
// not, the message degrades to a "metadata unreadable" variant so
// callers always get an actionable string.
func (e *LockHeldError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Holder.Command == "" {
		return fmt.Sprintf("state: runtime lock held at %s (holder metadata unreadable)", e.Path)
	}
	return fmt.Sprintf(
		"state: runtime lock at %s held by %q (pid %d, started %s)",
		e.Path,
		e.Holder.Command,
		e.Holder.PID,
		e.Holder.StartedAt.UTC().Format(time.RFC3339),
	)
}

// Unwrap returns [ErrRuntimeLockHeld] so [errors.Is] matches the
// sentinel regardless of whether the holder's metadata was readable.
func (e *LockHeldError) Unwrap() error { return ErrRuntimeLockHeld }

// AcquireRuntimeLock implements the PRD §26 / acquisition
// protocol for the global runtime lock at path:
//  1. open the lock file (creating it with mode 0o600 if absent),
//  2. attempt flock LOCK_EX | LOCK_NB via [TryLockExclusive],
//  3. on success, truncate + write the holder JSON
//     ([RuntimeLockInfo] populated from meta plus os.Getpid),
//  4. fsync to persist metadata before any state-changing work,
//  5. return a [RuntimeLockHandle] that holds the fd until
//     [RuntimeLockHandle.Release].
//
// On contention (another wdm process holds the lock),
// AcquireRuntimeLock returns a non-nil [*LockHeldError] wrapping
// [ErrRuntimeLockHeld]. The lock file is read best-effort so the
// returned error carries the holder's metadata; a missing or
// unparseable file surfaces an empty Holder rather than failing the
// contention report.
// All other syscall failures (open, flock, write, fsync) are wrapped
// with the "state.AcquireRuntimeLock:" prefix. On every failure path
// the file descriptor is closed before return; close errors are joined
// onto the primary via [errors.Join] so neither the primary cause nor
// a close failure is silently swallowed.
// Inode-identity verification (split-brain guard). After the
// non-blocking flock succeeds and BEFORE the holder metadata is
// written, the locked fd's inode is compared with the file currently at
// path via [os.SameFile]. This closes the window [ClearStaleRuntimeLock]
// opens: that writer is the first code in wdm that ever UNLINKS
// runtime.lock, so an acquirer can open the path, the clearer can then
// unlink that inode and release its own flock, and the acquirer's
// non-blocking flock can then SUCCEED on the now-orphaned inode — two
// processes would each believe they hold THE global lock (the exact
// PRD §26 violation). When the locked inode is no longer the file at
// path, the fd is closed (dropping the orphaned flock) and the whole
// open+flock sequence is retried ONCE against the fresh path; a second
// mismatch fails with the [*LockHeldError] busy refusal (the
// operationally honest answer under the non-blocking contract — try
// again). Per-stack .wdm.lock acquisition needs no such guard: it is
// never unlinked, so its inode
// is stable. [ProbeRuntimeLock]'s momentary shared flock defers the
// same check as read-only defense-in-depth — a probe never mutates and
// a stale read is harmless, so it tolerates the swap rather than
// retrying.
// ctx is honored at entry only — the syscalls below are local and fast,
// so per-step cancellation buys nothing and would obscure the failure
// mode. The path MUST be absolute; relative paths are rejected up front
// so callers cannot accidentally place runtime.lock at the process's
// working directory.
// Path expansion (~/ → $HOME) happens upstream in the engine per
// resolved.
func AcquireRuntimeLock(ctx context.Context, path string, meta RuntimeLockMetadata) (*RuntimeLockHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("state.AcquireRuntimeLock: %w", err)
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("state.AcquireRuntimeLock: path must be absolute, got %q", path)
	}
	if meta.Command == "" {
		return nil, fmt.Errorf("state.AcquireRuntimeLock: meta.Command is required")
	}
	if meta.WDMVersion == "" {
		return nil, fmt.Errorf("state.AcquireRuntimeLock: meta.WDMVersion is required")
	}

	// Bounded retry: at most two attempts. The second attempt exists
	// only for the case where the first locked a detached inode (a
	// concurrent clear unlinked the path in the open→flock window); a
	// second mismatch refuses busy rather than spinning under an unlink
	// storm.
	const maxAttempts = 2
	var lastErr error
	for range maxAttempts {
		handle, retry, err := acquireRuntimeLockOnce(path, meta)
		if err == nil {
			return handle, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
	}
	// Both attempts saw the locked inode detached: refuse busy with the
	// final attempt's report rather than spinning.
	return nil, lastErr
}

// acquireRuntimeLockOnce performs one open+flock+verify+write attempt of
// the runtime lock. It returns:
//   - (handle, false, nil) on success;
//   - (nil, true, err) when the locked inode was detached by a
//     concurrent clear — err is retry-eligible and the caller re-runs the
//     open+flock sequence on the fresh path;
//   - (nil, false, err) on a terminal failure (held by another
//     process, or a hard open/flock/write error).
//
// Splitting the attempt out of [AcquireRuntimeLock] keeps the retry loop
// trivial while reusing one body for both attempts, so the error texts
// and semantics stay byte-stable with the pre-retry implementation on
// every non-detached path.
func acquireRuntimeLockOnce(path string, meta RuntimeLockMetadata) (*RuntimeLockHandle, bool, error) {
	// G304 is suppressed here because path is engine-controlled, not
	// user-supplied: anchors runtime.lock at
	// ~/.local/state/wdm/runtime.lock (XDG-clean, expanded upstream)
	// and the absolute-path check above forecloses relative re-injection.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: engine-controlled XDG path, validated absolute
	if err != nil {
		return nil, false, fmt.Errorf("state.AcquireRuntimeLock: opening %q: %w", path, err)
	}

	got, err := TryLockExclusive(f)
	if err != nil {
		return nil, false, errors.Join(
			fmt.Errorf("state.AcquireRuntimeLock: %w", err),
			f.Close(),
		)
	}
	if !got {
		holder := readHolderBestEffort(f)
		return nil, false, errors.Join(
			&LockHeldError{Path: path, Holder: holder},
			f.Close(),
		)
	}

	// Inode-identity verification. The test seam fires between the flock
	// success and the verification so a test can unlink+recreate the
	// path at exactly the racy moment.
	afterRuntimeLockFlock()

	if !lockedInodeMatchesPath(f, path) {
		// The locked inode is no longer the file at path: a concurrent
		// clear unlinked/replaced it and we hold an orphaned flock. Drop
		// it and signal a retry against the fresh path. The Holder is
		// left zero — there is no live holder of THIS detached inode to
		// report; a second mismatch surfaces this same busy report.
		return nil, true, errors.Join(
			&LockHeldError{Path: path},
			f.Close(),
		)
	}

	pid := os.Getpid()
	info := RuntimeLockInfo{
		SchemaVersion: runtimeLockSchemaVersion,
		PID:           pid,
		Command:       meta.Command,
		StartedAt:     time.Now().UTC(),
		WDMVersion:    meta.WDMVersion,
	}
	// Capture the holder's start-time best-effort: a read failure
	// (no /proc on this platform, or a transient permission/parse
	// error) leaves StartedTime zero, which the probe treats as
	// "no PID-reuse signal" and falls back to signal-0 liveness.
	if started, ok := readProcStartTime(pid); ok {
		info.StartedTime = started
	}
	if err := writeRuntimeLock(f, info); err != nil {
		return nil, false, errors.Join(
			fmt.Errorf("state.AcquireRuntimeLock: writing %q: %w", path, err),
			f.Close(),
		)
	}

	return &RuntimeLockHandle{path: path, file: f, info: info}, false, nil
}

// lockedInodeMatchesPath reports whether the inode behind the locked fd
// f is still the file currently at path. It fstats the locked fd and
// lstats the path; if either stat fails (the path was unlinked, so the
// lstat returns ENOENT) or the two name different inodes, the locked
// flock is on a detached inode a concurrent [ClearStaleRuntimeLock]
// removed. [os.Lstat] (not Stat) is used so a symlink swapped in at path
// is treated as a mismatch rather than followed.
func lockedInodeMatchesPath(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	li, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return os.SameFile(fi, li)
}

// afterRuntimeLockFlock is the package-private test seam invoked between
// the flock success and the inode-identity verification in
// [acquireRuntimeLockOnce]. Production points it at a no-op; tests swap
// it via export_test.go's SwapAfterRuntimeLockFlockForTest to unlink and
// recreate the lock path at exactly the racy moment, deterministically
// driving the split-brain inode-swap path.
var afterRuntimeLockFlock = func() {}

// readHolderBestEffort reads the runtime.lock JSON from f and returns
// a populated [RuntimeLockInfo] on success. Any read or parse failure
// yields the zero RuntimeLockInfo — the contention error path uses the
// zero value to signal "holder metadata unavailable" without crashing
// the diagnostic message.
// Best-effort by design: the caller already knows acquisition failed,
// and the holder's metadata is purely informational for the hint.
// Surfacing read errors here would only add noise to the primary
// "lock is held" signal.
func readHolderBestEffort(f *os.File) RuntimeLockInfo {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return RuntimeLockInfo{}
	}
	raw, err := io.ReadAll(f)
	if err != nil || len(raw) == 0 {
		return RuntimeLockInfo{}
	}
	var info RuntimeLockInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return RuntimeLockInfo{}
	}
	return info
}

// RuntimeLockProbe is the read-only observation [ProbeRuntimeLock]
// collects about the global runtime.lock so PRD §18's "a stale
// runtime lock affects the app" condition and PRD §26's "Detect stale
// locks where practical" clause can be evaluated without acquiring or
// mutating the lock.
type RuntimeLockProbe struct {
	// Exists reports whether the runtime.lock file is present on
	// disk. A released lock leaves its file behind by design
	// ([RuntimeLockHandle.Release] does not remove it), so existence
	// alone carries no held-or-stale signal.
	Exists bool

	// Held reports whether another file description currently holds
	// the exclusive flock — i.e. a state-changing operation appears
	// to be in flight. Because the kernel releases flocks when the
	// holder's last fd closes (including on process death), Held is
	// the live signal; a crashed holder never leaves Held true.
	Held bool

	// HolderAlive reports whether the recorded holder process is
	// still running. The base signal is the kill(pid, 0) liveness
	// probe; when the lock file ALSO recorded the holder's
	// start-time ([RuntimeLockInfo.StartedTime]) and the PID's live
	// start-time can be read, a MISMATCH means the PID was recycled
	// onto a different process and HolderAlive is false even though
	// signal-0 alone would have said alive.
	// Fail-safe direction: every degradation errs toward ALIVE so a
	// genuinely live lock is never misclassified as stale
	// #45's forbidden-to-weaken "a live lock is never clearable").
	// When the recorded start-time is absent (old lock file) or the
	// live start-time cannot be read (permissions, no /proc), the
	// signal-0 answer stands. Meaningful only when Held is true;
	// false when the holder metadata was unreadable or carried no
	// PID.
	HolderAlive bool

	// Holder is the on-disk metadata read best-effort from the lock
	// file. Zero value when the file was empty, truncated, or
	// unparseable — mirroring [LockHeldError.Holder] semantics.
	Holder RuntimeLockInfo

	// ModTime is the lock file's modification time, captured as the
	// staleness-age fallback when Holder.StartedAt is unavailable
	// Zero when stat failed.
	ModTime time.Time
}

// ProbeRuntimeLock observes the runtime.lock at path without blocking
// and without mutating it: the file is opened O_RDONLY (never
// created), held-ness is tested with a non-blocking SHARED flock via
// [TryLockShared], and holder metadata is read best-effort.
// A shared probe succeeds exactly when no writer holds LOCK_EX, so a
// held probe means a state-changing operation is (or appears) in
// flight. The probe's own shared lock exists only for the microseconds
// between flock and close; PRD §26's "read-only commands … only when
// they cannot conflict with the active operation" is honored because
// writers acquire the lock once at operation start and the probe never
// waits, never writes, and never removes the file. Stale-lock recovery is a
// separate clear operation.
// A missing file returns a zero [RuntimeLockProbe] with a nil error —
// no lock file means no lock to be stale. path MUST be absolute; ctx
// is honored at entry only.
func ProbeRuntimeLock(ctx context.Context, path string) (RuntimeLockProbe, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeLockProbe{}, fmt.Errorf("state.ProbeRuntimeLock: %w", err)
	}
	if path == "" || !filepath.IsAbs(path) {
		return RuntimeLockProbe{}, fmt.Errorf("state.ProbeRuntimeLock: path must be absolute, got %q", path)
	}

	// G304 is suppressed: path is the engine-controlled XDG
	// runtime.lock location, validated absolute above. Same rationale
	// as AcquireRuntimeLock.
	f, err := os.OpenFile(path, os.O_RDONLY, 0) //nolint:gosec // G304: engine-controlled XDG path, validated absolute
	if errors.Is(err, os.ErrNotExist) {
		return RuntimeLockProbe{}, nil
	}
	if err != nil {
		return RuntimeLockProbe{}, fmt.Errorf("state.ProbeRuntimeLock: opening %q: %w", path, err)
	}

	got, err := TryLockShared(f)
	if err != nil {
		return RuntimeLockProbe{}, errors.Join(
			fmt.Errorf("state.ProbeRuntimeLock: %w", err),
			f.Close(),
		)
	}

	probe := RuntimeLockProbe{Exists: true, Held: !got}
	if probe.Held {
		probe.Holder = readHolderBestEffort(f)
		probe.HolderAlive = holderStillAlive(probe.Holder)
		if info, statErr := f.Stat(); statErr == nil {
			probe.ModTime = info.ModTime()
		}
	}

	// Closing the fd releases the probe's shared lock (when it was
	// acquired); the file stays on disk untouched.
	if err := f.Close(); err != nil {
		return RuntimeLockProbe{}, fmt.Errorf("state.ProbeRuntimeLock: closing %q: %w", path, err)
	}
	return probe, nil
}

// processAlive reports whether pid names a currently running process,
// using the conventional kill(pid, 0) liveness probe. EPERM means the
// process exists but belongs to another user — alive for staleness
// purposes. Non-positive PIDs are never probed: 0 and negatives
// address process groups, and the zero value means "metadata
// unreadable".
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// holderStillAlive fuses the signal-0 liveness probe with the
// start-time PID-reuse check. It returns true when
// the recorded holder is genuinely still running and false when the
// holder is gone OR its PID has been recycled onto a different process.
// Decision logic, fail-safe biased toward ALIVE so a live lock is
// never misclassified as stale:
//   - signal-0 says the PID is dead → return false (the holder is gone,
//     regardless of any recorded start-time).
//   - signal-0 says the PID is alive AND the lock recorded a start-time
//     AND the PID's live start-time reads successfully AND the two
//     DIFFER → return false (the PID was recycled; the original holder
//     is gone).
//   - any other case — no recorded start-time, the live read fails, or
//     the two match → return the signal-0 answer (alive). Degradation
//     never flips a live-looking holder to stale.
func holderStillAlive(holder RuntimeLockInfo) bool {
	if !processAlive(holder.PID) {
		return false
	}
	if holder.StartedTime == 0 {
		// Old lock file or non-Linux acquisition: no disambiguation
		// signal recorded. Trust signal-0.
		return true
	}
	live, ok := readProcStartTime(holder.PID)
	if !ok {
		// Cannot read the live start-time (no /proc, permissions, a
		// transient parse failure). Fail toward ALIVE.
		return true
	}
	return live == holder.StartedTime
}

// readProcStartTime is the package-private seam for reading a process's
// kernel start-time. Production points it at [readProcessStartTimeFromProc];
// tests swap it via export_test.go's SwapProcessStartTimeReaderForTest so
// the PID-reuse logic is exercised deterministically on platforms with no
// /proc (this Mac) as well as on Linux.
var readProcStartTime = readProcessStartTimeFromProc

// readProcessStartTimeFromProc reads /proc/<pid>/stat and returns the
// process's start-time (the 22nd field, "starttime" — clock ticks since
// boot) on Linux. It returns (0, false) on any failure, which the callers
// treat as "no PID-reuse signal available" and resolve toward ALIVE.
// On platforms without /proc (darwin and friends) the open simply fails
// and the function returns (0, false), so acquisition omits the field and
// the probe degrades to signal-0 liveness — no build tags required.
// The parse is hardened against a hostile comm (field 2): the comm is
// wrapped in parentheses and may itself contain spaces and parentheses
// (a process can rename itself to e.g. "(a b) c)"). The robust, kernel-
// blessed approach is to locate the LAST ')' in the line — everything
// after it is the fixed-layout remainder where starttime is the 20th
// whitespace-separated field (field 22 minus the pid and comm fields
// that precede the closing paren).
func readProcessStartTimeFromProc(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	// The path is built from a validated positive integer PID, not
	// user-supplied text, and names a kernel-synthetic file under /proc.
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	return parseProcStat(string(raw))
}

// parseProcStat extracts the starttime field (field 22) from a single
// /proc/<pid>/stat line. It is split out from the file read so the
// hostile-comm handling can be tested with a fixture line directly.
// Returns (0, false) when the line is too short or starttime is not a
// valid unsigned integer.
// The comm field (field 2) is wrapped in parentheses and may itself
// contain whitespace and parentheses, so the layout before the comm is
// not whitespace-stable. The robust parse locates the LAST ')' — every
// field after it is fixed-position — and reads the 20th of those
// whitespace-separated fields (field 22 minus the two fields, pid and
// comm, that close at or before the final paren).
func parseProcStat(stat string) (uint64, bool) {
	lastParen := strings.LastIndexByte(stat, ')')
	if lastParen < 0 || lastParen+1 >= len(stat) {
		return 0, false
	}

	// Fields after the comm's closing paren, in /proc(5) order, are:
	//   index 0 → field 3 (state)
	//   index 1 → field 4 (ppid)
	//   index 19 → field 22 (starttime)
	fields := strings.Fields(stat[lastParen+1:])
	const starttimeIndexAfterComm = 19
	if len(fields) <= starttimeIndexAfterComm {
		return 0, false
	}

	started, err := strconv.ParseUint(fields[starttimeIndexAfterComm], 10, 64)
	if err != nil {
		return 0, false
	}
	return started, true
}

// writeRuntimeLock implements steps 4–5 of 's atomicity
// protocol: truncate the file to zero length, rewrite from offset 0
// with the marshaled info, then fsync. The file descriptor is left
// open so the caller can keep holding the flock; closing remains
// [RuntimeLockHandle.Release]'s responsibility.
// Truncate runs before Seek because [os.File.Truncate] adjusts the
// file's size but does NOT move the write offset — a Truncate without
// a subsequent Seek would leave the offset wherever the prior reader
// left it and write at that position, producing a sparse file.
func writeRuntimeLock(f *os.File, info RuntimeLockInfo) error {
	raw, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	return nil
}
