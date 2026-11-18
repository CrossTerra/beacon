package coverage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/signal"
)

// Writer writes one body to Signal.
type Writer interface {
	Write(ctx context.Context, body []byte) (signal.Result, error)
}

// LeaseAPI is Signal's lease API, which holds the heartbeats.
type LeaseAPI interface {
	ClaimLease(ctx context.Context, name, run string, ttl time.Duration) (signal.LeaseResult, error)
}

// MinSpacing is the least time between two snapshots of one source, so a source that fails every other pass writes at
// most one event in that time.
const MinSpacing = 5 * time.Minute

// apiRetry is how long after a 404 the lease API is asked again for heartbeats, as for the send lease.
const apiRetry = time.Hour

// Subject is the subject of a source's coverage snapshots: one thread per source, which Signal keeps the latest
// snapshot of.
func Subject(person, source string) string {
	return "beacon:coverage:" + strings.TrimPrefix(person, "@") + ":" + source
}

// HeartbeatName is the name of a source's heartbeat lease in Signal.
func HeartbeatName(person, source string) string {
	return "beacon:heartbeat:" + strings.TrimPrefix(person, "@") + ":" + source
}

// HeartbeatTTL is how long a source's heartbeat lease is held: three times its interval, and at least six minutes, so
// a source whose pass runs long is not shown as stopped. It is renewed after a pass that collected, once a third of it
// has passed.
func HeartbeatTTL(every time.Duration) time.Duration {
	return 3 * max(every, 2*time.Minute)
}

// Snapshot is a source's coverage as Signal stores it: the state fields of a coverage event.
type Snapshot struct {
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	Failures     int    `json:"failures,omitempty"`
	FailingSince string `json:"failing_since,omitempty"`
	LastSuccess  string `json:"last_success,omitempty"`
	LastFailure  string `json:"last_failure,omitempty"`
	EverySeconds int64  `json:"every_seconds"`
	Cut          bool   `json:"cut"`
	Expired      bool   `json:"history_expired"`
	Paused       bool   `json:"paused"`
	RateLimited  bool   `json:"rate_limited"`
	Names        string `json:"names,omitempty"`
	// The channel fields are set for a source that reads channels (Slack).
	ChannelsTotal   *int   `json:"channels_total,omitempty"`
	ChannelsRead    *int   `json:"channels_read,omitempty"`
	ChannelsNever   *int   `json:"channels_never,omitempty"`
	ChannelsWaiting *int   `json:"channels_waiting,omitempty"`
	ChannelsGaps    *int   `json:"channels_gaps,omitempty"`
	ChannelsOldest  string `json:"channels_oldest_read,omitempty"`
	ChannelsBehind  *bool  `json:"channels_behind,omitempty"`
}

// SnapshotOf is s as Signal stores it, at now.
func SnapshotOf(s *Source, now time.Time) Snapshot {
	snap := Snapshot{Status: s.Status(now), EverySeconds: s.EverySeconds, Cut: s.Cut, Expired: s.Expired, Paused: s.Paused,
		RateLimited: s.Limited, Names: s.Names, LastSuccess: timeOf(s.LastSuccess), LastFailure: timeOf(s.LastFailure)}
	if s.LastFailure != nil {
		snap.Reason = s.Reason
	}
	if s.Failures > 0 {
		snap.Failures, snap.FailingSince = s.Failures, timeOf(s.FailingSince)
	}
	if c := s.Channels; c != nil {
		total, read, never, waiting, gaps, behind := c.Total, c.Read, c.Never, c.Waiting, c.Gaps, s.behind(now)
		snap.ChannelsTotal, snap.ChannelsRead, snap.ChannelsNever = &total, &read, &never
		snap.ChannelsWaiting, snap.ChannelsGaps, snap.ChannelsBehind = &waiting, &gaps, &behind
		snap.ChannelsOldest = timeOf(c.Oldest)
	}
	return snap
}

// signature is what a change of coverage is: the status and its reason, the flags, and the shape of the channel
// reach. Times and counts that move every pass are not in it, so an unchanged source writes nothing.
func (s Snapshot) signature() string {
	reason := ""
	if s.Status == StatusFailing {
		reason = s.Reason
	}
	ch := ""
	if s.ChannelsTotal != nil {
		ch = fmt.Sprintf("%d/%t/%t/%t/%t", *s.ChannelsTotal, deref(s.ChannelsNever) > 0, deref(s.ChannelsWaiting) > 0,
			deref(s.ChannelsGaps) > 0, s.ChannelsBehind != nil && *s.ChannelsBehind)
	}
	return fmt.Sprintf("%s|%s|%t|%t|%t|%t|%s|%d|%s", s.Status, reason, s.Cut, s.Expired, s.Paused, s.RateLimited, s.Names, s.EverySeconds, ch)
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func timeOf(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(millis)
}

const millis = "2006-01-02T15:04:05.000Z07:00"

type body struct {
	Subject    string `json:"subject"`
	Type       string `json:"type"`
	Key        string `json:"key"`
	OccurredAt string `json:"occurred_at"`
	Title      string `json:"title"`
	Open       struct {
		Title string `json:"title"`
	} `json:"open"`
	Metadata struct {
		Run string `json:"run"`
	} `json:"metadata"`
	State struct {
		Fields     Snapshot `json:"fields"`
		ObservedAt string   `json:"observed_at"`
		Version    string   `json:"version"`
	} `json:"state"`
}

// Reporter writes coverage to Signal for the instance that holds the send lease.
type Reporter struct {
	Outbox *outbox.Outbox
	Writer Writer
	// API holds the heartbeats. Nil writes no heartbeat.
	API    LeaseAPI
	Person string
	// Run names this instance in the heartbeats; the same across restarts, so a restart renews its own.
	Run string
	Now func() time.Time
	Log *logs.Logger

	renewed  map[string]time.Time
	retryAPI time.Time
}

// Report writes the named source's coverage, s, before until, the end of the send lease this instance holds. It sends
// a snapshot left pending first, then a new snapshot when the coverage changed and the last one is at least MinSpacing
// old, and renews the source's heartbeat after a pass that collected. Signal not answering, or refusing, costs nothing
// but the report: it is logged and the next pass tries again. Only a failure to record a body is returned.
func (r *Reporter) Report(ctx context.Context, name string, s *Source, until time.Time) error {
	now := r.Now()
	if s == nil || !now.Before(until) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, until.Sub(now))
	defer cancel()
	if err := r.snapshot(ctx, name, s, now); err != nil {
		return err
	}
	r.heartbeat(ctx, name, s, now)
	return nil
}

func (r *Reporter) snapshot(ctx context.Context, name string, s *Source, now time.Time) error {
	subject := Subject(r.Person, name)
	var last *outbox.Entry
	for _, e := range r.Outbox.Entries() {
		if e.Kind != outbox.Coverage || e.Subject != subject {
			continue
		}
		if e.State == outbox.Pending {
			// Signal has not answered an earlier snapshot. It goes first, as stored; a newer one waits behind it.
			res, err := r.Writer.Write(ctx, e.Body)
			if err != nil {
				return nil
			}
			if err := r.mark(e.Key, res); err != nil {
				return err
			}
			r.Log.Log(logs.Coverage, logs.Source(name), logs.CoverageStatus(statusOf(e.Body)), logs.Outcome(string(res.Outcome)), logs.Status(res.Status), logs.Code(res.Code))
			e, _ = r.Outbox.Get(e.Key)
		}
		last = &e
	}
	if s.Failures > 0 && s.Failures < FailingAfter {
		// One failure is not yet a change: the next pass says whether the source is failing or was not.
		return nil
	}
	snap := SnapshotOf(s, now)
	if last != nil {
		if now.Sub(last.AppendedAt) < MinSpacing {
			return nil
		}
		var prev body
		if json.Unmarshal(last.Body, &prev) == nil && prev.State.Fields.signature() == snap.signature() {
			return nil
		}
	}
	version := strconv.FormatInt(now.UnixMilli(), 10)
	var b body
	b.Subject = subject
	b.Type = "beacon.coverage"
	b.Key = subject + ":" + version
	b.OccurredAt = now.UTC().Format(millis)
	b.Title = name + ": " + snap.Status
	if snap.Status == StatusFailing {
		b.Title += " (" + snap.Reason + ")"
	}
	b.Open.Title = "Beacon coverage for " + r.Person + ": " + name
	b.Metadata.Run = r.Run
	b.State.Fields, b.State.ObservedAt, b.State.Version = snap, b.OccurredAt, version
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if _, exists := r.Outbox.Get(b.Key); exists {
		return nil
	}
	if err := r.Outbox.Append(outbox.Coverage, b.Key, subject, "", data); err != nil {
		return err
	}
	res, err := r.Writer.Write(ctx, data)
	if err != nil {
		// Stays pending, and goes first next time.
		r.Log.Log(logs.Coverage, logs.Source(name), logs.CoverageStatus(snap.Status), logs.Outcome("pending"))
		return nil
	}
	if err := r.mark(b.Key, res); err != nil {
		return err
	}
	r.Log.Log(logs.Coverage, logs.Source(name), logs.CoverageStatus(snap.Status), logs.Outcome(string(res.Outcome)), logs.Status(res.Status), logs.Code(res.Code))
	return nil
}

func statusOf(data []byte) string {
	var b body
	_ = json.Unmarshal(data, &b)
	return b.State.Fields.Status
}

func (r *Reporter) mark(key string, res signal.Result) error {
	switch res.Outcome {
	case signal.Written:
		return r.Outbox.Mark(key, outbox.Sent, res.EventID, res.Status, "", "")
	case signal.Replayed:
		return r.Outbox.Mark(key, outbox.Replayed, res.EventID, res.Status, "", "")
	}
	return r.Outbox.Mark(key, outbox.Rejected, "", res.Status, res.Code, res.Message)
}

// heartbeat renews the source's heartbeat lease after a pass that collected, once a third of its time has passed.
func (r *Reporter) heartbeat(ctx context.Context, name string, s *Source, now time.Time) {
	if r.API == nil || s.LastSuccess == nil || !s.LastSuccess.Equal(s.LastPass) || now.Before(r.retryAPI) {
		return
	}
	ttl := HeartbeatTTL(time.Duration(s.EverySeconds) * time.Second)
	if r.renewed == nil {
		r.renewed = map[string]time.Time{}
	}
	if last, ok := r.renewed[name]; ok && now.Sub(last) < ttl/3 {
		return
	}
	res, err := r.API.ClaimLease(ctx, HeartbeatName(r.Person, name), r.Run, ttl)
	var refused *signal.LeaseRefused
	switch {
	case errors.Is(err, signal.ErrNoLeases):
		// This Signal offers no leases: the snapshots are the whole report. Ask again in an hour.
		r.retryAPI = now.Add(apiRetry)
	case errors.As(err, &refused):
		r.Log.Log(logs.HeartbeatFailed, logs.Source(name), logs.Why(logs.ReasonLeaseRefused), logs.Status(refused.Status), logs.Code(refused.Code))
	case errors.Is(err, signal.ErrUnauthorized):
		r.Log.Log(logs.HeartbeatFailed, logs.Source(name), logs.Why(logs.ReasonUnauthorized))
	case errors.Is(err, context.DeadlineExceeded):
		r.Log.Log(logs.HeartbeatFailed, logs.Source(name), logs.Why(logs.ReasonLeaseExpired))
	case err != nil:
		r.Log.Log(logs.HeartbeatFailed, logs.Source(name), logs.Why(logs.ReasonUnavailable))
	case !res.Held:
		// Another instance's heartbeat for this source has not expired yet; it stops renewing once it stops sending.
	default:
		r.renewed[name] = now
	}
}
