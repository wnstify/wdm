package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/wnstify/wdm/pkg/types"
)

// EmitJSON wraps data in the wdm.v1 envelope (PRD §32) and writes it as JSON
// to w. Subcommands call this on the --json path; plain human-readable output
// is the default and writes directly to cmd.OutOrStdout without an envelope.
// The envelope ends with a trailing newline (json.Encoder's default) so
// scripts consuming line-delimited JSON see one complete envelope per line.
// [types.NewEnvelope] validates that data marshals to a JSON object (PRD §32
// requires envelope.data to be an object, not an array or scalar); validation
// errors propagate wrapped with the "cli:" prefix so the layer is identifiable
// in error chains.
func EmitJSON(w io.Writer, data any) error {
	env, err := types.NewEnvelope(data)
	if err != nil {
		return fmt.Errorf("cli: building envelope: %w", err)
	}
	if err := json.NewEncoder(w).Encode(env); err != nil {
		return fmt.Errorf("cli: encoding envelope: %w", err)
	}
	return nil
}
