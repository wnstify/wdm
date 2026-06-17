package system_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/system"
)

// TestCurrentIdentity_ReturnsLiveValues confirms [CurrentIdentity]
// captures the invoking user's actual identity. The test cannot
// forge os.Geteuid or os/user.Current results, so it asserts the
// fields are consistent with the runtime instead of pinned to
// hard-coded values.
// Test invariants:
//   - Username is non-empty (every Unix account has a name)
//   - Home is an absolute path (PRD §13 anchors XDG layout under it)
//   - EUID matches os.Geteuid at the moment of the call
//   - GID is non-negative (uids/gids are unsigned conceptually; the
//     int return on parse failure is the failure mode, never a
//     negative on success)
func TestCurrentIdentity_ReturnsLiveValues(t *testing.T) {
	t.Parallel()

	id, err := system.CurrentIdentity()
	require.NoError(t, err)

	assert.NotEmpty(t, id.Username, "Username must be populated")
	assert.True(t, filepath.IsAbs(id.Home), "Home must be absolute, got %q", id.Home)
	assert.Equal(t, os.Geteuid(), id.EUID, "EUID must match live os.Geteuid()")
	assert.GreaterOrEqual(t, id.GID, 0, "GID must be non-negative")
	assert.NotEmpty(t, id.GroupIDs, "GroupIDs must include at least the primary group")
	assert.Contains(t, id.GroupIDs, id.GID, "GroupIDs must contain the primary GID")
}

// TestIdentity_ZeroValueHasNoSemantics is a documentation test
// echoing the doc comment "Callers MUST NOT construct an Identity by
// hand: the zero value carries no meaningful semantics". The test
// asserts the obvious — zero EUID is the root UID — so the rule's
// rationale is visible in tests as well as docs.
func TestIdentity_ZeroValueHasNoSemantics(t *testing.T) {
	t.Parallel()

	var zero system.Identity
	assert.Equal(t, 0, zero.EUID, "zero value Identity.EUID is 0 — which is root, the very state PRD §11 refuses; this is why constructors must populate via CurrentIdentity")
	assert.Equal(t, "", zero.Username)
	assert.Equal(t, "", zero.Home)
}
