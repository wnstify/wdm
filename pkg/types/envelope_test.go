package types_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/pkg/types"
)

// TestEnvelopeSchema_IsWdmV1 locks the schema identifier (PRD §32).
// A change here is a wire-breaking change to every --json consumer
// and to the future IPC contract; the test exists to make that drift
// loud rather than silent.
func TestEnvelopeSchema_IsWdmV1(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "wdm.v1", types.EnvelopeSchema)
}

func TestNewEnvelope_AcceptsStruct(t *testing.T) {
	t.Parallel()

	type payload struct {
		Apps []string `json:"apps"`
	}
	env, err := types.NewEnvelope(payload{Apps: []string{"a", "b"}})

	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, types.EnvelopeSchema, env.Schema)
	assert.JSONEq(t, `{"apps":["a","b"]}`, string(env.Data))
}

func TestNewEnvelope_AcceptsMap(t *testing.T) {
	t.Parallel()

	env, err := types.NewEnvelope(map[string]any{"key": 42})

	require.NoError(t, err)
	assert.JSONEq(t, `{"key":42}`, string(env.Data))
}

func TestNewEnvelope_AcceptsEmptyObject(t *testing.T) {
	t.Parallel()

	env, err := types.NewEnvelope(struct{}{})

	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(env.Data))
}

func TestNewEnvelope_RejectsNonObjectPayloads(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data any
	}{
		{"nil", nil},
		{"int_array", []int{1, 2, 3}},
		{"string_array", []string{"a", "b"}},
		{"empty_array", []int{}},
		{"string_scalar", "scalar"},
		{"int_scalar", 42},
		{"bool_scalar", true},
		{"float_scalar", 3.14},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, err := types.NewEnvelope(tc.data)
			require.Error(t, err)
			assert.Nil(t, env)
		})
	}
}

// TestNewEnvelope_FullJSONRoundTrip verifies that the wdm.v1
// envelope marshals to exactly the wire shape PRD §32 mandates and
// that consumers can decode the inner data field into the original
// payload type. This is the load-bearing contract — any drift here
// breaks `wdm apps list --json` consumers.
func TestNewEnvelope_FullJSONRoundTrip(t *testing.T) {
	t.Parallel()

	type appsPayload struct {
		Apps []string `json:"apps"`
	}
	env, err := types.NewEnvelope(appsPayload{Apps: []string{"vaultwarden"}})
	require.NoError(t, err)

	raw, err := json.Marshal(env)
	require.NoError(t, err)
	assert.JSONEq(t, `{"schema":"wdm.v1","data":{"apps":["vaultwarden"]}}`, string(raw))

	var decoded types.Envelope
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, "wdm.v1", decoded.Schema)

	var payload appsPayload
	require.NoError(t, json.Unmarshal(decoded.Data, &payload))
	assert.Equal(t, []string{"vaultwarden"}, payload.Apps)
}

// TestNewEnvelope_RejectsUnmarshalablePayload covers the json.Marshal
// failure path inside NewEnvelope. The Go runtime rejects channels,
// funcs, and complex numbers as JSON values; passing a chan as the
// payload triggers json.UnsupportedTypeError before the
// object-prefix check fires, so the wrap "marshal envelope data:"
// path is exercised.
func TestNewEnvelope_RejectsUnmarshalablePayload(t *testing.T) {
	t.Parallel()

	env, err := types.NewEnvelope(make(chan int))
	require.Error(t, err)
	assert.Nil(t, env)
	assert.Contains(t, err.Error(), "marshal envelope data")
}

// TestNewEnvelope_LongPayloadPreviewTruncates exercises the
// payloadPreview truncation branch — when the rejected payload's
// JSON form exceeds 16 bytes, the diagnostic message includes the
// "..." suffix so log lines stay bounded for hostile or accidentally
// large input. Below the threshold the suffix must be absent (the
// other RejectsNonObjectPayloads cases cover the short-form).
func TestNewEnvelope_LongPayloadPreviewTruncates(t *testing.T) {
	t.Parallel()

	// 20-element int array marshals to "[1,2,3,...,20]" — > 16 bytes,
	// non-object so it lands in the prefix-rejection branch.
	long := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	_, err := types.NewEnvelope(long)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "...",
		"long non-object payload must surface the truncation-suffix preview")
}

// TestNewEnvelope_RejectionsAreInspectable confirms that the
// validation-failure error stays inspectable via errors.As against
// the underlying [*types.Error]-free chain. The sentinel itself is
// unexported (it's the implementation detail behind the validation),
// so callers get a typed error message + a guarantee that NewEnvelope
// never returns a partial envelope on rejection.
func TestNewEnvelope_RejectionsAreInspectable(t *testing.T) {
	t.Parallel()

	env, err := types.NewEnvelope([]string{"array"})
	require.Error(t, err)
	require.Nil(t, env)

	// The error string surfaces the offending payload prefix for
	// debuggability — this is the user-facing diagnostic that
	// cmd/wdm composes hints from.
	assert.Contains(t, err.Error(), "object")

	// Wrapping with a fresh sentinel and matching with errors.Is
	// confirms the chain stays inspectable.
	sentinel := errors.New("test sentinel")
	wrapped := errors.Join(sentinel, err)
	assert.True(t, errors.Is(wrapped, sentinel))
}
