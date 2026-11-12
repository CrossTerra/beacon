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
		typ, subject, title, author, kind string
		addressed, own                    bool
		before                            string
	}
	wants := map[string]want{
		"slack:D0DANA:1794132000.000100":   {TypeDM, "slack:dm:winton:D0DANA:1794132000.000100", "Direct message", "U0DANA", "im", true, false, ""},
		"slack:D0DANA:1794132600.000200":   {TypeDM, "slack:dm:winton:D0DANA:1794132600.000200", "Direct message", "U0DANA", "im", true, false, ""},
		"slack:D0DANA:1794133200.000300":   {TypeOwn, "slack:dm:winton:D0DANA:1794133200.000300", "Sent", "U0WINTON", "im", false, true, "1794133200.000300"},
		"slack:D0BOT:1794131000.000050":    {TypeDM, "slack:dm:winton:D0BOT:1794131000.000050", "Direct message", "B0JIRA", "im", true, false, ""},
		"slack:G0GROUP:1794136800.001200":  {TypeMentioned, "slack:dm:winton:G0GROUP:1794136800.001200", "Mentioned", "U0DANA", "mpim", true, false, ""},
		"slack:G0GROUP:1794137500.001400":  {TypeDM, "slack:dm:winton:G0GROUP:1794137500.001400", "Direct message", "U0CAROL", "mpim", true, false, ""},
		"slack:G0GROUP:1794137600.001500":  {TypeOwn, "slack:dm:winton:G0GROUP:1794137600.001500", "Sent", "U0WINTON", "mpim", false, true, ""},
		"slack:C0SECRET:1794134000.000450": {TypeMentioned, "slack:dm:winton:C0SECRET:1794134000.000450", "Mentioned", "U0HR", "private", true, false, ""},
		"slack:C0ENG:1794133800.000400":    {TypeMentioned, "slack:C0ENG:1794133800.000400", "Mentioned", "U0BOB", "channel", true, false, ""},
		"slack:C0ENG:1794134500.000750":    {TypeThreadReply, "slack:C0ENG:1794133800.000400", "Thread reply", "U0CAROL", "channel", false, false, ""},
		"slack:C0ENG:1794130000.000600":    {TypeOwn, "slack:C0ENG:1794120000.000500", "Replied", "U0WINTON", "channel", false, true, ""},
		"slack:C0ENG:1794134400.000700":    {TypeThreadReply, "slack:C0ENG:1794120000.000500", "Thread reply", "U0BOB", "channel", false, false, ""},
		"slack:C0REVIEW:1794136200.001100": {TypeOwn, "slack:C0REVIEW:1794136200.001100", "Sent", "U0WINTON", "channel", false, true, ""},
		"slack:C0REVIEW:1794135000.000900": {TypeReviewRequest, "slack:C0REVIEW:1794135000.000900", "Review requested", "U0ERIN", "channel", false, false, ""},
		"slack:C0REVIEW:1794135800.001060": {TypeMentioned, "slack:C0REVIEW:1794135800.001060", "Mentioned", "U0BOB", "channel", true, false, ""},
		"slack:C0REVIEW:1794135700.001050": {TypeMentioned, "slack:C0REVIEW:1794135700.001050", "Mentioned", "U0FRANK", "channel", true, false, ""},
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
			it.Fields["channel_type"] != w.kind || it.Addressed != w.addressed || it.Own != w.own || it.CloseSiblingsBefore != w.before {
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

	if _, ok := m["slack:C0REVIEW:1794135900.001070"]; ok {
		t.Fatal("a mention of a user group the person is not in is a mention")
	}
	if r := m["slack:C0SECRET:1794134000.000450"]; strings.Join(r.References, ",") != "github:pr:acme/hr#3" {
		t.Fatalf("private mention %+v", r)
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
	if c.Since != "2026-11-08T12:00:00Z" || c.Channels[slacktest.Review].Seen != "1794136200.001100" || c.Channels[slacktest.Review].Kind != "channel" ||
		c.Channels["G0GROUP"].Seen != "1794137600.001500" || c.Channels["G0GROUP"].Kind != "mpim" {
		t.Fatalf("cursor %s", got.Next)
	}
	eng := c.Threads["C0ENG/1794120000.000500"]
	if eng == nil || eng.Joined != "1794130000.000600" || eng.Seen != "1794134400.000700" || eng.Kind != "channel" || eng.Polled != "2026-11-08T12:00:00Z" {
		t.Fatalf("thread %s", got.Next)
	}
	if mention := c.Threads["C0ENG/1794133800.000400"]; mention == nil || mention.Joined != "1794133800.000400" {
		t.Fatalf("a thread the person was mentioned in is not followed: %s", got.Next)
	}
	if secret := c.Threads["C0SECRET/1794134000.000450"]; secret == nil || secret.Kind != "private" {
		t.Fatalf("private thread %s", got.Next)
	}
	if _, ok := c.Threads["D0DANA/1794133200.000300"]; ok {
		t.Fatal("a one-to-one direct message is followed as a thread; search already reads it")
	}
	// Own posts in C0ENG, C0RANDOM, C0REVIEW and G0GROUP; mentions in C0ENG, C0SECRET, G0GROUP and twice in C0REVIEW.
	if len(c.Threads) != 9 {
		t.Fatalf("threads %s", got.Next)
	}
	if len(c.Mentions) != 3 || len(c.Asked) != 9 {
		t.Fatalf("mentions %v asked %d", c.Mentions, len(c.Asked))
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
			want := " after:2026-11-06"
			if strings.HasPrefix(q.Get("query"), "<@") {
				// Mentions are searched over 7 days every pass, for edits that add one.
				want = " after:2026-10-30"
			}
			if !strings.HasSuffix(q.Get("query"), want) {
				t.Fatalf("search %v", q)
			}
		case "/api/conversations.replies":
			replied++
			if q.Get("inclusive") == "" && q.Get("ts") == "1794120000.000500" && q.Get("oldest") != "1794134400.000700" {
				t.Fatalf("replies %v", q)
			}
		case "/api/conversations.history":
			history++
			if (q.Get("channel") == slacktest.Review && q.Get("oldest") != "1794136200.001100") || (q.Get("channel") == "G0GROUP" && q.Get("oldest") != "1794137600.001500") {
				t.Fatalf("history %v", q)
			}
		}
	}
	// Each search stops at its first page: every match is older than the window. Nine threads, nine deletion checks,
	// the channel and the group direct message.
	if searched != 3 || replied != 18 || history != 2 {
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
		if r.URL.Query().Get("ts") != "1794120000.000500" || r.URL.Query().Get("inclusive") != "" {
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
	for k, it := range m {
		if it.Type == TypeDeleted {
			delete(m, k) // this route answers every existence check with nothing
		}
	}
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
			srv.Scopes = "search:read,channels:history,groups:history,im:history,mpim:history,chat:write"
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
	// Four searches (two pages of direct messages) wait three times; nine thread reads wait eight times, two history
	// reads once. User groups, the channel's info and the group list are one call each. The clock moves only by those
	// waits, so each is the whole spacing.
	if count[3*time.Second] != 3 || count[1200*time.Millisecond] != 9 || len(sl.got) != 12 {
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
		srv.URL + "/api/conversations.history?channel=C0REVIEW&oldest=1794120000.000500&latest=1794120000.000600&limit=200&cursor=bmV4dA%3D%3D",
		srv.URL + "/api/conversations.replies?channel=C0ENG&ts=1794120000.000500&oldest=1794120000.000600&inclusive=true&limit=2",
		srv.URL + "/api/conversations.info?channel=C0REVIEW",
		srv.URL + "/api/users.conversations?types=mpim&exclude_archived=true&limit=200",
		srv.URL + "/api/usergroups.list?include_users=true",
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
		srv.URL + "/api/conversations.history?channel=G0OTHER",
		srv.URL + "/api/conversations.info?channel=C0ENG",
		srv.URL + "/api/users.conversations?types=public_channel",
		srv.URL + "/api/usergroups.users.list?usergroup=S0ONCALL",
		srv.URL + "/api/users.list",
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
	if !strings.Contains(string(again.Next), `"C0REVIEW":{"kind":"channel","seen":"1794136200.001100"}`) {
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
	order := dueOrder(threads, testNow.Add(time.Hour), threadsPerPass)
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
	order = dueOrder(threads2, testNow, threadsPerPass)
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
	if id, err := Verify(context.Background(), srv.Client(), srv.URL, tok); err != nil || strings.Join(id.Missing, ",") != "channels:history,groups:history,im:history,mpim:history,channels:read,groups:read,mpim:read,usergroups:read" {
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

// A thread followed in a pass that cannot read it (the per-pass limit) starts at that pass's window, so a later pass
// reads every reply since.
func TestNewThreadStartsAtItsPass(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	threads := map[string]*thread{}
	for i := 0; i < threadsPerPass; i++ {
		threads[fmt.Sprintf("C0AAA/17941%05d.000000", i)] = &thread{Joined: "1794130000.000000", Last: "1794130000.000000", Seen: "1794130000.000000"}
	}
	start, _ := json.Marshal(cursorData{Since: "2026-11-08T11:00:00Z", Threads: threads})
	got, err := s.Collect(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	var c cursorData
	if err := json.Unmarshal(got.Next, &c); err != nil {
		t.Fatal(err)
	}
	random := c.Threads["C0RANDOM/1794137000.001300"]
	if random == nil || random.Polled != "" || random.Seen != formatTS(time.Date(2026, 11, 8, 10, 45, 0, 0, time.UTC)) {
		t.Fatalf("new thread %+v", random)
	}
	for _, q := range srv.Queries("/api/conversations.replies") {
		if q.Get("channel") == "C0RANDOM" {
			t.Fatal("the limit did not hold")
		}
	}
}

// History that stops at its page limit keeps its watermark and reads the gap below what it read on the next pass.
func TestHistoryGap(t *testing.T) {
	srv := slacktest.New(t)
	// The first read always has more: ten pages of one message each, newest first. The gap read ends.
	page := 0
	srv.Routes["/api/conversations.history"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("channel") != slacktest.Review {
			io.WriteString(w, `{"ok":true,"messages":[]}`)
			return
		}
		if q.Get("latest") != "" {
			io.WriteString(w, `{"ok":true,"messages":[{"type":"message","user":"U0ERIN","text":"PTAL https://github.com/acme/app/pull/9 review","ts":"1794138000.000001"}]}`)
			return
		}
		page++
		fmt.Fprintf(w, `{"ok":true,"messages":[{"type":"message","user":"U0FRANK","text":"hi","ts":"17941386%02d.000000"}],"response_metadata":{"next_cursor":"more"}}`, 99-page)
	}
	s, sl := newSource(t, srv)
	start, _ := json.Marshal(cursorData{Since: "2026-11-08T12:00:00Z", Channels: map[string]*watermark{slacktest.Review: {Kind: "channel", Seen: "1794137000.000000"}}})
	first, err := s.Collect(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	var c cursorData
	json.Unmarshal(first.Next, &c)
	w := c.Channels[slacktest.Review]
	if w.Seen != "1794137000.000000" || w.Gap != "1794138689.000000" || w.Head != "1794138698.000000" || first.Cut {
		t.Fatalf("watermark %+v cut %v", w, first.Cut)
	}
	_ = sl.sleep(context.Background(), 16*time.Minute)
	second, err := s.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	qs := srv.Queries("/api/conversations.history")
	last := qs[len(qs)-1]
	for _, q := range qs {
		if q.Get("channel") == slacktest.Review {
			last = q
		}
	}
	if last.Get("oldest") != "1794137000.000000" || last.Get("latest") != "1794138689.000000" {
		t.Fatalf("gap read %v", last)
	}
	if m := byKey(t, second.Items); m["slack:C0REVIEW:1794138000.000001"].Type != TypeReviewRequest {
		t.Fatalf("the gap's message was not read: %v", second.Items)
	}
	json.Unmarshal(second.Next, &c)
	if w := c.Channels[slacktest.Review]; w.Seen != "1794138698.000000" || w.Gap != "" || w.Head != "" {
		t.Fatalf("after the gap %+v", w)
	}
}

// A message that addressed the person and is deleted yields message_deleted on its subject, once.
func TestDeletedAsk(t *testing.T) {
	srv := slacktest.New(t)
	s, sl := newSource(t, srv)
	first, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(srv.Queries("/api/conversations.replies")), "inclusive:[true]") {
		t.Fatal("an ask seen this pass was checked again")
	}
	srv.Deleted["D0DANA/1794132000.000100"] = true
	_ = sl.sleep(context.Background(), 16*time.Minute)
	second, err := s.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, second.Items)
	d, ok := m["slack:D0DANA:1794132000.000100:deleted"]
	if !ok || len(m) != 1 || d.Type != TypeDeleted || d.Subject != "slack:dm:winton:D0DANA:1794132000.000100" || d.EventTitle != "Deleted" || d.Own || d.Actor != "" || d.Version != "deleted" ||
		strings.Join(d.CloseOnly, ",") != "slack:D0DANA:1794132000.000100" {
		t.Fatalf("got %+v", second.Items)
	}
	if strings.Contains(string(second.Next), "D0DANA/1794132000.000100") {
		t.Fatal("a deleted ask is still tracked")
	}
	_ = sl.sleep(context.Background(), 16*time.Minute)
	third, err := s.Collect(context.Background(), second.Next)
	if err != nil || len(third.Items) != 0 {
		t.Fatalf("again: %v %v", third.Items, err)
	}
}

// A mention search match older than the window that was not seen before is a mention an edit added, and is an item.
func TestEditAddsMention(t *testing.T) {
	srv := slacktest.New(t)
	s, sl := newSource(t, srv)
	first, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Routes["/api/search.messages"] = func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Query().Get("query"), "<@") {
			io.WriteString(w, `{"ok":true,"messages":{"paging":{"pages":1},"matches":[]}}`)
			return
		}
		io.WriteString(w, strings.Replace(slacktest.SearchMentions, `"matches":[`, `"matches":[
  {"type":"message","channel":{"id":"C0ENG","is_private":false},"user":"U0BOB","ts":"1794100000.000100","text":"edited: <@U0WINTON> please check","permalink":"https://acme.slack.com/archives/C0ENG/p1794100000000100"},`, 1))
	}
	_ = sl.sleep(context.Background(), 16*time.Minute)
	second, err := s.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, second.Items)
	if m["slack:C0ENG:1794100000.000100"].Type != TypeMentioned {
		t.Fatalf("got %v", second.Items)
	}
	for k := range m {
		if k == "slack:C0ENG:1794133800.000400" || k == "slack:G0GROUP:1794136800.001200" {
			t.Fatalf("%s was emitted again", k)
		}
	}
}

func TestKinds(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		c    searchChannel
		want string
	}{
		{searchChannel{ID: "D1", IsIM: false}, kindIM},
		{searchChannel{ID: "G1", IsMPIM: true, IsPrivate: &yes}, kindMPIM},
		{searchChannel{ID: "C1", IsPrivate: &yes}, kindPrivate},
		{searchChannel{ID: "C1", IsPrivate: &no}, kindChannel},
		{searchChannel{ID: "C1"}, kindPrivate},
		{searchChannel{ID: "G1", IsPrivate: &no}, kindPrivate},
	} {
		if got := tc.c.kind(); got != tc.want {
			t.Errorf("%+v: %s", tc.c, got)
		}
	}
	if (&message{channel: "C1"}).personal() != true {
		t.Fatal("a channel of unknown kind is not personal")
	}
}

func TestBroadcastOnlyInConfiguredChannels(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	s.groups = map[string]bool{"S0ONCALL": true}
	for text, want := range map[string]bool{
		"<!here> standup":                false,
		"<!channel|channel> fire drill":  false,
		"<!subteam^S0ONCALL> pager":      true,
		"<!subteam^S0DESIGN|@design> ok": false,
		"<@U0WINTON|winton> question":    true,
		"<@U0WINTONX> another person":    false,
	} {
		if got := s.mentions(&message{channel: "C0ENG", text: text}); got != want {
			t.Errorf("%q in C0ENG: %v", text, got)
		}
	}
	if !s.mentions(&message{channel: slacktest.Review, text: "<!everyone> hello"}) {
		t.Fatal("@everyone in a configured channel is not a mention")
	}
}

// The raise reads a message's text, and its thread root's, whether or not a rule sends them; the native id is the
// channel and ts (DEC-31, DEC-33).
func TestLocalTextAndNative(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items)
	dm := m["slack:D0DANA:1794132000.000100"]
	if _, ok := dm.Fields["body"]; ok {
		t.Fatal("a body without a rule listing it")
	}
	if dm.Local == nil || dm.Local.Text["body"] != "Can you check the deploy?" || dm.Native != "D0DANA:1794132000.000100" {
		t.Fatalf("dm %q %+v", dm.Native, dm.Local)
	}
	reply := m["slack:C0ENG:1794134500.000750"]
	if reply.Local.Text["body"] != "I'll handle it" || reply.Local.Text["thread_root"] != "<@U0WINTON> can you look at the alerts?" {
		t.Fatalf("reply local %+v", reply.Local)
	}
	if strings.Contains(reply.Native, "@") {
		t.Fatal("native holds @")
	}
}
