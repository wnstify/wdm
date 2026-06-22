package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/logging"
	"github.com/wnstify/wdm/internal/security"
)

// newJSONLogger builds the same slog chain the engine wires in
// buildDefaultLogger: a JSON handler over w wrapped by the redacting handler.
// Tests construct the logger this way so redaction, level, and source
// behavior stay covered through the production construction path rather than a
// dedicated constructor.
func newJSONLogger(w io.Writer, level slog.Level, addSource bool, redactor security.Redactor) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:     level,
		AddSource: addSource,
	})
	return slog.New(logging.NewRedactingHandler(base, redactor))
}

func TestLogger_RedactsMessagesAttributesAndGroups(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newJSONLogger(&buf, slog.LevelInfo, false, security.NewActiveRedactor([]string{"literal-secret"}))

	logger.Info("starting with literal-secret",
		slog.String("token", "literal-secret"),
		slog.Group("db",
			slog.String("password", "literal-secret"),
			slog.Int("port", 5432),
		),
		slog.Bool("ok", true),
	)

	record := decodeLogRecord(t, buf.Bytes())
	assert.Equal(t, "starting with [REDACTED]", record["msg"])
	assert.Equal(t, "[REDACTED]", record["token"])
	assert.Equal(t, true, record["ok"])

	db := requireMap(t, record["db"])
	assert.Equal(t, "[REDACTED]", db["password"])
	assert.Equal(t, float64(5432), db["port"])
}

func TestLogger_RedactsScopedAttributesAndGroupedRecords(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newJSONLogger(&buf, slog.LevelInfo, false, security.NewActiveRedactor([]string{"literal-secret"}))

	logger.
		With(slog.String("scoped", "literal-secret")).
		WithGroup("request").
		Info("request finished", slog.String("authorization", "Bearer literal-secret"))

	record := decodeLogRecord(t, buf.Bytes())
	assert.Equal(t, "[REDACTED]", record["scoped"])

	request := requireMap(t, record["request"])
	assert.Equal(t, "Bearer [REDACTED]", request["authorization"])
}

func TestLogger_HonorsConfiguredLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newJSONLogger(&buf, slog.LevelWarn, false, security.NoopRedactor)

	logger.Info("hidden")
	logger.Warn("visible")

	record := decodeLogRecord(t, buf.Bytes())
	assert.Equal(t, "WARN", record["level"])
	assert.Equal(t, "visible", record["msg"])
}

func TestLogger_AddSourceIncludesSourceAttribute(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := newJSONLogger(&buf, slog.LevelInfo, true, security.NoopRedactor)

	logger.Info("with source")

	record := decodeLogRecord(t, buf.Bytes())
	source := requireMap(t, record["source"])
	assert.Contains(t, source["function"], "TestLogger_AddSourceIncludesSourceAttribute")
	assert.Contains(t, source["file"], "logger_test.go")
	assert.Greater(t, source["line"], float64(0))
}

func TestRedactingHandler_PropagatesBaseErrors(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("sink failed")
	handler := logging.NewRedactingHandler(errorHandler{err: wantErr}, security.NoopRedactor)

	err := handler.Handle(t.Context(), slog.NewRecord(
		time.Time{},
		slog.LevelInfo,
		"message",
		0,
	))

	require.ErrorIs(t, err, wantErr)
}

type errorHandler struct {
	err error
}

func (h errorHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h errorHandler) Handle(context.Context, slog.Record) error {
	return h.err
}

func (h errorHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h errorHandler) WithGroup(string) slog.Handler {
	return h
}

func decodeLogRecord(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	var record map[string]any
	require.NoError(t, json.Unmarshal(raw, &record))
	return record
}

func requireMap(t *testing.T, value any) map[string]any {
	t.Helper()

	got, ok := value.(map[string]any)
	require.True(t, ok, "got %T, want object", value)
	return got
}
