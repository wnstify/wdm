package logging

import (
	"context"
	"log/slog"

	"github.com/wnstify/wdm/internal/security"
)

// redactingHandler passes the record message and every string
// attribute through a [security.Redactor] before forwarding to base.
// It is the single integration point between the slog pipeline and
// the redaction patterns in internal/security (PRD §11, §24).
// The package default is [security.NoopRedactor] so the handler runs
// end-to-end; wired the real pattern-driven Redactor into
// engine construction so production records are scrubbed.
type redactingHandler struct {
	base     slog.Handler
	redactor security.Redactor
}

// Compile-time check that *redactingHandler satisfies slog.Handler.
var _ slog.Handler = (*redactingHandler)(nil)

// NewRedactingHandler wraps base so every record passes through r
// before reaching base. Both arguments MUST be non-nil; callers
// without a real Redactor should pass [security.NoopRedactor] so
// the redacting pipeline still executes end-to-end.
func NewRedactingHandler(base slog.Handler, r security.Redactor) slog.Handler {
	return &redactingHandler{base: base, redactor: r}
}

// Enabled delegates to the wrapped handler unchanged: redaction
// transforms records, it never gates them (PRD §24: "wdm must always
// write a normal log").
func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

// Handle redacts the record message and every string attribute,
// including strings nested inside groups, before forwarding to the
// wrapped handler. Non-string kinds (int, bool, time, duration, any)
// pass through unchanged. [slog.LogValuer] chains are not expanded here.
func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, h.redactor.Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(redactAttr(h.redactor, a))
		return true
	})
	return h.base.Handle(ctx, nr)
}

// WithAttrs redacts every string attribute before delegating to the
// wrapped handler's WithAttrs. This covers logger-scoped attrs added
// via [*slog.Logger.With]; without it, a secret stashed on the logger
// once would be logged verbatim on every subsequent record.
func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redactAttr(h.redactor, a)
	}
	return &redactingHandler{
		base:     h.base.WithAttrs(redacted),
		redactor: h.redactor,
	}
}

// WithGroup delegates unchanged: the group name is not user data, and
// strings added later inside the group are redacted when they flow
// through [redactingHandler.Handle].
func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{
		base:     h.base.WithGroup(name),
		redactor: h.redactor,
	}
}

// redactAttr returns a copy of a with any string payload replaced by
// [security.Redactor.Redact]. Group attributes recurse so nested
// strings are redacted too; other [slog.Kind] values pass through.
func redactAttr(r security.Redactor, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, r.Redact(a.Value.String()))
	case slog.KindGroup:
		attrs := a.Value.Group()
		redacted := make([]slog.Attr, len(attrs))
		for i, ga := range attrs {
			redacted[i] = redactAttr(r, ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(redacted...)}
	default:
		return a
	}
}
