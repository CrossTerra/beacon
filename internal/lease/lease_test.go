package lease_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/lease"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func instance(t *testing.T, s *signaltest.Server, dir, run string, c *clock) (*lease.Lease, *outbox.Outbox) {
	t.Helper()
	store := keychain.NewMemory()
	store.Put("a", "tok")
	secret, _ := store.Get("a")
	u, _ := url.Parse(s.URL)
	cl := signal.New(u, egress.Guard([]string{egress.Address(u)}, http.DefaultTransport), secret)
	ob, err := outbox.Open(dir, c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	return &lease.Lease{Outbox: ob, Writer: cl, Person: "@winton", Run: run, Period: 5 * time.Minute, Now: c.now}, ob
}

func TestOneHolder(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	c := &clock{time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC)}
	a, _ := instance(t, s, t.TempDir(), "a:lease", c)
	b, _ := instance(t, s, t.TempDir(), "b:lease", c)

	st, err := a.Ensure(context.Background())
	if err != nil || !st.Held {
		t.Fatalf("a: %+v %v", st, err)
	}
	if want := time.Date(2026, 11, 7, 12, 10, 0, 0, time.UTC); !st.Until.Equal(want) {
		t.Fatalf("a holds until %v, want %v (current and next period)", st.Until, want)
	}
	st, err = b.Ensure(context.Background())
	if err != nil || st.Held {
		t.Fatalf("b took a held lease: %+v %v", st, err)
	}

	// The holder renews each period; the other never gets in while it does.
	for i := 0; i < 6; i++ {
		c.t = c.t.Add(5 * time.Minute)
		if st, _ := a.Ensure(context.Background()); !st.Held {
			t.Fatalf("period %d: a lost the lease", i)
		}
		if st, _ := b.Ensure(context.Background()); st.Held {
			t.Fatalf("period %d: b took the lease", i)
		}
	}

	// The holder stops. Within two periods the other takes over.
	c.t = c.t.Add(5 * time.Minute)
	if st, _ := b.Ensure(context.Background()); st.Held {
		t.Fatal("b took the period a claimed ahead")
	}
	c.t = c.t.Add(5 * time.Minute)
	if st, _ := b.Ensure(context.Background()); !st.Held {
		t.Fatal("b did not take the lease after a stopped")
	}
}

func TestClaimShape(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	c := &clock{time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC)}
	a, _ := instance(t, s, t.TempDir(), "a:lease", c)
	_, _ = a.Ensure(context.Background())
	c.t = c.t.Add(5 * time.Minute)
	_, _ = a.Ensure(context.Background())

	evs := s.Events()
	if len(evs) != 3 {
		t.Fatalf("%d claims, want 3", len(evs))
	}
	var last struct {
		Subject  string            `json:"subject"`
		Key      string            `json:"key"`
		To       []string          `json:"to"`
		Closes   []map[string]any  `json:"closes"`
		Metadata map[string]any    `json:"metadata"`
		Open     map[string]string `json:"open"`
	}
	if err := json.Unmarshal(evs[2].Body, &last); err != nil {
		t.Fatal(err)
	}
	if last.Subject != "beacon:lease:winton" || !strings.HasPrefix(last.Key, "claim:beacon:lease:winton:") ||
		len(last.To) != 1 || last.To[0] != "me" || last.Metadata["run"] != "a:lease" || last.Open["title"] == "" {
		t.Fatalf("claim: %s", evs[2].Body)
	}
	if len(last.Closes) != 1 || last.Closes[0]["key"] != evs[1].Key {
		t.Fatalf("claim does not close the previous one: %s", evs[2].Body)
	}
}

// A restarted instance has the same run and outbox: it replays its own claim and keeps the lease.
func TestRestartKeepsLease(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	c := &clock{time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC)}
	dir := t.TempDir()
	a, ob := instance(t, s, dir, "a:lease", c)
	// Signal writes the claim, but the answer is lost.
	s.Script = func(n int, _ signaltest.Request) signaltest.Reply {
		if n == 1 {
			return signaltest.Reply{Status: 503, Body: `{}`, Apply: true}
		}
		return signaltest.Reply{}
	}
	a.Writer.(*signal.Client).Attempts = 1
	if st, _ := a.Ensure(context.Background()); st.Held {
		t.Fatal("held without an answer")
	}
	ob.Close()
	s.Script = nil
	a2, ob2 := instance(t, s, dir, "a:lease", c)
	st, err := a2.Ensure(context.Background())
	if err != nil || !st.Held {
		t.Fatalf("restart: %+v %v", st, err)
	}
	e, _ := ob2.Get(a2.Key(st.Period))
	if e.State != outbox.Replayed {
		t.Fatalf("restart claim is %s, want replayed", e.State)
	}
}

func TestInstanceStable(t *testing.T) {
	dir := t.TempDir()
	a, err := lease.Instance(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := lease.Instance(dir)
	if a != b || len(a) != 16 {
		t.Fatalf("%q %q", a, b)
	}
}

// A claim left pending (Signal never answered) expires once its period has passed, instead of staying pending.
func TestStaleClaimsExpire(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	s.Script = func(int, signaltest.Request) signaltest.Reply { return signaltest.Reply{Status: 503, Body: `{}`} }
	c := &clock{time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC)}
	a, ob := instance(t, s, t.TempDir(), "a:lease", c)
	a.Writer.(*signal.Client).Attempts = 1
	if _, err := a.Ensure(context.Background()); !errors.Is(err, signal.ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	pending := ob.Pending()
	if len(pending) != 1 {
		t.Fatalf("pending %d", len(pending))
	}
	c.t = c.t.Add(5 * time.Minute)
	s.Script = nil
	st, err := a.Ensure(context.Background())
	if err != nil || !st.Held {
		t.Fatalf("%+v %v", st, err)
	}
	if e, _ := ob.Get(pending[0].Key); e.State != outbox.Expired {
		t.Fatalf("stale claim is %s", e.State)
	}
	for _, e := range ob.Pending() {
		t.Fatalf("still pending: %s", e.Key)
	}
}

// A refusal other than another holder is an error naming Signal's status and code, on this pass and the next.
func TestRefusedClaim(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	s.Script = func(int, signaltest.Request) signaltest.Reply {
		return signaltest.Reply{Status: 403, Body: `{"error":{"code":"forbidden","message":"x"}}`}
	}
	c := &clock{time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC)}
	a, _ := instance(t, s, t.TempDir(), "a:lease", c)
	for i := 0; i < 2; i++ {
		st, err := a.Ensure(context.Background())
		var refused *lease.RefusedError
		if st.Held || !errors.Is(err, lease.ErrRefused) || !errors.As(err, &refused) || refused.Status != 403 || refused.Code != "forbidden" {
			t.Fatalf("pass %d: %+v %v", i, st, err)
		}
	}
	if n := len(s.Requests()); n != 1 {
		t.Fatalf("a refused claim was sent %d times", n)
	}
}

// A claim's write ends with its period, however long Signal asks Beacon to wait.
func TestClaimEndsWithItsPeriod(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	s.Script = func(int, signaltest.Request) signaltest.Reply {
		return signaltest.Reply{Status: 429, Body: `{}`, Headers: map[string]string{"Retry-After": "3600"}}
	}
	// One second before the period ends.
	c := &clock{time.Date(2026, 11, 7, 12, 4, 59, 0, time.UTC)}
	a, ob := instance(t, s, t.TempDir(), "a:lease", c)
	start := time.Now()
	_, err := a.Ensure(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	if len(ob.Pending()) != 1 {
		t.Fatal("the claim should stay pending, to expire later")
	}
}
