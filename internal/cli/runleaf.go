package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/types"
)

// emitResult routes a leaf's typed engine result to the right stream per the
// root's --json persistent flag (PRD §32): under --json it wraps result in the
// wdm.v1 envelope on stdout via [EmitJSON]; otherwise it renders the
// human-readable form with writeText. This is the shared tail of every leaf
// that produces one result object — the byte-for-byte JSON-or-text dispatch —
// so the per-leaf body keeps only its engine call and its own text writer.
// result is a typed pointer (the engine's *Result) so the envelope payload and
// the text writer agree on the concrete type with no per-leaf casts. Both
// branches write to cmd.OutOrStdout, so writeText is the leaf's existing
// io.Writer-based renderer passed by reference.
func emitResult[T any](cmd *cobra.Command, useJSON bool, result T, writeText func(io.Writer, T) error) error {
	out := cmd.OutOrStdout()
	if useJSON {
		return EmitJSON(out, result)
	}
	return writeText(out, result)
}

// stateChangeIO builds the two state-changing-leaf collaborators that every
// lifecycle command wires identically: the shared [cliConfirmer] (over the
// command's stdout/stderr/stdin) and the progress callback. JSON mode suppresses
// progress so the single envelope is the only thing on stdout (PRD §32); plain
// mode streams progress to stderr (golang-cli: output to stdout, diagnostics to
// stderr).
// assumeYes and acceptDBRisk are passed through per leaf and NOT homogenized:
// each command owns its safe-confirmation and database-risk posture (only
// `apps update` wires acceptDBRisk true; `apps delete` hardwires assumeYes
// false), so the gating decision stays with the caller.
func stateChangeIO(cmd *cobra.Command, assumeYes, acceptDBRisk, useJSON bool) (*cliConfirmer, types.ProgressFn) {
	confirmer := newCLIConfirmer(cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin(), assumeYes, acceptDBRisk)

	var onProgress types.ProgressFn
	if !useJSON {
		onProgress = stderrProgress(cmd.ErrOrStderr())
	}
	return confirmer, onProgress
}
