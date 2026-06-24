package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// These tests pin the top-level `view-env` command: the redacted plain-mode
// table and the wdm.v1 JSON envelope. The engine redacts before this layer,
// so the fixtures already carry masked values; the assertions prove no raw
// secret reaches output.

func TestViewEnv_PlainRendersRedactedTable(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		viewEnvResult: &types.ViewEnvResult{
			AppID: "vaultwarden",
			Entries: []types.EnvEntry{
				{Key: "DOMAIN", Value: "https://vw.example", Secret: false},
				{Key: "ADMIN_TOKEN", Value: "********", Secret: true},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "view-env", "vaultwarden")
	require.NoError(t, err)

	assert.Equal(t, "vaultwarden", fake.viewEnvAppID)
	assert.Contains(t, stdout, "DOMAIN")
	assert.Contains(t, stdout, "https://vw.example")
	assert.Contains(t, stdout, "ADMIN_TOKEN")
	assert.Contains(t, stdout, "********")
	assert.Contains(t, stdout, "(secret)")
	assert.NotContains(t, stdout, "supersecretvalue", "raw secret must never reach output")
}

func TestViewEnv_JSONEmitsEnvelope(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{
		viewEnvResult: &types.ViewEnvResult{
			AppID: "vaultwarden",
			Entries: []types.EnvEntry{
				{Key: "ADMIN_TOKEN", Value: "********", Secret: true},
			},
		},
	}

	stdout, _, err := runLeaf(t, fake, "view-env", "vaultwarden", "--json")
	require.NoError(t, err)

	var env struct {
		Schema string              `json:"schema"`
		Data   types.ViewEnvResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &env))

	assert.Equal(t, "wdm.v1", env.Schema)
	assert.Equal(t, "vaultwarden", env.Data.AppID)
	require.Len(t, env.Data.Entries, 1)
	assert.Equal(t, "ADMIN_TOKEN", env.Data.Entries[0].Key)
	assert.Equal(t, "********", env.Data.Entries[0].Value)
	assert.True(t, env.Data.Entries[0].Secret)
	assert.False(t, strings.Contains(stdout, "supersecretvalue"), "raw secret must never reach JSON output")
}
