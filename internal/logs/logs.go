// Package logs writes Beacon's log: JSON lines with fixed event names.
//
// A log line never holds item content, a rule's match on content, a key, a token, a request body or an error's
// text. The attribute helpers below take only counts, source names, outcomes, status codes, Signal's error codes and
// fixed reason codes, so a call site cannot pass content by accident.
package logs

import (
	"io"
	"log/slog"
	"regexp"
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
	// RunStopped is a poll loop that exited after SIGTERM. It is not a failure.
	RunStopped Event = "run_stopped"
	// Retry is the summary of beacon outbox retry. Its attributes are counts only.
	Retry Event = "retry"
	// Withheld is a personal source whose send items were held or not recorded, because its subject prefix is not
	// restricted in Signal or the restrictions could not be read.
	Withheld Event = "withheld"
	// HistoryGap is a source that could not read every change since its cursor, and re-read what it could.
	HistoryGap Event = "history_gap"
	// ReadCut is a source whose read stopped at its limit, so older items in its window were not read.
	ReadCut Event = "read_cut"
	// ReadPaused is a source whose read stopped at a rate limit and will continue next pass. Not a failure.
	ReadPaused Event = "read_paused"
	// RateLimited is what an API said when its rate limit stopped a source: the status, the API's reason code, the
	// Retry-After it named, and the units the source had spent in the last minute when it keeps a budget.
	RateLimited Event = "rate_limited"
	// RestrictionsUnread is a send pass that could not read Signal's restricted prefixes.
	RestrictionsUnread Event = "restrictions_unread"
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
	// ReasonSigning is a binary whose certificate is not the one install recorded.
	ReasonSigning Reason = "signing_mismatch"
	ReasonItem    Reason = "item_invalid"
	ReasonProject Reason = "project_failed"
	// ReasonUnrestricted is a personal subject prefix Signal does not restrict: its events would be readable by
	// every reader key.
	ReasonUnrestricted Reason = "prefix_unrestricted"
	// ReasonRestrictionsUnknown is a pass that could not read Signal's restricted prefixes.
	ReasonRestrictionsUnknown Reason = "restrictions_unknown"
	// ReasonNoRead is a Signal key without the read scope, which listing the restrictions needs.
	ReasonNoRead Reason = "no_read_scope"
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

// LimitReason is an API's own rate-limit reason code. Anything but a short identifier is logged as "other", so a
// source cannot put text in the log through it.
func LimitReason(r string) Attr {
	if !limitReason.MatchString(r) {
		r = "other"
	}
	return Attr{slog.String("limit_reason", r)}
}

var limitReason = regexp.MustCompile(`^[A-Za-z_]{0,40}$`)

// RetryAfter is a Retry-After in whole seconds, rounded up; -1 when the answer named none.
func RetryAfter(d time.Duration) Attr {
	s := int64(-1)
	if d >= 0 {
		s = int64((d + time.Second - 1) / time.Second)
	}
	return Attr{slog.Int64("retry_after", s)}
}

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
