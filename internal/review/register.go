package review

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/webauthn"
)

func (s *Site) register(w http.ResponseWriter, r *http.Request) {
	if s.state == "" {
		http.Error(w, "registration unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if r.ParseForm() != nil {
		http.Error(w, "form refused", http.StatusBadRequest)
		return
	}
	rp := webauthn.RelyingParty{ID: rpID(s.origin.Host()), Origin: s.origin.URL()}
	if r.URL.Path == "/passkeys/new" {
		create, assert, salt, err := passkey.Begin(s.state, rp, r.PostForm.Get("code"), s.now())
		if err != nil {
			http.Error(w, "setup code refused", http.StatusUnauthorized)
			return
		}
		opts, err := json.Marshal(map[string]string{
			"rpId": rp.ID, "userId": userID(rp.ID),
			"create": base64.RawURLEncoding.EncodeToString(create),
			"assert": base64.RawURLEncoding.EncodeToString(assert),
			"salt":   base64.RawURLEncoding.EncodeToString(salt),
		})
		if err != nil {
			http.Error(w, "registration unavailable", http.StatusInternalServerError)
			return
		}
		render(w, "ceremony", http.StatusOK, ceremonyPage{JSON: template.JS(opts), Action: "/passkeys",
			Title: "Register a passkey", Button: "Register passkey"})
		return
	}
	raw := func(name string) []byte {
		b, err := base64.RawURLEncoding.Strict().DecodeString(r.PostForm.Get(name))
		if err != nil {
			return nil
		}
		return b
	}
	err := passkey.Finish(s.state, rp, passkey.Ceremony{
		ClientDataJSON: raw("create_client_data"), AttestationObject: raw("attestation_object"),
		PRFEnabled: r.PostForm.Get("prf_enabled") == "true",
	}, passkey.Ceremony{
		ClientDataJSON: raw("assert_client_data"), AuthenticatorData: raw("authenticator_data"),
		Signature: raw("signature"), CredentialID: raw("credential_id"), PRFOutput: raw("prf_output"),
	}, s.now())
	if err != nil {
		if errors.Is(err, approval.ErrJournalIO) || errors.Is(err, approval.ErrJournalUnhealthy) || errors.Is(err, approval.ErrLocked) {
			http.Error(w, "registration unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "registration refused", http.StatusUnauthorized)
		return
	}
	render(w, "registered", http.StatusOK, nil)
}

type ceremonyPage struct {
	JSON                        template.JS
	CSRF, Action, Title, Button string
	Cards                       []approvalCard
	Target                      *CredentialTarget
}

func rpID(host string) string {
	if host == "localhost:7317" {
		return "localhost"
	}
	return host
}

func userID(rpID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("beacon-user:" + rpID))
}
