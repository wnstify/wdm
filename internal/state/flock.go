//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// LockExclusive acquires an exclusive flock on f, blocking until the
// lock is available. The lock is released when the underlying file
// descriptor is closed (typically via [os.File.Close] or [Unlock]);
// the kernel also releases it on process exit, which is the safety
// net PRD §26 relies on ("Always release the runtime lock on clean
// exit").
// flock(2) locks are advisory and belong to the open file
// description: dup'd or inherited descriptors share one lock, but
// independently opened descriptors to the same file conflict even
// inside one process — on Linux and darwin alike — so a second
// LOCK_EX through a fresh open of the same path blocks until the
// first is released. In-process coordination is the caller's
// responsibility; cross-process exclusion is the property this helper
// provides.
// LockExclusive serves the per-stack .wdm.lock acquisition path
// where blocking on a busy stack is acceptable. The
// runtime.lock takes the non-blocking path via [TryLockExclusive].
func LockExclusive(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("state.LockExclusive: flock: %w", err)
	}
	return nil
}

// TryLockExclusive attempts to acquire an exclusive flock on f without
// blocking. It returns (true, nil) on success, (false, nil) if another
// process holds the lock, and (false, err) on any other failure
// (closed fd, EBADF, EINVAL).
// The "held by another process" outcome is distinguished from a true
// syscall failure by matching syscall.EWOULDBLOCK — the value Linux
// returns when LOCK_NB is set and the lock is held — so callers receive
// a clean boolean rather than interpreting errno themselves. On Linux
// EAGAIN and EWOULDBLOCK share the same numeric value, so the
// [errors.Is] match covers both spellings.
func TryLockExclusive(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, fmt.Errorf("state.TryLockExclusive: flock: %w", err)
}

// TryLockShared attempts to acquire a shared flock on f without
// blocking. It returns (true, nil) on success, (false, nil) if another
// file description holds an exclusive lock, and (false, err) on any
// other failure (closed fd, EBADF, EINVAL).
// TryLockShared exists for read-only observers (PRD §26 "Allow
// read-only commands, such as status checks, only when they cannot
// conflict with the active operation"): a shared lock succeeds exactly
// when no writer holds LOCK_EX, and concurrent shared holders do not
// contend with each other, so two status checks never produce a
// spurious "operation in progress" signal for one another. The errno
// interpretation matches [TryLockExclusive].
func TryLockShared(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, fmt.Errorf("state.TryLockShared: flock: %w", err)
}

// Unlock explicitly releases the flock on f. Most callers should rely
// on [os.File.Close] called from a defer — closing the last fd
// referring to the file releases the flock, and that is the
// kernel-level guarantee PRD §26's "Always release on clean exit"
// depends on.
// Unlock exists for the rare case where the caller needs to release
// the lock while keeping the file open (e.g. to perform further
// non-exclusive reads without holding cross-process exclusion).
func Unlock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("state.Unlock: flock unlock: %w", err)
	}
	return nil
}
