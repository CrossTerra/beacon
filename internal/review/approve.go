package review

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/webauthn"
)

type held struct {
	nonce   []byte
	session string
	items   []approval.Frozen
	expires time.Time
}

func (s *Site) decide(w http.ResponseWriter, r *http.Request) {
	if s.state == "" || s.resolve == nil {
		http.Error(w, "approval unavailable", http.StatusConflict)
		return
	}
	if r.URL.Path == "/freeze" {
		s.freeze(w, r)
		return
	}
	s.consume(w, r)
}

func (s *Site) freeze(w http.ResponseWriter, r *http.Request) {
	ids := r.PostForm["id"]
	if len(ids) == 0 || len(ids) > 10 {
		http.Error(w, "selection refused", http.StatusBadRequest)
		return
	}
	at := s.now()
	var frozen []approval.Frozen
	var run bool
	for _, id := range ids {
		view, _, err := s.journal.Get(id, at)
		if err != nil || view.Status != "pending" {
			http.Error(w, "selection refused", http.StatusConflict)
			return
		}
		if view.Type == approval.Run {
			run = true
		}
		if run && len(ids) != 1 {
			http.Error(w, "a run cannot join a batch", http.StatusBadRequest)
			return
		}
		binding, err := s.resolve(view.Proposal)
		if err != nil {
			http.Error(w, "account not resolved", http.StatusConflict)
			return
		}
		selected := ""
		if view.Type == approval.Run {
			selected = textParam(view.Params, "model")
		}
		f, err := approval.Freeze(view.Proposal, binding, selected)
		if err != nil {
			http.Error(w, "selection refused", http.StatusConflict)
			return
		}
		frozen = append(frozen, f)
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		http.Error(w, "approval unavailable", http.StatusServiceUnavailable)
		return
	}
	creds, err := passkey.List(s.state)
	if err != nil || len(creds) == 0 {
		http.Error(w, "no passkey", http.StatusConflict)
		return
	}
	journal, ok := s.journal.(interface{ ID() string })
	if !ok {
		http.Error(w, "approval unavailable", http.StatusServiceUnavailable)
		return
	}
	hashes := make([]string, len(frozen))
	for i, f := range frozen {
		hashes[i] = f.Hash
	}
	challenge, err := passkey.ApprovalChallenge(s.origin.URL(), journal.ID(), nonce, hashes)
	if err != nil {
		http.Error(w, "approval unavailable", http.StatusInternalServerError)
		return
	}
	opts, err := json.Marshal(map[string]any{
		"kind": "approve", "rpId": rpID(s.origin.Host()),
		"nonce":     base64.RawURLEncoding.EncodeToString(nonce),
		"challenge": base64.RawURLEncoding.EncodeToString(challenge),
		"salt":      base64.RawURLEncoding.EncodeToString(creds[0].Salt),
		"id":        base64.RawURLEncoding.EncodeToString(creds[0].ID),
	})
	if err != nil {
		http.Error(w, "approval unavailable", http.StatusInternalServerError)
		return
	}
	sid := sessionID(r, s.origin)
	if sid == "" {
		http.Error(w, "approval unavailable", http.StatusUnauthorized)
		return
	}
	s.sessions.mu.Lock()
	if s.held == nil {
		s.held = map[string]held{}
	}
	for k, h := range s.held {
		if !at.Before(h.expires) || h.session == sid {
			delete(s.held, k)
		}
	}
	s.held[base64.RawURLEncoding.EncodeToString(nonce)] = held{nonce: nonce, session: sid, items: frozen, expires: at.Add(2 * time.Minute)}
	s.sessions.mu.Unlock()
	csrf := ""
	if sess, ok := s.sessions.lookup(r, s.origin, at); ok {
		csrf = sess.csrf
	}
	render(w, "ceremony", http.StatusOK, ceremonyPage{JSON: template.JS(opts), CSRF: csrf, Action: "/approve"})
}

func (s *Site) consume(w http.ResponseWriter, r *http.Request) {
	key := r.PostForm.Get("nonce")
	sid := sessionID(r, s.origin)
	at := s.now()
	s.sessions.mu.Lock()
	if s.held != nil {
		for k, h := range s.held {
			if !at.Before(h.expires) {
				delete(s.held, k)
			}
		}
	}
	item, ok := s.held[key]
	if !ok || item.session != sid || !at.Before(item.expires) {
		s.sessions.mu.Unlock()
		http.Error(w, "approval refused", http.StatusConflict)
		return
	}
	delete(s.held, key)
	s.sessions.mu.Unlock()
	hashes := make([]string, len(item.items))
	for i, f := range item.items {
		hashes[i] = f.Hash
	}
	j, ok := s.journal.(interface{ ID() string })
	if !ok {
		http.Error(w, "approval unavailable", http.StatusServiceUnavailable)
		return
	}
	challenge, err := passkey.ApprovalChallenge(s.origin.URL(), j.ID(), item.nonce, hashes)
	if err != nil {
		http.Error(w, "approval refused", http.StatusConflict)
		return
	}
	raw := func(name string) []byte {
		b, err := base64.RawURLEncoding.Strict().DecodeString(r.PostForm.Get(name))
		if err != nil {
			return nil
		}
		return b
	}
	creds, err := passkey.List(s.state)
	if err != nil {
		http.Error(w, "approval refused", http.StatusConflict)
		return
	}
	id := raw("credential_id")
	var cred passkey.Stored
	found := false
	for _, c := range creds {
		if subtle.ConstantTimeCompare(c.ID, id) == 1 {
			cred, found = c, true
		}
	}
	if !found {
		http.Error(w, "approval refused", http.StatusConflict)
		return
	}
	assertion, err := webauthn.VerifyAssertion(webauthn.RelyingParty{ID: rpID(s.origin.Host()), Origin: s.origin.URL()}, challenge, cred.Credential(), id, raw("client_data_json"), raw("authenticator_data"), raw("signature"))
	if err != nil {
		http.Error(w, "approval refused", http.StatusUnauthorized)
		return
	}
	if err := passkey.NoteSignCount(s.state, id, assertion.SignCount); err != nil {
		http.Error(w, "approval refused", http.StatusUnauthorized)
		return
	}
	keyBytes, err := passkey.OpenVault(s.state, id, raw("prf_output"), s.origin.URL())
	if errors.Is(err, passkey.ErrReset) {
		http.Error(w, "stop beacon serve and run beacon setup --reset-passkeys", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "approval refused", http.StatusUnauthorized)
		return
	}
	for i := range keyBytes {
		keyBytes[i] = 0
	}
	approver, ok := s.journal.(interface {
		ApproveAll([]approval.Frozen, []byte, []byte, time.Time) error
	})
	if !ok {
		http.Error(w, "approval unavailable", http.StatusServiceUnavailable)
		return
	}
	batchChallenge := []byte(nil)
	if len(item.items) > 1 {
		batchChallenge = challenge
	}
	if err := approver.ApproveAll(item.items, id, batchChallenge, s.now()); err != nil {
		http.Error(w, "approval refused", http.StatusConflict)
		return
	}
	render(w, "sign-in", http.StatusOK, false)
}

func textParam(o approval.Object, key string) string {
	s, _ := o[key].(string)
	return s
}
