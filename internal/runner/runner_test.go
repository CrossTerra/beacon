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
	"sync/atomic"
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
	items   []source.Item
	dropped int
	err     error
	next    []byte
	cursor  []byte
}

func (f *fakeSource) Name() string          { return "fx" }
func (f *fakeSource) Fields() []string      { return []string{"title", "label", "body"} }
func (f *fakeSource) StateFields() []string { return nil }
func (f *fakeSource) Hosts() []string       { return nil }
func (f *fakeSource) Collect(_ context.Context, cursor []byte) (source.Collected, error) {
	f.cursor = append([]byte(nil), cursor...)
	return source.Collected{Items: f.items, Next: f.next, Dropped: f.dropped}, f.err
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

type memCursor struct {
	m       map[string][]byte
	commits int
}

func (m *memCursor) Get(name string) ([]byte, bool) {
	b, ok := m.m[name]
	return append([]byte(nil), b...), ok
}

func (m *memCursor) Commit(name string, next []byte) error {
	if m.m == nil {
		m.m = map[string][]byte{}
	}
	m.m[name] = append([]byte(nil), next...)
	m.commits++
	return nil
}

// The cursor moves only after a send pass has decided every item, and not on a dry run or a rate limit.
func TestCursorCommitsAfterDecide(t *testing.T) {
	f := newFixture(t)
	f.src.next = []byte("cursor-1")
	cur := &memCursor{}
	r, done := f.runner(true, nil)
	r.Cursors = cur
	defer done()
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cur.commits != 1 || string(cur.m["fx"]) != "cursor-1" {
		t.Fatalf("%d %q", cur.commits, cur.m["fx"])
	}
	f.src.next = []byte("cursor-2")
	r2, done2 := f.runner(false, nil)
	r2.Cursors = cur
	defer done2()
	if _, err := r2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if string(cur.m["fx"]) != "cursor-1" {
		t.Fatal("a dry run moved the cursor")
	}

	done()
	f.src.err = source.ErrRateLimited
	f.src.next = []byte("nope")
	r3, done3 := f.runner(true, nil)
	r3.Cursors = &memCursor{}
	defer done3()
	if _, err := r3.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r3.Cursors.(*memCursor).commits != 0 {
		t.Fatal("a rate limit moved the cursor")
	}
}

func TestUnacceptedAndSeed(t *testing.T) {
	f := newFixture(t)
	f.src.next = []byte("c")
	cur := &memCursor{}
	r, done := f.runner(true, nil)
	r.Cursors = cur
	r.Unaccepted = map[string]bool{"fx": true}
	defer done()
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cur.commits != 0 || !rep.Items[0].Unaccepted {
		t.Fatal("an unaccepted source was recorded or not marked")
	}
	if _, ok := r.Outbox.Get("fx:obj:1:1"); ok {
		t.Fatal("an unaccepted item was appended")
	}

	f = newFixture(t)
	r, done = f.runner(true, nil)
	r.Seed = seedDrop{"fx:obj:1": true}
	defer done()
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Outbox.Get("fx:obj:1:1"); ok {
		t.Fatal("a seeded never was sent")
	}
	if rule, ok := r.Queue.DroppedBy("fx:obj:1:1"); !ok || rule != "seed" {
		t.Fatalf("dropped by %q", rule)
	}
}

type seedDrop map[string]bool

func (s seedDrop) Drops(subject string) bool { return s[subject] }

// A bad item and a projection error are dropped and logged by count. They do not freeze the source.
func TestBadItemAndProjectionDoNotFreeze(t *testing.T) {
	f := newFixture(t)
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	good := source.Item{Source: "fx", Subject: "fx:obj:1", Key: "fx:obj:1:1", Type: "changed", OccurredAt: at,
		Fields: map[string]string{"title": "ok", "label": "send"}}
	bad := good
	bad.Key = "fx:obj:2:1"
	bad.Subject = "fx:obj:2"
	bad.Fields = map[string]string{"title": "SECRET-ITEM-TEXT", "nope": "SECRET-FIELD"}
	huge := good
	huge.Key = "fx:obj:3:1"
	huge.Subject = "fx:obj:3"
	huge.Fields = map[string]string{"title": "ok", "label": "send", "body": strings.Repeat("S", 70*1024)}
	stuck := good
	stuck.Key = "fx:obj:4:1"
	stuck.Subject = "fx:obj:4"
	stuck.Fields = map[string]string{"title": "ok", "label": strings.Repeat("L", 70*1024)}
	f.src.items = []source.Item{good, bad, huge, stuck}
	f.src.next = []byte("cursor-ok")
	cur := &memCursor{}
	rs, err := rules.Parse([]byte("rules:\n  - name: sends\n    match: {fields: {label: send}}\n    tier: send\n    fields: [title, body]\n  - name: labels\n    match: {}\n    tier: send\n    fields: [title, label]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, done := f.runner(true, nil)
	r.Rules = rs
	r.Cursors = cur
	defer done()
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cur.commits != 1 || string(cur.m["fx"]) != "cursor-ok" {
		t.Fatalf("cursor frozen: %d %q", cur.commits, cur.m["fx"])
	}
	if _, ok := r.Outbox.Get("fx:obj:1:1"); !ok {
		t.Fatal("the good item was not recorded")
	}
	if _, ok := r.Outbox.Get("fx:obj:2:1"); ok {
		t.Fatal("the invalid item was recorded")
	}
	sent, ok := r.Outbox.Get("fx:obj:3:1")
	if !ok || strings.Contains(string(sent.Body), "SSSS") || !strings.Contains(string(sent.Body), `"truncated":true`) {
		t.Fatal("oversized body was dropped or sent whole")
	}
	if _, ok := r.Outbox.Get("fx:obj:4:1"); ok {
		t.Fatal("an item that still does not fit was sent")
	}
	if !r.Queue.IsHeld("fx:obj:4:1") {
		t.Fatal("an item that still does not fit was dropped")
	}
	log := f.log.String()
	if strings.Contains(log, "SECRET") || strings.Contains(log, "fx:obj") {
		t.Fatalf("log holds content: %s", log)
	}
	if !strings.Contains(log, `"msg":"item_invalid"`) || !strings.Contains(log, `"reason":"item_invalid"`) || !strings.Contains(log, `"items":1`) {
		t.Fatalf("invalid item not counted: %s", log)
	}
	if !strings.Contains(log, `"msg":"project_failed"`) || !strings.Contains(log, `"reason":"project_failed"`) {
		t.Fatalf("projection error not counted: %s", log)
	}
	for _, p := range rep.Items {
		if p.Item.Key == "fx:obj:4:1" && p.Err == nil {
			t.Fatal("an item that still does not fit was not held as a projection error")
		}
	}
}

func TestDroppedDoesNotUndercountCollected(t *testing.T) {
	f := newFixture(t)
	f.src.items = items()[:1]
	f.src.dropped = 3
	r, done := f.runner(false, nil)
	defer done()
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := f.log.String()
	if strings.Contains(log, `"items":-`) || !strings.Contains(log, `"msg":"collected"`) {
		t.Fatalf("collected count: %s", log)
	}
	if !strings.Contains(log, `"msg":"item_invalid"`) || !strings.Contains(log, `"items":3`) {
		t.Fatalf("dropped count: %s", log)
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

// A key the same rule already dropped is an earlier decision: later passes report it as skipped, not as a new never.
// A rules change that drops it by another rule is a new decision again.
func TestDroppedEarlierIsSkipped(t *testing.T) {
	f := newFixture(t)
	r, done := f.runner(true, nil)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	done()
	for _, send := range []bool{true, false} {
		r, done := f.runner(send, nil)
		rep, err := r.Run(context.Background())
		done()
		if err != nil {
			t.Fatal(err)
		}
		if s, h, n, k := rep.Counts(); s != 0 || h != 0 || n != 0 || k != 4 {
			t.Fatalf("send=%v: counts %d %d %d %d, want every item handled earlier", send, s, h, n, k)
		}
		for _, p := range rep.Items {
			if p.Tier == rules.Never && p.Skipped != "dropped earlier" {
				t.Fatalf("send=%v: %s skipped %q", send, p.Item.Key, p.Skipped)
			}
		}
	}
	r, done = f.runner(true, nil)
	defer done()
	r.Rules, _ = rules.Parse([]byte(strings.Replace(ruleFile, "name: noise", "name: renamed-noise", 1)))
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, n, _ := rep.Counts(); n != 1 {
		t.Fatalf("a key another rule drops is a new decision; never count %d", n)
	}
}

// SIGTERM finishes the in-flight send and does not start the next one.
func TestStopFinishesCurrentSend(t *testing.T) {
	f := newFixture(t)
	f.src.items = nil
	w := &blockWriter{entered: make(chan struct{}), release: make(chan struct{})}
	r, done := f.runner(true, w)
	defer done()
	p := f.clock.t.Unix() / int64((5*time.Minute)/time.Second)
	for _, n := range []int64{p, p + 1} {
		key := r.Lease.Key(n)
		if err := r.Outbox.Append(outbox.Lease, key, "beacon:lease:winton", "", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := r.Outbox.Mark(key, outbox.Sent, "evt", 201, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"fx:a:1", "fx:b:1"} {
		if err := r.Outbox.Append(outbox.Event, key, "fx:obj", "sends", []byte(`{"key":"`+key+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	var stop atomic.Bool
	r.Stop = func() bool { return stop.Load() }
	errc := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background())
		errc <- err
	}()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("send did not start")
	}
	stop.Store(true)
	close(w.release)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pass did not finish")
	}
	if w.n != 1 {
		t.Fatalf("writes %d, want the in-flight send only", w.n)
	}
	a, _ := r.Outbox.Get("fx:a:1")
	b, _ := r.Outbox.Get("fx:b:1")
	if a.State != outbox.Sent || b.State != outbox.Pending {
		t.Fatalf("first %s, second %s", a.State, b.State)
	}
}

type blockWriter struct {
	n       int
	entered chan struct{}
	release chan struct{}
}

func (w *blockWriter) Write(ctx context.Context, _ []byte) (signal.Result, error) {
	w.n++
	if w.n == 1 {
		close(w.entered)
		select {
		case <-w.release:
		case <-ctx.Done():
			return signal.Result{}, ctx.Err()
		}
	}
	return signal.Result{Outcome: signal.Written, Status: 201, EventID: "evt_x"}, nil
}
