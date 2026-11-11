package google

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/google/googletest"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/source"
)

// fakeClock is a clock that moves only when the code under test sleeps, shared by the source and the fake Google.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	c.slept = append(c.slept, d)
	return nil
}

// perUserMinute makes the fake Google enforce Gmail's newer per-user quota: 6,000 units in each clock minute, at
// the newer costs, answering 429 rateLimitExceeded with no Retry-After past it, as Gmail does. It counts every
// request, the refused ones too.
type perUserMinute struct {
	clock   *fakeClock
	mu      sync.Mutex
	minute  time.Time
	spent   int
	refused int
}

func (p *perUserMinute) wrap(units int, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		m := p.clock.now().Truncate(time.Minute)
		if !m.Equal(p.minute) {
			p.minute, p.spent = m, 0
		}
		over := p.spent+units > gmailPerMinute
		p.spent += units
		if over {
			p.refused++
		}
		p.mu.Unlock()
		if over {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"code":429,"message":"Quota exceeded","errors":[{"reason":"rateLimitExceeded"}],"status":"RESOURCE_EXHAUSTED"}}`)
			return
		}
		next(w, r)
	}
}

// bigInbox is n inbox threads with one message each, an hour old, listed 100 to a page.
func bigInbox(srv *googletest.Server, n int, at time.Time) map[string]http.HandlerFunc {
	routes := map[string]http.HandlerFunc{}
	routes["/gmail/v1/users/me/threads"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("labelIds") != "INBOX" {
			io.WriteString(w, `{"threads":[]}`)
			return
		}
		start := 0
		fmt.Sscanf(r.URL.Query().Get("pageToken"), "p%d", &start)
		var ids []string
		for i := start; i < n && i < start+100; i++ {
			ids = append(ids, fmt.Sprintf(`{"id":"b%d"}`, i))
		}
		next := ""
		if start+100 < n {
			next = fmt.Sprintf(`,"nextPageToken":"p%d"`, start+100)
		}
		fmt.Fprintf(w, `{"threads":[%s]%s}`, strings.Join(ids, ","), next)
	}
	routes["/gmail/v1/users/me/messages"] = func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"messages":[]}`) }
	ms := at.Add(-time.Hour).UnixMilli()
	for i := 0; i < n; i++ {
		body := fmt.Sprintf(`{"id":"b%d","messages":[{"id":"bm%d","threadId":"b%d","labelIds":["INBOX"],"internalDate":"%d","payload":{"headers":[{"name":"From","value":"a@example.com"},{"name":"To","value":"winton@example.com"},{"name":"Subject","value":"s"}]}}]}`, i, i, i, ms)
		routes[fmt.Sprintf("/gmail/v1/users/me/threads/b%d", i)] = func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }
	}
	return routes
}

// TestGmailFirstRunStaysUnderThePerUserMinute is the live failure: a 14-day inbox of a few hundred threads, against
// a project on Gmail's newer quota (6,000 units per user per minute, threads.get 40 units). Paced by the older
// table (threads.get 10, 12,000 a minute), the scan was refused after about 150 threads in its first minute and
// paused on every pass. Paced by the newer one it finishes in one pass with no 429.
func TestGmailFirstRunStaysUnderThePerUserMinute(t *testing.T) {
	srv := googletest.New(t)
	clock := &fakeClock{t: testNow}
	limit := &perUserMinute{clock: clock}
	costs := map[string]int{"/gmail/v1/users/me/profile": 1, "/gmail/v1/users/me/labels": 1, "/gmail/v1/users/me/threads": 10, "/gmail/v1/users/me/messages": 5}
	const threads = 400
	for path, h := range bigInbox(srv, threads, testNow) {
		srv.Routes[path] = h
	}
	for path, h := range srv.Routes {
		if !strings.HasPrefix(path, "/gmail/") {
			continue
		}
		units, ok := costs[path]
		if !ok {
			units = 40
		}
		srv.Routes[path] = limit.wrap(units, h)
	}
	o := opts(t, srv)
	o.Now, o.Sleep = clock.now, clock.sleep
	g, err := NewGmail(o)
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.Collect(context.Background(), nil)
	if err != nil || got.Partial {
		t.Fatalf("err %v partial %v refused %d", err, got.Partial, limit.refused)
	}
	if limit.refused != 0 {
		t.Fatalf("Gmail refused %d calls", limit.refused)
	}
	if len(got.Items) != threads {
		t.Fatalf("items %d, want %d", len(got.Items), threads)
	}
	// 400 threads at 40 units is 16,000 units: at most 4,800 a minute, so the scan takes more than three minutes.
	if elapsed := clock.now().Sub(testNow); elapsed < 3*time.Minute {
		t.Fatalf("the scan took %s; it did not pace by the per-minute budget", elapsed)
	}
}

// A rate limit from Google means something else spent the person's minute too (another tool on the same OAuth
// client). Gmail waits a whole minute when no Retry-After is named, tries once more, and counts that retry in its
// budget. The HTTP client does not retry on its own, unpaced.
func TestGmailRateLimitWaitsOutTheMinuteAndCountsTheRetry(t *testing.T) {
	srv := googletest.New(t)
	clock := &fakeClock{t: testNow}
	refused := 0
	srv.Routes["/gmail/v1/users/me/threads/t2"] = func(w http.ResponseWriter, _ *http.Request) {
		if refused == 0 {
			refused++
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"code":429,"errors":[{"reason":"rateLimitExceeded"}]}}`)
			return
		}
		io.WriteString(w, googletest.ThreadT2)
	}
	o := opts(t, srv)
	o.Now, o.Sleep = clock.now, clock.sleep
	g, err := NewGmail(o)
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.Collect(context.Background(), nil)
	if err != nil || got.Partial {
		t.Fatalf("err %v partial %v", err, got.Partial)
	}
	if n := countPath(srv, "/gmail/v1/users/me/threads/t2"); n != 2 {
		t.Fatalf("t2 requested %d times, want the refused call and one retry", n)
	}
	waited := false
	for _, d := range clock.slept {
		waited = waited || d >= gmailLimitWait-time.Second
	}
	if !waited {
		t.Fatalf("did not wait out the minute: %v", clock.slept)
	}
	m := byKey(t, got.Items, GmailFields, GmailStateFields)
	if _, ok := m["gmail:t2:m3"]; !ok {
		t.Fatalf("the retried thread is missing: %v", keys(m))
	}
	// Both calls to t2 were counted: the scan's spend includes the refused one.
	spent := 0
	for _, s := range g.quota.spent {
		spent += s.units
	}
	if spent < 2*gmailUnits("threads/t2") {
		t.Fatalf("budget spent %d; the retry was not counted", spent)
	}
}

// A pause carries what Google said, for the log: the status, its reason code, the Retry-After, and Beacon's own
// spend in the last minute. A second refusal after the wait pauses the scan.
func TestGmailPauseCarriesTheLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		body       string
		reason     string
		wait       time.Duration
	}{
		{"429 with Retry-After", http.StatusTooManyRequests, "7", `{"error":{"code":429,"errors":[{"reason":"rateLimitExceeded"}]}}`, "rateLimitExceeded", 7 * time.Second},
		{"429 concurrent", http.StatusTooManyRequests, "", `{"error":{"code":429,"message":"Too many concurrent requests for user","errors":[{"reason":"rateLimitExceeded","message":"Too many concurrent requests for user"}]}}`, "concurrentRequests", -1},
		{"403 user rate", http.StatusForbidden, "", `{"error":{"code":403,"errors":[{"reason":"userRateLimitExceeded"}]}}`, "userRateLimitExceeded", -1},
		{"429 bare", http.StatusTooManyRequests, "", ``, "", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := googletest.New(t)
			clock := &fakeClock{t: testNow}
			srv.Routes["/gmail/v1/users/me/threads/t2"] = func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}
			o := opts(t, srv)
			o.Now, o.Sleep = clock.now, clock.sleep
			g, err := NewGmail(o)
			if err != nil {
				t.Fatal(err)
			}
			got, err := g.Collect(context.Background(), nil)
			if err != nil || !got.Partial || got.Limit == nil {
				t.Fatalf("err %v partial %v limit %+v", err, got.Partial, got.Limit)
			}
			l := got.Limit
			if l.Status != tc.status || l.Reason != tc.reason || l.RetryAfter != tc.wait || l.Units <= 0 {
				t.Fatalf("limit %+v", *l)
			}
			if n := countPath(srv, "/gmail/v1/users/me/threads/t2"); n != 2 {
				t.Fatalf("t2 requested %d times, want two", n)
			}
		})
	}
}

// A rate limit with no progress fails the collect, and the error still carries what Google said.
func TestGmailFailedCollectCarriesTheLimit(t *testing.T) {
	srv := googletest.New(t)
	clock := &fakeClock{t: testNow}
	srv.Routes["/gmail/v1/users/me/profile"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"errors":[{"reason":"rateLimitExceeded"}]}}`)
	}
	o := opts(t, srv)
	o.Now, o.Sleep = clock.now, clock.sleep
	g, err := NewGmail(o)
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Collect(context.Background(), nil)
	l := source.LimitOf(err)
	if !errors.Is(err, source.ErrRateLimited) || l == nil || l.Status != http.StatusForbidden || l.Reason != "rateLimitExceeded" {
		t.Fatalf("err %v limit %+v", err, l)
	}
}

// A Retry-After longer than a pass waits ends the pass, and the next pass does not call Gmail before it is over.
func TestGmailLongRetryAfterHoldsLaterPasses(t *testing.T) {
	srv := googletest.New(t)
	clock := &fakeClock{t: testNow}
	srv.Routes["/gmail/v1/users/me/profile"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}
	o := opts(t, srv)
	o.Now, o.Sleep = clock.now, clock.sleep
	g, err := NewGmail(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Collect(context.Background(), nil); !errors.Is(err, source.ErrRateLimited) {
		t.Fatal(err)
	}
	if n := countPath(srv, "/gmail/v1/users/me/profile"); n != 1 {
		t.Fatalf("profile requested %d times", n)
	}
	_, err = g.Collect(context.Background(), nil)
	if l := source.LimitOf(err); l == nil || l.RetryAfter <= 0 || l.Status != http.StatusTooManyRequests {
		t.Fatalf("second pass: err %v limit %+v", err, l)
	}
	if n := countPath(srv, "/gmail/v1/users/me/profile"); n != 1 {
		t.Fatal("the next pass called Gmail before the Retry-After was over")
	}
	clock.sleep(context.Background(), 10*time.Minute)
	srv.Routes["/gmail/v1/users/me/profile"] = func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"emailAddress":"`+googletest.Email+`","historyId":"300"}`)
	}
	if _, err := g.Collect(context.Background(), nil); err != nil {
		t.Fatalf("after the wait: %v", err)
	}
}

func TestGmailUnits(t *testing.T) {
	for path, want := range map[string]int{
		"profile": 1, "labels": 1, "history": 2, "messages": 5, "messages/m1": 60, "threads": 10, "threads/t1": 40,
	} {
		if got := gmailUnits(path); got != want {
			t.Errorf("%s: %d units, want %d", path, got, want)
		}
	}
	if n := gmailPathUnits("/gmail/v1/users/me/threads/t1"); n != 40 {
		t.Fatalf("path units %d", n)
	}
}

func TestRateReasonNamesNoText(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"errors":[{"reason":"userRateLimitExceeded","message":"secret subject"}]}}`: "userRateLimitExceeded",
		`{"error":{"status":"RESOURCE_EXHAUSTED"}}`:                                            "RESOURCE_EXHAUSTED",
		`{"error":{"errors":[{"reason":"somethingNew"}]}}`:                                     "",
		`{"error":{"message":"Too many concurrent requests for user"}}`:                        "concurrentRequests",
		`not json`: "",
	} {
		if got := rateReason([]byte(body)); got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
}

// The quota wait comes before the access token is read. A token that expires while Gmail waits for quota is
// refreshed before the call, not sent stale (which Google would refuse with a 401, failing the collect).
func TestGmailReadsTheTokenAfterTheQuotaWait(t *testing.T) {
	srv := googletest.New(t)
	clock := &fakeClock{t: testNow}
	auth, err := NewAuth(AuthOptions{
		TokenURL: srv.URL + "/token",
		Client:   func() (keychain.Secret, error) { return keychain.OAuthClient([]byte(googletest.ClientFile)) },
		Refresh:  func() (keychain.Secret, error) { return keychain.FromString(googletest.Refresh) },
		HTTP:     srv.Client(),
		Now:      clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	o := opts(t, srv)
	o.Auth, o.Now, o.Sleep = auth, clock.now, clock.sleep
	g, err := NewGmail(o)
	if err != nil {
		t.Fatal(err)
	}
	// The grant check reads a token (expires_in 3599). Then every call waits two hours for quota first.
	if _, err := g.grant(context.Background(), []string{ScopeGmailMetadata}); err != nil {
		t.Fatal(err)
	}
	g.quota.hold(2 * time.Hour)
	if _, err := g.call(context.Background(), http.MethodGet, g.url("profile", nil), 1<<20, "application/json"); err != nil {
		t.Fatal(err)
	}
	if n := countPath(srv, "/token"); n != 2 {
		t.Fatalf("token requests %d: the token was read before the wait, not refreshed after it", n)
	}
}
