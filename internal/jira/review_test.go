package jira

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLookbackAndServerCursor(t *testing.T) {
	const date = "Sun, 08 Nov 2026 12:00:00 GMT"
	var jql string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", date)
		switch r.URL.Path {
		case "/rest/api/3/myself":
			io.WriteString(w, `{"accountId":"acc-winton","timeZone":"America/Los_Angeles"}`)
		case "/rest/api/3/search/jql":
			body, _ := io.ReadAll(r.Body)
			jql = string(body)
			io.WriteString(w, `{"issues":[{"key":"ENG-1"}],"isLast":true}`)
		case "/rest/api/3/issue/ENG-1":
			io.WriteString(w, `{"key":"ENG-1","fields":{"summary":"Late","status":{"name":"In Progress"},"project":{"key":"ENG"}}}`)
		case "/rest/api/3/issue/ENG-1/changelog":
			io.WriteString(w, `{"isLast":true,"values":[{"id":"9","created":"2026-11-08T11:50:00.000+0000","author":{"accountId":"acc-dana"},"items":[{"field":"status","fromString":"To Do","toString":"In Progress"}]}]}`)
		case "/rest/api/3/issue/ENG-1/comment":
			io.WriteString(w, `{"comments":[],"total":0}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv)
	s.now = func() time.Time { return time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC) }
	got, err := s.Collect(context.Background(), []byte("2026-11-08T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	var search struct {
		JQL string `json:"jql"`
	}
	if err := json.Unmarshal([]byte(jql), &search); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(search.JQL, `updated >= "2026-11-08 03:45"`) {
		t.Fatalf("jql did not look back from the cursor: %s", search.JQL)
	}
	if string(got.Next) != "2026-11-08T12:00:00Z" {
		t.Fatalf("cursor used the local clock: %s", got.Next)
	}
	found := false
	for _, it := range got.Items {
		if it.Type == TypeTransitioned && it.OccurredAt.Equal(time.Date(2026, 11, 8, 11, 50, 0, 0, time.UTC)) {
			found = true
		}
	}
	if !found {
		t.Fatal("event inside the lookback was dropped")
	}
}

// The JQL text clause is not verified against live ADF. A mention node in a fetched comment is addressed even
// when the comment's plain text does not contain the account id.
func TestMovedIssueIsDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		switch r.URL.Path {
		case "/rest/api/3/myself":
			io.WriteString(w, `{"accountId":"acc-winton","timeZone":"UTC"}`)
		case "/rest/api/3/search/jql":
			io.WriteString(w, `{"issues":[{"key":"ENG-1"}],"isLast":true}`)
		case "/rest/api/3/issue/ENG-1":
			io.WriteString(w, `{"key":"OTHER-1","fields":{"summary":"Moved","status":{"name":"Open"},"project":{"key":"OTHER"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv)
	got, err := s.Collect(context.Background(), []byte("2026-11-08T00:00:00Z"))
	if err != nil || got.Next == nil || got.Dropped == 0 {
		t.Fatalf("moved issue failed the pass: %v next %s dropped %d", err, got.Next, got.Dropped)
	}
	for _, it := range got.Items {
		if strings.Contains(it.Key, "OTHER") {
			t.Fatal("out-of-scope issue was yielded")
		}
	}
}

func TestFetchedADFMentionIsAddressed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		switch r.URL.Path {
		case "/rest/api/3/myself":
			io.WriteString(w, `{"accountId":"acc-winton","timeZone":"UTC"}`)
		case "/rest/api/3/search/jql":
			io.WriteString(w, `{"issues":[{"key":"ENG-1"}],"isLast":true}`)
		case "/rest/api/3/issue/ENG-1":
			io.WriteString(w, `{"key":"ENG-1","fields":{"summary":"Mention","status":{"name":"Open"},"assignee":{"accountId":"acc-winton","displayName":"Winton"},"project":{"key":"ENG"}}}`)
		case "/rest/api/3/issue/ENG-1/changelog":
			io.WriteString(w, `{"isLast":true,"values":[]}`)
		case "/rest/api/3/issue/ENG-1/comment":
			io.WriteString(w, `{"comments":[{"id":"9","created":"2026-11-08T11:00:00.000+0000","author":{"accountId":"acc-dana"},"body":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"mention","attrs":{"id":"acc-winton","text":"@Winton"}}]}]}}],"total":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := newTest(t, srv)
	got, err := s.Collect(context.Background(), []byte("2026-11-08T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range got.Items {
		if it.Type != TypeCommented {
			continue
		}
		if strings.Contains(it.Fields["body"], "acc-winton") {
			t.Fatal("the account id was taken from the mention node as text")
		}
		if it.Addressed {
			found = true
		}
	}
	if !found {
		t.Fatal("an ADF mention in a fetched comment was not addressed")
	}
}
