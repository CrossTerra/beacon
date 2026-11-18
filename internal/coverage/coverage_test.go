package coverage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
	"github.com/CrossTerra/beacon/internal/source"
)

var t0 = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

func TestObserveAndStatus(t *testing.T) {
	var r Record
	r.Observe("gmail", 30*time.Second, Pass{Took: 1200 * time.Millisecond}, t0)
	s := r.Sources["gmail"]
	if s.Status(t0) != StatusOK || s.LastSuccess == nil || s.PassMillis != 1200 || s.EverySeconds != 30 {
		t.Fatalf("%+v", s)
	}
	// One failure is recorded, and is not failing yet.
	r.Observe("gmail", 30*time.Second, Pass{Failed: "source_unauthorized"}, t0.Add(time.Minute))
	if s.Status(t0) != StatusOK || s.Failures != 1 || s.Reason != "source_unauthorized" || !s.FailingSince.Equal(t0.Add(time.Minute)) {
		t.Fatalf("%+v", s)
	}
	// Two in a row is failing, since the first.
	r.Observe("gmail", 30*time.Second, Pass{Failed: "source_unauthorized"}, t0.Add(2*time.Minute))
	if s.Status(t0) != StatusFailing || s.Failures != 2 || !s.FailingSince.Equal(t0.Add(time.Minute)) || !s.LastSuccess.Equal(t0) {
		t.Fatalf("%+v", s)
	}
	// A pass that collects ends it; the last failure stays on record.
	r.Observe("gmail", 30*time.Second, Pass{Cut: true}, t0.Add(3*time.Minute))
	if s.Status(t0) != StatusLimited || s.Failures != 0 || s.FailingSince != nil || s.LastFailure == nil {
		t.Fatalf("%+v", s)
	}

	// Slack's channels: waiting past the per-pass limit, or a channel unread for longer than Behind, is limited.
	oldest := t0.Add(-10 * time.Minute)
	r.Observe("slack", 15*time.Second, Pass{Reach: &source.Reach{Total: 140, Read: 40, Oldest: oldest}}, t0)
	sl := r.Sources["slack"]
	if sl.Status(t0) != StatusOK || sl.Channels.Total != 140 || !sl.Channels.Oldest.Equal(oldest) {
		t.Fatalf("%+v %+v", sl, sl.Channels)
	}
	if sl.Status(t0.Add(25*time.Minute)) != StatusLimited {
		t.Fatal("a channel unread for 35 minutes is not limited")
	}
	r.Observe("slack", 15*time.Second, Pass{Reach: &source.Reach{Total: 140, Read: 40, Waiting: 3, Oldest: t0}}, t0)
	if sl.Status(t0) != StatusLimited {
		t.Fatal("channels waiting past the limit is not limited")
	}

	r.Keep([]string{"slack"})
	if r.Sources["gmail"] != nil || r.Sources["slack"] == nil {
		t.Fatal("Keep")
	}
}

func TestReadWrite(t *testing.T) {
	dir := t.TempDir()
	if r, err := Read(dir); err != nil || len(r.Sources) != 0 {
		t.Fatalf("missing file: %+v %v", r, err)
	}
	var r Record
	r.Observe("slack", 15*time.Second, Pass{Reach: &source.Reach{Total: 3, Read: 1, Never: 1, Oldest: t0}}, t0)
	r.Asleep(t0.Add(-8*time.Hour), t0)
	if err := Write(dir, r); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi.Mode(), err)
	}
	got, err := Read(dir)
	if err != nil || got.Sources["slack"].Channels.Never != 1 || !got.Slept.To.Equal(t0) {
		t.Fatalf("%+v %v", got, err)
	}
	// A record this version did not write is refused, and the error does not repeat it.
	for _, bad := range []string{
		`{"sources":{"Slack!":{"every_seconds":15,"last_pass":"2026-11-18T09:00:00Z","pass_ms":1}}}`,
		`{"sources":{"slack":{"every_seconds":15,"last_pass":"2026-11-18T09:00:00Z","pass_ms":1,"reason":"token xoxp-123 refused"}}}`,
		`{"sources":{"slack":{"every_seconds":15,"last_pass":"2026-11-18T09:00:00Z","pass_ms":1,"names":"Dana"}}}`,
		`{"sources":{"slack":{"every_seconds":15,"pass_ms":1}}}`,
		`not json`,
	} {
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(dir); !errors.Is(err, ErrRecord) || strings.Contains(err.Error(), "xoxp") || strings.Contains(err.Error(), "Dana") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
}

func TestFormat(t *testing.T) {
	var r Record
	r.Observe("gmail", 30*time.Second, Pass{}, t0.Add(-6*time.Hour))
	for i := 0; i < 720; i++ {
		r.Observe("gmail", 30*time.Second, Pass{Failed: "source_unauthorized"}, t0.Add(-6*time.Hour+time.Duration(i+1)*30*time.Second))
	}
	r.Observe("slack", 15*time.Second, Pass{Took: 40 * time.Second, Limited: true, Reach: &source.Reach{Total: 140, Read: 40, Never: 2, Waiting: 5, Oldest: t0.Add(-14 * time.Minute)}}, t0)
	r.Asleep(t0.Add(-9*time.Hour), t0.Add(-7*time.Hour))
	out := Format(r, t0)
	for _, want := range []string{
		"source: gmail\n  status: failing\n  every: 30s\n",
		"  last_success: 2026-11-18T03:00:00Z (6h0m0s ago)\n",
		"  last_failure: 2026-11-18T09:00:00Z (0s ago) source_unauthorized\n",
		"  failing: 720 passes in a row, since 2026-11-18T03:00:30Z (5h59m30s ago)\n",
		"source: slack\n  status: limited\n  every: 15s\n  last_pass: 2026-11-18T09:00:00Z (0s ago), collect took 40s\n",
		"  flags: rate_limited\n",
		"  channels: 40 read of 140, 2 never read, 5 waiting, 0 with a gap, oldest read 2026-11-18T08:46:00Z (14m0s ago)\n",
		"asleep: 2026-11-18T00:00:00Z to 2026-11-18T02:00:00Z (2h0m0s)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(Format(Record{}, t0), "coverage: none recorded yet") {
		t.Fatal(Format(Record{}, t0))
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func reporter(t *testing.T, s *signaltest.Server, c *clock, log *bytes.Buffer) (*Reporter, *outbox.Outbox) {
	t.Helper()
	store := keychain.NewMemory()
	store.Put("a", "tok")
	secret, _ := store.Get("a")
	u, _ := url.Parse(s.URL)
	cl := signal.New(u, egress.Guard([]string{egress.Address(u)}, http.DefaultTransport), secret)
	cl.Attempts = 1
	ob, err := outbox.Open(t.TempDir(), c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	return &Reporter{Outbox: ob, Writer: cl, API: cl, Person: "@winton", Run: "0123456789abcdef:heartbeat", Now: c.now, Log: logs.New(log)}, ob
}

func coverageEvents(s *signaltest.Server) []body {
	var out []body
	for _, e := range s.Events() {
		var b body
		if json.Unmarshal(e.Body, &b) == nil && b.Type == "beacon.coverage" {
			out = append(out, b)
		}
	}
	return out
}

// A source's coverage reaches Signal as a snapshot when it changes, at most once per MinSpacing, and its heartbeat lease
// is renewed after each pass that collected, once a third of its time has passed.
func TestReportWritesOnChange(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	s.Leases = true
	c := &clock{t0}
	var log bytes.Buffer
	r, ob := reporter(t, s, c, &log)
	until := t0.Add(time.Hour)
	var rec Record
	pass := func(p Pass) {
		t.Helper()
		rec.Observe("gmail", 30*time.Second, p, c.t)
		if err := r.Report(context.Background(), "gmail", rec.Sources["gmail"], until); err != nil {
			t.Fatal(err)
		}
	}

	pass(Pass{})
	evs := coverageEvents(s)
	if len(evs) != 1 || evs[0].Subject != "beacon:coverage:winton:gmail" || evs[0].State.Fields.Status != StatusOK ||
		evs[0].Title != "gmail: ok" || evs[0].Key != "beacon:coverage:winton:gmail:"+evs[0].State.Version {
		t.Fatalf("%+v", evs)
	}
	if s.LeaseClaims() != 1 {
		t.Fatalf("heartbeats %d", s.LeaseClaims())
	}
	// Unchanged: no event; the heartbeat waits a third of its six minutes.
	c.t = c.t.Add(30 * time.Second)
	pass(Pass{})
	if len(coverageEvents(s)) != 1 || s.LeaseClaims() != 1 {
		t.Fatalf("events %d heartbeats %d", len(coverageEvents(s)), s.LeaseClaims())
	}
	// Gmail starts refusing the token. The first failure is not failing; the second is, and is written at once.
	c.t = t0.Add(10 * time.Minute)
	pass(Pass{Failed: "source_unauthorized"})
	if len(coverageEvents(s)) != 1 {
		t.Fatal("one failure was reported")
	}
	c.t = c.t.Add(30 * time.Second)
	pass(Pass{Failed: "source_unauthorized"})
	evs = coverageEvents(s)
	if len(evs) != 2 {
		t.Fatalf("%d events", len(evs))
	}
	f := evs[1].State.Fields
	if f.Status != StatusFailing || f.Reason != "source_unauthorized" || f.Failures != 2 || f.LastSuccess == "" || evs[1].Title != "gmail: failing (source_unauthorized)" {
		t.Fatalf("%+v", evs[1])
	}
	// A failing source renews no heartbeat.
	claims := s.LeaseClaims()
	c.t = c.t.Add(10 * time.Minute)
	pass(Pass{Failed: "source_unauthorized"})
	if s.LeaseClaims() != claims || len(coverageEvents(s)) != 2 {
		t.Fatalf("heartbeats %d events %d", s.LeaseClaims(), len(coverageEvents(s)))
	}
	// It recovers a minute after the last snapshot: that waits for MinSpacing, then is written once.
	c.t = t0.Add(11 * time.Minute)
	pass(Pass{})
	if len(coverageEvents(s)) != 2 {
		t.Fatal("a snapshot within MinSpacing")
	}
	c.t = t0.Add(16 * time.Minute)
	pass(Pass{})
	c.t = c.t.Add(30 * time.Second)
	pass(Pass{})
	evs = coverageEvents(s)
	if len(evs) != 3 || evs[2].State.Fields.Status != StatusOK || evs[2].State.Fields.LastFailure == "" {
		t.Fatalf("%+v", evs)
	}
	// Every snapshot is in the outbox, as coverage, sent.
	n := 0
	for _, e := range ob.Entries() {
		if e.Kind == outbox.Coverage {
			n++
			if e.State != outbox.Sent {
				t.Fatalf("%+v", e)
			}
		}
	}
	if n != 3 {
		t.Fatalf("%d coverage entries", n)
	}
	if strings.Contains(log.String(), "tok") {
		t.Fatal("the log holds the key")
	}
}

// A snapshot Signal did not answer stays pending, and goes first, as stored, before any newer one.
func TestReportResendsPending(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	down := true
	s.Script = func(_ int, req signaltest.Request) signaltest.Reply {
		if down && req.Path == "/api/events" {
			return signaltest.Reply{Status: http.StatusServiceUnavailable}
		}
		return signaltest.Reply{}
	}
	c := &clock{t0}
	var log bytes.Buffer
	r, ob := reporter(t, s, c, &log)
	var rec Record
	rec.Observe("jira", time.Minute, Pass{}, c.t)
	if err := r.Report(context.Background(), "jira", rec.Sources["jira"], t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	pending := ob.Pending()
	if len(pending) != 1 || pending[0].Kind != outbox.Coverage {
		t.Fatalf("%+v", pending)
	}
	down = false
	c.t = c.t.Add(time.Minute)
	if err := r.Report(context.Background(), "jira", rec.Sources["jira"], t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ev, ok := s.Event(pending[0].Key)
	if !ok || !bytes.Equal(ev.Body, pending[0].Body) || len(coverageEvents(s)) != 1 {
		t.Fatalf("resent %v, %d events", ok, len(coverageEvents(s)))
	}
	// Signal offers no leases here: no heartbeat, and nothing fails.
	if strings.Contains(log.String(), "heartbeat_failed") {
		t.Fatal(log.String())
	}
}

// Nothing is sent once the send lease has run out.
func TestReportStopsAtTheLease(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	s.Leases = true
	c := &clock{t0}
	var log bytes.Buffer
	r, _ := reporter(t, s, c, &log)
	var rec Record
	rec.Observe("drive", 2*time.Minute, Pass{}, c.t)
	if err := r.Report(context.Background(), "drive", rec.Sources["drive"], t0); err != nil {
		t.Fatal(err)
	}
	if len(s.Requests()) != 0 {
		t.Fatalf("%d requests", len(s.Requests()))
	}
}

func TestHeartbeatTTL(t *testing.T) {
	if HeartbeatTTL(15*time.Second) != 6*time.Minute || HeartbeatTTL(5*time.Minute) != 15*time.Minute {
		t.Fatal(HeartbeatTTL(15*time.Second), HeartbeatTTL(5*time.Minute))
	}
}
