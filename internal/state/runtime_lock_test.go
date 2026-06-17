//go:build unix

package state_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/state"
)

// helperEnvVar gates the subprocess-only helper test
// [TestHelperHoldsRuntimeLock]. When unset (every normal `go test`
// run) the helper test skips immediately; when set to "1" by the
// subprocess-spawning parent test, the helper acquires the lock at
// helperPathEnvVar and holds it via a sleep until the parent kills
// it.
const (
	helperEnvVar     = "WDM_LOCK_HELPER"
	helperPathEnvVar = "WDM_LOCK_HELPER_PATH"
	helperSignalEnv  = "WDM_LOCK_HELPER_SIGNAL"
	helperCommand    = "test-helper"
	helperVersion    = "dev"
	helperHoldFor    = 30 * time.Second
)

// TestHelperHoldsRuntimeLock is the subprocess-only helper that
// backs both [TestAcquireRuntimeLock_Contention] and
// [TestTryLockExclusive_FalseWhileHeldByOtherProcess]. The test is
// gated on WDM_LOCK_HELPER=1 so normal `go test./...` runs skip
// it; the parent test re-invokes the test binary with the env var
// set and the path / signal paths threaded through.
// The helper writes a sentinel file at WDM_LOCK_HELPER_SIGNAL
// once acquisition succeeds — the parent polls for that file before
// running its contention assertion, so the test never races against
// the helper's startup.
func TestHelperHoldsRuntimeLock(t *testing.T) {
	if os.Getenv(helperEnvVar) != "1" {
		t.Skip("helper-only test; gated on " + helperEnvVar + "=1")
	}

	path := os.Getenv(helperPathEnvVar)
	signal := os.Getenv(helperSignalEnv)
	require.NotEmpty(t, path, helperPathEnvVar+" must be set when helper runs")
	require.NotEmpty(t, signal, helperSignalEnv+" must be set when helper runs")

	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    helperCommand,
		WDMVersion: helperVersion,
	})
	require.NoError(t, err, "helper subprocess failed to acquire the runtime lock")
	defer handle.Release()

	// Signal acquisition to the parent before sleeping. The parent
	// polls for this file and only then runs its contention check.
	require.NoError(t, os.WriteFile(signal, []byte("ok"), 0o600))

	// Hold the lock long enough for the parent's assertions; the
	// parent SIGKILLs the subprocess once done, which closes the fd
	// and releases the flock at the kernel level. If the parent
	// never kills us (e.g. it crashed), the sleep cap below ensures
	// the helper does not leak forever.
	time.Sleep(helperHoldFor)
}

// startRuntimeLockHelper spawns the helper subprocess and waits for
// it to signal lock acquisition. Returns the lock path the helper is
// holding and the *exec.Cmd handle (registered for cleanup so the
// subprocess is reaped at test end). Used by both
// TestAcquireRuntimeLock_Contention and
// TestTryLockExclusive_FalseWhileHeldByOtherProcess.
func startRuntimeLockHelper(t *testing.T) (string, *exec.Cmd) {
	t.Helper()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "runtime.lock")
	signal := filepath.Join(dir, "acquired.signal")

	// -test.run anchored on TestHelperHoldsRuntimeLock so the
	// subprocess runs only that test (and any same-package helpers).
	// -test.timeout 60s caps subprocess wall time at twice the
	// helper's hold duration so a crashed parent does not leave a
	// stuck child longer than that.
	cmd := exec.Command(
		os.Args[0],
		"-test.run", "^TestHelperHoldsRuntimeLock$",
		"-test.timeout", "60s",
	)
	cmd.Env = append(os.Environ(),
		helperEnvVar+"=1",
		helperPathEnvVar+"="+lockPath,
		helperSignalEnv+"="+signal,
	)
	// Pipe child output to stderr so a helper failure surfaces in
	// CI logs adjacent to the parent's test output.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	require.NoError(t, cmd.Start(), "starting helper subprocess")
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	// Poll for the acquisition signal. 5 seconds is generous —
	// AcquireRuntimeLock is sub-millisecond on a tmpfs tempdir even
	// under -race instrumentation.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(signal); err == nil {
			return lockPath, cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("helper subprocess did not signal lock acquisition within 5s at %q", signal)
	return "", nil // unreachable; placates the compiler
}

func TestAcquireRuntimeLock_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	_, err := state.AcquireRuntimeLock(t.Context(), "relative.lock", state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "dev",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestAcquireRuntimeLock_RejectsEmptyCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := state.AcquireRuntimeLock(t.Context(), filepath.Join(dir, "runtime.lock"), state.RuntimeLockMetadata{
		Command:    "",
		WDMVersion: "dev",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Command")
}

func TestAcquireRuntimeLock_RejectsEmptyVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := state.AcquireRuntimeLock(t.Context(), filepath.Join(dir, "runtime.lock"), state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WDMVersion")
}

// TestAcquireRuntimeLock_OpenErrorOnMissingParent covers the
// os.OpenFile error wrap by pointing path at a file whose parent
// directory does not exist. O_CREATE creates the file but does NOT
// create parent directories — the syscall fails with ENOENT, which
// must surface wrapped by the "state.AcquireRuntimeLock: opening %q"
// prefix. Cheapest realistic trigger of this wrap with no fixtures
// (no chmod, no read-only mount).
func TestAcquireRuntimeLock_OpenErrorOnMissingParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// dir exists; "missing-subdir" does not.
	path := filepath.Join(dir, "missing-subdir", "runtime.lock")

	_, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "dev",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opening")
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"missing-parent failure must wrap os.ErrNotExist via the *PathError chain; got %v", err)
}

func TestAcquireRuntimeLock_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := state.AcquireRuntimeLock(ctx, filepath.Join(dir, "runtime.lock"), state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "dev",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestAcquireRuntimeLock_HappyPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "runtime.lock")

	handle, err := state.AcquireRuntimeLock(t.Context(), lockPath, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	require.NotNil(t, handle)
	defer handle.Release()

	assert.Equal(t, lockPath, handle.Path())

	info := handle.Info()
	assert.Equal(t, 1, info.SchemaVersion)
	assert.Equal(t, "install", info.Command)
	assert.Equal(t, "0.1.0", info.WDMVersion)
	assert.Equal(t, os.Getpid(), info.PID)
	assert.False(t, info.StartedAt.IsZero(), "StartedAt must be populated at acquisition")

	// File created with 0o600 perms.
	st, err := os.Stat(lockPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

func TestRuntimeLockHandle_Release_Idempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "runtime.lock")

	handle, err := state.AcquireRuntimeLock(t.Context(), lockPath, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "dev",
	})
	require.NoError(t, err)

	// First release must succeed and close the underlying fd.
	require.NoError(t, handle.Release())

	// Second release is a no-op (file pointer is nil after first
	// close); idempotent so callers can safely defer Release after a
	// manual Release earlier in the function.
	assert.NoError(t, handle.Release())
}

func TestRuntimeLockHandle_NilReceiverIsSafe(t *testing.T) {
	t.Parallel()

	var h *state.RuntimeLockHandle
	assert.Equal(t, "", h.Path())
	assert.Equal(t, state.RuntimeLockInfo{}, h.Info())
	assert.NoError(t, h.Release())
}

func TestRuntimeLockHandle_ReacquireAfterRelease(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "runtime.lock")

	first, err := state.AcquireRuntimeLock(t.Context(), lockPath, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "dev",
	})
	require.NoError(t, err)
	require.NoError(t, first.Release())

	// After Release the same process must be able to re-acquire.
	// PRD §26 expects clean release on exit; this test asserts the
	// in-process equivalent.
	second, err := state.AcquireRuntimeLock(t.Context(), lockPath, state.RuntimeLockMetadata{
		Command:    "update",
		WDMVersion: "dev",
	})
	require.NoError(t, err)
	defer second.Release()

	// The re-acquisition overwrites the holder metadata.
	assert.Equal(t, "update", second.Info().Command)
}

// TestAcquireRuntimeLock_Contention is the cross-process exclusion
// test for the global runtime lock. The helper subprocess holds the
// lock with command="test-helper"; the parent then attempts to
// acquire the same path and must receive [*LockHeldError] wrapping
// [ErrRuntimeLockHeld], with [LockHeldError.Holder] populated from
// the subprocess's on-disk metadata.
// This is the load-bearing PRD §26 invariant. flock(2) is per-OFD on
// Linux, so the only correct test of cross-process exclusion is to
// hold the lock from another process — a same-process goroutine
// competition would silently pass even if exclusion were broken.
func TestAcquireRuntimeLock_Contention(t *testing.T) {
	t.Parallel()

	lockPath, _ := startRuntimeLockHelper(t)

	_, err := state.AcquireRuntimeLock(t.Context(), lockPath, state.RuntimeLockMetadata{
		Command:    "test-parent",
		WDMVersion: "dev",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, state.ErrRuntimeLockHeld),
		"want errors.Is(err, ErrRuntimeLockHeld); got %v", err)

	var heldErr *state.LockHeldError
	require.True(t, errors.As(err, &heldErr), "want *LockHeldError in chain; got %v", err)
	assert.Equal(t, lockPath, heldErr.Path)
	assert.Equal(t, helperCommand, heldErr.Holder.Command)
	assert.Equal(t, helperVersion, heldErr.Holder.WDMVersion)
	assert.NotZero(t, heldErr.Holder.PID, "Holder.PID must reflect the subprocess pid")
	assert.NotZero(t, heldErr.Holder.StartedAt, "Holder.StartedAt must round-trip from disk")
	assert.Equal(t, 1, heldErr.Holder.SchemaVersion)
}

func TestLockHeldError_ErrorString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *state.LockHeldError
		want string // substring check, not exact (PID is dynamic)
	}{
		{
			"nil",
			nil,
			"<nil>",
		},
		{
			"empty_holder",
			&state.LockHeldError{Path: "/var/runtime.lock", Holder: state.RuntimeLockInfo{}},
			"metadata unreadable",
		},
		{
			"populated_holder",
			&state.LockHeldError{
				Path: "/var/runtime.lock",
				Holder: state.RuntimeLockInfo{
					Command:    "install",
					PID:        12345,
					StartedAt:  time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC),
					WDMVersion: "dev",
				},
			},
			`held by "install"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Contains(t, tc.err.Error(), tc.want)
		})
	}
}

func TestLockHeldError_Unwrap_ReturnsSentinel(t *testing.T) {
	t.Parallel()

	heldErr := &state.LockHeldError{Path: "/a", Holder: state.RuntimeLockInfo{}}
	assert.Same(t, state.ErrRuntimeLockHeld, heldErr.Unwrap())

	// And via errors.Is — the canonical detection contract for
	// cmd/wdm's exit-code routing to PRD §27.
	wrapped := errors.Join(errors.New("outer"), heldErr)
	assert.True(t, errors.Is(wrapped, state.ErrRuntimeLockHeld))
}

func TestProbeRuntimeLock_MissingFileIsZeroProbe(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "runtime.lock")

	probe, err := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, err)
	assert.False(t, probe.Exists)
	assert.False(t, probe.Held)
	assert.False(t, probe.HolderAlive)

	// Read-only contract: probing must never create the lock file.
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "ProbeRuntimeLock must not create the lock file")
}

func TestProbeRuntimeLock_UnheldLeftoverFileIsNotHeld(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "runtime.lock")
	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	require.NoError(t, handle.Release())

	probe, probeErr := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, probeErr)
	assert.True(t, probe.Exists)
	assert.False(t, probe.Held, "a released lock's leftover file must not read as held")
}

// TestProbeRuntimeLock_HeldByLiveHolder observes a held lock through
// a second open file description (flock(2) locks belong to the open
// file description, so the probe sees the holder exactly the way
// another process would) and verifies the holder metadata, liveness,
// and mtime capture.
func TestProbeRuntimeLock_HeldByLiveHolder(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "runtime.lock")
	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "update",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	defer handle.Release()

	probe, probeErr := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, probeErr)
	assert.True(t, probe.Exists)
	assert.True(t, probe.Held)
	assert.True(t, probe.HolderAlive, "the holding process is this test binary and is alive")
	assert.Equal(t, "update", probe.Holder.Command)
	assert.Equal(t, os.Getpid(), probe.Holder.PID)
	assert.False(t, probe.ModTime.IsZero())

	// The probe must leave the holder's flock intact: a second probe
	// still observes Held.
	probe, probeErr = state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, probeErr)
	assert.True(t, probe.Held)
}

// TestProbeRuntimeLock_HeldWithUnreadableMetadata degrades the same
// way [LockHeldError] does: held-ness is still reported while the
// holder stays the zero value, and a nonexistent PID reads as not
// alive.
func TestProbeRuntimeLock_HeldWithUnreadableMetadata(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "runtime.lock")
	require.NoError(t, os.WriteFile(path, []byte("{truncated"), 0o600))

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	got, err := state.TryLockExclusive(f)
	require.NoError(t, err)
	require.True(t, got)

	probe, probeErr := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, probeErr)
	assert.True(t, probe.Held)
	assert.False(t, probe.HolderAlive, "zero holder metadata must not read as a live process")
	assert.Zero(t, probe.Holder.PID)
}

func TestProbeRuntimeLock_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	_, err := state.ProbeRuntimeLock(t.Context(), "relative/runtime.lock")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")
}

func TestProbeRuntimeLock_HonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := state.ProbeRuntimeLock(ctx, filepath.Join(t.TempDir(), "runtime.lock"))
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// TestParseProcStat_HostileComm pins the /proc/<pid>/stat field parse
// against a comm name (field 2) that itself contains spaces and a
// closing parenthesis — a process can rename itself to "(a b) c)". The
// canonical parse locates the LAST ')' and reads starttime as the 20th
// whitespace-separated field after it (field 22 minus pid and comm).
func TestParseProcStat_HostileComm(t *testing.T) {
	t.Parallel()

	// 22-field stat line. Field 1 = pid, field 2 = comm in parens
	// (hostile: contains spaces and an embedded ')'), field 3 = state,
	// fields 4..21 are filler, field 22 = starttime (the value under
	// test). The fields after the LAST ')' must be exactly:
	//   S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 998877
	// i.e. starttime is the 20th token (index 19) after the close.
	const starttime = uint64(998877)
	cases := []struct {
		name string
		line string
		want uint64
		ok   bool
	}{
		{
			name: "hostile_comm_with_spaces_and_paren",
			line: "4321 (a b) c) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 998877 0 0 0\n",
			want: starttime,
			ok:   true,
		},
		{
			name: "ordinary_comm",
			line: "1234 (bash) S 1000 1234 1000 0 -1 4194304 100 0 0 0 1 2 0 0 20 0 1 0 998877 1000 0\n",
			want: starttime,
			ok:   true,
		},
		{
			name: "comm_only_no_trailing_fields",
			line: "1234 (bash)\n",
			ok:   false,
		},
		{
			name: "no_closing_paren",
			line: "1234 (bash S 1 2 3",
			ok:   false,
		},
		{
			name: "starttime_not_a_number",
			line: "4321 (proc) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 notanumber 0 0\n",
			ok:   false,
		},
		{
			name: "too_few_fields_after_paren",
			line: "4321 (proc) S 1 2 3\n",
			ok:   false,
		},
		{
			name: "empty",
			line: "",
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := state.ParseProcStatForTest(tc.line)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.want, got)
			} else {
				assert.Zero(t, got)
			}
		})
	}
}

// TestAcquireRuntimeLock_RecordsStartTime proves acquisition writes the
// holder's process start-time into the lock metadata when the /proc
// reader yields one. The seam makes this deterministic on every
// platform: the suite is green on darwin (no /proc) and Linux alike.
func TestAcquireRuntimeLock_RecordsStartTime(t *testing.T) {
	const wantStart = uint64(424242)
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return wantStart, true
	})

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "runtime.lock")

	handle, err := state.AcquireRuntimeLock(t.Context(), lockPath, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	defer handle.Release()

	assert.Equal(t, wantStart, handle.Info().StartedTime)

	// The field round-trips to disk so a probe (or a contention report)
	// in another process can read it back.
	probe, probeErr := state.ProbeRuntimeLock(t.Context(), lockPath)
	require.NoError(t, probeErr)
	assert.Equal(t, wantStart, probe.Holder.StartedTime)
}

// TestAcquireRuntimeLock_StartTimeUnavailableOmitsField proves the field
// degrades gracefully: when the /proc reader fails (the darwin reality,
// or a transient Linux read error), acquisition still succeeds and the
// metadata simply carries no start-time. The on-disk JSON omits the key
// (omitempty), keeping old-schema parity.
func TestAcquireRuntimeLock_StartTimeUnavailableOmitsField(t *testing.T) {
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return 0, false
	})

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "runtime.lock")

	handle, err := state.AcquireRuntimeLock(t.Context(), lockPath, state.RuntimeLockMetadata{
		Command:    "install",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	defer handle.Release()

	assert.Zero(t, handle.Info().StartedTime)

	raw, readErr := os.ReadFile(lockPath)
	require.NoError(t, readErr)
	assert.NotContains(t, string(raw), "started_time",
		"an unavailable start-time must be omitted from the on-disk JSON")
}

// TestProbeRuntimeLock_OldSchemaDegradesToSignalZero proves that a lock
// file predating the start-time field (no started_time key) still
// classifies a live holder as alive: with no recorded start-time the
// probe falls back to the signal-0 liveness answer. The held lock is
// this test binary, so signal-0 reports alive.
func TestProbeRuntimeLock_OldSchemaDegradesToSignalZero(t *testing.T) {
	// Force acquisition to record NO start-time, simulating an
	// old-schema lock file.
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return 0, false
	})

	path := filepath.Join(t.TempDir(), "runtime.lock")
	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "update",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	defer handle.Release()

	probe, probeErr := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, probeErr)
	require.True(t, probe.Held)
	assert.Zero(t, probe.Holder.StartedTime, "old-schema fixture must carry no start-time")
	assert.True(t, probe.HolderAlive,
		"no recorded start-time must degrade to the signal-0 alive answer")
}

// TestProbeRuntimeLock_MatchingStartTimeIsAlive proves the happy path of
// the PID-reuse check: the recorded start-time equals the live process's
// start-time, so the holder is the original process and classifies alive.
func TestProbeRuntimeLock_MatchingStartTimeIsAlive(t *testing.T) {
	const start = uint64(555000)
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return start, true
	})

	path := filepath.Join(t.TempDir(), "runtime.lock")
	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "update",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	defer handle.Release()

	probe, probeErr := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, probeErr)
	require.True(t, probe.Held)
	assert.Equal(t, start, probe.Holder.StartedTime)
	assert.True(t, probe.HolderAlive,
		"a live holder whose start-time matches must classify alive")
}

// TestProbeRuntimeLock_RecycledPIDIsNotAlive is the core PID-reuse
// safety check: signal-0 reports the PID alive (it is this test binary),
// but the live start-time differs from the value recorded at
// acquisition, so the original holder is gone and HolderAlive is false.
// The simulation: acquisition records start-time A; the probe's live
// reader returns start-time B (A != B), modeling a PID recycled onto a
// different process between acquisition and probe.
func TestProbeRuntimeLock_RecycledPIDIsNotAlive(t *testing.T) {
	const recordedStart = uint64(100000)

	// Acquisition records recordedStart.
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return recordedStart, true
	})

	path := filepath.Join(t.TempDir(), "runtime.lock")
	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "update",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	defer handle.Release()

	// The probe's live reader now returns a DIFFERENT start-time,
	// simulating PID reuse. Re-swap (the cleanup chain restores the
	// production reader after the test).
	const liveStart = uint64(200000)
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return liveStart, true
	})

	probe, probeErr := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, probeErr)
	require.True(t, probe.Held, "a recycled PID still answers signal-0, so the flock check still sees the file")
	assert.Equal(t, recordedStart, probe.Holder.StartedTime)
	assert.False(t, probe.HolderAlive,
		"a recorded start-time that differs from the live process means the PID was recycled — NOT alive")
}

// TestProbeRuntimeLock_StartTimeReadFailureIsAlive pins the fail-safe
// direction: signal-0 says the PID is alive, the lock recorded a
// start-time, but the LIVE start-time read fails (permissions, /proc
// gone). The probe must err toward ALIVE so a genuinely live lock is
// never misclassified as stale (the invariant's forbidden-to-weaken
// "a live lock is never clearable").
func TestProbeRuntimeLock_StartTimeReadFailureIsAlive(t *testing.T) {
	const recordedStart = uint64(777777)

	// Acquisition records a start-time.
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return recordedStart, true
	})

	path := filepath.Join(t.TempDir(), "runtime.lock")
	handle, err := state.AcquireRuntimeLock(t.Context(), path, state.RuntimeLockMetadata{
		Command:    "update",
		WDMVersion: "0.1.0",
	})
	require.NoError(t, err)
	defer handle.Release()

	// The probe's live reader now fails. Signal-0 still says alive
	// (this test binary). Fail-safe must keep HolderAlive true.
	state.SwapProcessStartTimeReaderForTest(t, func(int) (uint64, bool) {
		return 0, false
	})

	probe, probeErr := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, probeErr)
	require.True(t, probe.Held)
	assert.True(t, probe.HolderAlive,
		"a failed live start-time read must fall back to the signal-0 alive answer (fail toward ALIVE)")
}

// TestProbeRuntimeLock_DeadPIDIsNotAliveRegardlessOfStartTime proves the
// start-time comparison never overrides a dead signal-0: a PID that is
// not running classifies not-alive even when a start-time was recorded.
// The unreadable-metadata fixture carries PID 0, which processAlive
// rejects up front, so this also exercises the "no live PID" arm.
func TestProbeRuntimeLock_DeadPIDIsNotAliveRegardlessOfStartTime(t *testing.T) {
	t.Parallel()

	// A held lock whose on-disk metadata is unparseable → zero Holder
	// (PID 0). signal-0 never probes PID 0, so it is not alive, and the
	// start-time branch is never consulted.
	path := filepath.Join(t.TempDir(), "runtime.lock")
	require.NoError(t, os.WriteFile(path, []byte("{truncated"), 0o600))

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	got, err := state.TryLockExclusive(f)
	require.NoError(t, err)
	require.True(t, got)

	probe, probeErr := state.ProbeRuntimeLock(t.Context(), path)
	require.NoError(t, probeErr)
	assert.True(t, probe.Held)
	assert.False(t, probe.HolderAlive)
	assert.Zero(t, probe.Holder.PID)
}
