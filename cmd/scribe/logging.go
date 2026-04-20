package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
)

var (
	logLevel   = new(slog.LevelVar) // runtime-mutable level
	initLogger sync.Once
)

// setupLogger installs a text handler that prints the timestamp + script +
// message, preserving the format cron log scrapers already parse. JSON output
// is available via SCRIBE_LOG_FORMAT=json for anyone who wants to pipe through
// jq. Level is INFO by default; SCRIBE_LOG_LEVEL=debug|warn|error overrides.
//
// Idempotent — safe to call more than once.
func setupLogger() {
	initLogger.Do(func() {
		switch strings.ToLower(os.Getenv("SCRIBE_LOG_LEVEL")) {
		case "debug", "trace":
			logLevel.Set(slog.LevelDebug)
		case "warn", "warning":
			logLevel.Set(slog.LevelWarn)
		case "error":
			logLevel.Set(slog.LevelError)
		default:
			logLevel.Set(slog.LevelInfo)
		}

		var handler slog.Handler
		switch strings.ToLower(os.Getenv("SCRIBE_LOG_FORMAT")) {
		case "json":
			handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
				Level:     logLevel,
				AddSource: false,
			})
		default:
			handler = &scribeTextHandler{level: logLevel}
		}
		slog.SetDefault(slog.New(handler))
	})
}

// scribeTextHandler formats output as "[YYYY-MM-DD HH:MM] script: msg key=val",
// matching the pre-slog format that cron logs already grep for. Non-script
// attributes trail the message.
type scribeTextHandler struct {
	level *slog.LevelVar
	attrs []slog.Attr
	group string
}

func (h *scribeTextHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *scribeTextHandler) Handle(_ context.Context, r slog.Record) error {
	script := ""
	var tail []string
	collect := func(a slog.Attr) {
		if a.Key == "script" {
			script = a.Value.String()
			return
		}
		tail = append(tail, fmt.Sprintf("%s=%v", a.Key, a.Value.Any()))
	}
	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		collect(a)
		return true
	})

	ts := r.Time.Format("2006-01-02 15:04")
	prefix := fmt.Sprintf("[%s]", ts)
	if script != "" {
		prefix += " " + script + ":"
	}
	line := prefix + " " + r.Message
	if len(tail) > 0 {
		line += " " + strings.Join(tail, " ")
	}
	fmt.Fprintln(os.Stdout, line)
	return nil
}

func (h *scribeTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := *h
	out.attrs = append(out.attrs[:len(out.attrs):len(out.attrs)], attrs...)
	return &out
}

func (h *scribeTextHandler) WithGroup(name string) slog.Handler {
	out := *h
	out.group = name
	return &out
}
