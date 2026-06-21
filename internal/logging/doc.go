// Package logging configures the wdm [log/slog] pipeline: a redacting
// handler that consults [internal/security.Redactor] before any record
// reaches disk (PRD §11, §24). The engine builds the slog chain directly —
// a [slog.NewJSONHandler] over the open sink wrapped by [NewRedactingHandler]
// — and supplies the concrete redaction pattern set.
// The on-disk sink is the concrete PRD §24 retention implementation:
// [OpenLogFile] prepares <stateDir>/logs, archives any prior latest.log
// under a timestamped wdm-YYYY-MM-DD-HHMMSS.log name, opens a fresh
// owner-only latest.log (dir 0700, file 0600), and prunes archives to the
// retention intersection — kept only when BOTH within [RetentionMaxAge]
// AND among the [RetentionMaxFiles] newest, with latest.log always kept.
// Import boundary: internal/logging may
// import other internal/* siblings (notably internal/security for
// [security.Redactor]) but must not depend on pkg/engine, cmd/wdm,
// internal/tui, internal/cli, or internal/core. Path expansion
// (~/ → $HOME) happens in the engine; internal/logging accepts a
// writer already resolved upstream.
// Public surface:
//   - [NewRedactingHandler] — wrap any [slog.Handler] with redaction
//   - [OpenLogFile] — open the PRD §24 file sink (archive + prune) and
//     return the owner-only latest.log handle the caller owns and closes
//   - [RetentionMaxAge], [RetentionMaxFiles], [LatestLogName] — the
//     PRD §24 retention policy as code-level constants
package logging
