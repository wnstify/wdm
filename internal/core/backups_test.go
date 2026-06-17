package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/pkg/types"
)

// TestRestoreBackup_ClosedEngineReturnsErrClosed keeps the closed-engine arm:
// a closed engine returns ErrClosed (it takes precedence over every other
// outcome) with a nil result. The full RestoreBackup surface lives in
// backups_restore_test.go.
func TestRestoreBackup_ClosedEngineReturnsErrClosed(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t)
	require.NoError(t, eng.Close())

	result, err := eng.RestoreBackup(
		t.Context(),
		types.RestoreBackupRequest{AppID: "uptime-kuma", SnapshotID: "1717000000000000000-update"},
		nil,
		nil,
	)
	require.ErrorIs(t, err, core.ErrClosed)
	assert.Nil(t, result)
}
