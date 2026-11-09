// Package logs writes Beacon's log: JSON lines with fixed event names.
//
// A log line never holds item content, a rule's match on content, a key, a token, a request body or an error's
// text. The attribute helpers below take only counts, source names, outcomes, status codes, Signal's error codes and
// fixed reason codes, so a call site cannot pass content by accident.
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
	LeaseExpired    Event = "lease_expired"
	RunFailed       Event = "run_failed"
	// Retry is the summary of beacon outbox retry. Its attributes are counts only.
	Retry Event = "retry"
)

// Reason is why something failed, as a fixed code. Errors are never logged as text: their text can name keys, paths
// and rule patterns.
type Reason string

// Reasons.
const (
	ReasonConfig       Reason = "config"
	ReasonSource       Reason = "source_failed"
	ReasonUnauthorized Reason = "unauthorized"
	ReasonUnavailable  Reason = "unavailable"
	ReasonLeaseRefused Reason = "lease_refused"
	ReasonLeaseExpired Reason = "lease_expired"
	ReasonEgress       Reason = "egress_denied"
	ReasonLocked       Reason = "outbox_locked"
	ReasonState        Reason = "state_files"
	ReasonKeychain     Reason = "keychain"
	ReasonCanceled     Reason = "canceled"
	ReasonInternal     Reason = "internal"
	ReasonRateLimit    Reason = "rate_limited"
	ReasonSourceAuth   Reason = "source_unauthorized"
	ReasonScope        Reason = "scope_denied"
	ReasonIdentity     Reason = "identity_mismatch"
	ReasonItem         Reason = "item_invalid"
	ReasonProject      Reason = "project_failed"
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

// Why is a failure's reason code.
func Why(r Reason) Attr { return Attr{slog.String("reason", string(r))} }

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
