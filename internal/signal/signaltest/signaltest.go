// Package signaltest is a fake Signal for tests. It implements POST /api/events with Signal's key rules: the same
// key and the same bytes replay (200), the same key and other bytes are 409 idempotency_conflict. Writes with `open`
// open threads as Signal does, thread.titled renames one, and GET /api/events/{id} and GET /api/threads/{id} read
// them back.
package signaltest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Event is an event the fake wrote.
type Event struct {
	ID   string
	Key  string
	Body []byte
}

// Request is one request the fake received.
type Request struct {
	Method string
	Path   string
	Key    string
	Body   []byte
}

// Reply overrides the fake's answer to one request. Status 0 means "answer normally".
type Reply struct {
	Status  int
	Body    string
	Headers map[string]string
	// Apply writes the event anyway before answering, as when Signal commits and the answer is lost.
	Apply bool
}

// Server is the fake.
type Server struct {
	*httptest.Server
	Token string

	mu       sync.Mutex
	events   []Event
	byKey    map[string]int
	requests []Request
	// Script, when set, is asked first for each request, with its number from 1.
	Script func(n int, req Request) Reply
	// Restricted are the prefixes GET /api/restrictions lists. NoRead answers it 403, as for a key without read.
	Restricted []string
	NoRead     bool
	// Actor is who GET /api/me reports for Token. Zero uses the default agent.
	Actor MeActor
	// Tokens are further bearer tokens the fake accepts, each with its own actor.
	Tokens map[string]MeActor

	// threads by number; subjects maps a subject to the thread number covering it; opened maps an event id to the
	// thread it opened.
	threads  map[int64]*Thread
	subjects map[string]int64
	opened   map[string]int64
	landed   map[string]int64

	// Leases turns on POST /api/leases (Signal DEC-280); without it the route is 404, as on a Signal that does not
	// offer leases. LeaseNow is the fake's clock for them. LeaseScript, when set, answers a lease claim first.
	Leases      bool
	LeaseNow    func() time.Time
	LeaseScript func(n int) Reply
	leases      map[string]leaseRow
	leaseClaims int
}

type leaseRow struct {
	actor, run string
	expires    time.Time
}

// Thread is a thread the fake holds.
type Thread struct {
	Number     int64
	Title      string
	MergedInto string
}

// MeActor is the actor GET /api/me returns for a token.
type MeActor struct {
	ID, Type, Handle, WorksFor string
}

// New starts a fake that accepts the bearer token.
func New(token string) *Server {
	s := &Server{Token: token, byKey: map[string]int{}, threads: map[int64]*Thread{}, subjects: map[string]int64{},
		opened: map[string]int64{}, landed: map[string]int64{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *Server) actor(token string) (MeActor, bool) {
	if token == "" {
		return MeActor{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.Tokens[token]; ok {
		return a, true
	}
	if token != s.Token {
		return MeActor{}, false
	}
	a := s.Actor
	if a.ID == "" {
		a = MeActor{ID: "act_test", Type: "agent", Handle: "beacon"}
	}
	return a, true
}

func jsonString(s string) string {
	if s == "" {
		return "null"
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(body, &parsed)
	req := Request{Method: r.Method, Path: r.URL.Path, Key: parsed.Key, Body: body}

	s.mu.Lock()
	s.requests = append(s.requests, req)
	n := len(s.requests)
	script := s.Script
	s.mu.Unlock()

	actor, ok := s.actor(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if !ok {
		reply(w, http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"Invalid credentials."}}`, nil)
		return
	}
	if r.URL.Path == "/api/me" && r.Method == http.MethodGet {
		reply(w, http.StatusOK, fmt.Sprintf(`{"organization":{"name":"Signal"},"actor":{"id":%q,"type":%q,"name":"Beacon","handle":%q,"works_for":%s},"scopes":["read","write","relay"],"key":{"id":"key_test","name":"beacon"}}`,
			actor.ID, actor.Type, actor.Handle, jsonString(actor.WorksFor)), nil)
		return
	}
	if r.URL.Path == "/api/restrictions" && r.Method == http.MethodGet {
		s.mu.Lock()
		noRead, prefixes := s.NoRead, append([]string(nil), s.Restricted...)
		s.mu.Unlock()
		if noRead {
			reply(w, http.StatusForbidden, `{"error":{"code":"forbidden","message":"Missing scope."}}`, nil)
			return
		}
		data := make([]map[string]string, 0, len(prefixes))
		for _, p := range prefixes {
			data = append(data, map[string]string{"prefix": p, "created_at": "2026-11-01T00:00:00.000Z"})
		}
		out, _ := json.Marshal(map[string]any{"data": data, "next_cursor": nil})
		reply(w, http.StatusOK, string(out), nil)
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/api/events/"); ok && r.Method == http.MethodGet {
		s.readEvent(w, id)
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/api/threads/"); ok && r.Method == http.MethodGet {
		s.readThread(w, id)
		return
	}
	if r.URL.Path == "/api/leases" && r.Method == http.MethodPost {
		s.claimLease(w, actor.ID, body)
		return
	}
	if r.URL.Path != "/api/events" || r.Method != http.MethodPost {
		reply(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"Not found."}}`, nil)
		return
	}
	if !json.Valid(body) {
		reply(w, http.StatusBadRequest, `{"error":{"code":"invalid","message":"Body is not JSON."}}`, nil)
		return
	}
	// A sealed write is checked as Signal checks it, before anything else answers (Signal DEC-250 to DEC-264).
	if field := SealedProblem(body); field != "" {
		out, _ := json.Marshal(map[string]any{"error": map[string]any{"code": "invalid", "message": "Invalid sealed write.", "details": map[string]string{"field": field}}})
		reply(w, http.StatusBadRequest, string(out), nil)
		return
	}
	if script != nil {
		if rp := script(n, req); rp.Status != 0 {
			if rp.Apply {
				s.write(parsed.Key, body)
			}
			reply(w, rp.Status, rp.Body, rp.Headers)
			return
		}
	}
	status, out := s.write(parsed.Key, body)
	reply(w, status, out, nil)
}

func (s *Server) claimLease(w http.ResponseWriter, actor string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Leases {
		reply(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"Not found."}}`, nil)
		return
	}
	s.leaseClaims++
	if s.LeaseScript != nil {
		if rp := s.LeaseScript(s.leaseClaims); rp.Status != 0 {
			reply(w, rp.Status, rp.Body, rp.Headers)
			return
		}
	}
	var c struct {
		Name string `json:"name"`
		Run  string `json:"run"`
		TTL  int64  `json:"ttl_seconds"`
	}
	if json.Unmarshal(body, &c) != nil || c.Name == "" || c.Run == "" || c.TTL < 10 || c.TTL > 86400 {
		reply(w, http.StatusBadRequest, `{"error":{"code":"invalid","message":"A lease claim is { name, run, ttl_seconds }."}}`, nil)
		return
	}
	now := time.Now()
	if s.LeaseNow != nil {
		now = s.LeaseNow()
	}
	if s.leases == nil {
		s.leases = map[string]leaseRow{}
	}
	row, ok := s.leases[c.Name]
	if ok && row.expires.After(now) && (row.actor != actor || row.run != c.Run) {
		reply(w, http.StatusConflict, fmt.Sprintf(`{"error":{"code":"conflict","message":"Another holder has this lease until it expires.","details":{"run":%q,"expires_at":%q}}}`,
			row.run, row.expires.UTC().Format(time.RFC3339Nano)), nil)
		return
	}
	row = leaseRow{actor: actor, run: c.Run, expires: now.Add(time.Duration(c.TTL) * time.Second)}
	s.leases[c.Name] = row
	reply(w, http.StatusOK, fmt.Sprintf(`{"name":%q,"run":%q,"expires_at":%q,"held":true,"acquired":%t}`, c.Name, c.Run,
		row.expires.UTC().Format(time.RFC3339Nano), !ok), nil)
}

// LeaseClaims is how many lease claims the fake received.
func (s *Server) LeaseClaims() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaseClaims
}

func (s *Server) write(key string, body []byte) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" {
		if i, ok := s.byKey[key]; ok {
			if bytes.Equal(s.events[i].Body, body) {
				return http.StatusOK, fmt.Sprintf(`{"event":{"id":%q},"replayed":true}`, s.events[i].ID)
			}
			return http.StatusConflict, `{"error":{"code":"idempotency_conflict","message":"Event key was already used for a different request."}}`
		}
		s.byKey[key] = len(s.events)
	}
	ev := Event{ID: fmt.Sprintf("evt_%04d", len(s.events)+1), Key: key, Body: append([]byte(nil), body...)}
	s.events = append(s.events, ev)
	s.place(ev)
	return http.StatusCreated, fmt.Sprintf(`{"event":{"id":%q},"replayed":false}`, ev.ID)
}

// place opens or finds the event's thread, and applies thread.titled. Called with mu held.
func (s *Server) place(ev Event) {
	var b struct {
		Subject string `json:"subject"`
		Type    string `json:"type"`
		Title   string `json:"title"`
		Open    *struct {
			Title string `json:"title"`
		} `json:"open"`
		Metadata struct {
			Title string `json:"title"`
		} `json:"metadata"`
	}
	if json.Unmarshal(ev.Body, &b) != nil {
		return
	}
	if n, ok := strings.CutPrefix(b.Subject, "signal:thread:"); ok {
		var num int64
		fmt.Sscan(n, &num)
		if t := s.threads[num]; t != nil && b.Type == "thread.titled" {
			t.Title = b.Metadata.Title
			s.landed[ev.ID] = num
		}
		return
	}
	if num, ok := s.subjects[b.Subject]; ok {
		s.landed[ev.ID] = num
		return
	}
	if b.Open == nil {
		return
	}
	title := b.Open.Title
	if title == "" {
		title = b.Title
	}
	num := int64(len(s.threads) + 1)
	s.threads[num] = &Thread{Number: num, Title: title}
	s.subjects[b.Subject] = num
	s.opened[ev.ID] = num
	s.landed[ev.ID] = num
}

func threadID(n int64) string { return fmt.Sprintf("thr_%04d", n) }

func (s *Server) readEvent(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, e := range s.events {
		found = found || e.ID == id
	}
	if !found {
		reply(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"Not found."}}`, nil)
		return
	}
	out := map[string]any{"id": id, "thread_id": nil, "open": nil}
	if n, ok := s.landed[id]; ok {
		t := s.threads[n]
		for t.MergedInto != "" {
			fmt.Sscanf(t.MergedInto, "thr_%d", &n)
			t = s.threads[n]
		}
		out["thread_id"] = threadID(n)
	}
	if n, ok := s.opened[id]; ok {
		out["open"] = map[string]any{"number": n, "title": s.firstTitle(id)}
	}
	data, _ := json.Marshal(out)
	reply(w, http.StatusOK, string(data), nil)
}

// firstTitle is the title an event opened its thread with.
func (s *Server) firstTitle(id string) string {
	for _, e := range s.events {
		if e.ID != id {
			continue
		}
		var b struct {
			Title string `json:"title"`
			Open  struct {
				Title string `json:"title"`
			} `json:"open"`
		}
		_ = json.Unmarshal(e.Body, &b)
		if b.Open.Title != "" {
			return b.Open.Title
		}
		return b.Title
	}
	return ""
}

func (s *Server) readThread(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	if _, err := fmt.Sscanf(id, "thr_%d", &n); err != nil || s.threads[n] == nil {
		reply(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"Not found."}}`, nil)
		return
	}
	t := s.threads[n]
	out := map[string]any{"id": id, "number": t.Number, "title": t.Title, "merged_into": nil}
	if t.MergedInto != "" {
		out["merged_into"] = t.MergedInto
	}
	data, _ := json.Marshal(out)
	reply(w, http.StatusOK, string(data), nil)
}

// ThreadFor returns the thread covering subject.
func (s *Server) ThreadFor(subject string) (Thread, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.subjects[subject]
	if !ok {
		return Thread{}, false
	}
	return *s.threads[n], true
}

// Rename sets a thread's title, as a person renaming it would.
func (s *Server) Rename(subject, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.subjects[subject]; ok {
		s.threads[n].Title = title
	}
}

// OpenAs opens a thread for subject as another writer would, so Beacon's events land in it.
func (s *Server) OpenAs(subject, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	num := int64(len(s.threads) + 1)
	s.threads[num] = &Thread{Number: num, Title: title}
	s.subjects[subject] = num
}

func reply(w http.ResponseWriter, status int, body string, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// Seed writes an event as if another writer had, for conflict tests.
func (s *Server) Seed(key string, body []byte) { s.write(key, body) }

// Events returns the events written, in order.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// Event returns the event with key.
func (s *Server) Event(key string) (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.byKey[key]
	if !ok {
		return Event{}, false
	}
	return s.events[i], true
}

// Requests returns every request received, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// RequestsFor returns the requests that named key.
func (s *Server) RequestsFor(key string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Key == key {
			out = append(out, r)
		}
	}
	return out
}

// Restrict sets the prefixes GET /api/restrictions lists.
func (s *Server) Restrict(prefixes ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Restricted = append([]string(nil), prefixes...)
}
