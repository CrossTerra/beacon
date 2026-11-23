package review

import (
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
	for _, id := range ids {
		form.Add("id", id)
	}
	r := s.request("POST", "/freeze", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	r.AddCookie(cookie)
	w := request(s, r)
	expectStatus(t, w, http.StatusOK)
	var opts struct{ Challenge, Nonce string }
	raw := w.Body.String()
	raw = raw[strings.Index(raw, `id="ceremony">`)+len(`id="ceremony">`):]
	raw = raw[:strings.Index(raw, "</script>")]
	must(t, json.Unmarshal([]byte(raw), &opts))
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
