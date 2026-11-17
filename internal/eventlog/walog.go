package eventlog

import (
	"context"
	"fmt"
	"log/slog"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// WhatsmeowLogger adapts an slog logger to whatsmeow's logger interface, so
// the library's internal logs share the format and destination of the event
// log. Its lines carry component=whatsmeow and the whatsmeow module name.
func WhatsmeowLogger(base *slog.Logger) waLog.Logger {
	return waLogger{base: base.With("component", "whatsmeow")}
}

type waLogger struct {
	base   *slog.Logger
	module string
}

func (l waLogger) Sub(module string) waLog.Logger {
	if l.module != "" {
		module = l.module + "/" + module
	}
	return waLogger{base: l.base, module: module}
}

func (l waLogger) Errorf(msg string, args ...any) { l.emit(slog.LevelError, msg, args) }
func (l waLogger) Warnf(msg string, args ...any)  { l.emit(slog.LevelWarn, msg, args) }
func (l waLogger) Infof(msg string, args ...any)  { l.emit(slog.LevelInfo, msg, args) }
func (l waLogger) Debugf(msg string, args ...any) { l.emit(slog.LevelDebug, msg, args) }

// emit checks the level first: whatsmeow logs a lot at debug level and the
// formatting is skipped when those lines are filtered out.
func (l waLogger) emit(level slog.Level, format string, args []any) {
	ctx := context.Background()
	if !l.base.Enabled(ctx, level) {
		return
	}
	l.base.Log(ctx, level, fmt.Sprintf(format, args...), "module", l.module)
}
