package logging

import (
	"io"
	"log/slog"

	"github.com/wnstify/wdm/internal/security"
)

// config carries the construction-time settings consumed by [New].
// It is unexported: callers configure New only through the With*
// options in this file.
type config struct {
	writer    io.Writer
	level     slog.Level
	redactor  security.Redactor
	addSource bool
}

// Option mutates a logger config during [New] (functional options).
// Returning an error lets an Option reject bad input at construction
// time without breaking existing call sites — the idiom established
// by pkg/engine.Option after the audit (docs/golang-skills.md).
type Option func(*config) error

// WithWriter sets the [io.Writer] the slog JSON handler writes to.
// The caller owns the writer's lifecycle; [New] does not close it.
// Today the engine passes [os.Stderr]; once the PRD §24 file sink
// lands it will pass an [*os.File] at
// ~/.local/state/wdm/logs/latest.log. Tests pass [io.Discard] or a
// [*bytes.Buffer] to capture output.
func WithWriter(w io.Writer) Option {
	return func(c *config) error {
		c.writer = w
		return nil
	}
}

// WithLevel sets the minimum log level. Defaults to [slog.LevelInfo],
// matching PRD §24's normal log level. Debug logging still uses redaction.
func WithLevel(l slog.Level) Option {
	return func(c *config) error {
		c.level = l
		return nil
	}
}

// WithRedactor wires the [security.Redactor] consulted for every
// record before it reaches the JSON handler. Defaults to
// [security.NoopRedactor]; wired the real pattern-driven
// Redactor into engine construction so secrets are scrubbed before
// any sink.
// A nil redactor is rejected with [ErrNilRedactor], not silently
// re-defaulted: the handler dereferences the stored Redactor on
// every record, so accepting nil would defer a panic to the first
// log call. Callers without a real Redactor MUST pass
// [security.NoopRedactor] explicitly.
func WithRedactor(r security.Redactor) Option {
	return func(c *config) error {
		if r == nil {
			return ErrNilRedactor
		}
		c.redactor = r
		return nil
	}
}

// WithAddSource toggles slog's "source" attribute (file:line of the
// call site). Off by default to keep the normal log compact; the CLI
// --debug flag turns it on for richer post-mortem context (PRD §24).
func WithAddSource(on bool) Option {
	return func(c *config) error {
		c.addSource = on
		return nil
	}
}
