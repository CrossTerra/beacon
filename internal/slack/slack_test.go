package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/slack/slacktest"
	"github.com/CrossTerra/beacon/internal/source"
)

var testNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// sleeps is the test clock: it starts at testNow and moves only when the source sleeps.
type sleeps struct {
	mu      sync.Mutex
	got     []time.Duration
	elapsed time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, d)
	s.elapsed += d
	return nil
}

func (s *sleeps) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return testNow.Add(s.elapsed)
}

func opts(srv *slacktest.Server, sl *sleeps) Options {
	return Options{
		Person: "winton", Workspace: slacktest.Workspace, User: slacktest.User, Channels: []string{slacktest.Review},
		Identity: "slack:user:" + slacktest.User,
		Token:    func() (keychain.Secret, error) { return keychain.FromString(slacktest.Token) },
		Base:     srv.URL, HTTP: srv.Client(), Now: sl.now, Sleep: sl.sleep,
	}
}

func newSource(t *testing.T, srv *slacktest.Server) (*Source, *sleeps) {
	t.Helper()
	sl := &sleeps{}
	s, err := New(opts(srv, sl))
	if err != nil {
		t.Fatal(err)
	}
	return s, sl
}

func byKey(t *testing.T, items []source.Item) map[string]source.Item {
	t.Helper()
	out := map[string]source.Item{}
	for _, it := range items {
		if err := source.Validate(it, Fields, StateFields); err != nil {
			t.Fatalf("%s: %v", it.Key, err)
		}
		if _, dup := out[it.Key]; dup {
			t.Fatalf("duplicate key %s", it.Key)
		}
		out[it.Key] = it
	}
	return out
}

func TestEveryItem(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items)
	type want struct {
		typ, subject, title, author string
		addressed, own, siblings    bool
	}
	wants := map[string]want{
		"slack:D0DANA:1794132000.000100":   {TypeDM, "slack:dm:winton:D0DANA:1794132000.000100", "Direct message", "U0DANA", true, false, false},
		"slack:D0DANA:1794132600.000200":   {TypeDM, "slack:dm:winton:D0DANA:1794132600.000200", "Direct message", "U0DANA", true, false, false},
		"slack:D0DANA:1794133200.000300":   {TypeOwn, "slack:dm:winton:D0DANA:1794133200.000300", "Sent", "U0WINTON", false, true, true},
		"slack:D0BOT:1794131000.000050":    {TypeDM, "slack:dm:winton:D0BOT:1794131000.000050", "Direct message", "B0JIRA", true, false, false},
		"slack:G0GROUP:1794136800.001200":  {TypeMentioned, "slack:dm:winton:G0GROUP:1794136800.001200", "Mentioned", "U0DANA", true, false, false},
		"slack:C0ENG:1794133800.000400":    {TypeMentioned, "slack:C0ENG:1794133800.000400", "Mentioned", "U0BOB", true, false, false},
		"slack:C0ENG:1794130000.000600":    {TypeOwn, "slack:C0ENG:1794120000.000500", "Replied", "U0WINTON", false, true, false},
		"slack:C0ENG:1794134400.000700":    {TypeThreadReply, "slack:C0ENG:1794120000.000500", "Thread reply", "U0BOB", false, false, false},
		"slack:C0REVIEW:1794136200.001100": {TypeOwn, "slack:C0REVIEW:1794136200.001100", "Sent", "U0WINTON", false, true, false},
		"slack:C0REVIEW:1794135000.000900": {TypeReviewRequest, "slack:C0REVIEW:1794135000.000900", "Review requested", "U0ERIN", false, false, false},
	}
	if len(m) != len(wants) {
		var ks []string
		for k, it := range m {
			ks = append(ks, k+" "+it.Type)
		}
		sort.Strings(ks)
		t.Fatalf("got %d items:\n%s", len(m), strings.Join(ks, "\n"))
	}
	for k, w := range wants {
		it, ok := m[k]
		if !ok {
			t.Fatalf("missing %s", k)
		}
		if it.Type != w.typ || it.Subject != w.subject || it.EventTitle != w.title || it.Fields["author"] != w.author ||
			it.Addressed != w.addressed || it.Own != w.own || it.CloseSiblings != w.siblings {
			t.Errorf("%s: %+v", k, it)
		}
		if it.Version != it.Fields["ts"] || !strings.HasSuffix(k, ":"+it.Version) {
			t.Errorf("%s: version %q", k, it.Version)
		}
		if _, ok := it.Fields["body"]; ok {
			t.Errorf("%s: a body was set though no rule lists it", k)
		}
		if !strings.HasPrefix(it.Fields["permalink"], "https://acme.slack.com/archives/"+it.Fields["channel"]+"/p") {
			t.Errorf("%s: permalink %q", k, it.Fields["permalink"])
		}
		if !it.OccurredAt.Equal(tsTime(it.Fields["ts"])) {
			t.Errorf("%s: occurred %s", k, it.OccurredAt)
		}
	}
	own := m["slack:D0DANA:1794133200.000300"]
	if own.Actor != "slack:user:U0WINTON" {
		t.Fatalf("own actor %q", own.Actor)
	}
	if a := m["slack:D0BOT:1794131000.000050"]; a.Actor != "slack:bot:B0JIRA" || strings.Join(a.References, ",") != "jira:ENG-1" {
		t.Fatalf("bot %+v", a)
	}
	reply := m["slack:C0ENG:1794134400.000700"]
	if reply.Fields["thread_ts"] != "1794120000.000500" || reply.Fields["permalink"] != "https://acme.slack.com/archives/C0ENG/p1794134400000700?thread_ts=1794120000.000500&cid=C0ENG" ||
		strings.Join(reply.References, ",") != "github:pr:acme/app#7" || reply.Actor != "slack:user:U0BOB" {
		t.Fatalf("thread reply %+v", reply)
	}
	if r := m["slack:C0REVIEW:1794135000.000900"]; strings.Join(r.References, ",") != "github:pr:acme/app#42" || r.Fields["permalink"] != "https://acme.slack.com/archives/C0REVIEW/p1794135000000900" {
		t.Fatalf("review request %+v", r)
	}
	if _, ok := m["slack:C0ENG:1794133800.000400"].Fields["thread_ts"]; ok {
		t.Fatal("a top-level message has thread_ts")
	}
	for _, it := range m {
		for _, words := range []string{"deploy", "budget", "alerts", "offsite", "PTAL", "Merged"} {
			if strings.Contains(it.EventTitle, words) {
				t.Fatalf("%s: the title holds message text", it.Key)
			}
		}
	}
	if got.Cut || got.Dropped != 0 {
		t.Fatalf("cut %v dropped %d", got.Cut, got.Dropped)
	}

	// The first run reads 7 days; search's date is two days wider and only the window is kept.
	for _, q := range srv.Queries("/api/search.messages") {
		if !strings.HasSuffix(q.Get("query"), " after:2026-10-30") || q.Get("sort") != "timestamp" || q.Get("sort_dir") != "desc" {
			t.Fatalf("search %v", q)
		}
	}
	var c cursorData
	if err := json.Unmarshal(got.Next, &c); err != nil {
		t.Fatal(err)
	}
	if c.Since != "2026-11-08T12:00:00Z" || c.Channels[slacktest.Review] != "1794136200.001100" {
		t.Fatalf("cursor %s", got.Next)
	}
	eng := c.Threads["C0ENG/1794120000.000500"]
	if eng == nil || eng.Joined != "1794130000.000600" || eng.Seen != "1794134400.000700" || eng.DM || eng.Polled != "2026-11-08T12:00:00Z" {
		t.Fatalf("thread %s", got.Next)
	}
	if _, ok := c.Threads["D0DANA/1794133200.000300"]; ok {
		t.Fatal("a one-to-one direct message is followed as a thread; search already reads it")
	}
	if len(c.Threads) != 3 {
		t.Fatalf("threads %s", got.Next)
	}
	if strings.Contains(string(got.Next), "deploy") || strings.Contains(string(got.Next), "Merged") {
		t.Fatal("the cursor holds message text")
	}
}

func TestBodiesOnlyWhenListed(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	s.SetBodies(true)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items)
	if b := m["slack:D0DANA:1794132000.000100"].Fields["body"]; b != "Can you check the deploy?" {
		t.Fatalf("body %q", b)
	}
}

// A second pass reads from the cursor: search from its window, threads and the channel from their watermarks. It
// yields nothing the first pass already did, and the keys of a re-read are the same.
func TestCursorWindows(t *testing.T) {
	srv := slacktest.New(t)
	s, sl := newSource(t, srv)
	first, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Items) != len(first.Items) {
		t.Fatal("a re-read differs")
	}
	for i := range again.Items {
		if again.Items[i].Key != first.Items[i].Key {
			t.Fatalf("key %s became %s", first.Items[i].Key, again.Items[i].Key)
		}
	}
	// Quiet threads are read again after 15 minutes.
	_ = sl.sleep(context.Background(), 16*time.Minute)
	before := len(srv.Requests())
	next, err := s.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Items) != 0 {
		t.Fatalf("second pass yielded %d items", len(next.Items))
	}
	var searched, replied, history int
	for _, r := range srv.Requests()[before:] {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/api/search.messages":
			searched++
			if !strings.HasSuffix(q.Get("query"), " after:2026-11-06") {
				t.Fatalf("search %v", q)
			}
		case "/api/conversations.replies":
			replied++
			if q.Get("ts") == "1794120000.000500" && q.Get("oldest") != "1794134400.000700" {
				t.Fatalf("replies %v", q)
			}
		case "/api/conversations.history":
			history++
			if q.Get("oldest") != "1794136200.001100" || q.Get("channel") != slacktest.Review {
				t.Fatalf("history %v", q)
			}
		}
	}
	// Each search stops at its first page: every match is older than the window.
	if searched != 3 || replied != 3 || history != 1 {
		t.Fatalf("calls: search %d, replies %d, history %d", searched, replied, history)
	}
}

func TestNewReplyInFollowedThread(t *testing.T) {
	srv := slacktest.New(t)
	s, sl := newSource(t, srv)
	first, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = sl.sleep(context.Background(), 16*time.Minute)
	srv.Routes["/api/conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ts") != "1794120000.000500" {
			io.WriteString(w, `{"ok":true,"messages":[]}`)
			return
		}
		io.WriteString(w, strings.Replace(slacktest.RepliesEng, "\n]", `,
  {"type":"message","user":"U0CAROL","text":"<@U0WINTON> thanks!","ts":"1794139000.000900","thread_ts":"1794120000.000500"},
  {"type":"message","user":"U0DAVE","text":"me too","ts":"1794139100.000950","thread_ts":"1794120000.000500"}
]`, 1))
	}
	next, err := s.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, next.Items)
	if len(m) != 2 || m["slack:C0ENG:1794139000.000900"].Type != TypeMentioned || m["slack:C0ENG:1794139100.000950"].Type != TypeThreadReply {
		t.Fatalf("got %+v", next.Items)
	}
	if m["slack:C0ENG:1794139100.000950"].Subject != "slack:C0ENG:1794120000.000500" {
		t.Fatal("a reply is not on its thread's subject")
	}
}

func TestRefusals(t *testing.T) {
	cases := map[string]struct {
		change func(*Options, *slacktest.Server)
		want   error
	}{
		"refused token": {func(o *Options, _ *slacktest.Server) {
			o.Token = func() (keychain.Secret, error) { return keychain.FromString("xoxp-wrong") }
		}, source.ErrUnauthorized},
		"missing scope": {func(_ *Options, srv *slacktest.Server) {
			srv.Scopes = "search:read,channels:history,chat:write"
		}, source.ErrUnauthorized},
		"another user":      {func(o *Options, _ *slacktest.Server) { o.User = "U0OTHER" }, source.ErrIdentity},
		"another workspace": {func(o *Options, _ *slacktest.Server) { o.Workspace = "T0OTHER" }, source.ErrIdentity},
		"identity mismatch": {func(o *Options, _ *slacktest.Server) { o.Identity = "slack:user:U0OTHER" }, source.ErrIdentity},
		"revoked mid-pass": {func(_ *Options, srv *slacktest.Server) {
			srv.Routes["/api/conversations.history"] = slacktest.Text(`{"ok":false,"error":"token_revoked"}`)
		}, source.ErrUnauthorized},
		"bot token": {func(_ *Options, srv *slacktest.Server) {
			srv.Routes["/api/auth.test"] = slacktest.Text(`{"ok":true,"team_id":"T0ACME","user_id":"U0WINTON","bot_id":"B0X"}`)
		}, source.ErrUnauthorized},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := slacktest.New(t)
			o := opts(srv, &sleeps{})
			tc.change(&o, srv)
			s, err := New(o)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Collect(context.Background(), nil)
			if !errors.Is(err, tc.want) || got.Next != nil {
				t.Fatalf("%v, next %q", err, got.Next)
			}
		})
	}
}

func TestRateLimits(t *testing.T) {
	srv := slacktest.New(t)
	n := 0
	srv.Routes["/api/search.messages"] = func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"ok":true,"messages":{"paging":{"pages":1},"matches":[]}}`)
	}
	s, sl := newSource(t, srv)
	if _, err := s.Collect(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range sl.got {
		found = found || d == 7*time.Second
	}
	if !found {
		t.Fatalf("Retry-After not honoured: %v", sl.got)
	}

	srv.Routes["/api/search.messages"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}
	if got, err := s.Collect(context.Background(), nil); !errors.Is(err, source.ErrRateLimited) || got.Next != nil {
		t.Fatalf("long wait: %v", err)
	}
	srv.Routes["/api/search.messages"] = slacktest.Text(`{"ok":false,"error":"ratelimited"}`)
	if _, err := s.Collect(context.Background(), nil); !errors.Is(err, source.ErrRateLimited) {
		t.Fatalf("ratelimited: %v", err)
	}
}

// Calls to one method are spaced by its tier: search 3 seconds, replies and history 1.2.
func TestPacing(t *testing.T) {
	srv := slacktest.New(t)
	s, sl := newSource(t, srv)
	if _, err := s.Collect(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	count := map[time.Duration]int{}
	for _, d := range sl.got {
		count[d]++
	}
	// Four searches (two pages of direct messages) wait three times; three thread reads wait twice; one history read
	// does not wait. The clock moves only by those waits, so each is the whole spacing.
	if count[3*time.Second] != 3 || count[1200*time.Millisecond] != 2 || len(sl.got) != 5 {
		t.Fatalf("waits %v", sl.got)
	}
}

func TestScope(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	a := s.api
	ok := []string{
		srv.URL + "/api/auth.test",
		srv.URL + "/api/search.messages?query=is%3Adm+after%3A2026-11-01&count=100&page=1&sort=timestamp&sort_dir=desc",
		srv.URL + "/api/search.messages?query=%3C%40U0WINTON%3E+after%3A2026-11-01",
		srv.URL + "/api/search.messages?query=from%3A%3C%40U0WINTON%3E+after%3A2026-11-01",
		srv.URL + "/api/conversations.replies?channel=C0ENG&ts=1794120000.000500&oldest=1794120000.000500&limit=200",
		srv.URL + "/api/conversations.history?channel=C0REVIEW&oldest=1794120000.000500&limit=200&cursor=bmV4dA%3D%3D",
	}
	for _, u := range ok {
		if err := a.allow(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	refused := []string{
		"https://evil.example/api/auth.test",
		strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/api/auth.test",
		srv.URL + "/api/chat.postMessage?channel=C0ENG",
		srv.URL + "/api/conversations.mark?channel=C0ENG",
		srv.URL + "/api/users.list",
		srv.URL + "/auth.test",
		srv.URL + "/api/auth.test?token=xoxp-x",
		srv.URL + "/api/search.messages?query=password",
		srv.URL + "/api/search.messages?query=%3C%40U0OTHER%3E+after%3A2026-11-01",
		srv.URL + "/api/search.messages?query=is%3Adm+after%3A2026-11-01&highlight=true",
		srv.URL + "/api/conversations.history?channel=C0ENG",
		srv.URL + "/api/conversations.history?channel=C0REVIEW&inclusive=true",
		srv.URL + "/api/conversations.replies?channel=C0ENG&ts=bad",
		srv.URL + "/api/conversations.replies?channel=C0ENG&ts=1794120000.000500&ts=1794120000.000600",
	}
	for _, u := range refused {
		if err := a.allow(u); !errors.Is(err, source.ErrScope) {
			t.Errorf("%s: allowed", u)
		}
	}
	if h := s.Hosts(); len(h) != 1 || h[0] != strings.TrimPrefix(srv.URL, "http://") {
		t.Fatalf("hosts %v", h)
	}
	if h := Hosts(Base); len(h) != 1 || h[0] != "slack.com:443" {
		t.Fatalf("hosts %v", h)
	}
}

// A followed thread that is gone is dropped from the cursor; the pass goes on.
func TestGoneThread(t *testing.T) {
	srv := slacktest.New(t)
	srv.Routes["/api/conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") == "C0RANDOM" {
			io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
			return
		}
		io.WriteString(w, `{"ok":true,"messages":[]}`)
	}
	s, _ := newSource(t, srv)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var c cursorData
	if err := json.Unmarshal(got.Next, &c); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Threads["C0RANDOM/1794137000.001300"]; ok || c.Threads["C0ENG/1794120000.000500"] == nil {
		t.Fatalf("threads %s", got.Next)
	}

	// A configured channel that cannot be read keeps its watermark.
	srv.Routes["/api/conversations.history"] = slacktest.Text(`{"ok":false,"error":"not_in_channel"}`)
	again, err := s.Collect(context.Background(), got.Next)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(again.Next), `"C0REVIEW":"1794136200.001100"`) {
		t.Fatalf("channel watermark lost: %s", again.Next)
	}

	srv.Routes["/api/conversations.replies"] = slacktest.Text(`{"ok":false,"error":"internal_error"}`)
	if got, err := s.Collect(context.Background(), nil); err == nil || got.Next != nil {
		t.Fatal("an unknown error did not fail the pass")
	}
}

// Threads are read longest-unread first, at most threadsPerPass a pass; old ones expire.
func TestThreadRotation(t *testing.T) {
	threads := map[string]*thread{}
	for i := 0; i < threadsPerPass+5; i++ {
		threads[fmt.Sprintf("C0ENG/17941%05d.000000", i)] = &thread{Joined: "1794100000.000000", Last: "1794100000.000000", Polled: "2026-11-08T11:00:00Z"}
	}
	threads["C0ENG/1794199999.000000"] = &thread{Joined: "1794100000.000000", Last: "1794100000.000000"}
	// None was read in the last 15 minutes, so all are due; the one never read comes first.
	order := pollOrder(threads, testNow.Add(time.Hour))
	if len(order) != threadsPerPass || order[0] != "C0ENG/1794199999.000000" {
		t.Fatalf("order %v", order)
	}
	// Two minutes after a read, a thread with a message in the last hour is due, and a quiet one is not until 15
	// minutes have passed.
	threads2 := map[string]*thread{
		"C0ENG/1794100000.000000": {Joined: "1794100000.000000", Last: "1794100000.000000", Polled: "2026-11-08T11:58:00Z"},
		"C0ENG/1794138000.000000": {Joined: "1794138000.000000", Last: "1794138000.000000", Polled: "2026-11-08T11:58:00Z"},
		"C0ENG/1794120000.000000": {Joined: "1794100000.000000", Last: "1794100000.000000", Seen: "1794138500.000000", Polled: "2026-11-08T11:58:00Z"},
		"C0ENG/1794110000.000000": {Joined: "1794100000.000000", Last: "1794100000.000000", Polled: "2026-11-08T11:40:00Z"},
	}
	order = pollOrder(threads2, testNow)
	if strings.Join(order, " ") != "C0ENG/1794110000.000000 C0ENG/1794120000.000000 C0ENG/1794138000.000000" {
		t.Fatalf("due %v", order)
	}

	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	stale, _ := json.Marshal(cursorData{Since: "2026-11-08T11:00:00Z", Threads: map[string]*thread{
		"C0OLD/1793000000.000000": {Joined: "1793000000.000000", Last: "1793000000.000000"},
	}})
	got, err := s.Collect(context.Background(), stale)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got.Next), "C0OLD") {
		t.Fatal("a thread the person last posted in more than 7 days ago is still followed")
	}
	for _, q := range srv.Queries("/api/conversations.replies") {
		if q.Get("channel") == "C0OLD" {
			t.Fatal("an expired thread was read")
		}
	}
}

func TestBadCursor(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	for _, c := range []string{"not json", `{"since":"yesterday"}`, `{"since":"2026-11-08T11:00:00Z","threads":{"C0ENG/x":{"joined":"1","last":"1"}}}`} {
		if _, err := s.Collect(context.Background(), []byte(c)); err == nil {
			t.Errorf("%s: accepted", c)
		}
	}
}

func TestVerify(t *testing.T) {
	srv := slacktest.New(t)
	srv.Scopes = slacktest.AllScopes + ",chat:write"
	tok, _ := keychain.FromString(slacktest.Token)
	id, err := Verify(context.Background(), srv.Client(), srv.URL, tok)
	if err != nil {
		t.Fatal(err)
	}
	if id.Workspace != slacktest.Workspace || id.User != slacktest.User || id.URL != "https://acme.slack.com" || len(id.Missing) != 0 || strings.Join(id.Extra, ",") != "chat:write" {
		t.Fatalf("%+v", id)
	}
	srv.Scopes = "search:read"
	if id, err := Verify(context.Background(), srv.Client(), srv.URL, tok); err != nil || strings.Join(id.Missing, ",") != "channels:history,groups:history,im:history,mpim:history" {
		t.Fatalf("%+v %v", id, err)
	}
}

func TestNewRefuses(t *testing.T) {
	srv := slacktest.New(t)
	for name, change := range map[string]func(*Options){
		"act handle":  func(o *Options) { o.Person = "act_123" },
		"no user":     func(o *Options) { o.User = "" },
		"dm channel":  func(o *Options) { o.Channels = []string{"D0DANA"} },
		"http remote": func(o *Options) { o.Base = "http://slack.example.com" },
		"path":        func(o *Options) { o.Base = srv.URL + "/api" },
	} {
		o := opts(srv, &sleeps{})
		change(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
