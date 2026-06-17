package logging

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/wnstify/wdm/internal/security"
)

// ErrNoWriter is returned by [New] when no writer was supplied via
// [WithWriter]. The logging package does not own log-file paths
// (: path expansion ~/ → $HOME happens in
// the engine), so it refuses to construct a logger without a writer.
var ErrNoWriter = errors.New("logging: missing writer")

// ErrNilRedactor is returned by [WithRedactor] when nil is supplied.
var ErrNilRedactor = errors.New("logging: WithRedactor requires non-nil redactor; pass security.NoopRedactor for no-op redaction")

// New constructs a [*slog.Logger] wrapping a [slog.NewJSONHandler]
// over the supplied writer with the redacting handler from
// [NewRedactingHandler]. Defaults: [slog.LevelInfo] (PRD §24
// "normal log"), [security.NoopRedactor], and AddSource off.
// The caller owns the writer's lifecycle; [New] never closes it.
// There is no Close on the slog handler chain — the upstream writer
// (the engine's open file) is what needs closing. Option errors are
// wrapped with the "logging.New:" prefix so they stay distinct from
// runtime logging errors after propagating through the engine.
func New(opts ...Option) (*slog.Logger, error) {
	cfg := &config{
		level:    slog.LevelInfo,
		redactor: security.NoopRedactor,
	}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("logging.New: %w", err)
		}
	}
	if cfg.writer == nil {
		return nil, ErrNoWriter
	}

	base := slog.NewJSONHandler(cfg.writer, &slog.HandlerOptions{
		Level:     cfg.level,
		AddSource: cfg.addSource,
	})
	return slog.New(NewRedactingHandler(base, cfg.redactor)), nil
}
