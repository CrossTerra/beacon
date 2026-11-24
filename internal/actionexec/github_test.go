package actionexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
)

type tripFunc func(*http.Request) (*http.Response, error)

func (f tripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonReply(status int, body any) *http.Response {
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b))}
}

func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func reason(err error) (string, string) {
	var r approval.Refusal
	if !errors.As(err, &r) {
		return "", ""
	}
	return r.Code, r.Detail
}

func secret(value string) keychain.Secret {
	s, err := keychain.FromString(value)
	if err != nil {
		panic(err)
	}
	return s
}

const (
	fixtureHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fixtureSHA  = "cccccccccccccccccccccccccccccccccccccccc"
	githubHost  = "https://gh.fixture.test"
	fixtureRepo = "/repos/CrossTerra/signal"
)

type fakeGitHub struct {
	t                                            *testing.T
	requests, pulls, puts, userCalls, otherPulls int
	state, base, head, mergeableState, userType  string
	merged, down, lose, redirect, repeatPages    bool
	failUser, blankSHA                           bool
	mergeable                                    *bool
	status                                       int
	user, writeUser, mergedBy, repoID            int64
	rule, classic                                []string
	reviews                                      []any
	runs                                         []githubRun
	statusPages                                  [][]map[string]string
	afterPull                                    func(*fakeGitHub)
}

func (f *fakeGitHub) handle(r *http.Request) (*http.Response, error) {
	f.requests++
	if r.URL.Scheme+"://"+r.URL.Host != githubHost {
		f.t.Error("contacted an unconfigured origin")
	}
	write := r.Header.Get("Authorization") == "Bearer fixture-write"
	if !write && r.Header.Get("Authorization") != "Bearer fixture-read" {
		f.t.Error("missing fixture token")
	}
	if f.down {
		return nil, io.ErrUnexpectedEOF
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /user":
		f.userCalls++
		if f.failUser && !write {
			return jsonReply(500, nil), nil
		}
		if f.redirect {
			a := jsonReply(302, nil)
			a.Header.Set("Location", "https://other.fixture.test/user")
			return a, nil
		}
		id := f.user
		if write && f.writeUser != 0 {
			id = f.writeUser
		}
		return jsonReply(200, githubUser{ID: id, Login: "fixture-login", Type: f.userType}), nil
	case "GET " + fixtureRepo + "/pulls/141":
		f.otherPulls++
		return jsonReply(200, map[string]any{
			"state": "closed", "merged": true, "mergeable": true, "mergeable_state": "unknown",
			"title": "Other fixture", "merge_commit_sha": fixtureSHA, "merged_by": githubUser{ID: f.user},
			"base": map[string]any{"ref": "main", "repo": map[string]any{"id": f.repoID, "full_name": "CrossTerra/signal"}},
			"head": map[string]any{"sha": fixtureHead},
		}), nil
	case "GET " + fixtureRepo + "/pulls/140":
		f.pulls++
		sha := fixtureSHA
		if f.blankSHA {
			sha = ""
		}
		a := jsonReply(200, map[string]any{
			"state": f.state, "merged": f.merged, "mergeable": f.mergeable, "mergeable_state": f.mergeableState,
			"title": "Fixture title", "merge_commit_sha": sha, "merged_by": githubUser{ID: f.mergedBy},
			"base": map[string]any{"ref": f.base, "repo": map[string]any{"id": f.repoID, "full_name": "CrossTerra/signal"}},
			"head": map[string]any{"sha": f.head},
		})
		if f.afterPull != nil {
			f.afterPull(f)
		}
		return a, nil
	case "GET " + fixtureRepo + "/pulls/140/reviews":
		return jsonReply(200, f.reviews), nil
	case "GET " + fixtureRepo + "/rules/branches/main":
		var rules = []any{}
		if len(f.rule) != 0 {
			var ctxs []githubContext
			for _, name := range f.rule {
				ctxs = append(ctxs, githubContext{name})
			}
			rules = append(rules, map[string]any{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": ctxs}})
		}
		return jsonReply(200, rules), nil
	case "GET " + fixtureRepo + "/branches/main":
		return jsonReply(200, map[string]any{"protection": map[string]any{"required_status_checks": map[string]any{"contexts": f.classic}}}), nil
	case "GET " + fixtureRepo + "/commits/" + fixtureHead + "/check-runs":
		if r.URL.Query().Get("filter") != "all" {
			f.t.Error("did not look at all check runs")
		}
		a := jsonReply(200, map[string]any{"check_runs": f.runs})
		if f.repeatPages {
			a.Header.Set("Link", `<https://other.fixture.test/?page=2>; rel="next"`)
		}
		return a, nil
	case "GET " + fixtureRepo + "/commits/" + fixtureHead + "/status":
		q := r.URL.Query().Get("page")
		statuses := []map[string]string{}
		if q == "1" && len(f.statusPages) >= 1 {
			statuses = f.statusPages[0]
		}
		if q == "2" && len(f.statusPages) == 2 {
			statuses = f.statusPages[1]
		}
		a := jsonReply(200, map[string]any{"statuses": statuses})
		if q == "1" && len(f.statusPages) == 2 {
			a.Header.Set("Link", `<https://other.fixture.test/?page=2>; rel="next"`)
		}
		return a, nil
	case "PUT " + fixtureRepo + "/pulls/140/merge":
		f.puts++
		var body map[string]string
		if !write || r.GetBody != nil || json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 ||
			body["sha"] != fixtureHead || body["merge_method"] != "squash" {
			f.t.Error("merge was not the non-replayable frozen write")
		}
		r.Body.Close()
		if f.mergedBy != 0 {
			f.merged, f.state = true, "closed"
		}
		if f.lose {
			return nil, io.ErrUnexpectedEOF
		}
		return jsonReply(f.status, map[string]any{"merged": f.merged, "sha": fixtureSHA}), nil
	}
	f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	return jsonReply(404, nil), nil
}

type mergeFixture struct {
	t  *testing.T
	h  *mergeHandler
	f  *fakeGitHub
	j  *approval.Journal
	p  approval.Proposal
	at time.Time
}

func mergeTest(t *testing.T) *mergeFixture {
	t.Helper()
	f := &fakeGitHub{t: t, state: "open", base: "main", head: fixtureHead, mergeableState: "clean", userType: "User",
		mergeable: new(bool), user: 123, repoID: 456, rule: []string{"ci"}, reviews: []any{},
		runs: []githubRun{{ID: 2, Name: "ci", Status: "completed", Conclusion: "success"}}, mergedBy: 123, status: 200}
	*f.mergeable = true
	m := &mergeFixture{t: t, f: f, at: time.Unix(1_800_000_000, 0)}
	dir := filepath.Join(t.TempDir(), "state")
	settings := Settings{GitHub: &GitHubSettings{Origin: githubHost, UserID: 123}}
	providers, err := New(settings, dir, func() time.Time { return m.at }, func([]string) http.RoundTripper { return tripFunc(f.handle) })
	requireOK(t, err)
	m.h = providers.github
	m.h.provider.read = func() (keychain.Secret, error) { return secret("fixture-read"), nil }
	m.h.provider.write = func([]byte) (keychain.Secret, error) { return secret("fixture-write"), nil }
	m.j, err = approval.OpenJournal(dir, m.at, "")
	requireOK(t, err)
	t.Cleanup(func() { m.j.Close() })
	m.p, err = m.j.Add(approval.Request{Type: approval.Merge, Agent: "fixture", Reason: "Fixture merge.", Params: approval.Object{
		"repo": "CrossTerra/signal", "number": int64(140), "base": "main", "head": fixtureHead, "method": "squash",
	}}, approval.PeerLabel{}, m.at)
	requireOK(t, err)
	return m
}

func (m *mergeFixture) start() approval.View {
	m.t.Helper()
	r, err := m.h.Resolve(m.t.Context(), m.p)
	requireOK(m.t, err)
	f, err := approval.Freeze(m.p, r.Binding, "")
	requireOK(m.t, err)
	requireOK(m.t, m.j.ApproveAll([]approval.Frozen{f}, []byte("fixture-credential"), nil, m.at))
	v, err := m.j.StartAsserted(m.p.ID, m.at)
	requireOK(m.t, err)
	return v
}

func (m *mergeFixture) run(v approval.View) {
	m.t.Helper()
	token, err := m.h.Unlock(v.Frozen, make([]byte, 32))
	requireOK(m.t, err)
	defer token.Clear()
	requireOK(m.t, m.h.Run(m.t.Context(), m.j, v, token))
}

func (m *mergeFixture) want(status, by, code string, puts int) {
	m.t.Helper()
	v, _, err := m.j.Get(m.p.ID, m.at)
	requireOK(m.t, err)
	var r approval.Result
	if v.Outcome != nil {
		r = *v.Outcome
	}
	if v.Status != status || r.ConfirmedBy != by || r.Error != code || m.f.puts != puts {
		m.t.Fatalf("want %s/%s/%s, %d writes; got %s/%+v, %d writes", status, by, code, puts, v.Status, r, m.f.puts)
	}
	if status == "executed" && r.Native != "github:merge:"+fixtureSHA {
		m.t.Fatal("wrong native id")
	}
}

func TestMergeRefusesBadPreflightAndChangedFinalReads(t *testing.T) {
	review := func(id int, state string) any {
		return map[string]any{"user": map[string]int{"id": id}, "state": state}
	}
	for _, tc := range []struct {
		code   string
		change func(*fakeGitHub)
	}{
		{"account_mismatch", func(f *fakeGitHub) { f.user++ }},
		{"not_a_user", func(f *fakeGitHub) { f.userType = "Bot" }},
		{"already_merged", func(f *fakeGitHub) { f.merged = true }},
		{"not_open", func(f *fakeGitHub) { f.state = "closed" }},
		{"head_moved", func(f *fakeGitHub) { f.head = strings.Repeat("e", 40) }},
		{"base_changed", func(f *fakeGitHub) { f.base = "release" }},
		{"mergeability_unknown", func(f *fakeGitHub) { f.mergeable = nil }},
		{"not_mergeable", func(f *fakeGitHub) { f.mergeableState = "blocked" }},
		{"changes_requested", func(f *fakeGitHub) { f.reviews = []any{review(9, "CHANGES_REQUESTED"), review(9, "COMMENTED")} }},
		{"checks_not_green", func(f *fakeGitHub) { f.runs[0].Conclusion = "failure" }},
		{"checks_not_green", func(f *fakeGitHub) { f.classic = []string{"lint"} }},
		{"unavailable", func(f *fakeGitHub) { f.down = true }},
	} {
		t.Run(tc.code, func(t *testing.T) {
			preview := mergeTest(t)
			tc.change(preview.f)
			if _, err := preview.h.Resolve(t.Context(), preview.p); err == nil || err.Error() != tc.code || preview.f.puts != 0 {
				t.Fatalf("preflight returned %v", err)
			}
			m := mergeTest(t)
			v := m.start()
			tc.change(m.f)
			m.run(v)
			m.want("failed", "pre_dispatch", tc.code, 0)
		})
	}
	m := mergeTest(t)
	m.f.reviews = []any{review(9, "CHANGES_REQUESTED"), review(9, "APPROVED")}
	m.run(m.start())
	m.want("executed", "reply", "", 1)
}

func TestMergeLastReadStopsADifferentRepositoryOrHead(t *testing.T) {
	for _, tc := range []struct {
		code   string
		change func(*fakeGitHub)
	}{
		{"repository_changed", func(f *fakeGitHub) { f.repoID++ }},
		{"head_moved", func(f *fakeGitHub) { f.head = strings.Repeat("b", 40) }},
		{"base_changed", func(f *fakeGitHub) { f.base = "release" }},
		{"already_merged", func(f *fakeGitHub) { f.merged = true }},
	} {
		t.Run(tc.code, func(t *testing.T) {
			m := mergeTest(t)
			v := m.start()
			m.f.afterPull = func(f *fakeGitHub) {
				if f.pulls == 2 {
					tc.change(f)
				}
			}
			m.run(v)
			m.want("failed", "pre_dispatch", tc.code, 0)
			if m.f.pulls != 3 {
				t.Fatal("did not re-read the PR")
			}
		})
	}
}

func TestMergeUsesLatestCheckPerAppButRequiresEveryCheck(t *testing.T) {
	run := func(id, app int64, result string, started time.Time) githubRun {
		r := githubRun{ID: id, Name: "ci", Status: "completed", Conclusion: result, Started: started}
		r.App.ID = app
		if result == "" {
			r.Status = "queued"
		}
		return r
	}
	at := time.Unix(1_800_000_000, 0).UTC()
	for _, tc := range []struct {
		name string
		runs []githubRun
		want string
	}{
		{"rerun", []githubRun{run(2, 1, "success", at), run(1, 1, "failure", at.Add(time.Hour))}, "1 passed"},
		{"newest fails", []githubRun{run(1, 1, "success", at), run(2, 1, "failure", at)}, "1 failed: ci"},
		{"no ids", []githubRun{run(0, 1, "success", at), run(0, 1, "failure", at.Add(-time.Hour))}, "1 passed"},
		{"unfinished newest", []githubRun{run(0, 1, "success", at), run(0, 1, "", time.Time{})}, "1 pending: ci"},
		{"unstarted finished oldest", []githubRun{run(0, 1, "success", at), run(0, 1, "failure", time.Time{})}, "1 passed"},
		{"other app", []githubRun{run(2, 1, "success", at), run(1, 2, "cancelled", at)}, "1 failed: ci"},
		{"skipped and neutral", []githubRun{run(1, 1, "skipped", at), run(1, 2, "neutral", at)}, "1 passed"},
		{"none reported", []githubRun{}, "none reported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mergeTest(t)
			m.f.rule, m.f.runs = nil, tc.runs
			r, err := m.h.Resolve(t.Context(), m.p)
			if err == nil && r.Facts["Checks"] == tc.want || err != nil && err.Error() == "checks_not_green" &&
				(strings.Contains(tc.want, "failed") || strings.Contains(tc.want, "pending")) {
				if err != nil {
					if _, got := reason(err); got != tc.want {
						t.Fatalf("report %q", got)
					}
				}
				return
			}
			t.Fatalf("checks %v; error %v", r.Facts, err)
		})
	}
	t.Run("unrequired red status on later page", func(t *testing.T) {
		m := mergeTest(t)
		m.f.rule = nil
		m.f.statusPages = [][]map[string]string{{{"context": "lint", "state": "success"}}, {{"context": "deploy", "state": "failure"}}}
		if _, err := m.h.Resolve(t.Context(), m.p); err == nil || err.Error() != "checks_not_green" {
			t.Fatal("ignored a later failing commit status", err)
		}
	})
}

func TestMergeReplyAndLostReply(t *testing.T) {
	for _, tc := range []struct {
		status        int
		lose          bool
		got, by, code string
	}{
		{200, false, "executed", "reply", ""},
		{200, true, "executed", "read_back", ""},
		{502, false, "executed", "read_back", ""},
		{409, false, "failed", "reply", "head_moved"},
		{405, false, "failed", "reply", "not_mergeable"},
		{422, false, "failed", "reply", "rejected"},
		{403, false, "failed", "reply", "forbidden"},
	} {
		m := mergeTest(t)
		m.f.status, m.f.lose = tc.status, tc.lose
		if tc.got != "executed" {
			m.f.mergedBy = 0
		}
		v := m.start()
		m.run(v)
		m.want(tc.got, tc.by, tc.code, 1)
		if err := m.h.Run(t.Context(), m.j, v, secret("fixture-write")); !errors.Is(err, approval.ErrTransition) {
			t.Fatal("a finished approval could be run again", err)
		}
	}
}

func TestMergeReadBackSettlesOpenOnlyAfterTwoSpacedReads(t *testing.T) {
	m := mergeTest(t)
	m.f.lose, m.f.mergedBy = true, 0
	m.run(m.start())
	m.want("outcome_unknown", "", "", 1)
	read := func() { t.Helper(); requireOK(t, m.h.Reconcile(t.Context(), m.j, m.p.ID)) }
	m.at = m.at.Add(30 * time.Second)
	read()
	m.at = m.at.Add(30 * time.Second)
	read()
	read()
	m.at = m.at.Add(59 * time.Second)
	read()
	m.want("outcome_unknown", "", "", 1)
	m.at = m.at.Add(time.Second)
	read()
	m.want("failed", "read_back", "not_merged", 1)
}

func TestMergeReadBackAfterRestartUsesApprovedRepositoryAndAccount(t *testing.T) {
	for _, actor := range []int64{77, 123} {
		m := mergeTest(t)
		_ = m.start() // Crash after the start: the merge may or may not have reached GitHub.
		requireOK(t, m.j.Close())
		j, err := approval.OpenJournal(m.h.dir, m.at, "")
		requireOK(t, err)
		m.j = j
		m.f.merged, m.f.state, m.f.mergedBy = true, "closed", actor
		read := func() { t.Helper(); requireOK(t, m.h.Reconcile(t.Context(), m.j, m.p.ID)) }
		m.f.repoID++
		read()
		m.want("outcome_unknown", "", "", 0)
		m.f.repoID--
		m.f.user++
		read()
		m.want("outcome_unknown", "", "", 0)
		m.f.user--
		read()
		if actor == 123 {
			m.want("executed", "read_back", "", 0)
		} else {
			m.want("failed", "read_back", "merged_otherwise", 0)
		}
	}
}

func TestMergeReadBoundsAndNoRedirect(t *testing.T) {
	m := mergeTest(t)
	m.h.preflight = newBudget(5, 2)
	for _, want := range []int{5, 5, 10} {
		if want == 10 {
			m.at = m.at.Add(time.Hour)
		}
		if _, err := m.h.Resolve(t.Context(), m.p); err == nil || err.Error() != "busy" || m.f.requests != want {
			t.Fatalf("read count %d; error %v", m.f.requests, err)
		}
	}
	m.h.preflight = newBudget(100, 2)
	m.f.repeatPages = true
	if _, err := m.h.Resolve(t.Context(), m.p); err == nil || err.Error() != "unavailable" || m.f.requests != 10+5+mergePageLimit {
		t.Fatalf("unbounded page list: %d, %v", m.f.requests, err)
	}
	m.f.redirect = true
	before := m.f.requests
	if _, err := m.h.Resolve(t.Context(), m.p); err == nil || m.f.requests != before+1 {
		t.Fatal("followed a redirect or used it as evidence", err)
	}
	m.p.Params["repo"] = "CrossTerra/.."
	if _, err := m.h.Resolve(t.Context(), m.p); err == nil || m.f.requests != before+1 {
		t.Fatal("a dot path was accepted", err)
	}
}

func TestMergePreviewNamesTheApprovedHead(t *testing.T) {
	m := mergeTest(t)
	r, err := m.h.Resolve(t.Context(), m.p)
	requireOK(t, err)
	if r.Facts["Head"] != fixtureHead {
		t.Fatalf("head fact = %v", r.Facts["Head"])
	}
}

func TestMergeReadBackFailsUnlessThisUserMergedWithACommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fakeGitHub)
	}{
		{"missing actor", func(f *fakeGitHub) { f.merged, f.state, f.mergedBy = true, "closed", 0 }},
		{"blank sha", func(f *fakeGitHub) { f.merged, f.state, f.blankSHA, f.mergedBy = true, "closed", true, 123 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mergeTest(t)
			v := m.start()
			requireOK(t, m.j.Unknown(v.ID, "reply_lost", m.at))
			tc.set(m.f)
			requireOK(t, m.h.Reconcile(t.Context(), m.j, v.ID))
			m.want("failed", "read_back", "merged_otherwise", 0)
		})
	}
}

func TestMergeReadAgainIsNotDelayedByBackoff(t *testing.T) {
	m := mergeTest(t)
	v := m.start()
	requireOK(t, m.j.Unknown(v.ID, "reply_lost", m.at))
	m.f.failUser = true
	requireOK(t, m.h.Reconcile(t.Context(), m.j, v.ID))
	m.at = m.at.Add(time.Second)
	if m.h.Due(mustView(t, m.j, v.ID, m.at)) {
		t.Fatal("automatic read-back did not wait")
	}
	before := m.f.userCalls
	requireOK(t, m.h.Reconcile(t.Context(), m.j, v.ID))
	if m.f.userCalls != before+1 {
		t.Fatal("Read again waited for automatic backoff")
	}
}

func TestStuckMergeBacksOffSoAnotherUnknownCanRead(t *testing.T) {
	m := mergeTest(t)
	m.h.recovery = newBudget(60, 2)
	v := m.start()
	requireOK(t, m.j.Unknown(v.ID, "reply_lost", m.at))
	m.f.failUser = true
	start, before := m.at, m.f.userCalls
	var got []int
	for minute := 0; minute <= 60; minute++ {
		m.at = start.Add(time.Duration(minute) * time.Minute)
		if m.h.Due(mustView(t, m.j, v.ID, m.at)) {
			got = append(got, minute)
			requireOK(t, m.h.Reconcile(t.Context(), m.j, v.ID))
		}
	}
	want := []int{0, 1, 3, 7, 15, 30, 45, 60}
	if len(got) != len(want) {
		t.Fatalf("read-back minutes %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("read-back minutes %v, want %v", got, want)
		}
	}
	if m.f.userCalls-before != len(want) {
		t.Fatalf("identity checks = %d, want %d", m.f.userCalls-before, len(want))
	}
	other := m.unknownNumber(141)
	m.f.failUser = false
	requireOK(t, m.h.Reconcile(t.Context(), m.j, other))
	if m.f.otherPulls != 1 {
		t.Fatal("second unknown merge was not read; the stuck merge starved recovery")
	}
	gotView := mustView(t, m.j, other, m.at)
	if gotView.Status != "executed" || gotView.Outcome == nil || gotView.Outcome.ConfirmedBy != "read_back" {
		t.Fatalf("second merge did not settle: %s %+v", gotView.Status, gotView.Outcome)
	}
}

func (m *mergeFixture) unknownNumber(number int64) string {
	m.t.Helper()
	p, err := m.j.Add(approval.Request{Type: approval.Merge, Agent: "fixture", Reason: "Other fixture merge.", Params: approval.Object{
		"repo": "CrossTerra/signal", "number": number, "base": "main", "head": fixtureHead, "method": "squash",
	}}, approval.PeerLabel{}, m.at)
	requireOK(m.t, err)
	f, err := approval.Freeze(p, approval.Binding{Origin: githubHost, Account: "github:user:123", Destination: approval.Object{
		"repository_id": int64(456), "pr": number, "base": "main",
	}}, "")
	requireOK(m.t, err)
	requireOK(m.t, m.j.ApproveAll([]approval.Frozen{f}, []byte("fixture-credential"), nil, m.at))
	_, err = m.j.StartAsserted(p.ID, m.at)
	requireOK(m.t, err)
	requireOK(m.t, m.j.Unknown(p.ID, "reply_lost", m.at))
	return p.ID
}

func mustView(t *testing.T, j *approval.Journal, id string, at time.Time) approval.View {
	t.Helper()
	v, _, err := j.Get(id, at)
	requireOK(t, err)
	return v
}

func TestMergeAbortBeforeClaimDoesNotLookLikeALostWrite(t *testing.T) {
	t.Run("cancelled after final reads", func(t *testing.T) {
		m := mergeTest(t)
		v := m.start()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		m.f.afterPull = func(f *fakeGitHub) {
			if f.pulls == 3 {
				cancel()
			}
		}
		runToken(t, m, v, ctx)
		m.want("failed", "pre_dispatch", "stopped", 0)
	})
	t.Run("stopping before reads", func(t *testing.T) {
		m := mergeTest(t)
		v := m.start()
		requireOK(t, m.j.Shutdown(m.at))
		runToken(t, m, v, t.Context())
		m.want("failed", "pre_dispatch", "stopped", 0)
	})
	t.Run("unhealthy journal", func(t *testing.T) {
		m := mergeTest(t)
		v := m.start()
		dir := m.h.dir
		requireOK(t, m.j.Close())
		token, err := m.h.Unlock(v.Frozen, make([]byte, 32))
		requireOK(t, err)
		defer token.Clear()
		if err := m.h.Run(t.Context(), m.j, v, token); !errors.Is(err, approval.ErrTransition) {
			t.Fatal(err)
		}
		j, err := approval.OpenJournal(dir, m.at, "")
		requireOK(t, err)
		defer j.Close()
		got := mustView(t, j, v.ID, m.at)
		if got.Status != "outcome_unknown" || got.Outcome != nil {
			t.Fatal("unhealthy abort recorded a result or lost the start", got.Status)
		}
	})
}

func runToken(t *testing.T, m *mergeFixture, v approval.View, ctx context.Context) {
	t.Helper()
	token, err := m.h.Unlock(v.Frozen, make([]byte, 32))
	requireOK(t, err)
	defer token.Clear()
	requireOK(t, m.h.Run(ctx, m.j, v, token))
}
