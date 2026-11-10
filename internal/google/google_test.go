package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/google/googletest"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/source"
)

var testNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func newAuth(t *testing.T, srv *googletest.Server) *Auth {
	t.Helper()
	a, err := NewAuth(AuthOptions{
		TokenURL: srv.URL + "/token",
		Client:   func() (keychain.Secret, error) { return keychain.OAuthClient([]byte(googletest.ClientFile)) },
		Refresh:  func() (keychain.Secret, error) { return keychain.FromString(googletest.Refresh) },
		HTTP:     srv.Client(),
		Now:      func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func opts(t *testing.T, srv *googletest.Server) Options {
	return Options{
		Person: "winton", Account: googletest.Email, Identity: "email:" + googletest.Email, Auth: newAuth(t, srv), Base: srv.URL,
		HTTP: srv.Client(), Now: func() time.Time { return testNow },
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
}

func byKey(t *testing.T, items []source.Item, fields, state []string) map[string]source.Item {
	t.Helper()
	out := map[string]source.Item{}
	for _, it := range items {
		if err := source.Validate(it, fields, state); err != nil {
			t.Fatalf("%s: %v", it.Key, err)
		}
		if _, dup := out[it.Key]; dup {
			t.Fatalf("duplicate key %s", it.Key)
		}
		out[it.Key] = it
	}
	return out
}

func keys(m map[string]source.Item) []string {
	var out []string
	for k, it := range m {
		out = append(out, k+" "+it.Type)
	}
	sort.Strings(out)
	return out
}

// history is the history id in a Gmail cursor.
func history(t *testing.T, next []byte) string {
	t.Helper()
	c, err := parseGmailCursor(next)
	if err != nil {
		t.Fatalf("cursor %q: %v", next, err)
	}
	return c.HistoryID
}

func send(t *testing.T, yaml string) *rules.Rule {
	t.Helper()
	f, err := rules.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return f.Rules[0]
}

func TestGmailEveryItem(t *testing.T) {
	srv := googletest.New(t)
	g, err := NewGmail(opts(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	g.SetBodies(true) // the grant is gmail.metadata, so no body is read
	got, err := g.Collect(context.Background(), []byte("100"))
	if err != nil {
		t.Fatal(err)
	}
	if history(t, got.Next) != "350" || !strings.Contains(string(got.Next), `"at":"2026-11-08T12:00:00Z"`) || got.Gap {
		t.Fatalf("next %q", got.Next)
	}
	if q := srv.Query("/gmail/v1/users/me/history"); q.Get("startHistoryId") != "100" {
		t.Fatalf("history query %v", q)
	}
	m := byKey(t, got.Items, GmailFields, GmailStateFields)
	want := map[string]string{
		"gmail:t1:m1":         TypeReceived,
		"gmail:t1:m2":         TypeSent,
		"gmail:t2:m3":         TypeReceived,
		"gmail:t4:m5":         TypeReceived,
		"gmail:t5:m6":         TypeReceived,
		"gmail:t1:starred:m1": TypeStarred,
	}
	if len(m) != len(want) {
		t.Fatalf("got %v", keys(m))
	}
	titles := map[string]string{
		"gmail:t1:m1": "New message", "gmail:t1:m2": "Replied", "gmail:t2:m3": "New message",
		"gmail:t4:m5": "New message", "gmail:t5:m6": "New message", "gmail:t1:starred:m1": "Starred",
	}
	for k, typ := range want {
		it := m[k]
		if it.Type != typ || it.EventTitle != titles[k] {
			t.Fatalf("%s: %s %q", k, it.Type, it.EventTitle)
		}
		if strings.Contains(it.EventTitle, it.Fields["subject"]) {
			t.Fatalf("%s title holds the subject", k)
		}
		if _, ok := it.Fields["body"]; ok {
			t.Fatalf("%s read a body under gmail.metadata", k)
		}
	}
	if srv.Called("/gmail/v1/users/me/messages/m1") {
		t.Fatal("a body was requested without gmail.readonly")
	}
	recv := m["gmail:t1:m1"]
	if recv.Subject != "gmail:winton:thread:t1" || !recv.Addressed || recv.Own || recv.Actor != "email:dana@example.com" || recv.Version != "m1" {
		t.Fatalf("received %+v", recv)
	}
	wantFields := map[string]string{
		"from": "dana@example.com", "to": "winton@example.com, ops@example.com", "subject": "Budget review",
		"date": "2026-11-08T10:00:00Z", "labels": "INBOX, Projects, STARRED, UNREAD",
		"link": "https://mail.google.com/mail/?authuser=winton%40example.com#all/t1",
	}
	for k, v := range wantFields {
		if recv.Fields[k] != v {
			t.Errorf("field %s = %q, want %q", k, recv.Fields[k], v)
		}
	}
	wantState := map[string]string{"unread": "true", "starred": "true", "labels": "INBOX, Projects, SENT, STARRED, UNREAD", "last_from": "winton@example.com"}
	for k, v := range wantState {
		if recv.State[k] != v {
			t.Errorf("state %s = %q, want %q", k, recv.State[k], v)
		}
	}
	if recv.ObservedAt == nil || !recv.ObservedAt.Equal(testNow) {
		t.Fatalf("observed %v", recv.ObservedAt)
	}
	sent := m["gmail:t1:m2"]
	if !sent.Own || sent.Addressed || sent.Actor != "email:winton@example.com" || sent.Fields["subject"] != "Budget review" {
		t.Fatalf("sent %+v", sent)
	}
	if m["gmail:t2:m3"].Addressed {
		t.Fatal("a message to a list addressed the person")
	}
	// A From header is the sender's claim. Without the SENT label, mail "from" the person is someone else's.
	if forged := m["gmail:t5:m6"]; forged.Own || forged.Type != TypeReceived || forged.Actor != "email:winton@example.com" || !forged.Addressed {
		t.Fatalf("forged From %+v", forged)
	}
	star := m["gmail:t1:starred:m1"]
	if star.Own || star.Actor != "" || star.Addressed {
		t.Fatalf("starred %+v", star)
	}
	_, body, err := project.Project(project.Input{Item: recv, Rule: send(t, "rules:\n  - name: t\n    match: {}\n    tier: send\n    fields: [subject, from]\n"), Person: project.Person{Handle: "@winton"}})
	if err != nil {
		t.Fatal(err)
	}
	wantBody := `{"subject":"gmail:winton:thread:t1","type":"gmail.received","key":"gmail:t1:m1","occurred_at":"2026-11-08T10:00:00.000Z","title":"New message","open":{"title":"Budget review"},"metadata":{"fields":{"from":"dana@example.com","subject":"Budget review"}}}`
	if string(body) != wantBody {
		t.Fatalf("got  %s\nwant %s", body, wantBody)
	}
	again, err := g.Collect(context.Background(), []byte("100"))
	if err != nil || len(again.Items) != len(got.Items) {
		t.Fatalf("unstable %v", err)
	}
	for i := range again.Items {
		if again.Items[i].Key != got.Items[i].Key {
			t.Fatal("keys moved between passes")
		}
	}
}

func TestGmailBodiesNeedReadonly(t *testing.T) {
	srv := googletest.New(t)
	srv.Scope = ScopeGmailReadonly
	g, err := NewGmail(opts(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.Collect(context.Background(), []byte("100"))
	if err != nil {
		t.Fatal(err)
	}
	if srv.Called("/gmail/v1/users/me/messages/m1") {
		t.Fatal("a body was read though no rule lists body")
	}
	g.SetBodies(true)
	got, err = g.Collect(context.Background(), []byte("100"))
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items, GmailFields, GmailStateFields)
	if m["gmail:t1:m1"].Fields["body"] != "Can you review the budget by Friday?" {
		t.Fatalf("body %q", m["gmail:t1:m1"].Fields["body"])
	}
	if _, ok := m["gmail:t1:starred:m1"].Fields["body"]; ok {
		t.Fatal("starring carried a body")
	}
}

func TestGmailFirstRun(t *testing.T) {
	srv := googletest.New(t)
	g, err := NewGmail(opts(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Called("/gmail/v1/users/me/history") {
		t.Fatal("a first run read history")
	}
	if !srv.Called("/gmail/v1/users/me/threads/t2") || !srv.Called("/gmail/v1/users/me/threads/t1") {
		t.Fatal("a recent draft on an old thread stopped the inbox walk")
	}
	if q := srv.Query("/gmail/v1/users/me/threads"); q.Get("labelIds") != "INBOX" || q.Get("q") != "" {
		t.Fatalf("inbox query %v", q)
	}
	if history(t, got.Next) != "300" {
		t.Fatalf("next %q", got.Next)
	}
	m := byKey(t, got.Items, GmailFields, GmailStateFields)
	want := []string{"gmail:t1:m2 sent", "gmail:t1:starred:m1 starred", "gmail:t2:m3 received", "gmail:t4:m5 received"}
	if strings.Join(keys(m), "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", keys(m))
	}
}

func TestGmailExpiredHistory(t *testing.T) {
	srv := googletest.New(t)
	srv.Routes["/gmail/v1/users/me/history"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}
	g, err := NewGmail(opts(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.Collect(context.Background(), []byte(`{"history_id":"5","at":"2026-11-08T09:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if history(t, got.Next) != "300" || !got.Gap {
		t.Fatalf("next %q gap %v", got.Next, got.Gap)
	}
	// Open state, and every inbox and sent message since the last pass: m1, the earlier message of t1, is not lost.
	m := byKey(t, got.Items, GmailFields, GmailStateFields)
	want := []string{"gmail:t1:m1 received", "gmail:t1:m2 sent", "gmail:t1:starred:m1 starred", "gmail:t2:m3 received", "gmail:t4:m5 received", "gmail:t5:m6 received"}
	if strings.Join(keys(m), "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", keys(m))
	}
	if q := srv.Query("/gmail/v1/users/me/messages"); q.Get("q") != "" {
		t.Fatal("a search needs gmail.readonly; the gap is read by label")
	}
	// A later last pass reads less: m1 (10:00) is before 10:45 less the lookback.
	got, err = g.Collect(context.Background(), []byte(`{"history_id":"5","at":"2026-11-08T10:45:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byKey(t, got.Items, GmailFields, GmailStateFields)["gmail:t1:m1"]; ok {
		t.Fatal("read before the last pass")
	}
}

func TestGmailIdentityScopeAndLimits(t *testing.T) {
	srv := googletest.New(t)
	o := opts(t, srv)
	o.Identity = "email:someone@example.com"
	g, _ := NewGmail(o)
	if _, err := g.Collect(context.Background(), []byte("100")); !errors.Is(err, source.ErrIdentity) {
		t.Fatalf("identity: %v", err)
	}
	o = opts(t, srv)
	o.Account, o.Identity = "other@example.com", ""
	g, _ = NewGmail(o)
	if _, err := g.Collect(context.Background(), []byte("100")); !errors.Is(err, source.ErrIdentity) {
		t.Fatalf("a grant for another account: %v", err)
	}

	srv = googletest.New(t)
	srv.Scope = ScopeCalendarEvents
	g, _ = NewGmail(opts(t, srv))
	if _, err := g.Collect(context.Background(), []byte("100")); !errors.Is(err, source.ErrUnauthorized) {
		t.Fatalf("scope: %v", err)
	}

	srv = googletest.New(t)
	srv.Routes["/gmail/v1/users/me/labels"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"errors":[{"reason":"userRateLimitExceeded"}]}}`)
	}
	g, _ = NewGmail(opts(t, srv))
	if _, err := g.Collect(context.Background(), []byte("100")); !errors.Is(err, source.ErrRateLimited) {
		t.Fatalf("rate limit: %v", err)
	}

	srv = googletest.New(t)
	calls := 0
	srv.Routes["/gmail/v1/users/me/labels"] = func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, googletest.Labels)
	}
	o = opts(t, srv)
	var slept time.Duration
	o.Sleep = func(_ context.Context, d time.Duration) error { slept = d; return nil }
	g, _ = NewGmail(o)
	if _, err := g.Collect(context.Background(), []byte("100")); err != nil || slept != time.Second {
		t.Fatalf("backoff: %v slept %s", err, slept)
	}

	srv = googletest.New(t)
	srv.Routes["/gmail/v1/users/me/labels"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}`)
	}
	g, _ = NewGmail(opts(t, srv))
	if _, err := g.Collect(context.Background(), []byte("100")); !errors.Is(err, source.ErrUnauthorized) {
		t.Fatalf("insufficient scope: %v", err)
	}
}

func TestRefusedGrant(t *testing.T) {
	srv := googletest.New(t)
	o := opts(t, srv)
	o.Auth.refresh = func() (keychain.Secret, error) { return keychain.FromString("revoked") }
	g, _ := NewGmail(o)
	if _, err := g.Collect(context.Background(), []byte("100")); !errors.Is(err, source.ErrUnauthorized) {
		t.Fatalf("%v", err)
	}
	if strings.Contains(source.ErrUnauthorized.Error(), "revoked") {
		t.Fatal("error text names the token")
	}
}

func TestCalendarEveryItem(t *testing.T) {
	srv := googletest.New(t)
	c, err := NewCalendar(opts(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	q := srv.Query("/calendar/v3/calendars/primary/events")
	if q.Get("updatedMin") != "" || q.Get("singleEvents") != "true" || q.Get("timeMin") != "2026-11-08T12:00:00Z" || strings.Contains(q.Get("fields"), "description") {
		t.Fatalf("first query %v", q)
	}
	m := byKey(t, first.Items, CalendarFields, CalendarStateFields)
	want := []string{"calendar:ev1:response_needed:3001 response_needed", "calendar:ev2:upcoming:3002 upcoming", "calendar:ev4:upcoming:3004 upcoming"}
	if strings.Join(keys(m), "|") != strings.Join(want, "|") {
		t.Fatalf("first %v", keys(m))
	}
	rn := m["calendar:ev1:response_needed:3001"]
	if rn.Subject != "calendar:winton:ev1" || !rn.Addressed || rn.Own || rn.Actor != "email:dana@example.com" || rn.EventTitle != "Response needed" {
		t.Fatalf("response needed %+v", rn)
	}
	if rn.DueAt == nil || !rn.DueAt.Equal(time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("due %v", rn.DueAt)
	}
	wantFields := map[string]string{"summary": "Design review", "start": "2026-11-09T17:00:00Z", "end": "2026-11-09T18:00:00Z",
		"organizer": "dana@example.com", "attendees": "3", "response": "needsAction", "link": "https://www.google.com/calendar/event?eid=ev1"}
	for k, v := range wantFields {
		if rn.Fields[k] != v {
			t.Errorf("field %s = %q, want %q", k, rn.Fields[k], v)
		}
	}
	if rn.State["status"] != "confirmed" || rn.State["response"] != "needsAction" || rn.Version != "3001" {
		t.Fatalf("state %v %s", rn.State, rn.Version)
	}
	allDay := m["calendar:ev4:upcoming:3004"]
	if allDay.DueAt == nil || !allDay.DueAt.Equal(time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)) || allDay.Fields["start"] != "2026-11-12" {
		t.Fatalf("all day %v %v", allDay.DueAt, allDay.Fields)
	}
	if strings.Contains(string(first.Next), "Design review") || strings.Contains(string(first.Next), "Room 1") {
		t.Fatalf("cursor holds event content: %s", first.Next)
	}

	second, err := c.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if q := srv.Query("/calendar/v3/calendars/primary/events"); q.Get("updatedMin") != "2026-11-08T11:45:00Z" || q.Get("showDeleted") != "true" {
		t.Fatalf("second query %v", q)
	}
	m = byKey(t, second.Items, CalendarFields, CalendarStateFields)
	want = []string{
		"calendar:ev1:responded:3101 responded",
		"calendar:ev2:changed:3102 changed",
		"calendar:ev4:cancelled:3104 cancelled",
		"calendar:ev5:created:3105 created",
		"calendar:ev6:response_needed:3106 response_needed",
	}
	if strings.Join(keys(m), "|") != strings.Join(want, "|") {
		t.Fatalf("second %v (another attendee's response must not be an event)", keys(m))
	}
	resp := m["calendar:ev1:responded:3101"]
	if !resp.Own || resp.Actor != "email:winton@example.com" || resp.EventTitle != "Responded: accepted" || resp.Addressed {
		t.Fatalf("responded %+v", resp)
	}
	// Calendar does not say who changed or cancelled an event, so organising it does not make the change own.
	moved := m["calendar:ev2:changed:3102"]
	if moved.Own || moved.Actor != "" || moved.EventTitle != "Rescheduled" {
		t.Fatalf("changed %+v", moved)
	}
	if c := m["calendar:ev4:cancelled:3104"]; c.Own || c.Actor != "" || c.EventTitle != "Cancelled" || c.State["status"] != "cancelled" {
		t.Fatalf("cancelled %+v", c)
	}
	if cr := m["calendar:ev5:created:3105"]; cr.Own || cr.Actor != "email:dana@example.com" || cr.EventTitle != "Created" {
		t.Fatalf("created %+v", cr)
	}
	if _, ok := m["calendar:ev6:response_needed:3106"].Fields["body"]; ok {
		t.Fatal("description read though no rule lists body")
	}
	again, err := c.Collect(context.Background(), first.Next)
	if err != nil || len(again.Items) != len(second.Items) {
		t.Fatalf("unstable %v", err)
	}
	third, err := c.Collect(context.Background(), second.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Items) != 0 {
		t.Fatalf("unchanged events yielded %v", keys(byKey(t, third.Items, CalendarFields, CalendarStateFields)))
	}
	c.SetBodies(true)
	withBody, err := c.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srv.Query("/calendar/v3/calendars/primary/events").Get("fields"), "description") {
		t.Fatal("description not asked for when a rule lists body")
	}
	for _, it := range withBody.Items {
		if it.Key == "calendar:ev6:response_needed:3106" && it.Fields["body"] != "Agenda: candidates" {
			t.Fatalf("body %q", it.Fields["body"])
		}
	}
}

func TestCalendarGoneReadsTheWindow(t *testing.T) {
	srv := googletest.New(t)
	c, _ := NewCalendar(opts(t, srv))
	first, err := c.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if q := srv.Query("/calendar/v3/calendars/primary/events"); q.Get("showDeleted") != "true" {
		t.Fatalf("a window read without deleted events: %v", q)
	}
	// updatedMin is too old. The full window holds ev2 and ev4 unchanged; ev1 and ev3 are missing from it.
	srv.Routes["/calendar/v3/calendars/primary/events"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("updatedMin") != "" {
			w.WriteHeader(http.StatusGone)
			return
		}
		if q.Get("showDeleted") != "true" {
			t.Errorf("the full re-read must include deleted events: %v", q)
		}
		io.WriteString(w, strings.Replace(strings.Replace(googletest.EventsFirst, `"id":"ev1"`, `"id":"evX1"`, 1), `"id":"ev3"`, `"id":"evX3"`, 1))
	}
	// ev3 was deleted outright; ev1 moved beyond the window.
	srv.Routes["/calendar/v3/calendars/primary/events/ev3"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }
	srv.Routes["/calendar/v3/calendars/primary/events/ev1"] = func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id":"ev1","status":"confirmed","etag":"\"3301\"","summary":"Design review","location":"Room 1","start":{"dateTime":"2026-12-20T17:00:00Z"},"end":{"dateTime":"2026-12-20T18:00:00Z"},
  "created":"2026-11-01T00:00:00Z","updated":"2026-11-08T12:40:00Z","organizer":{"email":"dana@example.com"},"attendees":[{"email":"winton@example.com","self":true,"responseStatus":"needsAction"}]}`)
	}
	got, err := c.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items, CalendarFields, CalendarStateFields)
	for k := range m {
		if strings.HasPrefix(k, "calendar:evX") {
			delete(m, k)
		}
	}
	want := []string{"calendar:ev1:changed:3301 changed", "calendar:ev3:cancelled:gone cancelled"}
	if strings.Join(keys(m), "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", keys(m))
	}
	if gone := m["calendar:ev3:cancelled:gone"]; gone.Own || gone.Subject != "calendar:winton:ev3" {
		t.Fatalf("%+v", gone)
	}
	// The cancel is remembered: a second full re-read does not cancel it again.
	again, err := c.Collect(context.Background(), got.Next)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range again.Items {
		if strings.HasPrefix(it.Key, "calendar:ev3:") || strings.HasPrefix(it.Key, "calendar:ev1:") {
			t.Fatalf("again: %s", it.Key)
		}
	}
}

func TestCalendarCreatedByThePerson(t *testing.T) {
	srv := googletest.New(t)
	c, _ := NewCalendar(opts(t, srv))
	first, _ := c.Collect(context.Background(), nil)
	srv.Routes["/calendar/v3/calendars/primary/events"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("singleEvents") != "true" {
			io.WriteString(w, `{"items":[]}`)
			return
		}
		io.WriteString(w, `{"timeZone":"UTC","items":[
  {"id":"ev7","status":"confirmed","etag":"\"3107\"","summary":"Focus","start":{"dateTime":"2026-11-09T08:00:00Z"},"end":{"dateTime":"2026-11-09T09:00:00Z"},
   "created":"2026-11-08T12:30:00Z","updated":"2026-11-08T12:30:00Z","organizer":{"email":"winton@example.com","self":true},"creator":{"email":"winton@example.com","self":true}},
  {"id":"ev8","status":"confirmed","etag":"\"3108\"","summary":"Booked for you","start":{"dateTime":"2026-11-09T10:00:00Z"},"end":{"dateTime":"2026-11-09T11:00:00Z"},
   "created":"2026-11-08T12:31:00Z","updated":"2026-11-08T12:31:00Z","organizer":{"email":"winton@example.com","self":true},"creator":{"email":"assistant@example.com"}}
]}`)
	}
	got, err := c.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items, CalendarFields, CalendarStateFields)
	if it := m["calendar:ev7:created:3107"]; !it.Own || it.Actor != "email:winton@example.com" {
		t.Fatalf("created by the person %+v", it)
	}
	if it := m["calendar:ev8:created:3108"]; it.Own || it.Actor != "" {
		t.Fatalf("created by a delegate %+v", it)
	}
}

func TestDriveEveryItem(t *testing.T) {
	srv := googletest.New(t)
	d, err := NewDrive(opts(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Collect(context.Background(), []byte("2026-11-08T10:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Next) != "2026-11-08T12:00:00Z" {
		t.Fatalf("next %q", got.Next)
	}
	fqs := srv.Queries("/drive/v3/files")
	if len(fqs) != 2 || !strings.HasPrefix(fqs[0].Get("q"), "modifiedTime > '2026-11-08T09:45:00Z'") || !strings.Contains(fqs[0].Get("q"), "trashed = false") ||
		!strings.HasPrefix(fqs[1].Get("q"), "viewedByMeTime > '2026-10-25T12:00:00Z'") || fqs[1].Get("orderBy") != "viewedByMeTime desc" {
		t.Fatalf("files queries %v", fqs)
	}
	if n := len(srv.Queries("/drive/v3/files/f1/comments")); n != 1 {
		t.Fatalf("f1, both modified and viewed, had its comments read %d times", n)
	}
	cq := srv.Query("/drive/v3/files/f1/comments")
	if cq.Get("startModifiedTime") != "2026-11-08T09:45:00Z" || strings.Contains(cq.Get("fields"), "content") {
		t.Fatalf("comments query %v", cq)
	}
	if srv.Called("/drive/v3/files/n1/export") {
		t.Fatal("notes content read though no rule lists body")
	}
	if srv.Called("/drive/v3/files/n2") || srv.Called("/drive/v3/files/n3") || srv.Called("/drive/v3/files/n4") {
		t.Fatal("notes read for a meeting the person did not attend, that is not over, or that is not notes")
	}
	m := byKey(t, got.Items, DriveFields, DriveStateFields)
	want := []string{
		"drive:f1:comment:c1 mentioned",
		"drive:f1:comment:c2 assigned",
		"drive:f1:comment:c3 commented",
		"drive:f1:reply:c1:r1 resolved",
		"drive:f1:reply:c1:r2 mentioned",
		"drive:f2:comment:c7 mentioned",
		"drive:notes:n1 notes_created",
	}
	if strings.Join(keys(m), "|") != strings.Join(want, "|") {
		t.Fatalf("got %v (f2 was not modified; it was opened recently)", keys(m))
	}
	assigned := m["drive:f1:comment:c2"]
	if assigned.Subject != "drive:winton:f1" || !assigned.Addressed || assigned.Own || assigned.EventTitle != "Assigned" || assigned.Version != "c2" {
		t.Fatalf("assigned %+v", assigned)
	}
	if assigned.Fields["link"] != "https://docs.google.com/document/d/f1/edit?usp=drivesdk&disco=c2" || assigned.Fields["title"] != "Q4 plan" ||
		assigned.Fields["file_type"] != "document" || assigned.Fields["author"] != "Dana Lee" {
		t.Fatalf("fields %v", assigned.Fields)
	}
	if assigned.State["assignee"] != "winton@example.com" || assigned.State["resolved"] != "false" {
		t.Fatalf("state %v", assigned.State)
	}
	if r := m["drive:f1:reply:c1:r1"]; !r.Own || r.Actor != "email:winton@example.com" || r.EventTitle != "Resolved" || r.Version != "r1" {
		t.Fatalf("resolved %+v", r)
	}
	if r := m["drive:f1:comment:c3"]; !r.Own || r.EventTitle != "Commented" {
		t.Fatalf("own comment %+v", r)
	}
	notes := m["drive:notes:n1"]
	if notes.Subject != "drive:winton:n1" || notes.EventTitle != "Meeting notes created" || notes.Fields["meeting"] != "Weekly sync" ||
		notes.Fields["link"] != "https://docs.google.com/document/d/n1/edit" || notes.Fields["meeting_start"] != "2026-11-07T15:00:00Z" {
		t.Fatalf("notes %+v", notes)
	}
	if _, ok := notes.Fields["body"]; ok {
		t.Fatal("notes carried content")
	}
	for _, it := range got.Items {
		if _, ok := it.Fields["body"]; ok {
			t.Fatalf("%s carried a body", it.Key)
		}
	}
	d.SetBodies(true)
	withBody, err := d.Collect(context.Background(), []byte("2026-11-08T10:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	m = byKey(t, withBody.Items, DriveFields, DriveStateFields)
	if m["drive:notes:n1"].Fields["body"] != "Weekly sync\nDecisions: ship on Friday.\n" || m["drive:f1:comment:c2"].Fields["body"] != "Assign: update the forecast" {
		t.Fatalf("bodies %q %q", m["drive:notes:n1"].Fields["body"], m["drive:f1:comment:c2"].Fields["body"])
	}
}

func TestDriveFirstRun(t *testing.T) {
	srv := googletest.New(t)
	d, _ := NewDrive(opts(t, srv))
	got, err := d.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if fq := srv.Queries("/drive/v3/files")[0]; !strings.Contains(fq.Get("q"), "modifiedTime > '2026-11-01T12:00:00Z'") {
		t.Fatalf("files query %v", fq)
	}
	if srv.Query("/calendar/v3/calendars/primary/events").Get("updatedMin") != "" {
		t.Fatal("a first run limited notes by updatedMin")
	}
	m := byKey(t, got.Items, DriveFields, DriveStateFields)
	// c1 is resolved and c3 and r1 are own acts; c5 is older than 7 days.
	want := []string{"drive:f1:comment:c2 assigned", "drive:f2:comment:c7 mentioned", "drive:notes:n1 notes_created"}
	if strings.Join(keys(m), "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", keys(m))
	}
}

func TestDriveNeedsCalendarScope(t *testing.T) {
	srv := googletest.New(t)
	srv.Scope = ScopeDriveReadonly
	d, _ := NewDrive(opts(t, srv))
	if _, err := d.Collect(context.Background(), nil); !errors.Is(err, source.ErrUnauthorized) {
		t.Fatalf("%v", err)
	}
}

func TestScopeAndHosts(t *testing.T) {
	srv := googletest.New(t)
	a := newAuth(t, srv)
	g, _ := NewGmail(Options{Person: "winton", Auth: a})
	c, _ := NewCalendar(Options{Person: "winton", Auth: a})
	d, _ := NewDrive(Options{Person: "winton", Auth: a})
	for _, tc := range []struct {
		s     interface{ allow(string) error }
		raw   string
		allow bool
	}{
		{&g.common, "https://gmail.googleapis.com/gmail/v1/users/me/history?startHistoryId=1", true},
		{&g.common, "https://gmail.googleapis.com/gmail/v1/users/me/threads/abc123?format=metadata&metadataHeaders=From", true},
		{&g.common, "https://gmail.googleapis.com/gmail/v1/users/me/threads/abc123", false},
		{&g.common, "https://gmail.googleapis.com/gmail/v1/users/me/messages/abc123?format=raw", false},
		{&g.common, "https://gmail.googleapis.com/gmail/v1/users/me/messages/abc123?format=full&alt=media", false},
		{&g.common, "https://gmail.googleapis.com/gmail/v1/users/me/history?startHistoryId=1&q=from:x", false},
		{&g.common, "https://gmail.googleapis.com/gmail/v1/users/me/messages/abc/attachments/x", false},
		{&g.common, "https://gmail.googleapis.com/gmail/v1/users/other@example.com/profile", false},
		{&g.common, "https://www.googleapis.com/gmail/v1/users/me/profile", false},
		{&c.common, "https://www.googleapis.com/calendar/v3/calendars/primary/events?singleEvents=true&showDeleted=true", true},
		{&c.common, "https://www.googleapis.com/calendar/v3/calendars/primary/events?x=1", false},
		{&c.common, "https://www.googleapis.com/calendar/v3/calendars/primary/events/ev1?fields=id", true},
		{&c.common, "https://www.googleapis.com/calendar/v3/calendars/primary/events/ev1/instances", false},
		{&c.common, "https://www.googleapis.com/calendar/v3/users/me/calendarList", false},
		{&c.common, "https://www.googleapis.com/drive/v3/files", false},
		{&d.common, "https://www.googleapis.com/drive/v3/files/abc/comments?startModifiedTime=x&fields=y", true},
		{&d.common, "https://www.googleapis.com/drive/v3/files/abc?fields=id,name", true},
		{&d.common, "https://www.googleapis.com/drive/v3/files/abc?alt=media", false},
		{&d.common, "https://www.googleapis.com/drive/v3/files/abc?fields=id&acknowledgeAbuse=true", false},
		{&d.common, "https://www.googleapis.com/drive/v3/files/abc/export?mimeType=text/plain", true},
		{&d.common, "https://www.googleapis.com/drive/v3/files/abc/export?mimeType=application/pdf", false},
		{&d.common, "https://www.googleapis.com/drive/v3/files/abc/comments?alt=media", false},
		{&d.common, "https://www.googleapis.com/drive/v3/about?fields=user&alt=json", false},
		{&d.common, "https://www.googleapis.com/drive/v3/files/abc/permissions", false},
		{&d.common, "https://www.googleapis.com/drive/v3/files/../about", false},
		{&d.common, "http://www.googleapis.com/drive/v3/about", false},
		{&d.common, "https://evil.example/drive/v3/about", false},
	} {
		if err := tc.s.allow(tc.raw); (err == nil) != tc.allow {
			t.Errorf("%s: allowed %v, want %v", tc.raw, err == nil, tc.allow)
		}
	}
	tokenHost := a.Host()
	if got := strings.Join(g.Hosts(), ","); got != "gmail.googleapis.com:443,"+tokenHost {
		t.Fatal(got)
	}
	if got := strings.Join(d.Hosts(), ","); got != "www.googleapis.com:443,"+tokenHost {
		t.Fatal(got)
	}
	prod, _ := NewAuth(AuthOptions{})
	if prod.Host() != "oauth2.googleapis.com:443" {
		t.Fatal(prod.Host())
	}
	if _, err := NewGmail(Options{Person: "act_123x", Auth: a}); err == nil {
		t.Fatal("an act_ handle cannot be a personal subject's person")
	}
}

func TestGuard(t *testing.T) {
	srv := googletest.New(t)
	o := opts(t, srv)
	g, _ := NewGmail(o)
	g.SetHTTP(&http.Client{Transport: egress.Guard([]string{"gmail.googleapis.com:443", "oauth2.googleapis.com:443"}, srv.Client().Transport), Timeout: 5 * time.Second})
	if _, err := g.Collect(context.Background(), nil); err == nil || len(srv.Requests()) != 0 {
		t.Fatalf("err %v requests %d", err, len(srv.Requests()))
	}
}

func TestScopes(t *testing.T) {
	got := strings.Join(Scopes([]string{"gmail", "calendar", "drive"}, false), " ")
	if got != googletest.AllScopes {
		t.Fatal(got)
	}
	if got := Scopes([]string{"gmail"}, true); len(got) != 1 || got[0] != ScopeGmailReadonly {
		t.Fatal(got)
	}
	for _, s := range Scopes([]string{"gmail", "calendar", "drive"}, true) {
		if !strings.Contains(s, "readonly") {
			t.Fatalf("%s is not read-only", s)
		}
	}
}

func TestGmailThreadFailures(t *testing.T) {
	// A thread deleted since the history named it is skipped, and the cursor moves.
	srv := googletest.New(t)
	srv.Routes["/gmail/v1/users/me/threads/t2"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }
	g, _ := NewGmail(opts(t, srv))
	got, err := g.Collect(context.Background(), []byte("100"))
	if err != nil || history(t, got.Next) != "350" {
		t.Fatalf("%v %q", err, got.Next)
	}
	for _, it := range got.Items {
		if strings.HasPrefix(it.Key, "gmail:t2:") {
			t.Fatal("a deleted thread yielded an item")
		}
	}
	// Any 403 fails the pass: the history cursor has no lookback, so a skipped thread would never be read again.
	srv = googletest.New(t)
	srv.Routes["/gmail/v1/users/me/threads/t2"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"errors":[{"reason":"forbidden"}]}}`)
	}
	g, _ = NewGmail(opts(t, srv))
	if got, err := g.Collect(context.Background(), []byte("100")); err == nil || got.Next != nil {
		t.Fatalf("a 403 on a thread did not fail the pass: %v %q", err, got.Next)
	}
}

func TestCalendarMovedOutOfTheWindow(t *testing.T) {
	srv := googletest.New(t)
	c, _ := NewCalendar(opts(t, srv))
	first, err := c.Collect(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Routes["/calendar/v3/calendars/primary/events"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("singleEvents") == "true" {
			// The windowed list no longer holds ev3 (moved past 30 days) or ev2 (moved into the past).
			io.WriteString(w, `{"timeZone":"UTC","items":[]}`)
			return
		}
		if q.Get("timeMin") != "" || q.Get("timeMax") != "" || q.Get("updatedMin") != "2026-11-08T11:45:00Z" {
			t.Errorf("moved-out query %v", q)
		}
		io.WriteString(w, `{"timeZone":"UTC","items":[
  {"id":"ev3","status":"confirmed","etag":"\"3203\"","summary":"Quarterly","start":{"dateTime":"2026-12-20T09:00:00Z"},"end":{"dateTime":"2026-12-20T10:00:00Z"},
   "created":"2026-10-01T00:00:00Z","updated":"2026-11-08T12:30:00Z","organizer":{"email":"dana@example.com"},"attendees":[{"email":"winton@example.com","self":true,"responseStatus":"accepted"}]},
  {"id":"ev2","status":"cancelled","etag":"\"3202\"","updated":"2026-11-08T12:31:00Z","organizer":{"email":"winton@example.com","self":true}},
  {"id":"series1","status":"confirmed","etag":"\"3299\"","summary":"Standup","start":{"dateTime":"2026-11-09T09:00:00Z"},"end":{"dateTime":"2026-11-09T09:15:00Z"},
   "created":"2026-01-01T00:00:00Z","updated":"2026-11-08T12:32:00Z","organizer":{"email":"dana@example.com"}}
]}`)
	}
	got, err := c.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(t, got.Items, CalendarFields, CalendarStateFields)
	want := []string{"calendar:ev2:cancelled:3202 cancelled", "calendar:ev3:changed:3203 changed"}
	if strings.Join(keys(m), "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", keys(m))
	}
	if m["calendar:ev3:changed:3203"].EventTitle != "Rescheduled" {
		t.Fatalf("%+v", m["calendar:ev3:changed:3203"])
	}
}

func TestGmailBodyForbiddenKeepsTheItem(t *testing.T) {
	srv := googletest.New(t)
	srv.Scope = ScopeGmailReadonly
	srv.Routes["/gmail/v1/users/me/messages/m1"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"errors":[{"reason":"forbidden"}]}}`)
	}
	g, _ := NewGmail(opts(t, srv))
	g.SetBodies(true)
	got, err := g.Collect(context.Background(), []byte("100"))
	if err != nil || history(t, got.Next) != "350" {
		t.Fatalf("%v %q", err, got.Next)
	}
	m := byKey(t, got.Items, GmailFields, GmailStateFields)
	if it, ok := m["gmail:t1:m1"]; !ok || it.Fields["body"] != "" {
		t.Fatalf("item %+v", it)
	}
	srv.Routes["/gmail/v1/users/me/messages/m1"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"errors":[{"reason":"rateLimitExceeded"}]}}`)
	}
	if _, err := g.Collect(context.Background(), []byte("100")); !errors.Is(err, source.ErrRateLimited) {
		t.Fatalf("a rate limit on a body: %v", err)
	}
}

func TestGmailGapReadIsCut(t *testing.T) {
	srv := googletest.New(t)
	srv.Routes["/gmail/v1/users/me/history"] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }
	pages := 0
	srv.Routes["/gmail/v1/users/me/messages"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("labelIds") == "INBOX" {
			pages++
			// Every page is recent and there is always another.
			io.WriteString(w, `{"messages":[{"id":"m3","threadId":"t2"}],"nextPageToken":"more"}`)
			return
		}
		io.WriteString(w, googletest.SentMessages)
	}
	g, _ := NewGmail(opts(t, srv))
	got, err := g.Collect(context.Background(), []byte(`{"history_id":"5","at":"2026-11-08T09:00:00Z"}`))
	if err != nil || !got.Gap || !got.Cut || pages != gapPages {
		t.Fatalf("err %v gap %v cut %v pages %d", err, got.Gap, got.Cut, pages)
	}
	if history(t, got.Next) != "300" {
		t.Fatal("a cut gap must still move past the expired history id, or every pass stops at the same place")
	}
}

func TestCalendarFullReadSkipsAnUnreadableEvent(t *testing.T) {
	srv := googletest.New(t)
	c, _ := NewCalendar(opts(t, srv))
	first, _ := c.Collect(context.Background(), nil)
	srv.Routes["/calendar/v3/calendars/primary/events"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("updatedMin") != "" {
			w.WriteHeader(http.StatusGone)
			return
		}
		io.WriteString(w, strings.Replace(googletest.EventsFirst, `"id":"ev1"`, `"id":"evX1"`, 1))
	}
	srv.Routes["/calendar/v3/calendars/primary/events/ev1"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"errors":[{"reason":"forbidden"}]}}`)
	}
	got, err := c.Collect(context.Background(), first.Next)
	if err != nil {
		t.Fatalf("one unreadable event stalled the source: %v", err)
	}
	if strings.Contains(string(got.Next), `"ev1"`) {
		t.Fatal("an unreadable event is kept and read again on every full re-read")
	}
}

func TestGmailFirstRunCutIsReported(t *testing.T) {
	srv := googletest.New(t)
	srv.Routes["/gmail/v1/users/me/threads"] = func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"threads":[{"id":"t2"}],"nextPageToken":"more"}`)
	}
	g, _ := NewGmail(opts(t, srv))
	got, err := g.Collect(context.Background(), nil)
	if err != nil || !got.Cut || got.Gap {
		t.Fatalf("err %v cut %v gap %v", err, got.Cut, got.Gap)
	}
	srv = googletest.New(t)
	g, _ = NewGmail(opts(t, srv))
	if got, err := g.Collect(context.Background(), nil); err != nil || got.Cut {
		t.Fatalf("a read that ends in the window is not cut: %v %v", err, got.Cut)
	}
}

// Notes are attached while the meeting runs, which is the event's last update. The pass after it ends still finds
// them, though the event was not updated since.
func TestDriveNotesForAMeetingThatEndsLater(t *testing.T) {
	srv := googletest.New(t)
	now := testNow
	srv.Routes["/calendar/v3/calendars/primary/events"] = func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		timeMin, _ := time.Parse(time.RFC3339, q.Get("timeMin"))
		timeMax, _ := time.Parse(time.RFC3339, q.Get("timeMax"))
		updatedMin, _ := time.Parse(time.RFC3339, q.Get("updatedMin"))
		start, end, updated := time.Date(2026, 11, 8, 15, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 16, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 15, 30, 0, 0, time.UTC)
		// Google's filters: the end after timeMin, the start before timeMax, updated at or after updatedMin.
		if !end.After(timeMin) || !start.Before(timeMax) || (!updatedMin.IsZero() && updated.Before(updatedMin)) {
			io.WriteString(w, `{"items":[]}`)
			return
		}
		io.WriteString(w, `{"timeZone":"UTC","items":[{"id":"evD","status":"confirmed","summary":"Later today","start":{"dateTime":"2026-11-08T15:00:00Z"},"end":{"dateTime":"2026-11-08T16:00:00Z"},"updated":"2026-11-08T15:30:00Z",
  "organizer":{"self":true},"attachments":[{"fileId":"n4","title":"Later today - Notes by Gemini","mimeType":"application/vnd.google-apps.document"}]}]}`)
	}
	srv.Routes["/drive/v3/files/n4"] = func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id":"n4","name":"Later today - Notes by Gemini","mimeType":"application/vnd.google-apps.document","webViewLink":"https://docs.google.com/document/d/n4/edit","createdTime":"2026-11-08T15:30:00Z"}`)
	}
	o := opts(t, srv)
	o.Now = func() time.Time { return now }
	o.Auth.now = o.Now
	d, _ := NewDrive(o)
	collect := func(at time.Time, cursor string) []source.Item {
		now = at
		got, err := d.Collect(context.Background(), []byte(cursor))
		if err != nil {
			t.Fatal(err)
		}
		return got.Items
	}
	// 15:40: the notes are attached and the meeting is still running.
	for _, it := range collect(time.Date(2026, 11, 8, 15, 40, 0, 0, time.UTC), "2026-11-08T15:39:00Z") {
		if it.Type == TypeNotesCreated {
			t.Fatal("notes for a meeting that has not ended")
		}
	}
	// 16:01: the meeting ended. It was not updated since 15:30.
	found := false
	for _, it := range collect(time.Date(2026, 11, 8, 16, 1, 0, 0, time.UTC), "2026-11-08T16:00:00Z") {
		found = found || it.Key == "drive:notes:n4"
	}
	if !found {
		t.Fatal("notes lost: the meeting ended after its last update")
	}
}
