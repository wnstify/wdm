package system

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRefuseRootOrSudo_PureHelper_TableDriven exercises the
// unexported pure-function core of [RefuseRootOrSudo]. The split
// between [RefuseRootOrSudo] (live os.Geteuid + os.Getenv) and
// [refuseRootOrSudo] (pure (euid, sudoUser)) is what makes both
// branches testable without needing root privileges or environment
// mutation — see the audit-log entry for the rationale.
// All cases run in parallel because the helper takes its inputs as
// arguments and touches no globals.
func TestRefuseRootOrSudo_PureHelper_TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		euid      int
		sudoUser  string
		wantNil   bool
		wantIsErr error
	}{
		// Root branch wins regardless of $SUDO_USER state.
		{"euid_zero_no_sudo", 0, "", false, ErrRunningAsRoot},
		{"euid_zero_with_sudo", 0, "alice", false, ErrRunningAsRoot},
		{"euid_zero_whitespace_sudo", 0, "   ", false, ErrRunningAsRoot},

		// Sudo branch: any non-empty, non-whitespace value triggers.
		{"euid_user_sudo_named", 1000, "alice", false, ErrRunningWithSudo},
		{"euid_user_sudo_root_string", 1000, "root", false, ErrRunningWithSudo},
		{"euid_high_user_sudo_set", 99999, "service", false, ErrRunningWithSudo},

		// Accepted: normal user with no sudo footprint.
		{"euid_user_empty_sudo", 1000, "", true, nil},
		{"euid_user_whitespace_sudo", 1000, "   ", true, nil},
		{"euid_user_tab_sudo", 1000, "\t", true, nil},
		{"euid_user_newline_sudo", 1000, "\n", true, nil},
		{"euid_high_no_sudo", 99999, "", true, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := refuseRootOrSudo(tc.euid, tc.sudoUser)
			if tc.wantNil {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.wantIsErr),
				"want errors.Is(err, %v); got %v", tc.wantIsErr, err)
		})
	}
}

// TestRefuseRootOrSudo_LiveWrapper exercises the exported wrapper
// against the actual process state. The test cannot exercise the
// euid==0 branch without running as root (out of scope for unit
// tests), so it focuses on the SUDO_USER side which CAN be mutated
// from a non-root process via t.Setenv.
// t.Setenv enforces no-parallel execution at the runtime level —
// calling t.Parallel inside a test that uses t.Setenv panics — so
// these sub-tests run sequentially. The parallel-safe coverage of
// every other branch combination lives in
// TestRefuseRootOrSudo_PureHelper_TableDriven above.
func TestRefuseRootOrSudo_LiveWrapper_NoSudo(t *testing.T) {
	// Clear $SUDO_USER for this test (t.Setenv restores on cleanup).
	t.Setenv("SUDO_USER", "")

	// `make test` runs as a normal user, so euid != 0 and SUDO_USER
	// is empty → the wrapper MUST return nil.
	if os.Geteuid() == 0 {
		t.Skip("test runs as root; the wrapper would fire the root branch")
	}
	assert.NoError(t, RefuseRootOrSudo())
}

func TestRefuseRootOrSudo_LiveWrapper_SudoSet(t *testing.T) {
	t.Setenv("SUDO_USER", "alice")

	if os.Geteuid() == 0 {
		t.Skip("test runs as root; the root branch wins before SUDO_USER is consulted")
	}

	err := RefuseRootOrSudo()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRunningWithSudo))
}

// TestRefuseRootOrSudo_SentinelsAreStable locks the two exported
// sentinels. 's cmd/wdm exit-code mapper keys on
// errors.Is(err, ErrRunningAsRoot) and errors.Is(err, ErrRunningWithSudo)
// to route to PRD §27 (exit code 6), so collapsing or renaming
// either sentinel is a breaking change.
func TestRefuseRootOrSudo_SentinelsAreStable(t *testing.T) {
	t.Parallel()

	require.NotNil(t, ErrRunningAsRoot)
	require.NotNil(t, ErrRunningWithSudo)
	assert.NotEqual(t, ErrRunningAsRoot, ErrRunningWithSudo)
	assert.Contains(t, ErrRunningAsRoot.Error(), "root")
	assert.Contains(t, ErrRunningWithSudo.Error(), "sudo")
}
