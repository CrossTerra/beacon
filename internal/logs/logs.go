// Package logs writes Beacon's log: JSON lines with fixed event names.
//
// A log line never holds item content, a rule's match on content, a key, a token or a request body. The attribute
// helpers below take only counts, source and rule names, outcomes, status codes and Signal's error codes, so a call
// site cannot pass content by accident.
package logs

import (
	"io"
	"log/slog"
	"time"
)

// Event is a log event name. Every name Beacon logs is declared here.
type Event string

// Events.
const (
	RunStarted      Event = "run_started"
	RunFinished     Event = "run_finished"
	CollectFailed   Event = "collect_failed"
	Collected       Event = "collected"
	ItemInvalid     Event = "item_invalid"
	ProjectFailed   Event = "project_failed"
	Decided         Event = "decided"
	LeaseHeld       Event = "lease_held"
	LeaseNotHeld    Event = "lease_not_held"
	LeaseFailed     Event = "lease_failed"
	SendResult      Event = "send_result"
	SendStopped     Event = "send_stopped"
	DryRunNoSending Event = "dry_run"
)

// Attr is a log attribute built by one of the helpers below.
type Attr struct{ a slog.Attr }

// Count is a number of things.
func Count(name string, n int) Attr { return Attr{slog.Int(name, n)} }

// Source is a source name from the config.
func Source(name string) Attr { return Attr{slog.String("source", name)} }

// Mode is the send mode.
func Mode(m string) Attr { return Attr{slog.String("mode", m)} }

// Outcome is a send outcome.
func Outcome(o string) Attr { return Attr{slog.String("outcome", o)} }

// Status is an HTTP status.
func Status(s int) Attr { return Attr{slog.Int("status", s)} }

// Code is Signal's error code.
func Code(c string) Attr { return Attr{slog.String("code", c)} }

// Period is a lease period number.
func Period(p int64) Attr { return Attr{slog.Int64("period", p)} }

// Until is a time.
func Until(t time.Time) Attr { return Attr{slog.Time("until", t)} }

// Kind is an outbox kind.
func Kind(k string) Attr { return Attr{slog.String("kind", k)} }

// Err is an error from Beacon's own code. Beacon's errors name files, keys' sources and HTTP statuses, not content.
func Err(err error) Attr { return Attr{slog.String("error", err.Error())} }

// Logger writes events.
type Logger struct{ l *slog.Logger }

// New returns a logger writing JSON lines to w.
func New(w io.Writer) *Logger {
	return &Logger{l: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))}
}

// Discard returns a logger that writes nothing.
func Discard() *Logger { return New(io.Discard) }

// Log writes one event.
func (l *Logger) Log(e Event, attrs ...Attr) {
	args := make([]any, 0, len(attrs))
	for _, a := range attrs {
		args = append(args, a.a)
	}
	l.l.Info(string(e), args...)
}
