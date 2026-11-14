package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// Checks starting or running are not events; only a conclusion is. The snapshot still says pending.
func TestChecksOnlyConclusions(t *testing.T) {
	head := &struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}{Ref: "feature", SHA: "def456"}
	pr := pull{Number: 9, Title: "T", State: "open", User: &ghUser{Login: "winton"}, Head: head, owner: "acme", name: "app"}
	at := "2026-11-07T00:00:00Z"
	obs := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		suites []checkSuite
		key    string
	}{
		{"queued", []checkSuite{{Status: "queued", UpdatedAt: at}}, ""},
		{"running beside a pass", []checkSuite{{Status: "in_progress", UpdatedAt: at}, {Status: "completed", Conclusion: "success", UpdatedAt: at}}, ""},
		{"no conclusion yet", []checkSuite{{Status: "completed", Conclusion: "", UpdatedAt: at}}, ""},
		{"none", nil, ""},
		{"passing", []checkSuite{{Status: "completed", Conclusion: "success", UpdatedAt: at}}, "github:acme/app#9:checks:def456:pass"},
		{"failing", []checkSuite{{Status: "in_progress", UpdatedAt: at}, {Status: "completed", Conclusion: "failure", UpdatedAt: at}}, "github:acme/app#9:checks:def456:fail"},
	} {
		it, ok := checksItem(buildInput{PR: pr, Checks: tc.suites, Fetched: true, Login: "winton"}, obs)
		if tc.key == "" {
			if ok {
				t.Errorf("%s: emitted %s", tc.name, it.Key)
			}
			continue
		}
		if !ok || it.Key != tc.key {
			t.Errorf("%s: %v %s", tc.name, ok, it.Key)
		}
	}
	snap := snapshot(pr, nil, []checkSuite{{Status: "in_progress", UpdatedAt: at}}, true, obs)
	if snap["checks"] != "pending" {
		t.Fatalf("snapshot %v", snap)
	}
}

// GitHub makes a suite for every app that listens to pushes. An app that never runs leaves its suite queued with no
// check runs for good. Counted, the checks read pending forever and never conclude.
func TestIdleSuitesAreNotCounted(t *testing.T) {
	head := &struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}{Ref: "feature", SHA: "def456"}
	pr := pull{Number: 9, Title: "T", State: "open", User: &ghUser{Login: "winton"}, Head: head, owner: "acme", name: "app"}
	obs := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	zero, one := 0, 1
	old := obs.Add(-time.Hour).Format(time.RFC3339)
	recent := obs.Add(-time.Minute).Format(time.RFC3339)
	idle := checkSuite{Status: "queued", UpdatedAt: old, Runs: &zero}
	for _, tc := range []struct {
		name   string
		suites []checkSuite
		want   string
	}{
		{"idle beside a pass", []checkSuite{idle, {Status: "completed", Conclusion: "success", UpdatedAt: old, Runs: &one}}, "pass"},
		{"idle beside a failure", []checkSuite{idle, {Status: "completed", Conclusion: "failure", UpdatedAt: old, Runs: &one}}, "fail"},
		{"only idle", []checkSuite{idle}, "none"},
		{"queued a minute, no runs yet", []checkSuite{{Status: "queued", UpdatedAt: recent, Runs: &zero}, {Status: "completed", Conclusion: "success", UpdatedAt: old, Runs: &one}}, "pending"},
		{"queued with runs", []checkSuite{{Status: "queued", UpdatedAt: old, Runs: &one}}, "pending"},
		{"count not given", []checkSuite{{Status: "queued", UpdatedAt: old}}, "pending"},
	} {
		if got := aggregateChecks(tc.suites, obs); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	in := buildInput{PR: pr, Checks: []checkSuite{idle, {Status: "completed", Conclusion: "success", UpdatedAt: recent, Runs: &one}}, Fetched: true, Login: "winton"}
	it, ok := checksItem(in, obs)
	if !ok || it.Key != "github:acme/app#9:checks:def456:pass" || !it.OccurredAt.Equal(obs.Add(-time.Minute)) {
		t.Fatalf("checks item %v %+v", ok, it)
	}
}

// A merge, a close and a check conclusion each come with a snapshot that covers it, carrying the same state.
func TestSnapshotsBesideStateChanges(t *testing.T) {
	head := &struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}{Ref: "feature", SHA: "def456"}
	merged := pull{ID: 1, Number: 9, Title: "T", State: "closed", Merged: true, User: &ghUser{Login: "winton"}, Head: head,
		Created: "2026-10-01T00:00:00Z", owner: "acme", name: "app"}
	obs := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	one := 1
	in := buildInput{
		Name: "github", PR: merged, Login: "winton", Since: obs.Add(-time.Hour), Observed: obs,
		Timeline: []timelineEvent{{ID: "15", Event: "merged", CreatedAt: "2026-11-07T23:30:00Z", Actor: &ghUser{Login: "dana"}}},
		Checks:   []checkSuite{{Status: "completed", Conclusion: "success", UpdatedAt: "2026-11-07T22:00:00Z", Runs: &one}}, Fetched: true,
	}
	items, err := build(in)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]int{}
	for i, it := range items {
		byKey[it.Key] = i
	}
	snap, ok := byKey["github:acme/app#9:snapshot:timeline:15"]
	if !ok || len(items) != 2 {
		t.Fatalf("items %v", byKey)
	}
	s := items[snap]
	if s.Type != TypeSnapshot || s.Covers != "github:acme/app#9:timeline:15" || s.EventTitle != "State: merged" || s.Own || s.Addressed || s.Actor != "" {
		t.Fatalf("snapshot %+v", s)
	}
	if s.State["state"] != "merged" || s.State["checks"] != "pass" || s.ObservedAt == nil || s.Version != "snapshot:timeline:15" {
		t.Fatalf("snapshot state %v", s.State)
	}
	if err := source.Validate(s, Fields, StateFields); err != nil {
		t.Fatal(err)
	}

	open := merged
	open.State, open.Merged = "open", false
	in = buildInput{Name: "github", PR: open, Login: "winton", Since: obs.Add(-time.Hour), Observed: obs,
		Checks: []checkSuite{{Status: "completed", Conclusion: "failure", UpdatedAt: "2026-11-07T23:50:00Z", Runs: &one}}, Fetched: true}
	items, err = build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[1].Key != "github:acme/app#9:snapshot:checks:def456:fail" || items[1].EventTitle != "State: checks failing" || items[1].Covers != items[0].Key {
		t.Fatalf("checks %+v", items)
	}
}

// GitHub answers mergeable null until it has worked it out. A pass that yields items for an open pull request reads it
// once more after a pause. A pull request merged inside the window is read for checks, so its snapshot says how they
// ended.
func TestCollectRefreshesMergeableAndMergedChecks(t *testing.T) {
	var reads5, suites6 int
	var paused []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		switch {
		case r.URL.Path == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case r.URL.Path == "/user/teams":
			io.WriteString(w, `[]`)
		case r.URL.Path == "/search/issues":
			io.WriteString(w, `{"total_count":2,"items":[
				{"number":5,"updated_at":"2026-11-08T11:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}},
				{"number":6,"updated_at":"2026-11-08T11:00:00Z","user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/5"):
			reads5++
			mergeable := "null"
			if reads5 > 1 {
				mergeable = "false"
			}
			fmt.Fprintf(w, `{"id":5,"number":5,"title":"t","html_url":"https://github.com/acme/app/pull/5","state":"open","user":{"login":"winton"},"created_at":"2026-10-01T00:00:00Z","mergeable":%s,"head":{"sha":"aaa","ref":"a"},"base":{"ref":"main"}}`, mergeable)
		case strings.HasSuffix(r.URL.Path, "/issues/5/timeline"):
			io.WriteString(w, `[{"id":51,"event":"commented","user":{"login":"dana"},"body":"hi","created_at":"2026-11-08T11:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/commits/aaa/check-suites"):
			io.WriteString(w, `{"check_suites":[]}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/6"):
			io.WriteString(w, `{"id":6,"number":6,"title":"t","html_url":"https://github.com/acme/app/pull/6","state":"closed","merged":true,"user":{"login":"winton"},"created_at":"2026-10-01T00:00:00Z","closed_at":"2026-11-08T11:00:00Z","head":{"sha":"bbb","ref":"b"},"base":{"ref":"main"}}`)
		case strings.HasSuffix(r.URL.Path, "/issues/6/timeline"):
			io.WriteString(w, `[{"id":61,"event":"merged","actor":{"login":"dana"},"created_at":"2026-11-08T11:00:00Z"}]`)
		case strings.HasSuffix(r.URL.Path, "/commits/bbb/check-suites"):
			suites6++
			io.WriteString(w, `{"check_suites":[
				{"status":"queued","updated_at":"2026-11-01T00:00:00Z","latest_check_runs_count":0},
				{"status":"completed","conclusion":"success","updated_at":"2026-11-08T10:00:00Z","latest_check_runs_count":3}]}`)
		default:
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv, []string{"acme/app"}, nil)
	s.http.Sleep = func(_ context.Context, d time.Duration) error { paused = append(paused, d); return nil }
	got, err := s.Collect(context.Background(), []byte("2026-11-08T10:30:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if reads5 != 2 || len(paused) != 1 || paused[0] != mergeWait || suites6 != 1 {
		t.Fatalf("reads %d, pauses %v, suites %d", reads5, paused, suites6)
	}
	byKey := map[string]source.Item{}
	for _, it := range got.Items {
		byKey[it.Key] = it
	}
	if it := byKey["github:acme/app#5:comment:51"]; it.State["mergeable"] != "false" {
		t.Fatalf("mergeable %v", it.State)
	}
	snap, ok := byKey["github:acme/app#6:snapshot:timeline:61"]
	if !ok || snap.State["state"] != "merged" || snap.State["checks"] != "pass" || snap.Covers != "github:acme/app#6:timeline:61" {
		t.Fatalf("merge snapshot %+v", snap)
	}
}

// A checks snapshot copies its conclusion's stamp, which can be the local clock: it must not become the cursor.
func TestSnapshotsDoNotMoveTheCursor(t *testing.T) {
	local := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	items := []source.Item{
		{Type: TypeChecksChanged, OccurredAt: local},
		{Type: TypeSnapshot, OccurredAt: local, Covers: "github:acme/app#9:checks:def456:pass"},
		{Type: TypeCommented, OccurredAt: local.Add(-time.Hour)},
	}
	got := sourceTimes(items)
	if len(got) != 1 || !got[0].Equal(local.Add(-time.Hour)) {
		t.Fatalf("times %v", got)
	}
}
