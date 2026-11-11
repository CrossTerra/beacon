// Package signaltest is a fake Signal for tests. It implements POST /api/events with Signal's key rules: the same
// key and the same bytes replay (200), the same key and other bytes are 409 idempotency_conflict.
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
}

// MeActor is the actor GET /api/me returns for a token.
type MeActor struct {
	ID, Type, Handle, WorksFor string
}

// New starts a fake that accepts the bearer token.
func New(token string) *Server {
	s := &Server{Token: token, byKey: map[string]int{}}
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
	if r.URL.Path != "/api/events" || r.Method != http.MethodPost {
		reply(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"Not found."}}`, nil)
		return
	}
	if !json.Valid(body) {
		reply(w, http.StatusBadRequest, `{"error":{"code":"invalid","message":"Body is not JSON."}}`, nil)
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
	return http.StatusCreated, fmt.Sprintf(`{"event":{"id":%q},"replayed":false}`, ev.ID)
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
