package courier

import (
	"context"
	"log/slog"
)

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

var nopLogger = slog.New(discardHandler{})

// NopLogger returns a logger that emits nothing and does negligible work
// (LOG-04). It never touches slog.Default() (LOG-01).
func NopLogger() *slog.Logger { return nopLogger }

// LoggerOrNop returns l, or the no-op logger when l is nil.
func LoggerOrNop(l *slog.Logger) *slog.Logger {
	if l == nil {
		return nopLogger
	}
	return l
}
