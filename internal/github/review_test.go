package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/cursor"
	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/lease"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/queue"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/runner"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/signal/signaltest"
	"github.com/CrossTerra/beacon/internal/source"
)

func TestLookbackAndServerCursor(t *testing.T) {
	const date = "Sun, 08 Nov 2026 12:00:00 GMT"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", date)
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/user/teams":
			io.WriteString(w, `[]`)
		case r.URL.Path == "/search/issues":
			q := r.URL.Query().Get("q")
			if !strings.Contains(q, "is:open") && !strings.Contains(q, "2026-11-08T11:45:00Z") {
				t.Errorf("search did not look back: %s", q)
			}
			io.WriteString(w, `{"total_count":1,"items":[{"number":1,"updated_at":"2026-11-08T11:50:00Z","user":{"login":"dana"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			io.WriteString(w, `{"id":1,"number":1,"title":"Late","html_url":"https://github.com/acme/app/pull/1","state":"open","user":{"login":"dana"},"assignees":[{"login":"winton"}],"created_at":"2026-10-01T00:00:00Z","head":{"ref":"x","sha":"abc"},"base":{"ref":"main"}}`)
		case strings.HasSuffix(r.URL.Path, "/issues/1/timeline"):
			io.WriteString(w, `[{"id":9,"event":"commented","user":{"login":"dana"},"body":"indexed late","created_at":"2026-11-08T11:50:00Z"}]`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/app"}, nil)
	s.now = func() time.Time { return time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC) }
	got, err := s.Collect(context.Background(), []byte("2026-11-08T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Next) != "2026-11-08T12:00:00Z" {
		t.Fatalf("cursor used the local clock: %s", got.Next)
	}
	found := false
	for _, it := range got.Items {
		if it.Type == TypeCommented && it.Fields["body"] == "indexed late" {
			found = true
		}
	}
	if !found {
		t.Fatal("event inside the lookback was dropped")
	}
}

func TestSearchCapAndOldHits(t *testing.T) {
	var fetchedOld bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/user/teams":
			io.WriteString(w, `[]`)
		case strings.Contains(r.URL.Path, "/pulls/2"):
			fetchedOld = true
			http.NotFound(w, r)
		case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "is:open"):
			io.WriteString(w, `{"total_count":0,"items":[]}`)
		case r.URL.Path == "/search/issues":
			if !strings.Contains(r.URL.Query().Get("q"), "updated:>=2026-11-07T23:45:00Z") {
				t.Errorf("search is not a timestamp qualifier: %s", r.URL.Query().Get("q"))
			}
			io.WriteString(w, `{"total_count":2,"items":[
				{"number":2,"updated_at":"2026-10-01T00:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}},
				{"number":1,"updated_at":"2026-11-08T12:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}
			]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			io.WriteString(w, pr9)
		case strings.Contains(r.URL.Path, "check-suites"):
			io.WriteString(w, `{"check_suites":[]}`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/app"}, nil)
	if _, err := s.Collect(context.Background(), []byte("2026-11-08T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if fetchedOld {
		t.Fatal("fetched a hit older than the lookback window")
	}

	capSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		if r.URL.Path == "/user" {
			io.WriteString(w, `{"login":"winton"}`)
			return
		}
		if r.URL.Path == "/user/teams" {
			io.WriteString(w, `[]`)
			return
		}
		io.WriteString(w, `{"total_count":1001,"incomplete_results":false,"items":[]}`)
	}))
	defer capSrv.Close()
	s = newTest(t, capSrv, []string{"acme/app"}, nil)
	got, err := s.Collect(context.Background(), []byte("2026-11-08T00:00:00Z"))
	if err == nil || got.Next != nil {
		t.Fatalf("over-cap search committed: %v %s", err, got.Next)
	}
}

func TestPushIgnoresCommitTime(t *testing.T) {
	in := buildInput{
		Name: "github", Login: "winton", Observed: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		Since: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
		PR: pull{ID: 1, Number: 1, owner: "acme", name: "app", Title: "t", HTMLURL: "https://github.com/acme/app/pull/1",
			User: &ghUser{Login: "winton"}, Head: &struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}{SHA: "newhead"}},
		Timeline: []timelineEvent{
			{ID: "1", Event: "reviewed", User: &ghUser{Login: "winton"}, State: "APPROVED", CommitID: "old", SubmittedAt: "2026-11-01T00:00:00Z"},
			{Event: "committed", SHA: "newhead", Author: &ghUser{Email: "a@b.c", Date: "2026-10-01T00:00:00Z"}, Committer: &ghUser{Date: "2026-11-08T01:00:00Z"}},
		},
	}
	items, err := build(in)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it.Type == TypePushed && strings.HasSuffix(it.Key, ":push:newhead") {
			found = true
		}
	}
	if !found {
		t.Fatal("push with an old commit time was dropped")
	}
}

func TestFutureAuthorDateDoesNotJumpCursor(t *testing.T) {
	const date = "Sun, 08 Nov 2026 12:00:00 GMT"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", date)
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/user/teams":
			io.WriteString(w, `[]`)
		case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "is:open"):
			io.WriteString(w, `{"total_count":0,"items":[]}`)
		case r.URL.Path == "/search/issues":
			io.WriteString(w, `{"total_count":1,"items":[{"number":1,"updated_at":"2026-11-08T12:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			io.WriteString(w, `{"id":1,"number":1,"title":"Skew","html_url":"https://github.com/acme/app/pull/1","state":"open","user":{"login":"dana"},"created_at":"2026-10-01T00:00:00Z","head":{"ref":"x","sha":"future"},"base":{"ref":"main"}}`)
		case strings.HasSuffix(r.URL.Path, "/issues/1/timeline"):
			io.WriteString(w, `[{"id":1,"event":"reviewed","user":{"login":"winton"},"state":"APPROVED","commit_id":"old","submitted_at":"2026-11-01T00:00:00Z"},{"event":"committed","sha":"future","author":{"email":"a@b.c","date":"2030-01-01T00:00:00Z"},"committer":{"date":"2026-11-08T01:00:00Z"}}]`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/app"}, nil)
	got, err := s.Collect(context.Background(), []byte("2026-11-08T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Next) != "2026-11-08T12:00:00Z" {
		t.Fatalf("author date moved the cursor: %s", got.Next)
	}
	found := false
	for _, it := range got.Items {
		if it.Type == TypePushed && strings.HasSuffix(it.Key, ":push:future") {
			found = true
		}
	}
	if !found {
		t.Fatal("push with a future author date was dropped")
	}
}

func TestBare403FailsThePass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		if r.URL.Path == "/user" {
			io.WriteString(w, `{"login":"winton"}`)
			return
		}
		if r.URL.Path == "/user/teams" {
			io.WriteString(w, `[]`)
			return
		}
		if r.URL.Path == "/search/issues" {
			io.WriteString(w, `{"total_count":1,"items":[{"number":1,"updated_at":"2026-11-08T12:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
			return
		}
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/app"}, nil)
	got, err := s.Collect(context.Background(), []byte("2026-11-01T00:00:00Z"))
	if err == nil || got.Next != nil {
		t.Fatalf("bare 403 was skipped: %v %s", err, got.Next)
	}

	rate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		if r.URL.Path == "/user" {
			io.WriteString(w, `{"login":"winton"}`)
			return
		}
		if r.URL.Path == "/user/teams" {
			io.WriteString(w, `[]`)
			return
		}
		if r.URL.Path == "/search/issues" {
			io.WriteString(w, `{"total_count":1,"items":[{"number":1,"updated_at":"2026-11-08T12:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
			return
		}
		http.Error(w, "You have exceeded a secondary rate limit.", http.StatusForbidden)
	}))
	defer rate.Close()
	s = newTest(t, rate, []string{"acme/app"}, nil)
	got, err = s.Collect(context.Background(), []byte("2026-11-01T00:00:00Z"))
	if err == nil || got.Next != nil {
		t.Fatalf("secondary rate limit was skipped: %v %s", err, got.Next)
	}
}

func TestPermission403SkipsTheObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/user/teams":
			io.WriteString(w, `[]`)
		case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "is:open"):
			io.WriteString(w, `{"total_count":0,"items":[]}`)
		case r.URL.Path == "/search/issues":
			io.WriteString(w, `{"total_count":2,"items":[
				{"number":1,"updated_at":"2026-11-08T12:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}},
				{"number":2,"updated_at":"2026-11-08T12:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}
			]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			http.Error(w, "Resource not accessible by integration", http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/pulls/2"):
			io.WriteString(w, pr9)
		case strings.Contains(r.URL.Path, "check-suites"):
			io.WriteString(w, `{"check_suites":[]}`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/app"}, nil)
	got, err := s.Collect(context.Background(), []byte("2026-11-01T00:00:00Z"))
	if err != nil || got.Next == nil {
		t.Fatalf("permission 403 failed the pass: %v %s", err, got.Next)
	}
	for _, it := range got.Items {
		if strings.Contains(it.Key, "#1:") {
			t.Fatal("an unreadable pull yielded an item")
		}
	}
}

func TestTeamReviewRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/user/teams":
			io.WriteString(w, `[{"slug":"eng","organization":{"login":"acme"}},{"slug":"eng","organization":{"login":"other"}}]`)
		case r.URL.Path == "/search/issues":
			io.WriteString(w, `{"total_count":1,"items":[{"number":4,"user":{"login":"dana"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/4"):
			io.WriteString(w, `{"id":4,"number":4,"title":"Team","html_url":"https://github.com/acme/app/pull/4","state":"open","user":{"login":"dana"},"created_at":"2026-11-02T00:00:00Z","requested_teams":[{"slug":"eng","organization":{"login":"acme"}}],"head":{"sha":"abc","ref":"t"},"base":{"ref":"main"}}`)
		case strings.HasSuffix(r.URL.Path, "/issues/4/timeline"):
			io.WriteString(w, `[{"id":40,"event":"review_requested","created_at":"2026-11-02T01:00:00Z","review_requester":{"login":"dana"},"requested_team":{"slug":"eng","organization":{"login":"acme"}}},{"id":41,"event":"review_requested","created_at":"2026-11-02T01:00:00Z","review_requester":{"login":"dana"},"requested_team":{"slug":"eng","organization":{"login":"other"}}}]`)
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
	found := false
	for _, it := range got.Items {
		if it.Type == TypeReviewRequested && it.Addressed && strings.Contains(it.Key, "timeline:40") {
			found = true
		}
	}
	if !found {
		t.Fatal("team review request was not addressed on the first run")
	}
	for _, it := range got.Items {
		if strings.Contains(it.Key, "timeline:41") {
			t.Fatal("a team outside the configured owners was addressed")
		}
	}
}

func TestOpenedAddressedByBodyMention(t *testing.T) {
	in := buildInput{Name: "github", Login: "winton", First: true, Observed: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
		PR: pull{ID: 3, Number: 3, owner: "acme", name: "app", Title: "t", Body: "hey @winton", HTMLURL: "https://github.com/acme/app/pull/3",
			State: "open", User: &ghUser{Login: "dana"}, Created: "2026-11-02T00:00:00Z"}}
	items, err := build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !items[0].Addressed || items[0].Type != TypeOpened {
		t.Fatalf("%+v", items)
	}
}

func TestRestartDoesNotSendTwice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/user/teams":
			io.WriteString(w, `[]`)
		case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "is:open"):
			io.WriteString(w, `{"total_count":0,"items":[]}`)
		case r.URL.Path == "/search/issues":
			io.WriteString(w, `{"total_count":1,"items":[{"number":1,"updated_at":"2026-11-08T11:55:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			io.WriteString(w, `{"id":1,"number":1,"title":"Restart","html_url":"https://github.com/acme/app/pull/1","state":"closed","user":{"login":"winton"},"created_at":"2026-10-01T00:00:00Z","head":{"ref":"x","sha":"abc"},"base":{"ref":"main"}}`)
		case strings.HasSuffix(r.URL.Path, "/issues/1/timeline"):
			io.WriteString(w, `[{"id":9,"event":"commented","user":{"login":"dana"},"body":"again","created_at":"2026-11-08T11:55:00Z"}]`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/app"}, nil)
	dir := t.TempDir()
	now := time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC)
	rs, err := rules.Parse([]byte("rules:\n  - name: all\n    match: {}\n    tier: send\n    fields: [title]\n"))
	if err != nil {
		t.Fatal(err)
	}
	ob, err := outbox.Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	q, err := queue.Open(dir, ob)
	if err != nil {
		t.Fatal(err)
	}
	st, err := cursor.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sig := signaltest.New("tok")
	defer sig.Close()
	store := keychain.NewMemory()
	store.Put("a", "tok")
	secret, err := store.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(sig.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := signal.New(u, egress.Transport([]string{egress.Address(u)}), secret)
	client.Sleep = func(context.Context, time.Duration) error { return nil }
	run := func() {
		t.Helper()
		r := &runner.Runner{
			Sources: []source.Source{s}, Rules: rs,
			Person: project.Person{Handle: "@winton", Identities: map[string]string{"github": "github:user:winton"}},
			Send:   true, Outbox: ob, Queue: q, Writer: client, Cursors: st,
			Log: logs.Discard(), Now: func() time.Time { return now },
			Lease: &lease.Lease{Outbox: ob, Writer: client, Person: "@winton", Run: "test:lease", Period: 5 * time.Minute, Now: func() time.Time { return now }},
		}
		if _, err := r.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Commit("github", []byte("2026-11-08T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	run()
	first := githubSends(t, sig)
	if first == 0 {
		t.Fatal("first pass sent nothing")
	}
	entries := len(ob.Entries())
	run()
	if githubSends(t, sig) != first {
		t.Fatalf("restart sent again: %d then %d", first, githubSends(t, sig))
	}
	if len(ob.Entries()) != entries {
		t.Fatalf("restart grew the outbox: %d to %d", entries, len(ob.Entries()))
	}
}

func TestFirstDateAndCursorBounds(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		} else {
			w.Header().Set("Date", "Sun, 08 Nov 2026 12:40:00 GMT")
		}
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/user/teams":
			io.WriteString(w, `[]`)
		case r.URL.Path == "/search/issues":
			io.WriteString(w, `{"total_count":1,"items":[{"number":1,"updated_at":"2026-11-08T12:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			io.WriteString(w, `{"id":1,"number":1,"title":"t","html_url":"https://github.com/acme/app/pull/1","state":"closed","user":{"login":"winton"},"created_at":"2026-10-01T00:00:00Z","head":{"sha":"abc","ref":"t"},"base":{"ref":"main"}}`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/app"}, nil)
	got, err := s.Collect(context.Background(), []byte("2026-11-08T00:00:00Z"))
	if err != nil || string(got.Next) != "2026-11-08T12:00:00Z" {
		t.Fatalf("latest date won: %s %v", got.Next, err)
	}
	back, err := s.Collect(context.Background(), []byte("2026-11-08T18:00:00Z"))
	if err != nil || string(back.Next) != "2026-11-08T18:00:00Z" {
		t.Fatalf("cursor moved backwards: %s %v", back.Next, err)
	}

	ahead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 15:00:00 GMT")
		switch r.URL.Path {
		case "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case "/user/teams":
			io.WriteString(w, `[]`)
		default:
			io.WriteString(w, `{"total_count":0,"items":[]}`)
		}
	}))
	defer ahead.Close()
	s = newTest(t, ahead, []string{"acme/app"}, nil)
	s.now = func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }
	got, err = s.Collect(context.Background(), []byte("2026-11-08T00:00:00Z"))
	if err == nil || got.Next != nil {
		t.Fatalf("future Date committed: %v %s", err, got.Next)
	}
}

func TestSearchSplitsByRepository(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/user/teams":
			io.WriteString(w, `[]`)
		case r.URL.Path == "/users/acme":
			io.WriteString(w, `{"login":"acme","type":"Organization"}`)
		case r.URL.Path == "/orgs/acme/repos":
			io.WriteString(w, `[{"name":"app"},{"name":"lib"}]`)
		case r.URL.Path == "/search/issues":
			q := r.URL.Query().Get("q")
			queries = append(queries, q)
			if strings.Contains(q, "org:acme") {
				io.WriteString(w, `{"total_count":1001,"items":[]}`)
				return
			}
			io.WriteString(w, `{"total_count":1,"items":[{"number":1,"updated_at":"2026-11-08T12:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			io.WriteString(w, pr9)
		case strings.Contains(r.URL.Path, "check-suites"):
			io.WriteString(w, `{"check_suites":[]}`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/*"}, nil)
	got, err := s.Collect(context.Background(), []byte("2026-11-08T00:00:00Z"))
	if err != nil || got.Next == nil {
		t.Fatalf("over-cap owner search stalled: %v %s", err, got.Next)
	}
	split := false
	for _, q := range queries {
		if strings.Contains(q, "repo:acme/app") {
			split = true
		}
	}
	if !split {
		t.Fatalf("did not split by repository: %v", queries)
	}
}

func TestUserSplitListsPrivateRepos(t *testing.T) {
	var sawUserRepos, sawPublicOnly bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		switch {
		case r.URL.Path == "/user" && r.URL.RawQuery == "":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/user/teams":
			io.WriteString(w, `[]`)
		case r.URL.Path == "/users/winton":
			io.WriteString(w, `{"login":"winton","type":"User"}`)
		case r.URL.Path == "/user/repos":
			sawUserRepos = true
			io.WriteString(w, `[{"name":"app"}]`)
		case r.URL.Path == "/users/winton/repos":
			sawPublicOnly = true
			io.WriteString(w, `[]`)
		case r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "user:winton"):
			io.WriteString(w, `{"total_count":1001,"items":[]}`)
		case r.URL.Path == "/search/issues":
			io.WriteString(w, `{"total_count":1,"items":[{"number":1,"updated_at":"2026-11-08T12:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/winton/app","pull_request":{"url":"x"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			io.WriteString(w, strings.ReplaceAll(pr9, "acme/app", "winton/app"))
		case strings.Contains(r.URL.Path, "check-suites"):
			io.WriteString(w, `{"check_suites":[]}`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"winton/*"}, nil)
	got, err := s.Collect(context.Background(), []byte("2026-11-08T00:00:00Z"))
	if err != nil || got.Next == nil || !sawUserRepos || sawPublicOnly {
		t.Fatalf("private repos missed: err %v next %s user %v public %v", err, got.Next, sawUserRepos, sawPublicOnly)
	}
}

func TestOldPushNotEmitted(t *testing.T) {
	in := buildInput{
		Name: "github", Login: "winton", Observed: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		Since: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
		PR: pull{ID: 1, Number: 1, owner: "acme", name: "app", Title: "t", HTMLURL: "https://github.com/acme/app/pull/1",
			User: &ghUser{Login: "winton"}, Head: &struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}{SHA: "oldhead"}},
		Timeline: []timelineEvent{
			{ID: "1", Event: "reviewed", User: &ghUser{Login: "winton"}, State: "APPROVED", CommitID: "base", SubmittedAt: "2026-10-01T00:00:00Z"},
			{Event: "committed", SHA: "oldhead", Author: &ghUser{Date: "2026-11-08T02:00:00Z"}, Committer: &ghUser{Date: "2026-10-02T00:00:00Z"}},
		},
	}
	items, err := build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Type == TypePushed {
			t.Fatalf("push older than the first run was emitted: %+v", it)
		}
	}
}

func githubSends(t *testing.T, sig *signaltest.Server) int {
	t.Helper()
	n := 0
	for _, req := range sig.Requests() {
		if strings.HasPrefix(req.Key, "github:") {
			n++
		}
	}
	return n
}
