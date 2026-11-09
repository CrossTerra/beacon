package jira

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/source"
)

func TestCollectEveryItem(t *testing.T) {
	var jql string
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "OTHER-1") {
			t.Errorf("request outside projects: %s", r.URL.Path)
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("no authorization")
		}
		switch r.URL.Path {
		case "/rest/api/3/myself":
			io.WriteString(w, `{"accountId":"acc-winton","timeZone":"America/Los_Angeles"}`)
		case "/rest/api/3/search/jql":
			body, _ := io.ReadAll(r.Body)
			jql = string(body)
			io.WriteString(w, `{"issues":[{"key":"ENG-1"},{"key":"OTHER-1"}],"isLast":true}`)
		case "/rest/api/3/issue/ENG-1":
			io.WriteString(w, eng1)
		case "/rest/api/3/issue/ENG-1/changelog":
			io.WriteString(w, changelog)
		case "/rest/api/3/issue/ENG-1/comment":
			io.WriteString(w, comments)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv)
	got, err := s.Collect(context.Background(), []byte("2026-11-07T20:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	var search struct {
		JQL string `json:"jql"`
	}
	if err := json.Unmarshal([]byte(jql), &search); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(search.JQL, `updated >= "2026-11-07 12:00"`) || !strings.Contains(search.JQL, "project in (ENG)") || strings.Contains(search.JQL, "OTHER") {
		t.Fatalf("jql %s", search.JQL)
	}
	byKey := map[string]source.Item{}
	for _, it := range got.Items {
		byKey[it.Key] = it
		if it.Subject != "jira:ENG-1" || !strings.HasPrefix(it.Key, "jira:ENG-1:") {
			t.Fatalf("shape %+v", it)
		}
	}
	want := map[string]string{
		"jira:ENG-1:501":          TypeAssigned,
		"jira:ENG-1:503":          TypeTransitioned,
		"jira:ENG-1:510":          TypeTransitioned,
		"jira:ENG-1:504":          TypeMentioned,
		"jira:ENG-1:505":          TypeDueChanged,
		"jira:ENG-1:508:assignee": TypeUnassigned,
		"jira:ENG-1:508:status":   TypeTransitioned,
		"jira:ENG-1:601":          TypeCommented,
		"jira:ENG-1:602":          TypeCommented,
	}
	if len(byKey) != len(want) {
		var keys []string
		for k, it := range byKey {
			keys = append(keys, k+" "+it.Type)
		}
		t.Fatalf("got %v", keys)
	}
	for k, typ := range want {
		if byKey[k].Type != typ {
			t.Fatalf("%s: %+v", k, byKey[k])
		}
	}
	assigned := byKey["jira:ENG-1:501"]
	if !assigned.Addressed || assigned.Own || assigned.Actor != "jira:account:acc-dana" {
		t.Fatalf("assigned %+v", assigned)
	}
	tr := byKey["jira:ENG-1:503"]
	if !tr.Addressed || tr.Own || tr.Fields["from_status"] != "To Do" || tr.Fields["to_status"] != "In Progress" {
		t.Fatalf("transition %+v", tr)
	}
	own := byKey["jira:ENG-1:510"]
	if !own.Own || own.Addressed {
		t.Fatalf("own transition %+v", own)
	}
	if !byKey["jira:ENG-1:504"].Addressed || byKey["jira:ENG-1:504"].Type != TypeMentioned {
		t.Fatal("mention")
	}
	comment := byKey["jira:ENG-1:601"]
	if !comment.Addressed || comment.Type != TypeCommented || !strings.Contains(comment.Fields["body"], "please look") {
		t.Fatalf("comment %+v", comment)
	}
	if _, ok := byKey["jira:ENG-1:601:mentioned"]; ok {
		t.Fatal("a comment mention also yielded mentioned")
	}
	if byKey["jira:ENG-1:603"].Key != "" {
		t.Fatal("comment before the cursor")
	}
	due := byKey["jira:ENG-1:505"]
	if due.DueAt == nil || due.DueAt.UTC().Format(time.RFC3339) != "2026-11-10T07:59:59Z" || due.State["due"] != "2026-11-09" || due.State["status"] != "In Progress" || due.Fields["url"] != "https://acme.atlassian.net/browse/ENG-1" {
		t.Fatalf("due %+v %v", due.DueAt, due)
	}
	again, err := s.Collect(context.Background(), []byte("2026-11-07T20:00:00Z"))
	if err != nil || len(again.Items) != len(got.Items) {
		t.Fatalf("unstable %v %d", err, len(again.Items))
	}
}

func TestFirstRunAndScope(t *testing.T) {
	var jql string
	var comments bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comment") {
			comments = true
		}
		switch r.URL.Path {
		case "/rest/api/3/myself":
			io.WriteString(w, `{"accountId":"acc-winton","timeZone":"America/Los_Angeles"}`)
		case "/rest/api/3/search/jql":
			body, _ := io.ReadAll(r.Body)
			jql = string(body)
			io.WriteString(w, `{"issues":[{"key":"ENG-3"}],"isLast":true}`)
		case "/rest/api/3/issue/ENG-3":
			io.WriteString(w, strings.Replace(eng1, "ENG-1", "ENG-3", -1))
		case "/rest/api/3/issue/ENG-3/changelog":
			io.WriteString(w, `{"values":[{"id":"900","created":"2026-11-01T00:00:00.000+0000","author":{"accountId":"acc-dana"},"items":[{"field":"assignee","to":"acc-winton","toString":"Winton"}]}],"total":1,"isLast":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv)
	got, err := s.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jql, "resolution = Unresolved") || !strings.Contains(jql, "assignee = currentUser()") || strings.Contains(jql, "updated >=") {
		t.Fatalf("jql %s", jql)
	}
	if comments {
		t.Fatal("first run read comments")
	}
	if len(got.Items) != 1 || got.Items[0].Key != "jira:ENG-3:900" || got.Items[0].Type != TypeAssigned || !got.Items[0].Addressed {
		t.Fatalf("%+v", got.Items)
	}
	sc, err := newScope("acme.atlassian.net", []string{"ENG"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.allow("https://acme.atlassian.net/rest/api/3/issue/ENG-1/changelog"); err != nil {
		t.Fatal(err)
	}
	if err := sc.allow("https://acme.atlassian.net/rest/api/3/issue/OTHER-1"); err == nil {
		t.Fatal("other project allowed")
	}
	if err := sc.allow("https://evil.example/rest/api/3/myself"); err == nil {
		t.Fatal("other host allowed")
	}
	if s.Hosts()[0] != "acme.atlassian.net:443" {
		t.Fatal(s.Hosts())
	}
}

func TestRateLimitAndPagination(t *testing.T) {
	var calls int
	var slept time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/3/myself":
			io.WriteString(w, `{"accountId":"acc-winton","timeZone":"UTC"}`)
		case "/rest/api/3/search/jql":
			calls++
			if calls == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if calls == 2 {
				io.WriteString(w, `{"issues":[{"key":"ENG-1"}],"nextPageToken":"p2","isLast":false}`)
				return
			}
			io.WriteString(w, `{"issues":[{"key":"ENG-2"}],"isLast":true}`)
		case "/rest/api/3/issue/ENG-1", "/rest/api/3/issue/ENG-2":
			key := "ENG-1"
			if strings.HasSuffix(r.URL.Path, "ENG-2") {
				key = "ENG-2"
			}
			io.WriteString(w, strings.Replace(eng1, "ENG-1", key, -1))
		case "/rest/api/3/issue/ENG-1/changelog", "/rest/api/3/issue/ENG-2/changelog":
			io.WriteString(w, `{"values":[],"total":0,"isLast":true}`)
		case "/rest/api/3/issue/ENG-1/comment", "/rest/api/3/issue/ENG-2/comment":
			io.WriteString(w, `{"comments":[],"total":0}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv)
	s.http.Sleep = func(_ context.Context, d time.Duration) error {
		slept = d
		return nil
	}
	got, err := s.Collect(context.Background(), []byte("2026-11-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if slept != time.Second || len(got.Items) != 0 {
		t.Fatalf("slept %s items %d", slept, len(got.Items))
	}
	if calls < 3 {
		t.Fatalf("pages %d", calls)
	}
}

func TestLongRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	s := newTest(t, srv)
	slept := false
	s.http.Sleep = func(context.Context, time.Duration) error {
		slept = true
		return nil
	}
	_, err := s.Collect(context.Background(), nil)
	if !errors.Is(err, source.ErrRateLimited) || slept {
		t.Fatalf("%v slept %v", err, slept)
	}
}

func TestGuard(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	s := newTest(t, srv)
	s.SetHTTP(&http.Client{Transport: egress.Guard([]string{"acme.atlassian.net:443"}, srv.Client().Transport), Timeout: 5 * time.Second})
	if _, err := s.Collect(context.Background(), nil); err == nil || hit {
		t.Fatalf("err %v hit %v", err, hit)
	}
}

func newTest(t *testing.T, srv *httptest.Server) *Source {
	t.Helper()
	s, err := New(Options{
		Site: "acme", Projects: []string{"ENG"}, Base: srv.URL,
		Creds: func() (keychain.Secret, error) { return keychain.Pair("a@b.c", "jira-token") },
		HTTP:  srv.Client(),
		Now:   func() time.Time { return time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const eng1 = `{
  "key": "ENG-1",
  "fields": {
    "summary": "Fix the gate",
    "status": {"name": "In Progress"},
    "assignee": {"accountId": "acc-winton", "displayName": "Winton"},
    "priority": {"name": "High"},
    "issuetype": {"name": "Task"},
    "duedate": "2026-11-09",
    "project": {"key": "ENG"}
  }
}`

const changelog = `{
  "isLast": true,
  "total": 6,
  "values": [
    {"id": "501", "created": "2026-11-07T21:00:00.000+0000", "author": {"accountId": "acc-dana"}, "items": [{"field": "assignee", "from": "", "to": "acc-winton", "toString": "Winton"}]},
    {"id": "503", "created": "2026-11-07T22:00:00.000+0000", "author": {"accountId": "acc-dana"}, "items": [{"field": "status", "fromString": "To Do", "toString": "In Progress"}]},
    {"id": "510", "created": "2026-11-07T22:10:00.000+0000", "author": {"accountId": "acc-winton"}, "items": [{"field": "status", "fromString": "In Progress", "toString": "In Review"}]},
    {"id": "504", "created": "2026-11-07T23:00:00.000+0000", "author": {"accountId": "acc-dana"}, "items": [{"field": "description", "fromString": "old", "toString": "see [~accountid:acc-winton]"}]},
    {"id": "505", "created": "2026-11-07T23:30:00.000+0000", "author": {"accountId": "acc-dana"}, "items": [{"field": "duedate", "fromString": "", "toString": "2026-11-09"}]},
    {"id": "508", "created": "2026-11-07T23:40:00.000+0000", "author": {"accountId": "acc-dana"}, "items": [{"field": "assignee", "from": "acc-winton", "to": "", "toString": ""}, {"field": "status", "fromString": "In Review", "toString": "Done"}]}
  ]
}`

const comments = `{
  "total": 3,
  "comments": [
    {"id": "603", "created": "2026-11-01T00:00:00.000+0000", "author": {"accountId": "acc-dana"}, "body": {"type": "doc", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "old"}]}]}},
    {"id": "601", "created": "2026-11-07T23:50:00.000+0000", "author": {"accountId": "acc-dana"}, "body": {"type": "doc", "content": [{"type": "paragraph", "content": [{"type": "mention", "attrs": {"id": "acc-winton", "text": "@Winton"}}, {"type": "text", "text": " please look"}]}]}},
    {"id": "602", "created": "2026-11-07T23:55:00.000+0000", "author": {"accountId": "acc-winton"}, "body": {"type": "doc", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "done"}]}]}}
  ]
}`
