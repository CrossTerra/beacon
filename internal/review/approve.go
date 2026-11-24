package review

import (
	"context"
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
	session    string
	challenge  []byte
	credential []byte
	items      []approval.Frozen
	target     string // nonempty only for a separate settings assertion
	expires    time.Time
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
	creds, err := passkey.List(s.state)
	if err != nil || len(creds) == 0 {
		refused(w, "Resolve approval", approval.Refusal{Code: "no_passkey"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	var item held
	var cards []approvalCard
	var hashes []string
	for _, p := range proposals {
		resolved, err := s.actions.Resolve(ctx, p)
		if err != nil {
			refused(w, "Resolve approval", err)
			return
		}
		f, err := approval.Freeze(p, resolved.Binding, "")
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
	s.challenge(w, r, creds[0], item, page, func(instance string, nonce []byte) ([]byte, error) {
		return passkey.ApprovalChallenge(s.origin.URL(), instance, nonce, hashes)
	})
}

func (s *Site) challenge(w http.ResponseWriter, r *http.Request, cred passkey.Stored, item held, page ceremonyPage,
	build func(instance string, nonce []byte) ([]byte, error)) {
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
	sess, ok := s.sessions.lookup(r, s.origin, at)
	if !ok {
		http.Error(w, "passkey unavailable", http.StatusUnauthorized)
		return
	}
	item.session, item.challenge, item.credential, item.expires = sessionID(r, s.origin), challenge, cred.ID, at.Add(2*time.Minute)
	s.sessions.mu.Lock()
	if s.held == nil {
		s.held = make(map[string]held)
	}
	for k, h := range s.held {
		if !at.Before(h.expires) || h.session == item.session {
			delete(s.held, k)
		}
	}
	s.held[key] = item
	s.sessions.mu.Unlock()
	page.JSON, page.CSRF = template.JS(opts), sess.csrf
	render(w, "ceremony", http.StatusOK, page)
}

func (s *Site) takeChallenge(r *http.Request, settings bool) (held, bool) {
	at := s.now()
	key, sid := r.PostForm.Get("nonce"), sessionID(r, s.origin)
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	for k, h := range s.held {
		if !at.Before(h.expires) {
			delete(s.held, k)
		}
	}
	item, ok := s.held[key]
	if !ok || item.session != sid || settings != (item.target != "") {
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
			return item, key
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
	defer clear(key)
	batchChallenge := []byte(nil)
	if len(item.items) > 1 {
		batchChallenge = item.challenge
	}
	if s.actions.Approve != nil {
		if err := s.actions.Approve(item.items, item.credential, batchChallenge, key); err != nil {
			refused(w, "Approve actions", err)
			return
		}
	} else {
		// A trusted resolver with no dispatcher can only record an approval. The live service never supplies
		// a resolver for a missing handler, including agent.run until the supervisor joins it.
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
