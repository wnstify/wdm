package core

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAppendBackupHistory_EncodesOperationAndClonesExisting pins the on-disk
// ledger contract of the shared backup-history appender: prior entries are
// cloned byte-for-byte and the new record carries the caller's operation under
// the persisted {path,operation,at} field names. The update and reconfigure
// commit points both depend on this exact shape, so the operation passed in
// must be the only thing that varies between them.
func TestAppendBackupHistory_EncodesOperationAndClonesExisting(t *testing.T) {
	seeded := json.RawMessage(`{"path":"/old","operation":"update","at":"2020-01-01T00:00:00Z","keep":"verbatim"}`)
	at := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

	for _, op := range []string{"update", "reconfigure"} {
		t.Run(op, func(t *testing.T) {
			got, err := appendBackupHistory([]json.RawMessage{seeded}, op, "/snap", at)
			require.NoError(t, err)
			require.Len(t, got, 2)
			assert.JSONEq(t, string(seeded), string(got[0]), "prior entry must be preserved verbatim")
			expected := fmt.Sprintf(`{"path":"/snap","operation":%q,"at":"2026-06-26T12:00:00Z"}`, op)
			assert.JSONEq(t, expected, string(got[1]), "new entry must keep the persisted field contract")
		})
	}
}

// TestAppendBackupHistory_EmptyPathAppendsNothing proves an absent snapshot
// path records no ledger entry, only the cloned history.
func TestAppendBackupHistory_EmptyPathAppendsNothing(t *testing.T) {
	seeded := json.RawMessage(`{"path":"/old","operation":"update","at":"2020-01-01T00:00:00Z"}`)
	got, err := appendBackupHistory([]json.RawMessage{seeded}, "update", "", time.Unix(0, 0).UTC())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.JSONEq(t, string(seeded), string(got[0]))
}

// TestAppendBackupHistory_ClonesWithoutAliasing proves the returned history
// does not share backing storage with the caller's slice entries.
func TestAppendBackupHistory_ClonesWithoutAliasing(t *testing.T) {
	in := []json.RawMessage{json.RawMessage(`{"path":"/old"}`)}
	got, err := appendBackupHistory(in, "update", "/snap", time.Unix(0, 0).UTC())
	require.NoError(t, err)
	got[0][0] = 'X'
	assert.Equal(t, byte('{'), in[0][0], "input entry must not be mutated through the returned clone")
}
