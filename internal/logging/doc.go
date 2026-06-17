// Package logging configures the wdm [log/slog] pipeline: a JSON
// handler over a caller-supplied [io.Writer] wrapped by a redacting
// handler that consults [internal/security.Redactor] before any
// record reaches disk (PRD §11, §24).
// The caller supplies the concrete redaction pattern set. [NoopRotator]
// is available for callers that do not want file rotation.
// Import boundary: internal/logging may
// import other internal/* siblings (notably internal/security for
// [security.Redactor]) but must not depend on pkg/engine, cmd/wdm,
// internal/tui, internal/cli, or internal/core. Path expansion
// (~/ → $HOME) happens in the engine; internal/logging accepts a
// writer already resolved upstream.
// Public surface:
//   - [New] — construct a [*slog.Logger] from functional options
//   - [Option], [WithWriter], [WithLevel], [WithRedactor], [WithAddSource]
//   - [ErrNoWriter] — sentinel returned when no writer was supplied
//   - [NewRedactingHandler] — wrap any [slog.Handler] with redaction
//   - [Rotator] — interface for log file rotation + retention pruning
//   - [NoopRotator] — passthrough Rotator wired by callers
//   - [RetentionMaxAge], [RetentionMaxFiles], [LatestLogName] — the
//     PRD §24 retention policy as code-level constants
package logging
