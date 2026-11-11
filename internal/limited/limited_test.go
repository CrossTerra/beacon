package limited

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

func TestBackoffThenSuccess(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	var slept time.Duration
	c := &Client{HTTP: srv.Client(), Sleep: func(_ context.Context, d time.Duration) error {
		slept = d
		return nil
	}}
	resp, err := c.Do(context.Background(), mustReq(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || slept != time.Second || hits != 2 {
		t.Fatalf("status %d slept %s hits %d", resp.StatusCode, slept, hits)
	}
}

func TestLongWaitEndsThePass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	slept := false
	c := &Client{HTTP: srv.Client(), Sleep: func(context.Context, time.Duration) error {
		slept = true
		return nil
	}}
	_, err := c.Do(context.Background(), mustReq(t, srv.URL))
	if !errors.Is(err, source.ErrRateLimited) || slept {
		t.Fatalf("err %v slept %v", err, slept)
	}
}

func TestResetHeader(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(30*time.Second).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	var slept time.Duration
	c := &Client{HTTP: srv.Client(), Now: func() time.Time { return now }, Sleep: func(_ context.Context, d time.Duration) error {
		slept = d
		return nil
	}}
	_, err := c.Do(context.Background(), mustReq(t, srv.URL))
	if !errors.Is(err, source.ErrRateLimited) {
		t.Fatal(err)
	}
	if slept < 29*time.Second || slept > 31*time.Second {
		t.Fatalf("slept %s", slept)
	}
}

func mustReq(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// A rate limit returns what the API said, and a source's hook sees every attempt, retries included.
func TestRateLimitErrorCarriesTheAnswer(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"reason":"slowDown","message":"private text"}`)
	}))
	defer srv.Close()
	before := 0
	var slept []time.Duration
	c := &Client{HTTP: srv.Client(),
		Sleep:  func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
		Before: func(context.Context, *http.Request) error { before++; return nil },
		Reason: func(body []byte) string {
			if strings.Contains(string(body), "slowDown") {
				return "slowDown"
			}
			return ""
		},
	}
	_, err := c.Do(context.Background(), mustReq(t, srv.URL))
	l := source.LimitOf(err)
	if !errors.Is(err, source.ErrRateLimited) || l == nil || l.Status != 429 || l.Reason != "slowDown" || l.RetryAfter != 2*time.Second {
		t.Fatalf("err %v limit %+v", err, l)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatal("the error carries body text")
	}
	if hits != 3 || before != 3 {
		t.Fatalf("hits %d before %d, want every attempt counted", hits, before)
	}
	if len(slept) != 2 {
		t.Fatalf("slept %v: the last attempt must not sleep", slept)
	}
}

// One attempt: the rate limit is returned at once, without a sleep, for the source to back off itself.
func TestOneAttemptDoesNotSleep(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), MaxAttempts: 1, Sleep: func(context.Context, time.Duration) error {
		t.Fatal("slept")
		return nil
	}}
	_, err := c.Do(context.Background(), mustReq(t, srv.URL))
	if l := source.LimitOf(err); l == nil || l.RetryAfter != -1 || hits != 1 {
		t.Fatalf("err %v hits %d", err, hits)
	}
}

// A Retry-After longer than a pass waits is remembered: later requests are refused without being sent until it
// has passed.
func TestLongRetryAfterHoldsLaterRequests(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Retry-After", "900")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	c := &Client{HTTP: srv.Client(), Now: func() time.Time { return now }}
	if _, err := c.Do(context.Background(), mustReq(t, srv.URL)); !errors.Is(err, source.ErrRateLimited) {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	_, err := c.Do(context.Background(), mustReq(t, srv.URL))
	if l := source.LimitOf(err); l == nil || l.Status != 429 || l.RetryAfter != 10*time.Minute || hits != 1 {
		t.Fatalf("err %v limit %+v hits %d", err, l, hits)
	}
	now = now.Add(10 * time.Minute)
	resp, err := c.Do(context.Background(), mustReq(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits != 2 {
		t.Fatalf("hits %d", hits)
	}
}
