package review

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/webauthn"
	"github.com/CrossTerra/beacon/internal/webauthntest"
)

// A trusted binding with no execution wiring is still recorded-only.
func (s *Site) SetResolver(fn func(approval.Proposal) (approval.Binding, error)) {
	s.SetActions(Actions{Resolve: func(_ context.Context, p approval.Proposal) (approval.Resolved, error) {
		b, err := fn(p)
		return approval.Resolved{Binding: b}, err
	}})
}

func TestAssertionApprovesTheFrozenBatchAndDoesNotStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	code, err := passkey.Issue(dir, start)
	must(t, err)
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	createCh, assertCh, _, err := passkey.Begin(dir, rp, code, start)
	must(t, err)
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	must(t, err)
	created, err := key.Register("localhost", approval.LocalOrigin, createCh)
	must(t, err)
	wrapped, err := key.Assert("localhost", approval.LocalOrigin, assertCh, false)
	must(t, err)
	prf := bytes32()
	must(t, passkey.Finish(dir, rp, passkey.Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, passkey.Ceremony{
		ClientDataJSON: wrapped.ClientDataJSON, AuthenticatorData: wrapped.AuthenticatorData,
		Signature: wrapped.Signature, CredentialID: wrapped.CredentialID, PRFOutput: append([]byte(nil), prf...),
	}, start))
	j, err := approval.OpenJournal(dir, start, "")
	must(t, err)
	t.Cleanup(func() { j.Close() })
	hash, err := passkey.VaultHash(dir)
	must(t, err)
	must(t, j.RecordPasskey(hash, start))
	s := New(makeOrigin(t, approval.LocalOrigin, ""), j, func() time.Time { return start })
	s.SetState(dir)
	s.SetResolver(func(p approval.Proposal) (approval.Binding, error) {
		return approval.Binding{Origin: "https://slack.com", Account: "slack:user:U1", Destination: approval.Object{
			"workspace": p.Params["workspace"], "channel": p.Params["channel"], "thread_ts": p.Params["thread_ts"],
		}}, nil
	})
	var ids []string
	for range 2 {
		p, err := j.Add(approval.Request{Type: approval.Post, Agent: "bosun", Reason: "Because.", Params: approval.Object{
			"workspace": "T1", "channel": "C1", "thread_ts": nil, "text": "Fixture.",
		}}, approval.PeerLabel{}, start)
		must(t, err)
		ids = append(ids, p.ID)
	}
	cookie := signIn(t, s)
	form := url.Values{"csrf": {s.sessions.active[cookie.Value].csrf}}
	form["id"] = []string{ids[1], ids[0]} // Form order cannot change the order shown on the desk.
	r := s.request("POST", "/freeze", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	r.AddCookie(cookie)
	w := request(s, r)
	expectStatus(t, w, http.StatusOK)
	page := w.Body.String()
	if i, k := strings.Index(page, ids[0]), strings.Index(page, ids[1]); i < 0 || k <= i {
		t.Fatal("freeze did not keep the pending list's order")
	}
	opts := ceremonyOptions(t, page)
	item := s.held[opts.Nonce]
	if len(item.items) != 2 || item.items[0].ID != ids[0] || item.items[1].ID != ids[1] {
		t.Fatal("assertion order differed from display order")
	}
	challenge, err := base64.RawURLEncoding.DecodeString(opts.Challenge)
	must(t, err)
	asserted, err := key.Assert("localhost", approval.LocalOrigin, challenge, false)
	must(t, err)
	post := url.Values{
		"csrf":               {s.sessions.active[cookie.Value].csrf},
		"nonce":              {opts.Nonce},
		"credential_id":      {base64.RawURLEncoding.EncodeToString(asserted.CredentialID)},
		"client_data_json":   {base64.RawURLEncoding.EncodeToString(asserted.ClientDataJSON)},
		"authenticator_data": {base64.RawURLEncoding.EncodeToString(asserted.AuthenticatorData)},
		"signature":          {base64.RawURLEncoding.EncodeToString(asserted.Signature)},
		"prf_output":         {base64.RawURLEncoding.EncodeToString(prf)},
	}
	r = s.request("POST", "/approve", strings.NewReader(post.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	r.AddCookie(cookie)
	w = request(s, r)
	expectStatus(t, w, http.StatusOK)
	for _, id := range ids {
		view, _, err := j.Get(id, start)
		must(t, err)
		if view.Status != "approved" {
			t.Fatalf("status %s", view.Status)
		}
		if _, err := j.Start(id, start); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunCannotJoinABatch(t *testing.T) {
	s, j := site(t, false, nil)
	s.SetResolver(func(approval.Proposal) (approval.Binding, error) {
		return approval.Binding{}, nil
	})
	run, err := j.Add(approval.Request{Type: approval.Run, Agent: "bosun", Reason: "Because.", Params: approval.Object{
		"checkout": "signal", "prompt": "Read it.", "model": "a/b",
	}}, approval.PeerLabel{}, start)
	must(t, err)
	post, err := j.Add(approval.Request{Type: approval.Post, Agent: "bosun", Reason: "Because.", Params: approval.Object{
		"workspace": "T1", "channel": "C1", "thread_ts": nil, "text": "Fixture.",
	}}, approval.PeerLabel{}, start)
	must(t, err)
	cookie := signIn(t, s)
	form := url.Values{"csrf": {s.sessions.active[cookie.Value].csrf}}
	form.Add("id", run.ID)
	form.Add("id", post.ID)
	r := s.request("POST", "/freeze", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	r.AddCookie(cookie)
	w := request(s, r)
	expectStatus(t, w, http.StatusBadRequest)
}

func TestFreezeNonceIsBoundToTheSessionAndANewFreezeCancelsTheOld(t *testing.T) {
	s, cookie, key, prf := registeredPreview(t)
	p := addPost(t, s)
	first := freezeNonce(t, s, cookie, p.ID)
	second := freezeNonce(t, s, cookie, p.ID)
	postApproval(t, s, cookie, key, prf, first.Nonce, first.Challenge, http.StatusConflict)
	other := signIn(t, s)
	postApproval(t, s, other, key, prf, second.Nonce, second.Challenge, http.StatusConflict)
	postApproval(t, s, cookie, key, prf, second.Nonce, second.Challenge, http.StatusOK)
}

func TestNoNonceIsHeldWhenTheCeremonyCannotBeBuilt(t *testing.T) {
	s, j := site(t, false, nil)
	s.SetResolver(func(p approval.Proposal) (approval.Binding, error) {
		return approval.Binding{Origin: "https://slack.com", Account: "slack:user:U1", Destination: approval.Object{
			"workspace": p.Params["workspace"], "channel": p.Params["channel"], "thread_ts": p.Params["thread_ts"],
		}}, nil
	})
	p := addPost(t, s)
	cookie := signIn(t, s)
	form := url.Values{"csrf": {s.sessions.active[cookie.Value].csrf}, "id": {p.ID}}
	r := s.request("POST", "/freeze", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	r.AddCookie(cookie)
	w := request(s, r)
	expectStatus(t, w, http.StatusConflict)
	if len(s.held) != 0 {
		t.Fatal("nonce held before the ceremony existed")
	}
	_ = j
}

func TestApprovalStoresANonZeroSignCount(t *testing.T) {
	s, cookie, key, prf := registeredPreview(t)
	key.SignCount = 0
	p := addPost(t, s)
	opts := freezeNonce(t, s, cookie, p.ID)
	challenge, err := base64.RawURLEncoding.DecodeString(opts.Challenge)
	must(t, err)
	asserted, err := key.Assert("localhost", approval.LocalOrigin, challenge, true)
	must(t, err)
	post := url.Values{
		"csrf":               {s.sessions.active[cookie.Value].csrf},
		"nonce":              {opts.Nonce},
		"credential_id":      {base64.RawURLEncoding.EncodeToString(asserted.CredentialID)},
		"client_data_json":   {base64.RawURLEncoding.EncodeToString(asserted.ClientDataJSON)},
		"authenticator_data": {base64.RawURLEncoding.EncodeToString(asserted.AuthenticatorData)},
		"signature":          {base64.RawURLEncoding.EncodeToString(asserted.Signature)},
		"prf_output":         {base64.RawURLEncoding.EncodeToString(prf)},
	}
	r := s.request("POST", "/approve", strings.NewReader(post.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	r.AddCookie(cookie)
	w := request(s, r)
	expectStatus(t, w, http.StatusOK)
	creds, err := passkey.List(s.state)
	must(t, err)
	if creds[0].SignCount == 0 {
		t.Fatal("sign count was not stored")
	}
	r = s.request("POST", "/approve", strings.NewReader(post.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	r.AddCookie(cookie)
	w = request(s, r)
	expectStatus(t, w, http.StatusConflict)
}

type ceremonyOpts struct{ Challenge, Nonce string }

func ceremonyOptions(t *testing.T, page string) ceremonyOpts {
	t.Helper()
	const sep = `id="ceremony">`
	_, raw, ok := strings.Cut(page, sep)
	if !ok {
		t.Fatal("missing ceremony")
	}
	raw, _, _ = strings.Cut(raw, "</script>")
	var opts ceremonyOpts
	must(t, json.Unmarshal([]byte(raw), &opts))
	return opts
}

func registeredPreview(t *testing.T) (*Site, *http.Cookie, *webauthntest.Key, []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	code, err := passkey.Issue(dir, start)
	must(t, err)
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	createCh, assertCh, _, err := passkey.Begin(dir, rp, code, start)
	must(t, err)
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	must(t, err)
	created, err := key.Register("localhost", approval.LocalOrigin, createCh)
	must(t, err)
	wrapped, err := key.Assert("localhost", approval.LocalOrigin, assertCh, false)
	must(t, err)
	prf := bytes32()
	must(t, passkey.Finish(dir, rp, passkey.Ceremony{
		ClientDataJSON: created.ClientDataJSON, AttestationObject: created.AttestationObject, PRFEnabled: true,
	}, passkey.Ceremony{
		ClientDataJSON: wrapped.ClientDataJSON, AuthenticatorData: wrapped.AuthenticatorData,
		Signature: wrapped.Signature, CredentialID: wrapped.CredentialID, PRFOutput: append([]byte(nil), prf...),
	}, start))
	j, err := approval.OpenJournal(dir, start, "")
	must(t, err)
	t.Cleanup(func() { j.Close() })
	hash, err := passkey.VaultHash(dir)
	must(t, err)
	must(t, j.RecordPasskey(hash, start))
	s := New(makeOrigin(t, approval.LocalOrigin, ""), j, func() time.Time { return start })
	s.SetState(dir)
	s.SetResolver(func(p approval.Proposal) (approval.Binding, error) {
		return approval.Binding{Origin: "https://slack.com", Account: "slack:user:U1", Destination: approval.Object{
			"workspace": p.Params["workspace"], "channel": p.Params["channel"], "thread_ts": p.Params["thread_ts"],
		}}, nil
	})
	return s, signIn(t, s), key, prf
}

func addPost(t *testing.T, s *Site) approval.Proposal {
	t.Helper()
	j := s.journal.(*approval.Journal)
	p, err := j.Add(approval.Request{Type: approval.Post, Agent: "bosun", Reason: "Because.", Params: approval.Object{
		"workspace": "T1", "channel": "C1", "thread_ts": nil, "text": "Fixture.",
	}}, approval.PeerLabel{}, start)
	must(t, err)
	return p
}

func freezeNonce(t *testing.T, s *Site, cookie *http.Cookie, id string) ceremonyOpts {
	t.Helper()
	form := url.Values{"csrf": {s.sessions.active[cookie.Value].csrf}, "id": {id}}
	r := s.request("POST", "/freeze", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	r.AddCookie(cookie)
	w := request(s, r)
	expectStatus(t, w, http.StatusOK)
	return ceremonyOptions(t, w.Body.String())
}

func postApproval(t *testing.T, s *Site, cookie *http.Cookie, key *webauthntest.Key, prf []byte, nonce, challengeB64 string, status int) {
	t.Helper()
	challenge, err := base64.RawURLEncoding.DecodeString(challengeB64)
	must(t, err)
	asserted, err := key.Assert("localhost", approval.LocalOrigin, challenge, false)
	must(t, err)
	post := url.Values{
		"csrf":               {s.sessions.active[cookie.Value].csrf},
		"nonce":              {nonce},
		"credential_id":      {base64.RawURLEncoding.EncodeToString(asserted.CredentialID)},
		"client_data_json":   {base64.RawURLEncoding.EncodeToString(asserted.ClientDataJSON)},
		"authenticator_data": {base64.RawURLEncoding.EncodeToString(asserted.AuthenticatorData)},
		"signature":          {base64.RawURLEncoding.EncodeToString(asserted.Signature)},
		"prf_output":         {base64.RawURLEncoding.EncodeToString(prf)},
	}
	r := s.request("POST", "/approve", strings.NewReader(post.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	r.AddCookie(cookie)
	w := request(s, r)
	expectStatus(t, w, status)
}
