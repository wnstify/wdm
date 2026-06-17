package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wnstify/wdm/pkg/engine"
	"github.com/wnstify/wdm/pkg/types"
)

// newAppsLogsCmd builds the `apps logs <app-id>` leaf (PRD §24, §32;
// injected factory and relays each redacted [types.LogLine] the engine streams
// via the [types.LogLineFn] callback. Output form depends on the root's --json
// persistent flag:
//   - Plain mode: one grep-friendly line per entry on stdout —
//     "<timestamp> <service> <stream> | <text>".
//   - JSON mode: newline-delimited JSON (NDJSON) — one wdm.v1 envelope per log
//     line on stdout, each wrapping a single LogLine object.
//
// JSON-mode shape rationale (PRD §32): a log read is a stream of N lines, not
// a single object, and [types.NewEnvelope] rejects a JSON array — the data
// field must be an object. With --follow the stream is unbounded, so buffering
// to wrap one envelope is impossible. One wdm.v1 envelope per line keeps the
// mandated envelope on every record, streams correctly for finite tails and
// follow mode, and is the line-delimited-JSON shape [EmitJSON] documents. A
// LogLine marshals to a JSON object, so each envelope reuses [EmitJSON].
// Flags map one-to-one onto [types.LogsRequest]:
//   - --follow / -f: stream new lines as they arrive (LogsRequest.Follow).
//   - --tail: limit initial output to the last N lines per service
//     (LogsRequest.Tail). The engine rejects a negative value.
//   - --service: restrict streaming to the named services; repeatable
//     (LogsRequest.Services). The engine refuses unknown services with the
//     known set in the error hint.
//
// Exit code on a canceled stream: when the caller cancels the command context
// mid-stream, the engine surfaces [types.ErrCodeUserCanceled] (PRD §27 →
// exit 7) and this leaf returns it unchanged. A keyboard Ctrl+C is NOT that
// path today: cmd/wdm installs no signal context, so SIGINT terminates through
// Go's default disposition and the shell reports 130. The exit-7 promise for
// interactive interrupts returns when cmd/wdm gains signal.NotifyContext — a
// cmd-side change, not this leaf's.
// Logs is read-only (PRD §26): no runtime lock, no progress, no
// [types.Confirmer]. The engine factory is invoked inside RunE, and only
// there, so `wdm apps logs --help` never reaches [engine.New] (PRD §14
// self-update smoke-check invariant, mirrored from `apps list`).
func newAppsLogsCmd(newEngine func() (engine.Engine, error)) *cobra.Command {
	var (
		follow   bool
		tail     int
		services []string
	)

	cmd := &cobra.Command{
		Use:   "logs <app-id>",
		Short: "Stream redacted logs from a managed stack",
		Long: `Logs streams container logs for a managed app. Lines are
redacted before output: secret-shaped content such as passwords,
tokens, keys, and URL credentials is scrubbed before any line
reaches the terminal or a pipe.

With --json each line is emitted as its own wdm.v1 envelope
(newline-delimited JSON), which streams correctly under --follow.
Press Ctrl+C to stop a --follow stream.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return fmt.Errorf("apps logs: reading --json: %w", err)
			}

			eng, err := newEngine()
			if err != nil {
				return err
			}
			defer eng.Close() //nolint:errcheck // best-effort cleanup; engine.Close releases flock handles and logs internally

			req := types.LogsRequest{
				AppID:    args[0],
				Follow:   follow,
				Tail:     tail,
				Services: services,
			}

			sink := newLogSink(cmd.OutOrStdout(), useJSON)
			if err := eng.Logs(cmd.Context(), req, sink.emit); err != nil {
				return err
			}
			// A streamed-write failure cannot abort the engine mid-stream
			// (LogLineFn returns nothing), so the sink records it and surfaces
			// it once the engine returns cleanly. The engine error above takes
			// precedence when both occur.
			return sink.err
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new log lines as they arrive")
	cmd.Flags().IntVar(&tail, "tail", 0, "show only the last N lines per service (0 streams all history)")
	cmd.Flags().StringArrayVar(&services, "service", nil, "restrict logs to the named service (repeatable)")

	return cmd
}

// logSink relays streamed [types.LogLine] values to w in plain or JSON form.
// It holds the first write error so the streaming callback — which cannot
// return an error — records a failing writer for the RunE handler to surface
// after the engine returns. The reachable cases are a redirected stdout
// hitting a real write fault (ENOSPC) and injected writers in tests; a broken
// pipe on stdout (`wdm apps logs | head`) never reaches this latch, because
// the Go runtime re-raises SIGPIPE for EPIPE on fd 1/2 and the process dies
// with 141 first. Once a write fails, later lines are dropped rather than
// retried so a persistent writer fault does not spin.
type logSink struct {
	w       io.Writer
	useJSON bool
	err     error
}

// newLogSink builds a logSink writing to w. useJSON selects NDJSON
// (one wdm.v1 envelope per line) over the plain single-line form.
func newLogSink(w io.Writer, useJSON bool) *logSink {
	return &logSink{w: w, useJSON: useJSON}
}

// emit is the [types.LogLineFn] passed to [engine.Engine.Logs]. It renders one
// line and records the first write error, after which it becomes a no-op so a
// wedged writer is not hammered for every remaining line in the stream.
func (s *logSink) emit(line types.LogLine) {
	if s.err != nil {
		return
	}
	if s.useJSON {
		if err := EmitJSON(s.w, line); err != nil {
			s.err = err
		}
		return
	}
	if _, err := io.WriteString(s.w, formatLogLine(line)); err != nil {
		s.err = fmt.Errorf("apps logs: writing log line: %w", err)
	}
}

// formatLogLine renders one plain-mode log line in a grep-friendly,
// column-stable shape: an RFC3339 timestamp, the service name, the stream, then
// the redacted text after a "| " separator. The pipe keeps the message text
// distinct from the metadata prefix without table-art.
func formatLogLine(line types.LogLine) string {
	var b strings.Builder
	b.WriteString(line.Timestamp.Format(time.RFC3339))
	b.WriteByte(' ')
	b.WriteString(line.Service)
	if line.Stream != "" {
		b.WriteByte(' ')
		b.WriteString(line.Stream)
	}
	b.WriteString(" | ")
	b.WriteString(line.Text)
	b.WriteByte('\n')
	return b.String()
}
