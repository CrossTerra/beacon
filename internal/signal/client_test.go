package signal_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
)

const token = "sig_test_token"

func client(t *testing.T, s *signaltest.Server) (*signal.Client, *[]time.Duration) {
	t.Helper()
	store := keychain.NewMemory()
	store.Put("acct", token)
	secret, _ := store.Get("acct")
	u, _ := url.Parse(s.URL)
	c := signal.New(u, egress.Guard([]string{egress.Address(u)}, http.DefaultTransport), secret)
	var slept []time.Duration
	c.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	return c, &slept
}

var body = []byte(`{"subject":"s","type":"t","key":"k1","open":{}}`)

func TestWrittenReplayedConflict(t *testing.T) {
	s := signaltest.New(token)
	defer s.Close()
	c, _ := client(t, s)
	res, err := c.Write(context.Background(), body)
	if err != nil || res.Outcome != signal.Written || res.Status != 201 || res.EventID == "" {
		t.Fatalf("first: %+v %v", res, err)
	}
	again, err := c.Write(context.Background(), body)
	if err != nil || again.Outcome != signal.Replayed || again.Status != 200 || again.EventID != res.EventID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	other, err := c.Write(context.Background(), []byte(`{"subject":"s","type":"t","key":"k1","open":{"title":"x"}}`))
	if err != nil || other.Outcome != signal.Conflict || other.Status != 409 || other.Code != "idempotency_conflict" {
		t.Fatalf("conflict: %+v %v", other, err)
	}
	if n := len(s.RequestsFor("k1")); n != 3 {
		t.Fatalf("a final answer was retried: %d requests", n)
	}
}

func TestRateLimitRespected(t *testing.T) {
	s := signaltest.New(token)
	defer s.Close()
	s.Script = func(n int, _ signaltest.Request) signaltest.Reply {
		if n == 1 {
			return signaltest.Reply{Status: 429, Body: `{"error":{"code":"rate_limited","message":"Too many requests."}}`,
				Headers: map[string]string{"Retry-After": "7"}}
		}
		return signaltest.Reply{}
	}
	c, slept := client(t, s)
	res, err := c.Write(context.Background(), body)
	if err != nil || res.Outcome != signal.Written {
		t.Fatalf("%+v %v", res, err)
	}
	if len(*slept) != 1 || (*slept)[0] != 7*time.Second {
		t.Fatalf("slept %v, want [7s]", *slept)
	}
}

func TestServerErrorsBackOffAndResendSameBytes(t *testing.T) {
	s := signaltest.New(token)
	defer s.Close()
	s.Script = func(n int, _ signaltest.Request) signaltest.Reply {
		if n <= 3 {
			return signaltest.Reply{Status: 503, Body: `{"error":{"code":"unavailable","message":"x"}}`}
		}
		return signaltest.Reply{}
	}
	c, slept := client(t, s)
	res, err := c.Write(context.Background(), body)
	if err != nil || res.Outcome != signal.Written {
		t.Fatalf("%+v %v", res, err)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; len(*slept) != 3 || (*slept)[2] != want[2] {
		t.Fatalf("backoff %v, want %v", *slept, want)
	}
	for _, r := range s.Requests() {
		if !bytes.Equal(r.Body, body) {
			t.Fatalf("attempt sent %s, want %s", r.Body, body)
		}
	}
}

func TestUnavailableAfterAttempts(t *testing.T) {
	s := signaltest.New(token)
	defer s.Close()
	s.Script = func(int, signaltest.Request) signaltest.Reply {
		return signaltest.Reply{Status: 502, Body: `bad gateway`}
	}
	c, _ := client(t, s)
	_, err := c.Write(context.Background(), body)
	if !errors.Is(err, signal.ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	if len(s.Requests()) != c.Attempts {
		t.Fatalf("%d requests, want %d", len(s.Requests()), c.Attempts)
	}
}

func TestRefusedAndUnauthorized(t *testing.T) {
	s := signaltest.New(token)
	defer s.Close()
	s.Script = func(int, signaltest.Request) signaltest.Reply {
		return signaltest.Reply{Status: 422, Body: `{"error":{"code":"ask_not_found","message":"No event with asks matches that entry in closes."}}`}
	}
	c, _ := client(t, s)
	res, err := c.Write(context.Background(), body)
	if err != nil || res.Outcome != signal.Refused || res.Code != "ask_not_found" || len(s.Requests()) != 1 {
		t.Fatalf("%+v %v", res, err)
	}

	bad := signaltest.New("another token")
	defer bad.Close()
	c, _ = client(t, bad)
	if _, err := c.Write(context.Background(), body); !errors.Is(err, signal.ErrUnauthorized) || len(bad.Requests()) != 1 {
		t.Fatalf("unauthorized: %v, %d requests", err, len(bad.Requests()))
	}
}

func TestBearerHeader(t *testing.T) {
	s := signaltest.New(token)
	defer s.Close()
	c, _ := client(t, s)
	if res, err := c.Write(context.Background(), body); err != nil || res.Outcome != signal.Written {
		t.Fatalf("the fake accepts only the right bearer: %+v %v", res, err)
	}
}

func TestEgressGuardIsFinal(t *testing.T) {
	store := keychain.NewMemory()
	store.Put("a", token)
	secret, _ := store.Get("a")
	u, _ := url.Parse("https://signal.example.test")
	c := signal.New(u, egress.Guard([]string{"elsewhere.test:443"}, http.DefaultTransport), secret)
	c.Sleep = func(context.Context, time.Duration) error { t.Fatal("retried a denied host"); return nil }
	_, err := c.Write(context.Background(), body)
	var denied *egress.DeniedError
	if !errors.As(err, &denied) || denied.Address != "signal.example.test:443" {
		t.Fatalf("err %v", err)
	}
}

// The client, on the production transport, treats a redirect as a final answer and never follows it.
func TestRedirectNotFollowed(t *testing.T) {
	var followed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			followed.Add(1)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusPermanentRedirect)
	}))
	defer srv.Close()
	store := keychain.NewMemory()
	store.Put("a", token)
	secret, _ := store.Get("a")
	u, _ := url.Parse(srv.URL)
	c := signal.New(u, egress.Transport([]string{egress.Address(u)}), secret)
	res, err := c.Write(context.Background(), body)
	if err != nil || res.Outcome != signal.Refused || res.Status != http.StatusPermanentRedirect || followed.Load() != 0 {
		t.Fatalf("%+v %v, followed %d", res, err, followed.Load())
	}
}

// A context deadline ends a write, including a wait Signal asked for.
func TestDeadlineEndsRetryAfter(t *testing.T) {
	s := signaltest.New(token)
	defer s.Close()
	s.Script = func(int, signaltest.Request) signaltest.Reply {
		return signaltest.Reply{Status: 429, Body: `{}`, Headers: map[string]string{"Retry-After": "60"}}
	}
	store := keychain.NewMemory()
	store.Put("acct", token)
	secret, _ := store.Get("acct")
	u, _ := url.Parse(s.URL)
	c := signal.New(u, egress.Transport([]string{egress.Address(u)}), secret)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Write(ctx, body)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestRestrictions(t *testing.T) {
	s := signaltest.New(token)
	defer s.Close()
	c, _ := client(t, s)
	got, err := c.Restrictions(context.Background())
	if err != nil || len(got) != 0 || got == nil {
		t.Fatalf("%v %v", got, err)
	}
	s.Restrict("gmail:*", "drive:*")
	got, err = c.Restrictions(context.Background())
	if err != nil || len(got) != 2 || got[0] != "gmail:*" {
		t.Fatalf("%v %v", got, err)
	}
	s.NoRead = true
	if _, err := c.Restrictions(context.Background()); !errors.Is(err, signal.ErrNoRead) {
		t.Fatal(err)
	}
	for _, reply := range []string{`{"data":[{"prefix":"gmail:*"}],"next_cursor":"more"}`, `{}`, `not json`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(reply)) }))
		u, _ := url.Parse(srv.URL)
		store := keychain.NewMemory()
		store.Put("acct", token)
		secret, _ := store.Get("acct")
		c := signal.New(u, http.DefaultTransport, secret)
		if _, err := c.Restrictions(context.Background()); !errors.Is(err, signal.ErrUnavailable) {
			t.Errorf("%s: %v", reply, err)
		}
		srv.Close()
	}
}
