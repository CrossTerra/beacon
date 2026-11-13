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
	// A claim is bookkeeping: it asks no one, so there is nothing to close.
	if last.Subject != "beacon:lease:winton" || !strings.HasPrefix(last.Key, "claim:beacon:lease:winton:") ||
		last.To != nil || last.Metadata["run"] != "a:lease" || last.Open["title"] == "" || last.Closes != nil {
		t.Fatalf("claim: %s", evs[2].Body)
	}
	if strings.Contains(string(evs[2].Body), `"to"`) {
		t.Fatalf("a claim asks: %s", evs[2].Body)
	}
}

// legacyClaim is a sent claim from a version whose claims were asks to Beacon itself.
// With s, Signal holds it too.
func legacyClaim(t *testing.T, ob *outbox.Outbox, key string, s ...*signaltest.Server) {
	t.Helper()
	body := `{"subject":"beacon:lease:winton","type":"beacon.lease_claimed","key":"` + key + `","open":{"title":"Beacon lease for @winton"},"to":["me"],"metadata":{"run":"a:lease"}}`
	if err := ob.Append(outbox.Lease, key, "beacon:lease:winton", "", []byte(body)); err != nil {
		t.Fatal(err)
	}
	id := "evt_legacy"
	if len(s) > 0 {
		s[0].Seed(key, []byte(body))
		e, _ := s[0].Event(key)
		id = e.ID
	}
	if err := ob.Mark(key, outbox.Sent, id, 201, "", ""); err != nil {
		t.Fatal(err)
	}
}

// Once the lease is held after an upgrade, one event closes the asks earlier claims left open; claims themselves
// close nothing. A close Signal does not answer stays pending and is resent as the same bytes; one it refuses is
// recorded, does not cost the lease, and is not written again.
func TestClaimsCloseLegacyAsks(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	c := &clock{time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC)}
	a, ob := instance(t, s, t.TempDir(), "a:lease", c)
	a.Writer.(*signal.Client).Attempts = 1
	legacyClaim(t, ob, "claim:beacon:lease:winton:100")
	legacyClaim(t, ob, "claim:beacon:lease:winton:101")
	// The close is not answered the first time.
	s.Script = func(_ int, r signaltest.Request) signaltest.Reply {
		if strings.Contains(string(r.Body), "lease_moved") {
			return signaltest.Reply{Status: 503, Body: `{}`}
		}
		return signaltest.Reply{}
	}
	if st, err := a.Ensure(context.Background()); err != nil || !st.Held {
		t.Fatalf("%+v %v", st, err)
	}
	var closing string
	for _, e := range ob.Pending() {
		closing = e.Key
	}
	if !strings.HasPrefix(closing, "beacon:lease:winton:closed:") {
		t.Fatalf("pending %v", ob.Pending())
	}
	s.Script = nil
	c.t = c.t.Add(5 * time.Minute)
	if st, err := a.Ensure(context.Background()); err != nil || !st.Held {
		t.Fatalf("%+v %v", st, err)
	}
	var claims, closes int
	for _, e := range s.Events() {
		switch {
		case strings.Contains(string(e.Body), "lease_moved"):
			closes++
			if e.Key != closing || !strings.Contains(string(e.Body), `"closes":[{"key":"claim:beacon:lease:winton:100"},{"key":"claim:beacon:lease:winton:101"}]`) {
				t.Fatalf("close %s", e.Body)
			}
		case strings.Contains(string(e.Body), "closes"):
			t.Fatalf("a claim closes: %s", e.Body)
		default:
			claims++
		}
	}
	if closes != 1 || claims != 3 {
		t.Fatalf("%d closes, %d claims", closes, claims)
	}
	// A refused close (the asks were closed in Signal some other way) does not cost the lease and is not retried.
	b, ob2 := instance(t, s, t.TempDir(), "a:lease", c)
	legacyClaim(t, ob2, "claim:beacon:lease:winton:200")
	s.Script = func(_ int, r signaltest.Request) signaltest.Reply {
		if strings.Contains(string(r.Body), "lease_moved") {
			return signaltest.Reply{Status: 409, Body: `{"error":{"code":"conflict","message":"x"}}`}
		}
		return signaltest.Reply{}
	}
	for i := 0; i < 2; i++ {
		if st, err := b.Ensure(context.Background()); err != nil || !st.Held {
			t.Fatalf("refused close: %+v %v", st, err)
		}
		c.t = c.t.Add(5 * time.Minute)
	}
	n := 0
	for _, r := range s.Requests() {
		if strings.Contains(string(r.Body), "claim:beacon:lease:winton:200") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("a refused close was sent %d times", n)
	}
}

// With Signal's lease API the lease is a row: renewed once a period for two, no event and no ask. Another instance
// is refused until the holder stops renewing, then takes it within two periods. The open asks earlier claims left are
// closed once, by one event.
func TestLeaseRow(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	c := &clock{time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC)}
	s.Leases, s.LeaseNow = true, c.now
	a, ob := instance(t, s, t.TempDir(), "a:lease", c)
	b, _ := instance(t, s, t.TempDir(), "b:lease", c)
	a.API, b.API = a.Writer.(*signal.Client), b.Writer.(*signal.Client)
	a.Threads = a.Writer.(*signal.Client)
	legacyClaim(t, ob, "claim:beacon:lease:winton:100", s)

	st, err := a.Ensure(context.Background())
	if err != nil || !st.Held || !st.Until.Equal(c.t.Add(15*time.Minute)) {
		t.Fatalf("a: %+v %v", st, err)
	}
	if st, err := b.Ensure(context.Background()); err != nil || st.Held {
		t.Fatalf("b took a held lease: %+v %v", st, err)
	}
	// Within a period the holder does not call Signal again.
	claims := s.LeaseClaims()
	for i := 0; i < 4; i++ {
		c.t = c.t.Add(time.Minute)
		if st, _ := a.Ensure(context.Background()); !st.Held {
			t.Fatal("a lost the lease within its period")
		}
	}
	if s.LeaseClaims() != claims {
		t.Fatalf("renewed %d times within one period", s.LeaseClaims()-claims)
	}
	for i := 0; i < 4; i++ {
		c.t = c.t.Add(5 * time.Minute)
		if st, _ := a.Ensure(context.Background()); !st.Held {
			t.Fatalf("period %d: a lost the lease", i)
		}
		if st, _ := b.Ensure(context.Background()); st.Held {
			t.Fatalf("period %d: b took the lease", i)
		}
	}
	// Only the event that withdraws the earlier claim's ask, and the archive of its thread, were written, once each.
	evs := s.Events()[1:]
	if len(evs) != 2 || !strings.Contains(string(evs[0].Body), `"type":"beacon.lease_moved"`) ||
		!strings.Contains(string(evs[0].Body), `"closes":[{"key":"claim:beacon:lease:winton:100"}],"outcome":"withdrawn"`) || strings.Contains(string(evs[0].Body), `"to"`) {
		t.Fatalf("events: %s", evs)
	}
	if string(evs[1].Body) != `{"subject":"signal:thread:1","type":"thread.archived","key":"beacon:lease:winton:archived:1"}` {
		t.Fatalf("archive: %s", evs[1].Body)
	}
	// A thread merged into another conversation is not archived: that would archive the other conversation.
	s2 := signaltest.New("tok")
	defer s2.Close()
	s2.Leases, s2.LeaseNow = true, c.now
	m, mob := instance(t, s2, t.TempDir(), "m:lease", c)
	m.API, m.Threads = m.Writer.(*signal.Client), m.Writer.(*signal.Client)
	s2.OpenAs("other:conversation", "Another conversation")
	legacyClaim(t, mob, "claim:beacon:lease:winton:300", s2)
	s2.Merge("beacon:lease:winton", "other:conversation")
	if st, err := m.Ensure(context.Background()); err != nil || !st.Held {
		t.Fatalf("%+v %v", st, err)
	}
	for _, e := range s2.Events() {
		if strings.Contains(string(e.Body), "thread.archived") {
			t.Fatalf("archived a merged thread's survivor: %s", e.Body)
		}
	}
	// The holder stops. Within three periods the other takes over.
	for i := 0; i < 2; i++ {
		c.t = c.t.Add(5 * time.Minute)
		if st, _ := b.Ensure(context.Background()); st.Held {
			t.Fatal("b took the lease before it expired")
		}
	}
	c.t = c.t.Add(5 * time.Minute)
	if st, _ := b.Ensure(context.Background()); !st.Held {
		t.Fatal("b did not take the lease after a stopped")
	}
	// The old holder comes back and is refused.
	if st, _ := a.Ensure(context.Background()); st.Held {
		t.Fatal("a held the lease b took")
	}
}

// A Signal without leases answers 404: the lease is claimed by event, and the API is not asked again for an hour.
// Once Signal offers leases, the instance moves to them and claims no more.
func TestLeaseRowFallsBackToClaims(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	c := &clock{time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC)}
	a, _ := instance(t, s, t.TempDir(), "a:lease", c)
	a.API = a.Writer.(*signal.Client)
	for i := 0; i < 3; i++ {
		if st, err := a.Ensure(context.Background()); err != nil || !st.Held {
			t.Fatalf("%+v %v", st, err)
		}
		c.t = c.t.Add(5 * time.Minute)
	}
	n := 0
	for _, r := range s.Requests() {
		if r.Path == "/api/leases" {
			n++
		}
	}
	if n != 1 || len(s.Events()) != 4 {
		t.Fatalf("%d lease API calls, %d claims", n, len(s.Events()))
	}
	s.Leases, s.LeaseNow = true, c.now
	c.t = c.t.Add(time.Hour)
	for i := 0; i < 4; i++ {
		if st, err := a.Ensure(context.Background()); err != nil || !st.Held {
			t.Fatalf("%+v %v", st, err)
		}
		c.t = c.t.Add(5 * time.Minute)
	}
	if s.LeaseClaims() == 0 || len(s.Events()) > 6 {
		t.Fatalf("%d lease claims, %d events", s.LeaseClaims(), len(s.Events()))
	}
}

// A lease refusal other than another holder is an error naming Signal's status and code.
func TestLeaseRowRefused(t *testing.T) {
	s := signaltest.New("tok")
	defer s.Close()
	s.Leases = true
	s.LeaseScript = func(int) signaltest.Reply {
		return signaltest.Reply{Status: 403, Body: `{"error":{"code":"forbidden","message":"x"}}`}
	}
	c := &clock{time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC)}
	a, _ := instance(t, s, t.TempDir(), "a:lease", c)
	a.API = a.Writer.(*signal.Client)
	st, err := a.Ensure(context.Background())
	var refused *lease.RefusedError
	if st.Held || !errors.As(err, &refused) || refused.Status != 403 || refused.Code != "forbidden" || len(s.Events()) != 0 {
		t.Fatalf("%+v %v", st, err)
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
