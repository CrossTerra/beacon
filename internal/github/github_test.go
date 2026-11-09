package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/source"
)

const token = "ghp_TESTTOKEN"

func TestCollectEveryItem(t *testing.T) {
	var paths, queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "other/secret") || strings.Contains(r.URL.Path, "/contents/") {
			t.Errorf("request outside scope: %s", r.URL.RequestURI())
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/users/acme":
			io.WriteString(w, `{"login":"acme","type":"Organization"}`)
		case r.URL.Path == "/search/issues":
			queries = append(queries, r.URL.Query().Get("q"))
			io.WriteString(w, searchBody)
		case strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, pr7)
		case strings.HasSuffix(r.URL.Path, "/issues/7/timeline"):
			io.WriteString(w, timeline7)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			io.WriteString(w, `[{"id":12,"user":{"login":"dana"},"state":"APPROVED","submitted_at":"2026-11-02T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/pulls/8"):
			io.WriteString(w, pr8)
		case strings.HasSuffix(r.URL.Path, "/issues/8/timeline"):
			io.WriteString(w, timeline8)
		case strings.HasSuffix(r.URL.Path, "/pulls/8/reviews"):
			io.WriteString(w, `[{"id":23,"user":{"login":"winton"},"state":"CHANGES_REQUESTED","submitted_at":"2026-11-03T01:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/pulls/8/comments"):
			io.WriteString(w, `[{"id":77,"user":{"login":"dana"},"body":"nit @winton","created_at":"2026-11-04T02:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/pulls/9"):
			io.WriteString(w, pr9)
		case strings.HasSuffix(r.URL.Path, "/issues/9/timeline"):
			io.WriteString(w, `[{"id":1,"event":"commented","user":{"login":"dana"},"body":"old","created_at":"2026-10-02T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/pulls/9/reviews"):
			io.WriteString(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/commits/def456/check-suites"):
			io.WriteString(w, `{"check_suites":[{"status":"completed","conclusion":"failure","updated_at":"2026-11-07T00:00:00Z","head_sha":"def456"}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/3"):
			io.WriteString(w, pr3)
		case strings.HasSuffix(r.URL.Path, "/issues/3/timeline"):
			io.WriteString(w, `[{"id":31,"event":"commented","user":{"login":"bob"},"body":"hey @winton","created_at":"2026-11-02T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/pulls/3/reviews"), strings.HasSuffix(r.URL.Path, "/pulls/3/comments"), strings.HasSuffix(r.URL.Path, "/pulls/7/comments"), strings.HasSuffix(r.URL.Path, "/pulls/9/comments"):
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := newTest(t, srv, []string{"acme/app", "acme/*"}, []string{"ENG"})
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	got, err := s.Collect(context.Background(), []byte("2026-11-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Next) != "2026-11-08T12:00:00Z" {
		t.Fatalf("cursor %s", got.Next)
	}
	joined := strings.Join(queries, "\n")
	if len(queries) != 2 || !strings.Contains(joined, "repo:acme/app") || !strings.Contains(joined, "org:acme") || strings.Contains(joined, "other/secret") || !strings.Contains(joined, "updated:>=2026-11-01") {
		t.Fatalf("query %q", queries)
	}
	byKey := map[string]source.Item{}
	for _, it := range got.Items {
		if _, ok := byKey[it.Key]; ok {
			t.Fatalf("duplicate key %s", it.Key)
		}
		byKey[it.Key] = it
		if it.Source != "github" || !strings.HasPrefix(it.Subject, "github:pr:") || !strings.HasPrefix(it.Key, "github:") {
			t.Fatalf("shape %+v", it)
		}
		if err := source.Validate(it, Fields, StateFields); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{
		"github:acme/app#7:12":                 TypeReviewed,
		"github:acme/app#7:13":                 TypeCommented,
		"github:acme/app#7:14":                 TypeCommented,
		"github:acme/app#7:aaa111":             TypePushed,
		"github:acme/app#7:15":                 TypeMerged,
		"github:acme/app#8:800":                TypeOpened,
		"github:acme/app#8:21":                 TypeReviewRequested,
		"github:acme/app#8:22":                 TypeReviewRequestRemoved,
		"github:acme/app#8:23":                 TypeReviewed,
		"github:acme/app#8:24":                 TypeReady,
		"github:acme/app#8:26":                 TypeCommented,
		"github:acme/app#8:ccc999":             TypePushed,
		"github:acme/app#8:25":                 TypeClosed,
		"github:acme/app#8:77":                 TypeCommented,
		"github:acme/app#9:checks:def456:fail": TypeChecksChanged,
		"github:acme/lib#3:31":                 TypeCommented,
	}
	if len(byKey) != len(want) {
		var keys []string
		for k, it := range byKey {
			keys = append(keys, k+" "+it.Type)
		}
		t.Fatalf("items %d, want %d: %v", len(byKey), len(want), keys)
	}
	for k, typ := range want {
		it, ok := byKey[k]
		if !ok || it.Type != typ {
			t.Fatalf("%s: %+v", k, it)
		}
	}
	if _, ok := byKey["github:acme/app#7:16"]; ok {
		t.Fatal("a merge also yielded closed")
	}
	if _, ok := byKey["github:acme/app#7:10"]; ok {
		t.Fatal("event before the cursor was emitted")
	}
	c13 := byKey["github:acme/app#7:13"]
	if !c13.Addressed || c13.Own || c13.Fields["body"] != "see @winton" || c13.Actor != "github:user:dana" {
		t.Fatalf("mention %+v", c13)
	}
	if byKey["github:acme/app#8:26"].Addressed {
		t.Fatal("@winton2 was treated as a mention of winton")
	}
	if !byKey["github:acme/app#8:21"].Addressed || byKey["github:acme/app#8:21"].Actor != "github:user:dana" {
		t.Fatal("review request")
	}
	rev := byKey["github:acme/app#8:23"]
	if !rev.Own || rev.Fields["review_state"] != "changes_requested" {
		t.Fatalf("own review %+v", rev)
	}
	if !byKey["github:acme/app#7:15"].Own || byKey["github:acme/app#7:15"].Type != TypeMerged {
		t.Fatal("own merge")
	}
	if !byKey["github:acme/app#8:25"].Own == false || byKey["github:acme/app#8:25"].Type != TypeClosed {
		t.Fatal("close without merge")
	}
	refs := byKey["github:acme/app#7:15"].References
	if len(refs) != 1 || refs[0] != "jira:ENG-12" {
		t.Fatalf("references %v", refs)
	}
	checks := byKey["github:acme/app#9:checks:def456:fail"]
	if checks.State["checks"] != "fail" || checks.State["draft"] != "true" || checks.State["review_decision"] != "review_required" || checks.State["head_sha"] != "def456" || checks.Version != "checks:def456:fail" {
		t.Fatalf("state %+v", checks)
	}
	if checks.State["mergeable"] != "true" || checks.Fields["repo"] != "acme/app" || checks.Fields["number"] != "9" {
		t.Fatalf("fields %+v", checks)
	}
	again, err := s.Collect(context.Background(), []byte("2026-11-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Items) != len(got.Items) {
		t.Fatal("keys changed across a pass")
	}
	for _, it := range again.Items {
		if byKey[it.Key].Type == "" {
			t.Fatalf("new key %s", it.Key)
		}
	}
	later, err := s.Collect(context.Background(), got.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(later.Items) != 0 {
		t.Fatalf("cursor did not filter: %d", len(later.Items))
	}
	assertStarterRules(t, got.Items)
}

func TestFirstRunIsOpenState(t *testing.T) {
	var query string
	var comments bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			comments = true
		}
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/search/issues":
			query = r.URL.Query().Get("q")
			io.WriteString(w, `{"items":[{"number":9,"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}},{"number":8,"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/9"):
			io.WriteString(w, pr9)
		case strings.HasSuffix(r.URL.Path, "/issues/9/timeline"):
			io.WriteString(w, `[{"id":1,"event":"commented","user":{"login":"dana"},"body":"old history","created_at":"2026-10-02T00:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/commits/def456/check-suites"):
			io.WriteString(w, `{"check_suites":[{"status":"completed","conclusion":"success","updated_at":"2026-11-07T00:00:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/8"):
			io.WriteString(w, strings.Replace(strings.Replace(pr8, `"state": "closed"`, `"state": "open"`, 1), `"requested_reviewers": []`, `"requested_reviewers": [{"login": "winton"}]`, 1))
		case strings.HasSuffix(r.URL.Path, "/issues/8/timeline"):
			io.WriteString(w, `[{"id":21,"event":"review_requested","created_at":"2026-11-02T02:00:00Z","review_requester":{"login":"dana"},"requested_reviewer":{"login":"winton"}},{"id":99,"event":"commented","user":{"login":"dana"},"body":"old","created_at":"2026-10-01T00:00:00Z"}]`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/app"}, nil)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "is:open") || strings.Contains(query, "author:") || strings.Contains(query, "updated:") {
		t.Fatalf("first-run query %q", query)
	}
	if comments {
		t.Fatal("first run read comment history")
	}
	types := map[string]bool{}
	for _, it := range got.Items {
		types[it.Key+" "+it.Type] = true
		if it.Type == TypeCommented {
			t.Fatalf("first run emitted history %s", it.Key)
		}
	}
	if !types["github:acme/app#9:900 "+TypeOpened] || !types["github:acme/app#9:checks:def456:pass "+TypeChecksChanged] || !types["github:acme/app#8:21 "+TypeReviewRequested] {
		t.Fatalf("%v", types)
	}
}

func TestPaginationAndRateLimit(t *testing.T) {
	var pages int
	var slept time.Duration
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			io.WriteString(w, `{"login":"winton"}`)
			return
		}
		if r.URL.Path != "/search/issues" {
			http.NotFound(w, r)
			return
		}
		pages++
		if pages == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if pages == 2 {
			w.Header().Set("Link", `<`+srv.URL+`/search/issues?page=2>; rel="next"`)
			io.WriteString(w, `{"items":[{"number":1,"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
			return
		}
		io.WriteString(w, `{"items":[]}`)
	}))
	defer srv.Close()
	// The search is rate-limited once, then returns a page that would need pull fetches. Stop after proving backoff
	// and the next link was followed: the pull fetch 404s, which is a collect error, and the cursor must not be
	// required for this assertion.
	s := newTest(t, srv, []string{"acme/app"}, nil)
	s.http.Sleep = func(_ context.Context, d time.Duration) error {
		slept = d
		return nil
	}
	_, err := s.Collect(context.Background(), []byte("2026-11-01T00:00:00Z"))
	if err == nil {
		t.Fatal("expected the missing pull to fail the pass")
	}
	if slept != time.Second || pages < 3 {
		t.Fatalf("slept %s pages %d err %v", slept, pages, err)
	}
	if errors.Is(err, source.ErrRateLimited) {
		t.Fatal("a one-second wait should have been slept, not ended")
	}
}

func TestLongRateLimitEndsThePass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	slept := false
	s := newTest(t, srv, []string{"acme/app"}, nil)
	s.http.Sleep = func(context.Context, time.Duration) error {
		slept = true
		return nil
	}
	_, err := s.Collect(context.Background(), nil)
	if !errors.Is(err, source.ErrRateLimited) || slept {
		t.Fatalf("err %v slept %v", err, slept)
	}
}

func TestScopeAndEgress(t *testing.T) {
	sc, err := newScope("api.github.com", []string{"acme/app", "acme/*"})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"https://api.github.com/user",
		"https://api.github.com/search/issues?q=1",
		"https://api.github.com/users/acme",
		"https://api.github.com/repos/acme/app/pulls/1",
		"https://api.github.com/repos/acme/lib/issues/3/timeline",
		"https://api.github.com/repos/acme/app/commits/abc/check-suites",
	} {
		if err := sc.allow(raw); err != nil {
			t.Errorf("allowed %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"https://evil.example/user",
		"https://api.github.com/repos/other/secret/pulls/1",
		"https://api.github.com/repos/acme/app/contents/README",
		"https://api.github.com/repos/acme/app/git/trees/x",
		"http://api.github.com/user",
	} {
		if err := sc.allow(raw); err == nil {
			t.Errorf("allowed %s", raw)
		}
	}
	s, err := New(Options{Repos: []string{"acme/app"}})
	if err != nil || s.Hosts()[0] != "api.github.com:443" {
		t.Fatalf("%v %v", s, err)
	}
	ghe, err := New(Options{Host: "ghe.example.com", Repos: []string{"acme/app"}})
	if err != nil || ghe.Hosts()[0] != "ghe.example.com:443" || !strings.Contains(ghe.base, "/api/v3") {
		t.Fatalf("%v %s", ghe.Hosts(), ghe.base)
	}

	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	blocked := newTest(t, srv, []string{"acme/app"}, nil)
	blocked.SetHTTP(limitedClient(egress.Guard([]string{"api.github.com:443"}, srv.Client().Transport)))
	if _, err := blocked.Collect(context.Background(), nil); err == nil || hit {
		t.Fatalf("guard err %v hit %v", err, hit)
	}
}

func TestIdentityMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"login":"winton"}`)
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/app"}, nil)
	s.identity = "github:user:other"
	_, err := s.Collect(context.Background(), nil)
	if !errors.Is(err, source.ErrIdentity) || strings.Contains(err.Error(), "winton") {
		t.Fatal(err)
	}
}

func newTest(t *testing.T, srv *httptest.Server, repos, projects []string) *Source {
	t.Helper()
	s, err := New(Options{
		Repos: repos, Projects: projects, Base: srv.URL,
		Token: func() (keychain.Secret, error) { return keychain.FromString(token) },
		HTTP:  srv.Client(),
		Now:   func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func limitedClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

const searchBody = `{"incomplete_results":false,"items":[
	{"number":7,"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}},
	{"number":8,"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}},
	{"number":9,"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}},
	{"number":3,"repository_url":"https://api.github.com/repos/acme/lib","pull_request":{"url":"x"}},
	{"number":1,"repository_url":"https://api.github.com/repos/other/secret","pull_request":{"url":"x"}}
]}`

const pr7 = `{
  "id": 700, "number": 7, "title": "Add retries ENG-12 OTHER-9",
  "html_url": "https://github.com/acme/app/pull/7", "state": "closed", "merged": true, "draft": false,
  "user": {"login": "winton"}, "created_at": "2026-10-01T00:00:00Z", "mergeable": false,
  "base": {"ref": "main"}, "head": {"ref": "ENG-12-fix", "sha": "abc111"}, "requested_reviewers": []
}`

const timeline7 = `[
  {"id": 10, "event": "reviewed", "user": {"login": "winton"}, "state": "COMMENTED", "body": "early", "submitted_at": "2026-10-15T00:00:00Z"},
  {"id": 12, "event": "reviewed", "user": {"login": "dana"}, "state": "APPROVED", "body": "LGTM", "submitted_at": "2026-11-02T00:00:00Z"},
  {"id": 13, "event": "commented", "user": {"login": "dana"}, "body": "see @winton", "created_at": "2026-11-02T01:00:00Z"},
  {"id": 14, "event": "commented", "user": {"login": "winton"}, "body": "done", "created_at": "2026-11-03T00:00:00Z"},
  {"sha": "aaa111", "event": "committed", "author": {"email": "winton@example.com", "date": "2026-11-04T00:00:00Z"}},
  {"id": 15, "event": "merged", "actor": {"login": "winton"}, "created_at": "2026-11-05T00:00:00Z"},
  {"id": 16, "event": "closed", "actor": {"login": "winton"}, "created_at": "2026-11-05T00:00:01Z"}
]`

const pr8 = `{
  "id": 800, "number": 8, "title": "Fix login", "html_url": "https://github.com/acme/app/pull/8",
  "state": "closed", "merged": false, "draft": false, "user": {"login": "dana"},
  "created_at": "2026-11-02T00:00:00Z", "base": {"ref": "main"}, "head": {"ref": "login", "sha": "bbb222"},
  "requested_reviewers": []
}`

const timeline8 = `[
  {"id": 21, "event": "review_requested", "created_at": "2026-11-02T02:00:00Z", "review_requester": {"login": "dana"}, "requested_reviewer": {"login": "winton"}},
  {"id": 22, "event": "review_request_removed", "created_at": "2026-11-03T00:00:00Z", "actor": {"login": "dana"}, "requested_reviewer": {"login": "winton"}},
  {"id": 23, "event": "reviewed", "user": {"login": "winton"}, "state": "CHANGES_REQUESTED", "body": "fix this", "submitted_at": "2026-11-03T01:00:00Z"},
  {"id": 24, "event": "ready_for_review", "created_at": "2026-11-04T00:00:00Z", "actor": {"login": "dana"}},
  {"id": 26, "event": "commented", "user": {"login": "dana"}, "body": "hello @winton2", "created_at": "2026-11-04T01:00:00Z"},
  {"sha": "ccc999", "event": "committed", "author": {"date": "2026-11-05T00:00:00Z"}},
  {"id": 25, "event": "closed", "created_at": "2026-11-06T00:00:00Z", "actor": {"login": "dana"}}
]`

const pr9 = `{
  "id": 900, "number": 9, "title": "Own open", "html_url": "https://github.com/acme/app/pull/9",
  "state": "open", "merged": false, "draft": true, "user": {"login": "winton"},
  "created_at": "2026-10-01T00:00:00Z", "mergeable": true,
  "base": {"ref": "main"}, "head": {"ref": "checks", "sha": "def456"},
  "requested_reviewers": [{"login": "dana"}]
}`

const pr3 = `{
  "id": 300, "number": 3, "title": "Lib", "html_url": "https://github.com/acme/lib/pull/3",
  "state": "open", "user": {"login": "alice"}, "created_at": "2026-10-01T00:00:00Z",
  "base": {"ref": "main"}, "head": {"ref": "lib", "sha": "fff"}
}`

func assertStarterRules(t *testing.T, items []source.Item) {
	t.Helper()
	data, err := os.ReadFile("../../examples/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f, err := rules.Parse(bytesReplace(data, "YOUR_LOGIN", "winton"))
	if err != nil {
		t.Fatal(err)
	}
	person := project.Person{Handle: "@winton", Identities: map[string]string{"github": "github:user:winton"}}
	for _, it := range items {
		d := f.Decide(it)
		want := starterTier(it)
		if d.Tier != want {
			t.Errorf("%s %s: tier %s, want %s (rule %v)", it.Key, it.Type, d.Tier, want, ruleName(d))
		}
		if d.Tier != rules.Send {
			continue
		}
		_, body, err := project.Project(project.Input{Item: it, Rule: d.Rule, Person: person})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), it.Fields["body"]) && it.Fields["body"] != "" {
			t.Errorf("%s sent a body", it.Key)
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatal(err)
		}
		if it.Addressed && it.Type == TypeReviewRequested {
			if _, ok := parsed["to"]; !ok {
				t.Errorf("%s did not ask", it.Key)
			}
		}
	}
}

func starterTier(it source.Item) rules.Tier {
	switch {
	case it.Own:
		return rules.Send
	case it.Type == TypeReviewRequested && it.Addressed:
		return rules.Send
	case it.Type == TypeCommented && it.Addressed:
		return rules.Send
	case it.Type == TypeReviewed && it.Fields["author"] == "winton":
		return rules.Send
	case it.Type == TypeMerged && it.Fields["author"] == "winton":
		return rules.Send
	case it.Type == TypeChecksChanged && it.Fields["author"] == "winton":
		return rules.Send
	default:
		return rules.Never
	}
}

func ruleName(d rules.Decision) string {
	if d.Rule == nil {
		return ""
	}
	return d.Rule.Name
}

func bytesReplace(b []byte, old, new string) []byte {
	return []byte(strings.ReplaceAll(string(b), old, new))
}
