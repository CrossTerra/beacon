package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/slack/slacktest"
	"github.com/CrossTerra/beacon/internal/source"
)

// Channels the person is a member of, as users.conversations lists them: a public one, a private one, one that does
// not say whether it is private, and an archived one.
const memberList = `{"ok":true,"channels":[
  {"id":"C0ENG","name":"eng","is_channel":true,"is_private":false},
  {"id":"C0SECRET","name":"secret","is_channel":true,"is_private":true},
  {"id":"C0QUIET","name":"quiet","is_channel":true},
  {"id":"C0OLDARC","name":"old","is_channel":true,"is_private":false,"is_archived":true}
],"response_metadata":{"next_cursor":""}}`

// 2026-11-08 in UTC: 1794139200 is 12:00.
const (
	// C0ENG's top level, newest first: Frank's message with no replies, Bob's @here, Carol's root with two replies,
	// Erin's review request, an app's message, and Dana's message from 7 November (UTC) with no replies.
	historyEng = `{"ok":true,"messages":[
  {"type":"message","user":"U0FRANK","text":"prod is slow for acme","ts":"1794137000.000100"},
  {"type":"message","user":"U0BOB","text":"<!here> standup moved","ts":"1794136000.000100"},
  {"type":"message","user":"U0CAROL","text":"the export job failed","ts":"1794135000.000100","thread_ts":"1794135000.000100","reply_count":2,"latest_reply":"1794135600.000100"},
  {"type":"message","user":"U0ERIN","text":"PTAL https://github.com/acme/app/pull/5","ts":"1794134000.000100"},
  {"type":"message","subtype":"bot_message","bot_id":"B0CI","text":"build green","ts":"1794133000.000100"},
  {"type":"message","user":"U0DANA","text":"night owl","ts":"1794050000.000100"}
]}`
	repliesExport = `{"ok":true,"messages":[
  {"type":"message","user":"U0CAROL","text":"the export job failed","ts":"1794135000.000100","thread_ts":"1794135000.000100","reply_count":2},
  {"type":"message","user":"U0DANA","text":"looking","ts":"1794135300.000100","thread_ts":"1794135000.000100"},
  {"type":"message","user":"U0FRANK","text":"fixed","ts":"1794135600.000100","thread_ts":"1794135000.000100"}
]}`
	historySecret = `{"ok":true,"messages":[
  {"type":"message","user":"U0HR","text":"offsite budget","ts":"1794136500.000100"},
  {"type":"message","user":"U0HR","text":"hiring plan","ts":"1794135500.000100","thread_ts":"1794135500.000100","reply_count":1,"latest_reply":"1794135800.000100"}
]}`
	repliesHiring = `{"ok":true,"messages":[
  {"type":"message","user":"U0HR","text":"hiring plan","ts":"1794135500.000100","thread_ts":"1794135500.000100","reply_count":1},
  {"type":"message","user":"U0DANA","text":"two roles","ts":"1794135800.000100","thread_ts":"1794135500.000100"}
]}`
)

// memberServer is a fake Slack whose person is a member of memberList's channels. Only one-to-one direct messages are
// in search; channels are read by history.
func memberServer(t *testing.T) *slacktest.Server {
	srv := slacktest.New(t)
	empty := slacktest.Text(`{"ok":true,"messages":{"paging":{"pages":1},"matches":[]}}`)
	srv.Routes["/api/search.messages"] = empty
	srv.Routes["/api/users.conversations"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("types") == "public_channel,private_channel" {
			io.WriteString(w, memberList)
			return
		}
		io.WriteString(w, `{"ok":true,"channels":[],"response_metadata":{"next_cursor":""}}`)
	}
	srv.Routes["/api/conversations.history"] = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("channel") {
		case "C0ENG":
			io.WriteString(w, historyEng)
		case "C0SECRET":
			io.WriteString(w, historySecret)
		default:
			io.WriteString(w, `{"ok":true,"messages":[]}`)
		}
	}
	srv.Routes["/api/conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("channel") + "/" + r.URL.Query().Get("ts") {
		case "C0ENG/1794135000.000100":
			io.WriteString(w, repliesExport)
		case "C0SECRET/1794135500.000100":
			io.WriteString(w, repliesHiring)
		default:
			io.WriteString(w, `{"ok":true,"messages":[]}`)
		}
	}
	return srv
}

func memberSource(t *testing.T, srv *slacktest.Server, change func(*Options)) (*Source, *sleeps) {
	t.Helper()
	sl := &sleeps{}
	o := opts(srv, sl)
	o.Channels, o.Member = nil, true
	if change != nil {
		change(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, sl
}

// Someone else's message in a channel the source reads, which no earlier type claims, is channel_message: top level
// or reply, an app's included. It asks no one and carries the fields of a mention, with reply_count on a top-level
// message and the text only when a rule lists body.
func TestChannelMessages(t *testing.T) {
	srv := memberServer(t)
	s, _ := memberSource(t, srv, nil)
	s.SetBodies(true)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items)
	for key, want := range map[string]struct{ typ, subject, title, replies string }{
		// No replies: the channel's day.
		"slack:C0ENG:1794137000.000100": {TypeChannel, "slack:C0ENG:2026-11-08", "Channel message", "0"},
		// @here in a channel read only by membership asks the room, not the person.
		"slack:C0ENG:1794136000.000100": {TypeChannel, "slack:C0ENG:2026-11-08", "Channel message", "0"},
		// A root with replies is its own thread, and so are its replies.
		"slack:C0ENG:1794135000.000100": {TypeChannel, "slack:C0ENG:1794135000.000100", "Channel message", "2"},
		"slack:C0ENG:1794135300.000100": {TypeChannel, "slack:C0ENG:1794135000.000100", "Channel reply", ""},
		"slack:C0ENG:1794135600.000100": {TypeChannel, "slack:C0ENG:1794135000.000100", "Channel reply", ""},
		// An earlier type still wins: a review request in a channel read by membership.
		"slack:C0ENG:1794134000.000100": {TypeReviewRequest, "slack:C0ENG:1794134000.000100", "Review requested", "0"},
		// An app's message is a channel message; the starter rules drop it by its author.
		"slack:C0ENG:1794133000.000100": {TypeChannel, "slack:C0ENG:2026-11-08", "Channel message", "0"},
		"slack:C0ENG:1794050000.000100": {TypeChannel, "slack:C0ENG:2026-11-07", "Channel message", "0"},
		// A private channel's are personal: the same shapes under slack:dm:.
		"slack:C0SECRET:1794136500.000100": {TypeChannel, "slack:dm:winton:C0SECRET:2026-11-08", "Channel message", "0"},
		"slack:C0SECRET:1794135500.000100": {TypeChannel, "slack:dm:winton:C0SECRET:1794135500.000100", "Channel message", "1"},
		"slack:C0SECRET:1794135800.000100": {TypeChannel, "slack:dm:winton:C0SECRET:1794135500.000100", "Channel reply", ""},
	} {
		it, ok := m[key]
		if !ok {
			t.Errorf("missing %s", key)
			continue
		}
		if it.Type != want.typ || it.Subject != want.subject || it.EventTitle != want.title || it.Fields["reply_count"] != want.replies {
			t.Errorf("%s: %s %s %q reply_count %q", key, it.Type, it.Subject, it.EventTitle, it.Fields["reply_count"])
		}
		if it.Addressed || it.Own || len(it.CloseOnly) > 0 || it.Fields["body"] == "" {
			t.Errorf("%s: %+v", key, it)
		}
		if strings.Contains(key, "1794135300") && it.Fields["thread_ts"] != "1794135000.000100" {
			t.Errorf("%s: thread_ts %q", key, it.Fields["thread_ts"])
		}
	}
	if len(m) != 11 {
		t.Fatalf("%d items: %v", len(m), got.Items)
	}
	if bot := m["slack:C0ENG:1794133000.000100"]; bot.Fields["author"] != "B0CI" || bot.Actor != "slack:bot:B0CI" {
		t.Fatalf("app %+v", bot)
	}
	if k := m["slack:C0SECRET:1794136500.000100"].Fields["channel_type"]; k != kindPrivate {
		t.Fatalf("private channel %q", k)
	}
	if k := m["slack:C0ENG:1794137000.000100"].Fields["channel_type"]; k != kindChannel {
		t.Fatalf("public channel %q", k)
	}
	for _, it := range got.Items {
		if strings.HasPrefix(it.Subject, "slack:C0SECRET") || (it.Fields["channel"] == "C0SECRET" && !strings.HasPrefix(it.Subject, PersonalStem)) {
			t.Fatalf("a private channel item on a shared subject: %s", it.Subject)
		}
	}

	// Without a rule that lists body, no text.
	s2, _ := memberSource(t, memberServer(t), nil)
	got2, err := s2.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range got2.Items {
		if _, ok := it.Fields["body"]; ok {
			t.Fatalf("%s: body without a rule", it.Key)
		}
	}
}

// A channel's day is the machine's local day: the same message is on 7 November in Los Angeles.
func TestChannelDayIsLocal(t *testing.T) {
	s, _ := memberSource(t, memberServer(t), func(o *Options) { o.Location = time.FixedZone("PST", -8*3600) })
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items)
	if sub := m["slack:C0ENG:1794137000.000100"].Subject; sub != "slack:C0ENG:2026-11-08" {
		t.Fatalf("%s", sub)
	}
	// 1794050000 is 2026-11-07T11:13Z, 03:13 in PST.
	if sub := m["slack:C0ENG:1794050000.000100"].Subject; sub != "slack:C0ENG:2026-11-07" {
		t.Fatalf("%s", sub)
	}
	if sub := m["slack:C0SECRET:1794136500.000100"].Subject; sub != "slack:dm:winton:C0SECRET:2026-11-08" {
		t.Fatalf("%s", sub)
	}
}

// With member, users.conversations lists the person's public and private channels, at most once an hour. Only those
// are read; one the person left is dropped, one Slack does not say is public is private, and an archived one is not
// read. Without member, that list cannot be asked for.
func TestMemberDiscovery(t *testing.T) {
	srv := memberServer(t)
	s, sl := memberSource(t, srv, nil)
	first, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	lists := func() []string {
		var out []string
		for _, q := range srv.Queries("/api/users.conversations") {
			out = append(out, q.Get("types"))
		}
		return out
	}
	if l := lists(); strings.Join(l, " ") != "public_channel,private_channel mpim" {
		t.Fatalf("lists %v", l)
	}
	if q := srv.Queries("/api/users.conversations")[0]; q.Get("exclude_archived") != "true" || q.Get("limit") != "200" {
		t.Fatalf("list %v", q)
	}
	read := map[string]bool{}
	for _, q := range srv.Queries("/api/conversations.history") {
		read[q.Get("channel")] = true
	}
	if !read["C0ENG"] || !read["C0SECRET"] || !read["C0QUIET"] || read["C0OLDARC"] || read[slacktest.Review] {
		t.Fatalf("read %v", read)
	}
	if len(srv.Queries("/api/conversations.info")) != 0 {
		t.Fatal("conversations.info for a channel users.conversations described")
	}
	var c cursorData
	json.Unmarshal(first.Next, &c)
	if c.Channels["C0ENG"].Kind != kindChannel || c.Channels["C0SECRET"].Kind != kindPrivate || c.Channels["C0QUIET"].Kind != kindPrivate {
		t.Fatalf("kinds %s", first.Next)
	}

	// Within the hour the list is not read again.
	_ = sl.sleep(context.Background(), 20*time.Minute)
	second, err := s.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(lists(), " "), "public_channel"); n != 1 {
		t.Fatalf("listed %d times in 20 minutes", n)
	}
	// After it, it is; the person left C0QUIET, whose watermark goes.
	srv.Routes["/api/users.conversations"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("types") == "mpim" {
			io.WriteString(w, `{"ok":true,"channels":[]}`)
			return
		}
		io.WriteString(w, strings.Replace(memberList, `{"id":"C0QUIET","name":"quiet","is_channel":true},`, "", 1))
	}
	_ = sl.sleep(context.Background(), time.Hour)
	third, err := s.Collect(context.Background(), second.Next)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(lists(), " "), "public_channel"); n != 2 {
		t.Fatalf("listed %d times after an hour", n)
	}
	c = cursorData{}
	json.Unmarshal(third.Next, &c)
	if c.Channels["C0QUIET"] != nil || c.Channels["C0ENG"] == nil {
		t.Fatalf("after leaving %s", third.Next)
	}
	// History is only for channels the person is in.
	if err := s.api.allow(srv.URL + "/api/conversations.history?channel=C0QUIET"); !errors.Is(err, source.ErrScope) {
		t.Fatal("history of a channel the person left")
	}
	if err := s.api.allow(srv.URL + "/api/conversations.history?channel=C0ENG"); err != nil {
		t.Fatal(err)
	}

	// Without member: not even the list.
	plain, _ := newSource(t, slacktest.New(t))
	if err := plain.api.allow(srv.URL + "/api/users.conversations?types=public_channel%2Cprivate_channel&exclude_archived=true&limit=200"); !errors.Is(err, source.ErrScope) {
		t.Fatal("the channel list without member")
	}
	for _, q := range []string{"public_channel", "im", "public_channel,private_channel,im"} {
		if err := s.api.allow(srv.URL + "/api/users.conversations?types=" + q); !errors.Is(err, source.ErrScope) {
			t.Fatalf("types=%s allowed", q)
		}
	}
	// A rate limit on the list ends the pass, and the cursor stays.
	srv.Routes["/api/users.conversations"] = slacktest.Text(`{"ok":false,"error":"ratelimited"}`)
	_ = sl.sleep(context.Background(), 2*time.Hour)
	if got, err := s.Collect(context.Background(), third.Next); !errors.Is(err, source.ErrRateLimited) || got.Next != nil {
		t.Fatalf("%v", err)
	}
}

// Named channels and member channels together: a named channel keeps @here as a mention, and is read once.
func TestMemberWithNamedChannels(t *testing.T) {
	srv := memberServer(t)
	s, _ := memberSource(t, srv, func(o *Options) { o.Channels = []string{"C0ENG"} })
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items)
	if it := m["slack:C0ENG:1794136000.000100"]; it.Type != TypeMentioned || !it.Addressed {
		t.Fatalf("@here in a named channel: %+v", it)
	}
	n := 0
	for _, q := range srv.Queries("/api/conversations.history") {
		if q.Get("channel") == "C0ENG" {
			n++
		}
	}
	if n != 1 || len(srv.Queries("/api/conversations.info")) != 1 {
		t.Fatalf("C0ENG read %d times, info %d", n, len(srv.Queries("/api/conversations.info")))
	}
}

// A later pass finds new replies in an older thread from the roots' latest_reply: it reads only those threads, from
// their mark, and items only the new replies. A root with no new reply is not read.
func TestSweepFindsNewReplies(t *testing.T) {
	srv := memberServer(t)
	s, sl := memberSource(t, srv, nil)
	first, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var c cursorData
	json.Unmarshal(first.Next, &c)
	if c.Replies["C0ENG/1794135000.000100"] != "1794135600.000100" || c.Channels["C0ENG"].Reply != "1794135600.000100" || c.Channels["C0ENG"].Swept == "" {
		t.Fatalf("marks %s", first.Next)
	}
	// An hour later: one more reply in Carol's thread. Frank's root (no replies) and the private thread are unchanged.
	_ = sl.sleep(context.Background(), time.Hour)
	srv.Routes["/api/conversations.history"] = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("channel") {
		case "C0ENG":
			io.WriteString(w, strings.Replace(historyEng, `"reply_count":2,"latest_reply":"1794135600.000100"`, `"reply_count":3,"latest_reply":"1794140000.000100"`, 1))
		case "C0SECRET":
			io.WriteString(w, historySecret)
		default:
			io.WriteString(w, `{"ok":true,"messages":[]}`)
		}
	}
	srv.Routes["/api/conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("channel") != "C0ENG" || q.Get("ts") != "1794135000.000100" {
			t.Errorf("read %v", q)
		}
		if q.Get("oldest") != "1794135600.000100" {
			t.Errorf("from %v", q)
		}
		io.WriteString(w, `{"ok":true,"messages":[
  {"type":"message","user":"U0CAROL","text":"the export job failed","ts":"1794135000.000100","thread_ts":"1794135000.000100","reply_count":3},
  {"type":"message","user":"U0BOB","text":"failed again","ts":"1794140000.000100","thread_ts":"1794135000.000100"}
]}`)
	}
	before := len(srv.Queries("/api/conversations.replies"))
	second, err := s.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Queries("/api/conversations.replies")) - before; n != 1 {
		t.Fatalf("%d thread reads", n)
	}
	if len(second.Items) != 1 || second.Items[0].Key != "slack:C0ENG:1794140000.000100" || second.Items[0].Subject != "slack:C0ENG:1794135000.000100" || second.Items[0].Type != TypeChannel {
		t.Fatalf("items %+v", second.Items)
	}
	c = cursorData{}
	json.Unmarshal(second.Next, &c)
	if c.Replies["C0ENG/1794135000.000100"] != "1794140000.000100" {
		t.Fatalf("mark %s", second.Next)
	}

	// A thread the person follows is read as a followed thread, not swept twice.
	p := &pass{cur: cursorData{}, sweep: map[string]sweepThread{}, floorTS: "1794000000.000000"}
	p.cur.init()
	p.cur.Threads["C0ENG/1794135000.000100"] = &thread{Joined: "1794135000.000100", Last: "1794135000.000100"}
	root := &message{channel: "C0ENG", ts: "1794135000.000100", replyCount: 3, latestReply: "1794140000.000100"}
	s.noteReplies(p, "C0ENG", &watermark{}, root)
	if len(p.sweep) != 0 {
		t.Fatal("a followed thread was swept")
	}
	delete(p.cur.Threads, "C0ENG/1794135000.000100")
	s.noteReplies(p, "C0ENG", &watermark{Swept: "1794138000.000000", Kind: kindChannel}, root)
	if p.sweep["C0ENG/1794135000.000100"].oldest != "1794138000.000000" {
		t.Fatalf("a root seen with replies for the first time reads from the last sweep: %+v", p.sweep)
	}
}

// Threads with new replies wait beyond sweepPerPass a pass, oldest mark first, and keep their marks, so a later pass
// reads them from there.
func TestSweepLimit(t *testing.T) {
	srv := memberServer(t)
	var roots []string
	for i := 0; i < sweepPerPass+5; i++ {
		roots = append(roots, fmt.Sprintf(`{"type":"message","user":"U0CAROL","text":"q","ts":"17941%05d.000000","thread_ts":"17941%05d.000000","reply_count":1,"latest_reply":"1794139000.000000"}`, 30000+i, 30000+i))
	}
	srv.Routes["/api/conversations.history"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") != "C0ENG" {
			io.WriteString(w, `{"ok":true,"messages":[]}`)
			return
		}
		io.WriteString(w, `{"ok":true,"messages":[`+strings.Join(roots, ",")+`]}`)
	}
	s, _ := memberSource(t, srv, nil)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Queries("/api/conversations.replies")); n > sweepPerPass+checksPerPass {
		t.Fatalf("%d thread reads", n)
	}
	var c cursorData
	json.Unmarshal(got.Next, &c)
	waiting := 0
	for k, mark := range c.Replies {
		if _, root, _ := strings.Cut(k, "/"); mark == root {
			waiting++
		}
	}
	if len(c.Replies) != sweepPerPass+5 || waiting != 5 {
		t.Fatalf("%d marks, %d waiting", len(c.Replies), waiting)
	}
}

// timedTransport records when each request was made, on the test clock.
type timedTransport struct {
	base http.RoundTripper
	now  func() time.Time
	mu   sync.Mutex
	at   map[string][]time.Time
}

func (t *timedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.at[strings.TrimPrefix(r.URL.Path, "/api/")] = append(t.at[strings.TrimPrefix(r.URL.Path, "/api/")], t.now())
	t.mu.Unlock()
	return t.base.RoundTrip(r)
}

// With many member channels a pass reads at most channelsPerPass of them, longest unread first, and every method
// stays under its Slack tier in any minute: Tier 2 20, Tier 3 50, Tier 4 100.
func TestMemberPacing(t *testing.T) {
	srv := memberServer(t)
	var list []string
	for i := 0; i < channelsPerPass+20; i++ {
		list = append(list, fmt.Sprintf(`{"id":"C0M%03d","is_private":false}`, i))
	}
	srv.Routes["/api/users.conversations"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("types") == "mpim" {
			io.WriteString(w, `{"ok":true,"channels":[]}`)
			return
		}
		io.WriteString(w, `{"ok":true,"channels":[`+strings.Join(list, ",")+`]}`)
	}
	// Each channel has a busy thread, so every pass sweeps as many threads as it may.
	srv.Routes["/api/conversations.history"] = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":true,"messages":[{"type":"message","user":"U0CAROL","text":"q","ts":"1794130000.000000","thread_ts":"1794130000.000000","reply_count":1,"latest_reply":"%s"}]}`, formatTS(testNow.Add(time.Hour)))
	}
	sl := &sleeps{}
	tr := &timedTransport{base: srv.Client().Transport, now: sl.now, at: map[string][]time.Time{}}
	o := opts(srv, sl)
	o.Channels, o.Member, o.HTTP = nil, true, &http.Client{Transport: tr}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tr.at[methodHistory]); n != channelsPerPass {
		t.Fatalf("%d channels read", n)
	}
	// The next pass, a minute later, reads the ones that waited first.
	_ = sl.sleep(context.Background(), time.Minute)
	if _, err := s.Collect(context.Background(), got.Next); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, q := range srv.Queries("/api/conversations.history") {
		seen[q.Get("channel")] = true
	}
	if len(seen) != channelsPerPass+20 {
		t.Fatalf("%d channels read in two passes", len(seen))
	}
	limits := map[string]int{methodSearch: 20, methodUserGroups: 20, methodHistory: 50, methodReplies: 50, methodInfo: 50,
		methodGroupDMs: 50, methodUser: 100, methodMembers: 100}
	for method, at := range tr.at {
		limit, ok := limits[method]
		if !ok {
			continue
		}
		for i := range at {
			n := 0
			for j := i; j < len(at) && at[j].Sub(at[i]) < time.Minute; j++ {
				n++
			}
			if n > limit {
				t.Fatalf("%s: %d calls in a minute", method, n)
			}
		}
		for i := 1; i < len(at); i++ {
			if at[i].Sub(at[i-1]) < spacing[method] {
				t.Fatalf("%s: calls %s apart", method, at[i].Sub(at[i-1]))
			}
		}
	}
}

// The cursor's sweep marks are checked like the rest of it.
func TestSweepCursor(t *testing.T) {
	for _, bad := range []string{
		`{"since":"2026-11-08T11:00:00Z","replies":{"C0ENG":"1794135600.000100"}}`,
		`{"since":"2026-11-08T11:00:00Z","replies":{"C0ENG/1794135000.000100":"soon"}}`,
		`{"since":"2026-11-08T11:00:00Z","channels":{"C0ENG":{"seen":"1794135600.000100","swept":"x"}}}`,
	} {
		if _, err := parseCursor([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// A channel's day is titled for the channel and the day; a thread with replies for the channel. Names leave only as
// the listed fields, never message text.
func TestChannelTitles(t *testing.T) {
	srv := memberServer(t)
	s, _ := memberSource(t, srv, nil)
	s.SetNames(true)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items)
	for key, want := range map[string][3]string{
		"slack:C0ENG:1794137000.000100":    {"#eng · Sun 8 Nov", "eng", "Frank Ocean"},
		"slack:C0ENG:1794135300.000100":    {"#eng thread", "eng", "Dana Scully"},
		"slack:C0SECRET:1794136500.000100": {"#secret · Sun 8 Nov", "secret", "Hana Reyes"},
	} {
		f := m[key].Fields
		if f[FieldTitle] != want[0] || f[FieldChannelName] != want[1] || f[FieldAuthorName] != want[2] {
			t.Errorf("%s: %q %q %q", key, f[FieldTitle], f[FieldChannelName], f[FieldAuthorName])
		}
	}
	for _, it := range got.Items {
		for _, word := range []string{"export", "slow", "budget", "hiring"} {
			if strings.Contains(it.Fields[FieldTitle], word) || strings.Contains(it.EventTitle, word) {
				t.Fatalf("%s: a title holds message text", it.Key)
			}
		}
	}
}

// A reply in a followed thread before the person joined is a channel message only when its root is in the window a
// channel's reads look back over, as a swept thread's would be; an older thread's earlier replies are not items.
func TestFollowedBackfillStaysInTheWindow(t *testing.T) {
	s, _ := memberSource(t, memberServer(t), nil)
	s.members = map[string]string{"C0ENG": kindChannel}
	reply := func(root time.Time) *message {
		return &message{channel: "C0ENG", ts: formatTS(testNow.Add(-time.Hour)), threadTS: formatTS(root), user: "U0BOB",
			kind: kindChannel, followed: true, joined: formatTS(testNow)}
	}
	if typ := s.typeOf(reply(testNow.Add(-24 * time.Hour))); typ != TypeChannel {
		t.Fatalf("a reply in a day-old thread: %q", typ)
	}
	if typ := s.typeOf(reply(testNow.Add(-5 * 24 * time.Hour))); typ != "" {
		t.Fatalf("a reply in a 5-day-old thread before the person joined: %q", typ)
	}
	// After joining it is a thread reply, whatever the root's age.
	late := reply(testNow.Add(-5 * 24 * time.Hour))
	late.joined = formatTS(testNow.Add(-2 * time.Hour))
	if typ := s.typeOf(late); typ != TypeThreadReply {
		t.Fatalf("%q", typ)
	}
}
