package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// EnvelopeSchema is the schema identifier embedded in every wdm JSON
// payload. Locked to "wdm.v1"; bumping it is a wire-breaking change to
// the CLI --json output and the future IPC contract (PRD §32).
const EnvelopeSchema = "wdm.v1"

// Envelope wraps every JSON payload emitted by wdm. PRD §32 mandates
// schema="wdm.v1" and an object-typed data field, so downstream parsers
// can rely on a stable outer shape even when the inner data evolves.
type Envelope struct {
	// Schema is the wire-format identifier; always EnvelopeSchema.
	Schema string `json:"schema"`

	// Data is the inner payload, kept as raw JSON so callers can decide
	// when (and into what type) to unmarshal it.
	Data json.RawMessage `json:"data"`
}

// errEnvelopeDataNotObject is the sentinel returned when NewEnvelope is
// handed a payload that does not marshal to a JSON object. Match it with
// errors.Is to distinguish this specific violation.
var errEnvelopeDataNotObject = errors.New("types: envelope data must marshal to a JSON object")

// NewEnvelope wraps data in a wdm.v1 envelope, enforcing PRD §32's
// "data is a JSON object" rule. nil payloads, arrays, scalars, and JSON
// null are rejected up-front so the wire contract is never quietly broken.
func NewEnvelope(data any) (*Envelope, error) {
	if data == nil {
		return nil, fmt.Errorf("nil payload: %w", errEnvelopeDataNotObject)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal envelope data: %w", err)
	}
	if !bytes.HasPrefix(raw, []byte("{")) {
		return nil, fmt.Errorf("payload starts with %s: %w", payloadPreview(raw), errEnvelopeDataNotObject)
	}
	return &Envelope{
		Schema: EnvelopeSchema,
		Data:   raw,
	}, nil
}

// payloadPreview returns a short, log-safe preview of raw for error
// messages, keeping output bounded when callers hand us large blobs.
func payloadPreview(raw []byte) string {
	const max = 16
	if len(raw) > max {
		return fmt.Sprintf("%q...", raw[:max])
	}
	return fmt.Sprintf("%q", raw)
}
