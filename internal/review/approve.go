package review

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"maps"
	"net/http"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/webauthn"
)

type held struct {
	selection
	challenge  []byte
	credential []byte
	items      []approval.Frozen
	target     string // nonempty only for a separate settings assertion
	expires    time.Time
	pin        io.Closer
}

func (h held) close() {
	if h.pin != nil {
		_ = h.pin.Close()
	}
}

type selection struct {
	sid      string
	revision uint64
}

type approvalCard struct {
	Type approval.Action
	approval.Frozen
	AccountLabel, DestinationLabel string
	Destination, Facts, Fields     []field
	Exact                          string
	SlackPreview                   *slackPreview
}

func (s *Site) freeze(w http.ResponseWriter, r *http.Request) {
	revision, ok := s.beginSelection(r)
	if !ok {
		http.Error(w, "passkey unavailable", http.StatusUnauthorized)
		return
	}
	if s.state == "" || s.actions.Resolve == nil {
		http.Error(w, "approval unavailable", http.StatusConflict)
		return
	}
	ids := r.PostForm["id"]
	if len(ids) == 0 || len(ids) > 10 {
		http.Error(w, "select one to ten actions", http.StatusBadRequest)
		return
	}
	selection := make(map[string]bool, len(ids))
	for _, id := range ids {
		if selection[id] || !approval.ValidID(id, "apr") {
			http.Error(w, "selection refused", http.StatusBadRequest)
			return
		}
		selection[id] = true
	}
	pending, err := s.journal.Pending(s.now())
	if err != nil {
		http.Error(w, "journal unavailable", http.StatusServiceUnavailable)
		return
	}
	// The list and challenge follow the same journal order, regardless of how the form encoded its ids.
	var proposals []approval.Proposal
	for _, p := range pending {
		if selection[p.ID] {
			if len(ids) > 1 && p.Type != approval.Merge && p.Type != approval.Post {
				http.Error(w, "a run cannot join a batch", http.StatusBadRequest)
				return
			}
			if !s.supports(p.Type) {
				http.Error(w, "approval unavailable for this action", http.StatusConflict)
				return
			}
			proposals = append(proposals, p)
		}
	}
	if len(proposals) != len(ids) {
		http.Error(w, "selection no longer pending", http.StatusConflict)
		return
	}
	selected := ""
	if models, ok := r.PostForm["model"]; ok {
		if len(proposals) != 1 || proposals[0].Type != approval.Run || len(models) != 1 || models[0] == "" {
			http.Error(w, "model selection refused", http.StatusBadRequest)
			return
		}
		selected = models[0]
	}
	creds, err := passkey.List(s.state)
	if err != nil || len(creds) == 0 {
		refused(w, "Resolve approval", approval.Refusal{Code: "no_passkey"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	var item held
	defer func() { item.close() }()
	var cards []approvalCard
	var hashes []string
	for _, p := range proposals {
		candidate := p
		if selected != "" {
			candidate.Params = maps.Clone(p.Params)
			candidate.Params["model"] = selected
		}
		resolved, err := s.actions.Resolve(ctx, candidate)
		if err != nil {
			refused(w, "Resolve approval", err)
			return
		}
		if resolved.Pin != nil {
			if p.Type != approval.Run || len(proposals) != 1 {
				_ = resolved.Pin.Close()
				refused(w, "Resolve approval", approval.ErrTransition)
				return
			}
			item.pin = resolved.Pin
		}
		f, err := approval.Freeze(p, resolved.Binding, selected)
		if err != nil {
			refused(w, "Resolve approval", err)
			return
		}
		b, err := approval.Canonical(f.Params)
		if err != nil {
			refused(w, "Resolve approval", err)
			return
		}
		item.items = append(item.items, f)
		hashes = append(hashes, f.Hash)
		cards = append(cards, approvalCard{Type: p.Type, Frozen: f,
			AccountLabel: resolved.AccountLabel, DestinationLabel: resolved.DestinationLabel,
			Destination: fields(f.Destination), Facts: fields(resolved.Facts), Fields: fields(f.Params), Exact: string(b),
			SlackPreview: slackDraft(p.Type, f.Params)})
	}
	page := ceremonyPage{Action: "/approve", Title: "Approve these frozen actions", Button: "Approve with passkey", Cards: cards}
	s.challenge(w, creds[0], revision, item, page, func(instance string, nonce []byte) ([]byte, error) {
		return passkey.ApprovalChallenge(s.origin.URL(), instance, nonce, hashes)
	})
	item.pin = nil // challenge either owns it now or has closed it
}

func (s *Site) challenge(w http.ResponseWriter, cred passkey.Stored, rev selection, item held, page ceremonyPage,
	build func(instance string, nonce []byte) ([]byte, error)) {
	defer func() { item.close() }()
	j, ok := s.journal.(interface{ ID() string })
	if !ok {
		http.Error(w, "passkey unavailable", http.StatusServiceUnavailable)
		return
	}
	nonce := make([]byte, 32)
	rand.Read(nonce)
	challenge, err := build(j.ID(), nonce)
	if err != nil {
		http.Error(w, "passkey unavailable", http.StatusInternalServerError)
		return
	}
	key := base64.RawURLEncoding.EncodeToString(nonce)
	// Both operations use the same assertion UI. Only the server's held operation selects the consumer.
	opts, err := json.Marshal(map[string]string{
		"kind": "assert", "rpId": rpID(s.origin.Host()), "nonce": key,
		"challenge": base64.RawURLEncoding.EncodeToString(challenge),
		"salt":      base64.RawURLEncoding.EncodeToString(cred.Salt), "id": base64.RawURLEncoding.EncodeToString(cred.ID),
	})
	if err != nil {
		http.Error(w, "passkey unavailable", http.StatusInternalServerError)
		return
	}
	at := s.now()
	item.selection, item.challenge, item.credential, item.expires = rev, challenge, cred.ID, at.Add(2*time.Minute)
	s.sessions.mu.Lock()
	sess, ok := s.currentSelection(rev, at)
	if !ok {
		s.sessions.mu.Unlock()
		http.Error(w, "selection no longer current", http.StatusConflict)
		return
	}
	if s.held == nil {
		s.held = make(map[string]held)
	}
	s.held[key] = item
	item.pin = nil
	s.sessions.mu.Unlock()
	page.JSON, page.CSRF = template.JS(opts), sess.csrf
	render(w, "ceremony", http.StatusOK, page)
}

// Revoke as soon as an authenticated selection begins, including one that cannot resolve.
func (s *Site) beginSelection(r *http.Request) (selection, bool) {
	id, at := sessionID(r, s.origin), s.now()
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	sess, ok := s.sessions.active[id]
	if s.closing || !ok || !at.Before(sess.expires) {
		return selection{}, false
	}
	s.pruneHeld(at, id)
	sess.revision++
	s.sessions.active[id] = sess
	return selection{id, sess.revision}, true
}

func (s *Site) currentSelection(rev selection, at time.Time) (session, bool) {
	sess, ok := s.sessions.active[rev.sid]
	return sess, !s.closing && ok && at.Before(sess.expires) && sess.revision == rev.revision
}

// pruneHeld runs with sessions.mu held. An empty sid prunes expired items only.
func (s *Site) pruneHeld(at time.Time, sid string) {
	for k, h := range s.held {
		if _, current := s.currentSelection(h.selection, at); !current || h.sid == sid || !at.Before(h.expires) {
			delete(s.held, k)
			h.close()
		}
	}
}

func (s *Site) ExpireChallenges(at time.Time) {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	s.pruneHeld(at, "")
}

func (s *Site) CloseChallenges() {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	s.closing = true
	s.pruneHeld(s.now(), "")
}

func (s *Site) takeChallenge(r *http.Request, settings bool) (held, bool) {
	at := s.now()
	key, sid := r.PostForm.Get("nonce"), sessionID(r, s.origin)
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	s.pruneHeld(at, "")
	item, ok := s.held[key]
	if !ok || item.sid != sid || settings != (item.target != "") {
		return held{}, false
	}
	delete(s.held, key)
	return item, true
}

func (s *Site) openAssertion(w http.ResponseWriter, r *http.Request, settings bool) (held, []byte) {
	item, ok := s.takeChallenge(r, settings)
	if !ok {
		http.Error(w, "passkey challenge refused", http.StatusConflict)
		return held{}, nil
	}
	defer func() { item.close() }()
	creds, err := passkey.List(s.state)
	if err != nil {
		http.Error(w, "passkey assertion refused", http.StatusConflict)
		return held{}, nil
	}
	raw := func(name string) []byte {
		b, err := base64.RawURLEncoding.Strict().DecodeString(r.PostForm.Get(name))
		if err != nil {
			return nil
		}
		return b
	}
	id := raw("credential_id")
	rp := webauthn.RelyingParty{ID: rpID(s.origin.Host()), Origin: s.origin.URL()}
	for _, cred := range creds {
		if subtle.ConstantTimeCompare(cred.ID, id) != 1 || subtle.ConstantTimeCompare(item.credential, id) != 1 {
			continue
		}
		assertion, err := webauthn.VerifyAssertion(rp, item.challenge, cred.Credential(), id,
			raw("client_data_json"), raw("authenticator_data"), raw("signature"))
		if err != nil || passkey.NoteSignCount(s.state, id, assertion.SignCount) != nil {
			break
		}
		prf := raw("prf_output")
		delete(r.PostForm, "prf_output")
		delete(r.Form, "prf_output")
		defer clear(prf)
		key, err := passkey.OpenVault(s.state, id, prf, rp.Origin)
		if errors.Is(err, passkey.ErrReset) {
			http.Error(w, "stop beacon serve and run beacon setup --reset-passkeys", http.StatusConflict)
			return held{}, nil
		}
		if err == nil {
			s.sessions.mu.Lock()
			_, current := s.currentSelection(item.selection, s.now())
			s.sessions.mu.Unlock()
			if !current {
				clear(key)
				http.Error(w, "selection no longer current", http.StatusConflict)
				return held{}, nil
			}
			authorized := item
			item.pin = nil
			return authorized, key
		}
		break
	}
	http.Error(w, "passkey assertion refused", http.StatusUnauthorized)
	return held{}, nil
}

func (s *Site) consume(w http.ResponseWriter, r *http.Request) {
	item, key := s.openAssertion(w, r, false)
	if key == nil {
		return
	}
	defer item.close()
	defer clear(key)
	batchChallenge := []byte(nil)
	if len(item.items) > 1 {
		batchChallenge = item.challenge
	}
	if s.actions.Approve != nil {
		if err := s.actions.Approve(item.items, item.credential, batchChallenge, key, item.pin); err != nil {
			refused(w, "Approve actions", err)
			return
		}
	} else {
		// A trusted resolver with no dispatcher can only record an approval. The live service never supplies
		// a resolver for a missing handler.
		j, ok := s.journal.(interface {
			ApproveAll([]approval.Frozen, []byte, []byte, time.Time) error
		})
		if !ok || j.ApproveAll(item.items, item.credential, batchChallenge, s.now()) != nil {
			http.Error(w, "approval refused", http.StatusConflict)
			return
		}
	}
	render(w, "submitted", http.StatusOK, item.items)
}

func refused(w http.ResponseWriter, operation string, err error) {
	r := approval.Refusal{Code: "unavailable"}
	_ = errors.As(err, &r)
	if errors.Is(err, approval.ErrStale) || errors.Is(err, approval.ErrTransition) {
		r.Code = "selection_changed"
	}
	render(w, "refused", http.StatusConflict, struct {
		Operation string
		approval.Refusal
	}{operation, r})
}
