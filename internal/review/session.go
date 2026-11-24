package review

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

const (
	codeLife    = 2 * time.Minute
	sessionLife = 12 * time.Hour
	tokenBytes  = 32
	maxCodes    = 64
	maxSessions = 128
)

type session struct {
	csrf    string
	expires time.Time
}

// Sessions belong to this service instance, not to the state directory or the journal.
type sessions struct {
	mu     sync.Mutex
	codes  map[string]time.Time
	active map[string]session
}

func token() string {
	var b [tokenBytes]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (s *sessions) issue(at time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for code, expiry := range s.codes {
		if !at.Before(expiry) {
			delete(s.codes, code)
		}
	}
	if len(s.codes) >= maxCodes {
		return "", false
	}
	code := token()
	s.codes[code] = at.Add(codeLife)
	return code, true
}

func (s *sessions) exchange(code string, at time.Time) (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expiry, ok := s.codes[code]; !ok || !at.Before(expiry) {
		delete(s.codes, code)
		return "", http.StatusUnauthorized
	}
	for id, session := range s.active {
		if !at.Before(session.expires) {
			delete(s.active, id)
		}
	}
	if len(s.active) >= maxSessions {
		return "", http.StatusServiceUnavailable
	}
	delete(s.codes, code)
	id := token()
	s.active[id] = session{csrf: token(), expires: at.Add(sessionLife)}
	return id, http.StatusSeeOther
}

func cookieName(o approval.Origin) string {
	if o.Secure() {
		return "__Host-beacon"
	}
	return "beacon_" + approval.ReviewPort
}

func sessionID(r *http.Request, o approval.Origin) string {
	cookies := r.CookiesNamed(cookieName(o))
	if len(cookies) != 1 || len(cookies[0].Value) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return ""
	}
	return cookies[0].Value
}

func (s *sessions) lookup(r *http.Request, o approval.Origin, at time.Time) (session, bool) {
	cookies := r.CookiesNamed(cookieName(o))
	if len(cookies) != 1 || len(cookies[0].Value) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := cookies[0].Value
	value, ok := s.active[id]
	if ok && !at.Before(value.expires) {
		delete(s.active, id)
		ok = false
	}
	return value, ok
}
