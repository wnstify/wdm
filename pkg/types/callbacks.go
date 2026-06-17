package types

import "context"

// ProgressFn reports progress for long-running operations. PRD §37
// mandates this exact signature; the matching IPC payload shape is
// [Progress]. Callers may pass nil to engine write methods that need no
// progress events; implementations must tolerate nil.
// Originally declared in pkg/engine; relocated to pkg/types at the
// acyclic. internal/core needs the callback shape but must not import
// pkg/engine after the bridge wires pkg/engine.New through to core.New.
// pkg/engine keeps a type alias, so callers still see engine.ProgressFn.
type ProgressFn func(step string, pct float64, msg string)

// LogLineFn delivers one parsed log line at a time during Engine.Logs.
// stream through it. Implementations should return quickly because the
// engine may block on the callback to apply back-pressure to the
// upstream reader.
// See [ProgressFn] for the relocation rationale.
type LogLineFn func(line LogLine)

// Confirmer authorizes destructive or otherwise consequential actions.
// PRD §37 requires every destructive engine operation to consult a
// Confirmer: the TUI implements it as an in-screen prompt, the future
// GUI as a modal dialog. The engine calls Confirm once a consequence is
// known — volume changes, port collisions, breaking template upgrades —
// and aborts the surrounding operation with [ErrCodeUserCanceled] (PRD
// §27) when Confirm returns (false, nil).
// See [ProgressFn] for the relocation rationale.
type Confirmer interface {
	// Confirm presents the prompt described by c and returns true when
	// the user authorizes the action. A non-nil error aborts the
	// surrounding operation regardless of the returned boolean.
	// The supplied ctx is the same [context.Context] the calling engine
	// method received; implementations must honor its cancellation.
	Confirm(ctx context.Context, c Confirmation) (bool, error)
}
