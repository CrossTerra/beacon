package limited

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
