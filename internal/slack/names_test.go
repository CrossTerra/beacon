package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/slack/slacktest"
	"github.com/CrossTerra/beacon/internal/source"
)

// messageWords are words from the fixtures' message text. None may appear in a title or a name.
var messageWords = []string{"deploy", "budget", "offsite", "alerts", "PTAL", "Merged", "salary", "Lunch", "pager", "freeze",
	"mockups", "hello", "rotation", "handle", "take this", "On it", "sure", "acme/app"}

func namedSource(t *testing.T, srv *slacktest.Server, change func(*Options)) (*Source, *sleeps) {
	t.Helper()
	sl := &sleeps{}
	o := opts(srv, sl)
	if change != nil {
		change(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	s.SetNames(true)
	return s, sl
}

func TestTitles(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := namedSource(t, srv, nil)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Warning != "" {
		t.Fatalf("warning %q with every scope", got.Warning)
	}
	m := byKey(t, got.Items)
	type want struct{ title, channel, author string }
	wants := map[string]want{
		// A direct message, and the person's own message there.
		"slack:D0DANA:1794132000.000100": {"DM with Dana Scully", "Dana Scully", "Dana Scully"},
		"slack:D0DANA:1794132600.000200": {"DM with Dana Scully", "Dana Scully", "Dana Scully"},
		"slack:D0DANA:1794133200.000300": {"You to Dana Scully", "Dana Scully", ""},
		// A group direct message names the others, not the person.
		"slack:G0GROUP:1794136800.001200": {"Group DM: Carol Danvers, Dana Scully", "Carol Danvers, Dana Scully", "Dana Scully"},
		"slack:G0GROUP:1794137500.001400": {"Group DM: Carol Danvers, Dana Scully", "Carol Danvers, Dana Scully", "Carol Danvers"},
		"slack:G0GROUP:1794137600.001500": {"You to Carol Danvers, Dana Scully", "Carol Danvers, Dana Scully", ""},
		// A channel mention, and a reply in its thread: one subject, one title.
		"slack:C0ENG:1794133800.000400": {"#eng: Bob Ross mentioned you", "eng", "Bob Ross"},
		"slack:C0ENG:1794134500.000750": {"#eng: Bob Ross mentioned you", "eng", "Carol Danvers"},
		// Replies in a thread someone else started.
		"slack:C0ENG:1794130000.000600": {"#eng thread", "eng", ""},
		"slack:C0ENG:1794134400.000700": {"#eng thread", "eng", "Bob Ross"},
		// The person's own top-level message in a channel.
		"slack:C0REVIEW:1794136200.001100": {"You in #review", "review", ""},
		"slack:C0REVIEW:1794135000.000900": {"#review: Erin Burnett asked for a review", "review", "Erin Burnett"},
		"slack:C0REVIEW:1794135800.001060": {"#review: Bob Ross mentioned you", "review", "Bob Ross"},
		"slack:C0REVIEW:1794135700.001050": {"#review: Frank Ocean mentioned you", "review", "Frank Ocean"},
		"slack:C0SECRET:1794134000.000450": {"#secret: Hana Reyes mentioned you", "secret", "Hana Reyes"},
		// An app's direct message: its other party is no user Slack describes, and an app has no author name.
		"slack:D0BOT:1794131000.000050": {"", "", ""},
	}
	for k, w := range wants {
		it, ok := m[k]
		if !ok {
			t.Fatalf("missing %s", k)
		}
		if it.Fields[FieldTitle] != w.title || it.Fields[FieldChannelName] != w.channel || it.Fields[FieldAuthorName] != w.author {
			t.Errorf("%s: title %q, channel %q, author %q", k, it.Fields[FieldTitle], it.Fields[FieldChannelName], it.Fields[FieldAuthorName])
		}
	}
	bySubject := map[string]string{}
	for _, it := range got.Items {
		for _, f := range []string{FieldTitle, FieldChannelName, FieldAuthorName} {
			for _, w := range messageWords {
				if strings.Contains(it.Fields[f], w) {
					t.Errorf("%s: %s holds message text %q", it.Key, f, w)
				}
			}
		}
		// The ids rules match on are still there.
		if it.Type != TypeDeleted && (it.Fields["author"] == "" || it.Fields["channel"] == "") {
			t.Errorf("%s: lost an id field", it.Key)
		}
		if prev, ok := bySubject[it.Subject]; ok && prev != it.Fields[FieldTitle] {
			t.Errorf("%s: two titles on one subject: %q and %q", it.Subject, prev, it.Fields[FieldTitle])
		}
		bySubject[it.Subject] = it.Fields[FieldTitle]
		if it.EventTitle == "" || strings.Contains(it.EventTitle, "#") {
			t.Errorf("%s: event title %q", it.Key, it.EventTitle)
		}
	}
	// Each name is read once a pass, and group direct message members once.
	if n := len(srv.Queries("/api/users.info")); n != 7 {
		t.Errorf("users.info called %d times", n)
	}
	if n := len(srv.Queries("/api/conversations.members")); n != 1 {
		t.Errorf("conversations.members called %d times", n)
	}
}

func TestNamesWithoutScope(t *testing.T) {
	srv := slacktest.New(t)
	srv.Scopes = slacktest.PassScopes
	s, _ := namedSource(t, srv, nil)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatalf("a token without the name scopes failed the pass: %v", err)
	}
	if got.Warning != source.WarnNamesScope || got.Next == nil || len(got.Items) != 16 {
		t.Fatalf("warning %q, next %v, %d items", got.Warning, got.Next != nil, len(got.Items))
	}
	for _, it := range got.Items {
		for _, f := range []string{FieldTitle, FieldChannelName, FieldAuthorName} {
			if _, ok := it.Fields[f]; ok {
				t.Errorf("%s: %s set without the scopes", it.Key, f)
			}
		}
		if it.EventTitle == "" {
			t.Errorf("%s: no event title", it.Key)
		}
	}
	if n := len(srv.Queries("/api/users.info")) + len(srv.Queries("/api/conversations.members")); n != 0 {
		t.Errorf("%d name lookups without the scopes", n)
	}
	// conversations.info only for the configured channel, as before.
	for _, q := range srv.Queries("/api/conversations.info") {
		if q.Get("channel") != slacktest.Review {
			t.Errorf("conversations.info on %s", q.Get("channel"))
		}
	}
	if title, err := s.ThreadTitle(context.Background(), "slack:C0ENG:1794133800.000400"); title != "" || err != nil {
		t.Fatalf("ThreadTitle without the scopes: %q %v", title, err)
	}
}

func TestNamesOff(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	got, err := s.Collect(context.Background(), nil)
	if err != nil || got.Warning != "" {
		t.Fatalf("%v %q", err, got.Warning)
	}
	for _, it := range got.Items {
		if _, ok := it.Fields[FieldTitle]; ok {
			t.Fatalf("%s: a title though no rule lists a name", it.Key)
		}
	}
	if n := len(srv.Queries("/api/users.info")); n != 0 {
		t.Fatalf("%d users.info calls though no rule lists a name", n)
	}
}

func TestNameLookupFails(t *testing.T) {
	srv := slacktest.New(t)
	srv.Routes["/api/users.info"] = slacktest.Text(`{"ok":false,"error":"ratelimited"}`)
	s, _ := namedSource(t, srv, nil)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatalf("a failed name lookup failed the pass: %v", err)
	}
	if got.Warning != source.WarnNamesFailed || len(got.Items) != 16 {
		t.Fatalf("warning %q, %d items", got.Warning, len(got.Items))
	}
	if n := len(srv.Queries("/api/users.info")); n != 1 {
		t.Fatalf("users.info called %d times after it failed", n)
	}
	// No fallback title: "#eng thread" for a mention would open the thread with a title no retitle replaces.
	// A title that needs no name ("#eng thread" for a thread Carol started) is complete and stands.
	for _, it := range got.Items {
		title, ok := it.Fields[FieldTitle]
		switch kind := it.Fields["channel_type"]; {
		case ok && (kind == kindIM || kind == kindMPIM):
			t.Errorf("%s: title %q after a failed lookup", it.Key, title)
		case ok && it.Subject == "slack:C0ENG:1794133800.000400":
			t.Errorf("%s: fallback title %q for a mention's thread", it.Key, title)
		case !ok && it.Subject == "slack:C0ENG:1794120000.000500":
			t.Errorf("%s: a title that needs no name was dropped", it.Key)
		}
		if n := it.Fields[FieldChannelName]; it.Fields["channel_type"] == kindMPIM && n != "" {
			t.Errorf("%s: a partial group name %q", it.Key, n)
		}
	}
	// A lookup that fails part way: titles built before it stand, none after it.
	srv1 := slacktest.New(t)
	calls := 0
	srv1.Routes["/api/users.info"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 2 {
			io.WriteString(w, `{"ok":false,"error":"ratelimited"}`)
			return
		}
		io.WriteString(w, `{"ok":true,"user":{"real_name":"Somebody Known"}}`)
	}
	s1, _ := namedSource(t, srv1, nil)
	got1, err := s1.Collect(context.Background(), nil)
	if err != nil || got1.Warning != source.WarnNamesFailed {
		t.Fatalf("%v %q", err, got1.Warning)
	}
	for _, it := range got1.Items {
		title := it.Fields[FieldTitle]
		if strings.HasSuffix(title, " thread") && it.Type == TypeMentioned {
			t.Errorf("%s: fallback title %q for a mention", it.Key, title)
		}
	}
	// A missing_scope from a token whose scopes auth.test did not list is the scope warning.
	srv2 := slacktest.New(t)
	srv2.Scopes = ""
	srv2.Routes["/api/users.info"] = slacktest.Text(`{"ok":false,"error":"missing_scope"}`)
	s2, _ := namedSource(t, srv2, nil)
	if got, err := s2.Collect(context.Background(), nil); err != nil || got.Warning != source.WarnNamesScope {
		t.Fatalf("%v %q", err, got.Warning)
	}
}

func TestNameCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CacheFile("slack"))
	srv := slacktest.New(t)
	s, sl := namedSource(t, srv, func(o *Options) { o.Cache = path })
	// A dry run does not write it.
	if _, err := s.Collect(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a pass that may not write state wrote the cache: %v", err)
	}
	s.SetPersist(true)
	if _, err := s.Collect(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode %v", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "Dana Scully") || strings.Contains(string(data), slacktest.Token) {
		t.Fatalf("cache: %s", data)
	}
	for _, w := range messageWords {
		if strings.Contains(string(data), w) {
			t.Fatalf("the cache holds message text %q", w)
		}
	}
	// A new process reads the cache and asks Slack for no name it holds.
	srv2 := slacktest.New(t)
	s2, sl2 := namedSource(t, srv2, func(o *Options) { o.Cache = path })
	sl2.elapsed = sl.elapsed + 30*time.Minute
	got, err := s2.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(srv2.Queries("/api/users.info")) + len(srv2.Queries("/api/conversations.members")); n != 0 {
		t.Fatalf("%d lookups with a fresh cache", n)
	}
	if m := byKey(t, got.Items); m["slack:D0DANA:1794132000.000100"].Fields[FieldTitle] != "DM with Dana Scully" {
		t.Fatal("a cached name was not used")
	}
	// Past its TTL, a name is read again.
	srv3 := slacktest.New(t)
	s3, sl3 := namedSource(t, srv3, func(o *Options) { o.Cache = path })
	sl3.elapsed = sl.elapsed + 25*time.Hour
	if _, err := s3.Collect(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(srv3.Queries("/api/users.info")); n == 0 {
		t.Fatal("an expired cache was used")
	}
	// A cache with something Beacon would not have written is not trusted.
	if err := os.WriteFile(path, []byte(`{"users":{"U0DANA":{"name":"Dana\u202eScully","at":"2026-11-08T12:00:00Z"},"x/y":{"name":"a","at":"2026-11-08T12:00:00Z"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := loadCache(path, testNow); len(c.Users) != 0 {
		t.Fatalf("loaded %+v", c.Users)
	}
}

func TestThreadTitle(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := namedSource(t, srv, nil)
	ctx := context.Background()
	if title, err := s.ThreadTitle(ctx, "slack:C0ENG:1794133800.000400"); title != "" || err != nil {
		t.Fatalf("before a pass: %q %v", title, err)
	}
	if _, err := s.Collect(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// The root each thread's inclusive read returns.
	roots := map[string]string{
		"C0ENG/1794133800.000400":    `{"user":"U0BOB","text":"<@U0WINTON> can you look at the alerts?","ts":"1794133800.000400"}`,
		"C0ENG/1794120000.000500":    `{"user":"U0CAROL","text":"Who can take the alerts rotation?","ts":"1794120000.000500"}`,
		"C0REVIEW/1794136200.001100": `{"user":"U0WINTON","text":"Merged the fix","ts":"1794136200.001100"}`,
		"G0GROUP/1794137500.001400":  `{"user":"U0CAROL","text":"Lunch at noon?","ts":"1794137500.001400"}`,
		"D0DANA/1794133200.000300":   `{"user":"U0DANA","text":"x","ts":"1794133200.000300"}`,
	}
	srv.Routes["/api/conversations.replies"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if root, ok := roots[q.Get("channel")+"/"+q.Get("ts")]; ok && q.Get("inclusive") == "true" && q.Get("limit") == "1" {
			io.WriteString(w, `{"ok":true,"messages":[`+root+`]}`)
			return
		}
		io.WriteString(w, `{"ok":false,"error":"thread_not_found"}`)
	}
	for subject, want := range map[string]string{
		"slack:C0ENG:1794133800.000400":             "#eng: Bob Ross mentioned you",
		"slack:C0ENG:1794120000.000500":             "#eng thread",
		"slack:C0REVIEW:1794136200.001100":          "You in #review",
		"slack:dm:winton:G0GROUP:1794137500.001400": "Group DM: Carol Danvers, Dana Scully",
	} {
		title, err := s.ThreadTitle(ctx, subject)
		if err != nil || title != want {
			t.Errorf("%s: %q %v, want %q", subject, title, err, want)
		}
	}
	if title, err := s.ThreadTitle(ctx, "slack:dm:winton:D0DANA:1794133200.000300"); err != nil || title != "DM with Dana Scully" {
		t.Errorf("direct message: %q %v", title, err)
	}
	for _, subject := range []string{"slack:C0GONE:1794133800.000400", "slack:dm:rahul:D0DANA:1794133200.000300", "gmail:winton:thread:1", "slack:D0DANA:1794133200.000300"} {
		if _, err := s.ThreadTitle(ctx, subject); !errors.Is(err, source.ErrNoTitle) {
			t.Errorf("%s: %v", subject, err)
		}
	}
}

func TestNameMethodsAllowed(t *testing.T) {
	srv := slacktest.New(t)
	s, _ := newSource(t, srv)
	a := s.api
	a.named = func(c string) bool { return c == "C0ENG" }
	for raw, ok := range map[string]bool{
		srv.URL + "/api/users.info?user=U0DANA":                       true,
		srv.URL + "/api/users.info?user=B0JIRA":                       false,
		srv.URL + "/api/users.info?user=U0DANA&include_locale=true":   false,
		srv.URL + "/api/conversations.info?channel=C0ENG":             true,
		srv.URL + "/api/conversations.info?channel=C0OTHER":           false,
		srv.URL + "/api/conversations.members?channel=C0ENG&limit=50": true,
		srv.URL + "/api/conversations.members?channel=C0OTHER":        false,
		srv.URL + "/api/users.list":                                   false,
	} {
		if err := a.allow(raw); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"  Dana   Scully ":      "Dana Scully",
		"Dana\nScully\t":        "Dana Scully",
		"Dana\u202eScully":      "DanaScully",
		"Zero\u200bWidth":       "ZeroWidth",
		strings.Repeat("a", 80): strings.Repeat("a", nameMax),
	} {
		if got := cleanName(in, nameMax); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
