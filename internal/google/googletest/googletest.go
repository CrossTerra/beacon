// Package googletest is a fake Google for tests: the token endpoint, and recorded Gmail, Calendar and Drive
// responses for one mailbox, winton@example.com, at 2026-11-08T12:00:00Z.
package googletest

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Fixed values the fixtures use.
const (
	Date         = "Sun, 08 Nov 2026 12:00:00 GMT"
	Email        = "winton@example.com"
	ClientID     = "client-123.apps.googleusercontent.com"
	ClientSecret = "GOCSPX-client-secret-value"
	Refresh      = "1//refresh-token-value"
	Access       = "ya29.access-token-value"
	// ClientFile is a Desktop app client file as Google Cloud Console downloads it.
	ClientFile = `{"installed":{"client_id":"` + ClientID + `","project_id":"p","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token","client_secret":"` + ClientSecret + `","redirect_uris":["http://localhost"]}}`
	// AllScopes is every scope the three sources need.
	AllScopes = "https://www.googleapis.com/auth/calendar.events.readonly https://www.googleapis.com/auth/drive.readonly https://www.googleapis.com/auth/gmail.metadata"
)

// Server is a fake Google. Every handler can be replaced through Routes before the first request.
type Server struct {
	*httptest.Server
	t testing.TB

	mu sync.Mutex
	// Scope is the scope the token endpoint grants.
	Scope string
	// Routes maps a path to its handler.
	Routes   map[string]http.HandlerFunc
	requests []*http.Request
	// Challenge is the PKCE challenge a code grant's verifier must hash to. Empty skips the check.
	Challenge string
	// Code is the authorisation code a code grant must carry.
	Code string
}

// New starts a fake Google with the recorded responses.
func New(t testing.TB) *Server {
	s := &Server{t: t, Scope: AllScopes, Code: "auth-code-1"}
	s.Routes = map[string]http.HandlerFunc{
		"/token":                                s.token,
		"/gmail/v1/users/me/profile":            text(`{"emailAddress":"` + Email + `","historyId":"300"}`),
		"/gmail/v1/users/me/labels":             text(Labels),
		"/gmail/v1/users/me/history":            s.history,
		"/gmail/v1/users/me/threads":            text(InboxThreads),
		"/gmail/v1/users/me/messages":           text(Starred),
		"/gmail/v1/users/me/threads/t1":         text(ThreadT1),
		"/gmail/v1/users/me/threads/t2":         text(ThreadT2),
		"/gmail/v1/users/me/threads/t3":         text(ThreadT3),
		"/gmail/v1/users/me/threads/t4":         text(ThreadT4),
		"/gmail/v1/users/me/threads/t9":         text(ThreadT9),
		"/gmail/v1/users/me/messages/m1":        text(FullM1),
		"/gmail/v1/users/me/messages/m2":        text(`{"id":"m2","payload":{"mimeType":"text/plain","body":{"data":"T24gaXQu"}}}`),
		"/gmail/v1/users/me/messages/m3":        text(`{"id":"m3","payload":{"mimeType":"text/html","body":{"data":"PGI-aGk8L2I-"}}}`),
		"/gmail/v1/users/me/messages/m5":        text(`{"id":"m5","payload":{"mimeType":"text/plain","body":{"data":"U2FsZQ=="}}}`),
		"/calendar/v3/calendars/primary/events": s.events,
		"/drive/v3/about":                       text(`{"user":{"emailAddress":"` + Email + `"}}`),
		"/drive/v3/files":                       text(Files),
		"/drive/v3/files/f1/comments":           text(CommentsF1),
		"/drive/v3/files/n1":                    text(NotesN1),
		"/drive/v3/files/n1/export":             text("Weekly sync\nDecisions: ship on Friday.\n"),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func text(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Date", Date)
	s.mu.Lock()
	s.requests = append(s.requests, r.Clone(r.Context()))
	h := s.Routes[r.URL.Path]
	s.mu.Unlock()
	if r.URL.Path != "/token" && r.Header.Get("Authorization") != "Bearer "+Access {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if h == nil {
		s.t.Errorf("googletest: unexpected request %s", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

// Requests returns every request made so far.
func (s *Server) Requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

// Called reports whether a path was requested.
func (s *Server) Called(path string) bool {
	for _, r := range s.Requests() {
		if r.URL.Path == path {
			return true
		}
	}
	return false
}

// Query returns the query of the last request to path.
func (s *Server) Query(path string) url.Values {
	var q url.Values
	for _, r := range s.Requests() {
		if r.URL.Path == path {
			q = r.URL.Query()
		}
	}
	return q
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("client_id") != ClientID || r.PostForm.Get("client_secret") != ClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid_client"}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.PostForm.Get("grant_type") {
	case "refresh_token":
		if r.PostForm.Get("refresh_token") != Refresh {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		io.WriteString(w, `{"access_token":"`+Access+`","expires_in":3599,"scope":"`+s.Scope+`","token_type":"Bearer"}`)
	case "authorization_code":
		v := r.PostForm.Get("code_verifier")
		sum := sha256.Sum256([]byte(v))
		if r.PostForm.Get("code") != s.Code || v == "" || (s.Challenge != "" && base64.RawURLEncoding.EncodeToString(sum[:]) != s.Challenge) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		io.WriteString(w, `{"access_token":"`+Access+`","refresh_token":"`+Refresh+`","expires_in":3599,"scope":"`+s.Scope+`","token_type":"Bearer"}`)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("pageToken") == "p2" {
		io.WriteString(w, HistoryPage2)
		return
	}
	io.WriteString(w, HistoryPage1)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case strings.Contains(q.Get("fields"), "attachments"):
		io.WriteString(w, NotesEvents)
	case q.Get("updatedMin") != "":
		io.WriteString(w, EventsChanged)
	default:
		io.WriteString(w, EventsFirst)
	}
}

// Gmail. Times: m1 10:00, m2 10:30, m3 11:00, m5 11:30 on 2026-11-08; m9 2026-10-01.
const (
	Labels = `{"labels":[{"id":"INBOX","name":"INBOX"},{"id":"SENT","name":"SENT"},{"id":"STARRED","name":"STARRED"},{"id":"UNREAD","name":"UNREAD"},{"id":"DRAFT","name":"DRAFT"},{"id":"CATEGORY_PROMOTIONS","name":"CATEGORY_PROMOTIONS"},{"id":"Label_1","name":"Projects"}]}`

	HistoryPage1 = `{"history":[
  {"id":"201","messagesAdded":[{"message":{"id":"m1","threadId":"t1","labelIds":["INBOX","UNREAD"]}}]},
  {"id":"202","messagesAdded":[{"message":{"id":"m2","threadId":"t1","labelIds":["SENT"]}}]}
],"nextPageToken":"p2","historyId":"340"}`

	HistoryPage2 = `{"history":[
  {"id":"203","messagesAdded":[{"message":{"id":"m3","threadId":"t2","labelIds":["INBOX","UNREAD"]}}]},
  {"id":"204","messagesAdded":[{"message":{"id":"m4","threadId":"t3","labelIds":["DRAFT"]}}]},
  {"id":"205","messagesAdded":[{"message":{"id":"m5","threadId":"t4","labelIds":["INBOX","CATEGORY_PROMOTIONS","UNREAD"]}}]},
  {"id":"206","labelsAdded":[{"message":{"id":"m1","threadId":"t1","labelIds":["INBOX","STARRED"]},"labelIds":["STARRED"]}]},
  {"id":"207","labelsAdded":[{"message":{"id":"m3","threadId":"t2","labelIds":["INBOX","Label_1"]},"labelIds":["Label_1"]}]}
],"historyId":"350"}`

	ThreadT1 = `{"id":"t1","messages":[
  {"id":"m1","threadId":"t1","labelIds":["INBOX","UNREAD","STARRED","Label_1"],"internalDate":"1794132000000","payload":{"headers":[
    {"name":"From","value":"Dana Lee <dana@example.com>"},{"name":"To","value":"Winton <winton@example.com>, ops@example.com"},{"name":"Subject","value":"Budget review"}]}},
  {"id":"m2","threadId":"t1","labelIds":["SENT"],"internalDate":"1794133800000","payload":{"headers":[
    {"name":"From","value":"Winton <winton@example.com>"},{"name":"To","value":"dana@example.com"},{"name":"Subject","value":"Re: Budget review"}]}}
]}`

	ThreadT2 = `{"id":"t2","messages":[
  {"id":"m3","threadId":"t2","labelIds":["INBOX","UNREAD","Label_1"],"internalDate":"1794135600000","payload":{"headers":[
    {"name":"From","value":"bob@example.com"},{"name":"To","value":"team@example.com"},{"name":"Subject","value":"FYI"}]}}
]}`

	ThreadT3 = `{"id":"t3","messages":[
  {"id":"m4","threadId":"t3","labelIds":["DRAFT"],"internalDate":"1794135600000","payload":{"headers":[
    {"name":"From","value":"winton@example.com"},{"name":"Subject","value":"draft"}]}}
]}`

	ThreadT4 = `{"id":"t4","messages":[
  {"id":"m5","threadId":"t4","labelIds":["INBOX","CATEGORY_PROMOTIONS","UNREAD"],"internalDate":"1794137400000","payload":{"headers":[
    {"name":"From","value":"Deals <deals@shop.example>"},{"name":"To","value":"winton@example.com"},{"name":"Subject","value":"Sale ends today"}]}}
]}`

	ThreadT9 = `{"id":"t9","messages":[
  {"id":"m9","threadId":"t9","labelIds":["INBOX"],"internalDate":"1790845200000","payload":{"headers":[
    {"name":"From","value":"old@example.com"},{"name":"To","value":"winton@example.com"},{"name":"Subject","value":"Old"}]}}
]}`

	InboxThreads = `{"threads":[{"id":"t4"},{"id":"t2"},{"id":"t1"},{"id":"t9"}]}`
	Starred      = `{"messages":[{"id":"m1","threadId":"t1"}]}`

	// FullM1's text/plain part is "Can you review the budget by Friday?".
	FullM1 = `{"id":"m1","payload":{"mimeType":"multipart/alternative","parts":[{"mimeType":"text/plain","body":{"data":"Q2FuIHlvdSByZXZpZXcgdGhlIGJ1ZGdldCBieSBGcmlkYXk_"}},{"mimeType":"text/html","body":{"data":"PGI-aGk8L2I-"}}]}}`
)

// Calendar. The calendar is in America/Los_Angeles. ev1 needs a response; ev2 is the person's; ev3 is beyond the
// first run's 7 days; ev4 is all day.
const (
	EventsFirst = `{"timeZone":"America/Los_Angeles","items":[
  {"id":"ev1","status":"confirmed","etag":"\"3001\"","summary":"Design review","location":"Room 1","start":{"dateTime":"2026-11-09T17:00:00Z"},"end":{"dateTime":"2026-11-09T18:00:00Z"},
   "created":"2026-11-01T00:00:00Z","updated":"2026-11-01T00:00:00Z","htmlLink":"https://www.google.com/calendar/event?eid=ev1",
   "organizer":{"email":"dana@example.com"},"attendees":[{"email":"dana@example.com","responseStatus":"accepted"},{"email":"winton@example.com","self":true,"responseStatus":"needsAction"},{"email":"bob@example.com","responseStatus":"accepted"}]},
  {"id":"ev2","status":"confirmed","etag":"\"3002\"","summary":"1:1","start":{"dateTime":"2026-11-10T09:00:00Z"},"end":{"dateTime":"2026-11-10T09:30:00Z"},
   "created":"2026-11-02T00:00:00Z","updated":"2026-11-02T00:00:00Z","htmlLink":"https://www.google.com/calendar/event?eid=ev2",
   "organizer":{"email":"winton@example.com","self":true},"attendees":[{"email":"winton@example.com","self":true,"responseStatus":"accepted"},{"email":"carol@example.com","responseStatus":"needsAction"}]},
  {"id":"ev3","status":"confirmed","etag":"\"3003\"","summary":"Quarterly","start":{"dateTime":"2026-11-25T09:00:00Z"},"end":{"dateTime":"2026-11-25T10:00:00Z"},
   "created":"2026-10-01T00:00:00Z","updated":"2026-10-01T00:00:00Z","htmlLink":"https://www.google.com/calendar/event?eid=ev3",
   "organizer":{"email":"dana@example.com"},"attendees":[{"email":"winton@example.com","self":true,"responseStatus":"accepted"},{"email":"bob@example.com","responseStatus":"needsAction"}]},
  {"id":"ev4","status":"confirmed","etag":"\"3004\"","summary":"Offsite","start":{"date":"2026-11-12"},"end":{"date":"2026-11-13"},
   "created":"2026-11-03T00:00:00Z","updated":"2026-11-03T00:00:00Z","htmlLink":"https://www.google.com/calendar/event?eid=ev4",
   "organizer":{"email":"winton@example.com","self":true}}
]}`

	EventsChanged = `{"timeZone":"America/Los_Angeles","items":[
  {"id":"ev1","status":"confirmed","etag":"\"3101\"","summary":"Design review","location":"Room 1","start":{"dateTime":"2026-11-09T17:00:00Z"},"end":{"dateTime":"2026-11-09T18:00:00Z"},
   "created":"2026-11-01T00:00:00Z","updated":"2026-11-08T12:10:00Z","htmlLink":"https://www.google.com/calendar/event?eid=ev1",
   "organizer":{"email":"dana@example.com"},"attendees":[{"email":"dana@example.com","responseStatus":"accepted"},{"email":"winton@example.com","self":true,"responseStatus":"accepted"},{"email":"bob@example.com","responseStatus":"accepted"}]},
  {"id":"ev2","status":"confirmed","etag":"\"3102\"","summary":"1:1","start":{"dateTime":"2026-11-10T10:00:00Z"},"end":{"dateTime":"2026-11-10T10:30:00Z"},
   "created":"2026-11-02T00:00:00Z","updated":"2026-11-08T12:11:00Z","htmlLink":"https://www.google.com/calendar/event?eid=ev2",
   "organizer":{"email":"winton@example.com","self":true},"attendees":[{"email":"winton@example.com","self":true,"responseStatus":"accepted"},{"email":"carol@example.com","responseStatus":"needsAction"}]},
  {"id":"ev3","status":"confirmed","etag":"\"3103\"","summary":"Quarterly","start":{"dateTime":"2026-11-25T09:00:00Z"},"end":{"dateTime":"2026-11-25T10:00:00Z"},
   "created":"2026-10-01T00:00:00Z","updated":"2026-11-08T12:12:00Z","htmlLink":"https://www.google.com/calendar/event?eid=ev3",
   "organizer":{"email":"dana@example.com"},"attendees":[{"email":"winton@example.com","self":true,"responseStatus":"accepted"},{"email":"bob@example.com","responseStatus":"declined"}]},
  {"id":"ev4","status":"cancelled","etag":"\"3104\"","summary":"Offsite","start":{"date":"2026-11-12"},"end":{"date":"2026-11-13"},
   "created":"2026-11-03T00:00:00Z","updated":"2026-11-08T12:13:00Z","htmlLink":"https://www.google.com/calendar/event?eid=ev4",
   "organizer":{"email":"winton@example.com","self":true}},
  {"id":"ev5","status":"confirmed","etag":"\"3105\"","summary":"Lunch","start":{"dateTime":"2026-11-11T12:00:00Z"},"end":{"dateTime":"2026-11-11T13:00:00Z"},
   "created":"2026-11-08T12:20:00Z","updated":"2026-11-08T12:20:00Z","htmlLink":"https://www.google.com/calendar/event?eid=ev5",
   "organizer":{"email":"dana@example.com"},"attendees":[{"email":"winton@example.com","self":true,"responseStatus":"accepted"}]},
  {"id":"ev6","status":"confirmed","etag":"\"3106\"","summary":"Hiring sync","description":"Agenda: candidates","start":{"dateTime":"2026-11-12T15:00:00Z"},"end":{"dateTime":"2026-11-12T15:30:00Z"},
   "created":"2026-11-08T12:25:00Z","updated":"2026-11-08T12:25:00Z","htmlLink":"https://www.google.com/calendar/event?eid=ev6",
   "organizer":{"email":"erin@example.com"},"attendees":[{"email":"erin@example.com","responseStatus":"accepted"},{"email":"winton@example.com","self":true,"responseStatus":"needsAction"}]}
]}`
)

// Drive. f1 is a Doc with comments; n1 is meeting notes for evA, which the person accepted.
const (
	Files = `{"files":[{"id":"f1","name":"Q4 plan","mimeType":"application/vnd.google-apps.document","webViewLink":"https://docs.google.com/document/d/f1/edit?usp=drivesdk"}]}`

	CommentsF1 = `{"comments":[
  {"id":"c1","createdTime":"2026-11-08T11:00:00Z","author":{"displayName":"Dana Lee","me":false},"mentionedEmailAddresses":["winton@example.com"],"resolved":true,"content":"@winton please check the numbers",
   "replies":[
     {"id":"r1","createdTime":"2026-11-08T11:30:00Z","author":{"displayName":"Winton","me":true},"action":"resolve","content":"Fixed"},
     {"id":"r2","createdTime":"2026-11-08T11:40:00Z","author":{"displayName":"Bob","me":false},"mentionedEmailAddresses":["winton@example.com"],"content":"+winton also row 7"}
   ]},
  {"id":"c2","createdTime":"2026-11-08T11:10:00Z","author":{"displayName":"Dana Lee","me":false},"mentionedEmailAddresses":["winton@example.com"],"assigneeEmailAddress":"winton@example.com","resolved":false,"content":"Assign: update the forecast"},
  {"id":"c3","createdTime":"2026-11-08T11:20:00Z","author":{"displayName":"Winton","me":true},"resolved":false,"content":"Note to self"},
  {"id":"c4","createdTime":"2026-11-08T11:15:00Z","author":{"displayName":"Dana Lee","me":false},"resolved":false,"content":"typo"},
  {"id":"c5","createdTime":"2026-11-01T11:15:00Z","author":{"displayName":"Dana Lee","me":false},"mentionedEmailAddresses":["winton@example.com"],"resolved":false,"content":"old"}
]}`

	NotesEvents = `{"timeZone":"America/Los_Angeles","items":[
  {"id":"evA","status":"confirmed","summary":"Weekly sync","start":{"dateTime":"2026-11-07T15:00:00Z"},"end":{"dateTime":"2026-11-07T16:00:00Z"},"updated":"2026-11-08T11:00:00Z",
   "organizer":{"self":false},"attendees":[{"self":true,"responseStatus":"accepted"}],
   "attachments":[{"fileId":"n1","title":"Weekly sync - 2026/11/07 15:00 GMT - Notes by Gemini","mimeType":"application/vnd.google-apps.document"}]},
  {"id":"evB","status":"confirmed","summary":"Skipped","start":{"dateTime":"2026-11-07T17:00:00Z"},"end":{"dateTime":"2026-11-07T18:00:00Z"},"updated":"2026-11-08T11:00:00Z",
   "organizer":{"self":false},"attendees":[{"self":true,"responseStatus":"declined"}],
   "attachments":[{"fileId":"n2","title":"Skipped - Notes by Gemini","mimeType":"application/vnd.google-apps.document"}]},
  {"id":"evC","status":"confirmed","summary":"Planning","start":{"dateTime":"2026-11-07T19:00:00Z"},"end":{"dateTime":"2026-11-07T20:00:00Z"},"updated":"2026-11-08T11:00:00Z",
   "organizer":{"self":true},
   "attachments":[{"fileId":"n3","title":"Planning slides","mimeType":"application/vnd.google-apps.presentation"}]},
  {"id":"evD","status":"confirmed","summary":"Later today","start":{"dateTime":"2026-11-08T15:00:00Z"},"end":{"dateTime":"2026-11-08T16:00:00Z"},"updated":"2026-11-08T11:00:00Z",
   "organizer":{"self":true},
   "attachments":[{"fileId":"n4","title":"Later today - Notes by Gemini","mimeType":"application/vnd.google-apps.document"}]}
]}`

	NotesN1 = `{"id":"n1","name":"Weekly sync - 2026/11/07 15:00 GMT - Notes by Gemini","mimeType":"application/vnd.google-apps.document","webViewLink":"https://docs.google.com/document/d/n1/edit","createdTime":"2026-11-07T16:05:00Z"}`
)
