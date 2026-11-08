package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/lease"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/queue"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
	"github.com/CrossTerra/beacon/internal/source"
)

type fakeSource struct {
	items []source.Item
	err   error
}

func (f *fakeSource) Name() string          { return "fx" }
func (f *fakeSource) Fields() []string      { return []string{"title", "label"} }
func (f *fakeSource) StateFields() []string { return nil }
func (f *fakeSource) Hosts() []string       { return nil }
func (f *fakeSource) Collect(context.Context) ([]source.Item, error) {
	return f.items, f.err
}

func items() []source.Item {
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	mk := func(n int, label string) source.Item {
		return source.Item{Source: "fx", Subject: fmt.Sprintf("fx:obj:%d", n), Key: fmt.Sprintf("fx:obj:%d:1", n), Type: "changed",
			OccurredAt: at.Add(time.Duration(n) * time.Minute), Actor: "fx:user:dana",
			Fields: map[string]string{"title": fmt.Sprintf("SECRET-TITLE-%d", n), "label": label}}
	}
	return []source.Item{mk(1, "send"), mk(2, "hold"), mk(3, "noise"), mk(4, "other")}
}

const ruleFile = `rules:
  - name: sends
    match: {fields: {label: send}}
    tier: send
    fields: [title]
  - name: holds
    match: {fields: {label: hold}}
    tier: hold
    fields: [title]
  - name: noise
    match: {fields: {label: noise}}
    tier: never
`

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type fixture struct {
	t      *testing.T
	dir    string
	server *signaltest.Server
	client *signal.Client
	log    bytes.Buffer
	clock  *clock
	src    *fakeSource
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, dir: t.TempDir(), server: signaltest.New("tok"), src: &fakeSource{items: items()},
		clock: &clock{time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC)}}
	t.Cleanup(f.server.Close)
	store := keychain.NewMemory()
	store.Put("a", "tok")
	secret, _ := store.Get("a")
	u, _ := url.Parse(f.server.URL)
	f.client = signal.New(u, egress.Transport([]string{egress.Address(u)}), secret)
	f.client.Sleep = func(context.Context, time.Duration) error { return nil }
	return f
}

// runner opens the state directory and returns a runner for one pass, and a function that closes it.
func (f *fixture) runner(send bool, w lease.Writer) (*Runner, func()) {
	f.t.Helper()
	rs, err := rules.Parse([]byte(ruleFile))
	if err != nil {
		f.t.Fatal(err)
	}
	r := &Runner{Sources: []source.Source{f.src}, Rules: rs, Person: project.Person{Handle: "@winton"}, Send: send,
		Log: logs.New(&f.log), Now: f.clock.now}
	var ob *outbox.Outbox
	if send {
		ob, err = outbox.Open(f.dir, f.clock.now)
	} else {
		ob, err = outbox.Read(f.dir)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var q *queue.Queue
	if send {
		q, err = queue.Open(f.dir, ob)
	} else {
		q, err = queue.Read(f.dir)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	r.Outbox, r.Queue = ob, q
	if w == nil {
		w = f.client
	}
	r.Writer = w
	r.Lease = &lease.Lease{Outbox: ob, Writer: w, Person: "@winton", Run: "test:lease", Period: 5 * time.Minute, Now: f.clock.now}
	return r, func() { ob.Close() }
}

func TestDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	r, done := f.runner(false, nil)
	defer done()
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s, h, n, k := rep.Counts(); s != 1 || h != 1 || n != 2 || k != 0 {
		t.Fatalf("counts %d %d %d %d", s, h, n, k)
	}
	entries, _ := os.ReadDir(f.dir)
	if len(entries) != 0 || len(f.server.Requests()) != 0 {
		t.Fatalf("dry run wrote %d files, made %d requests", len(entries), len(f.server.Requests()))
	}
}

func TestSendPass(t *testing.T) {
	f := newFixture(t)
	r, done := f.runner(true, nil)
	defer done()
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Lease.Held || len(rep.Results) != 1 || rep.Results[0].Key != "fx:obj:1:1" || rep.Results[0].Outcome != signal.Written || rep.Pending != 0 {
		t.Fatalf("%+v %+v", rep.Lease, rep.Results)
	}
	if !r.Queue.IsHeld("fx:obj:2:1") {
		t.Fatal("hold item not queued")
	}
	for _, req := range f.server.Requests() {
		if bytes.Contains(req.Body, []byte("SECRET-TITLE-2")) || bytes.Contains(req.Body, []byte("fx:obj:3")) {
			t.Fatalf("held or dropped item on the wire: %s", req.Body)
		}
	}
}

// The dropped record gets one line per dropped key, however many passes run.
func TestDroppedRecordDoesNotGrow(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 4; i++ {
		r, done := f.runner(true, nil)
		if _, err := r.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		done()
	}
	data, _ := os.ReadFile(filepath.Join(f.dir, queue.DroppedFile))
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("%d lines after four passes, want 2:\n%s", n, data)
	}
}

// blockingWriter answers lease claims through the fake Signal, refusing the claim on the next period so the lease
// ends with the current one, and holds every event write until its context ends.
type blockingWriter struct {
	next      lease.Writer
	nextClaim string
	events    int
}

func (b *blockingWriter) Write(ctx context.Context, body []byte) (signal.Result, error) {
	if bytes.Contains(body, []byte(`"key":"claim:`)) {
		if bytes.Contains(body, []byte(b.nextClaim)) {
			return signal.Result{Outcome: signal.Conflict, Status: 409, Code: "idempotency_conflict"}, nil
		}
		return b.next.Write(ctx, body)
	}
	b.events++
	<-ctx.Done()
	return signal.Result{}, ctx.Err()
}

// The lease bounds a send: a write still waiting when the lease ends is stopped, and its body stays pending.
func TestLeaseBoundsSend(t *testing.T) {
	f := newFixture(t)
	// 300 ms before the period ends; the next period belongs to someone else.
	f.clock.t = time.Date(2026, 11, 7, 12, 4, 59, 700_000_000, time.UTC)
	p := f.clock.t.Unix() / 300
	w := &blockingWriter{next: f.client, nextClaim: fmt.Sprintf("claim:beacon:lease:winton:%d", p+1)}
	r, done := f.runner(true, w)
	defer done()
	start := time.Now()
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second || w.events != 1 || rep.Pending != 1 {
		t.Fatalf("took %v, %d event writes, %d pending", time.Since(start), w.events, rep.Pending)
	}
	if !strings.Contains(f.log.String(), `"msg":"lease_expired"`) {
		t.Fatalf("log: %s", f.log.String())
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write(context.Context, []byte) (signal.Result, error) {
	return signal.Result{}, w.err
}

type refusingWriter struct{}

func (refusingWriter) Write(context.Context, []byte) (signal.Result, error) {
	return signal.Result{Outcome: signal.Refused, Status: 403, Code: "forbidden"}, nil
}

// A refused key or a refused claim fails the pass with the real reason, instead of reading as another holder.
func TestLeaseErrorsFailThePass(t *testing.T) {
	for name, c := range map[string]struct {
		w      lease.Writer
		fatal  bool
		reason logs.Reason
	}{
		"unauthorized": {failingWriter{signal.ErrUnauthorized}, true, logs.ReasonUnauthorized},
		"refused":      {refusingWriter{}, true, logs.ReasonLeaseRefused},
		"unavailable":  {failingWriter{fmt.Errorf("%w: x", signal.ErrUnavailable)}, false, logs.ReasonUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			r, done := f.runner(true, c.w)
			defer done()
			rep, err := r.Run(context.Background())
			if (err != nil) != c.fatal || rep.LeaseReason != c.reason || rep.Lease.Held || len(rep.Results) != 0 {
				t.Fatalf("err %v, reason %q, %+v", err, rep.LeaseReason, rep.Lease)
			}
			if err != nil && Classify(err) != c.reason {
				t.Fatalf("classified %q", Classify(err))
			}
		})
	}
}

// Logs carry reason codes, never error text.
func TestCollectErrorTextNotLogged(t *testing.T) {
	f := newFixture(t)
	f.src.err = errors.New(`fetching fx:obj:SECRET-KEY:1 matched regex "SECRET-PATTERN"`)
	r, done := f.runner(false, nil)
	defer done()
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := f.log.String()
	if strings.Contains(log, "SECRET") || !strings.Contains(log, `"reason":"source_failed"`) {
		t.Fatalf("log: %s", log)
	}
}

func TestClassify(t *testing.T) {
	cases := map[logs.Reason]error{
		logs.ReasonUnauthorized: fmt.Errorf("x: %w", signal.ErrUnauthorized),
		logs.ReasonUnavailable:  fmt.Errorf("%w: y", signal.ErrUnavailable),
		logs.ReasonLeaseRefused: &lease.RefusedError{Status: 403, Code: "forbidden"},
		logs.ReasonLeaseExpired: context.DeadlineExceeded,
		logs.ReasonCanceled:     context.Canceled,
		logs.ReasonEgress:       &url.Error{Op: "Post", URL: "x", Err: &egress.DeniedError{Address: "a:1"}},
		logs.ReasonLocked:       outbox.ErrLocked,
		logs.ReasonKeychain:     keychain.ErrNotFound,
		logs.ReasonState:        &fs.PathError{Op: "open", Path: "/state/outbox.jsonl", Err: fs.ErrPermission},
		logs.ReasonInternal:     errors.New(`outbox: key "fx:1" appended twice`),
	}
	for want, err := range cases {
		if got := Classify(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}
