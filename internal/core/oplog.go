package core

import (
	"context"
	"log/slog"
	"runtime"
)

// opLogger carries the PRD §24 normal-log identity for one engine
// operation: the action name plus the wdm version and host OS/arch that
// every record on this op shares. It logs only NON-secret facts —
// generated secret values, plaintext credentials, and .env contents never
// reach it (PRD §24 "Normal logs must not include"). The crux is rule (1):
// log facts ABOUT a secret (its placeholder name), never the value.
type opLogger struct {
	logger *slog.Logger
	action string
}

// newOpLogger binds the per-operation logger to an action and the engine's
// version. The supplied logger may be a secret-aware child (install) or the
// engine default; either way the §24 identity attributes attach to its
// start and result lines.
func (e *Engine) newOpLogger(logger *slog.Logger, action string) opLogger {
	return opLogger{
		logger: logger.With(
			slog.String("action", action),
			slog.String("wdm_version", e.version),
			slog.String("os", runtime.GOOS),
			slog.String("arch", runtime.GOARCH),
		),
		action: action,
	}
}

// start emits the op-start line at Info with the selected app (PRD §24
// "selected app"). The stack path is not resolved yet at start; the result
// line carries it. An empty app is dropped so whole-system operations
// (uninstall) stay clean.
func (o opLogger) start(ctx context.Context, app string) {
	o.logger.LogAttrs(ctx, slog.LevelInfo, "core: operation started", appStackAttrs(app, "")...)
}

// success emits the op-result line at Info: the operation completed with no
// failure point (PRD §24 "selected action", "failure point" absent on success).
func (o opLogger) success(ctx context.Context, app, stackPath string) {
	o.logger.LogAttrs(ctx, slog.LevelInfo, "core: operation completed", appStackAttrs(app, stackPath)...)
}

// failure emits the op-result line at Info naming the step that failed (PRD
// §24 "failure point"). The error string passes through the redacting
// handler before reaching the sink, so a secret-bearing detail is scrubbed;
// failurePoint is a fixed step label, not user data.
func (o opLogger) failure(ctx context.Context, app, stackPath, failurePoint string, err error) {
	attrs := appStackAttrs(app, stackPath)
	attrs = append(attrs, slog.String("failure_point", failurePoint))
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	o.logger.LogAttrs(ctx, slog.LevelInfo, "core: operation failed", attrs...)
}

// step records a §24 check/command fact at Info: the checks performed and
// the command names invoked, with exit codes when known. Callers pass only
// non-secret labels (step name, command name, exit code).
func (o opLogger) step(ctx context.Context, name string, attrs ...slog.Attr) {
	o.logger.LogAttrs(ctx, slog.LevelInfo, "core: "+name, attrs...)
}

// debug records command-argv summaries and validation detail visible only
// under wdm --debug. The redacting handler still scrubs every string, so a
// summary that accidentally carries a secret-shaped value is redacted.
func (o opLogger) debug(ctx context.Context, msg string, attrs ...slog.Attr) {
	o.logger.LogAttrs(ctx, slog.LevelDebug, "core: "+msg, attrs...)
}

func appStackAttrs(app, stackPath string) []slog.Attr {
	attrs := make([]slog.Attr, 0, 2)
	if app != "" {
		attrs = append(attrs, slog.String("app", app))
	}
	if stackPath != "" {
		attrs = append(attrs, slog.String("stack_path", stackPath))
	}
	return attrs
}
