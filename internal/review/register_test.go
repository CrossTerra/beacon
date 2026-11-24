package review

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/webauthntest"
)

func TestRegisterPageConsumesTheSetupCode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	code, err := passkey.Issue(dir, start)
	must(t, err)
	j, err := approval.OpenJournal(dir, start, "")
	must(t, err)
	t.Cleanup(func() { j.Close() })
	s := New(makeOrigin(t, approval.LocalOrigin, ""), j, func() time.Time { return start })
	s.SetState(dir)
	body := url.Values{"code": {code}}
	r := s.request("POST", "/passkeys/new", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	w := request(s, r)
	expectStatus(t, w, http.StatusOK)
	var opts struct{ RpId, Create, Assert, Salt string }
	raw := w.Body.String()
	i := strings.Index(raw, `id="ceremony">`)
	if i < 0 {
		t.Fatal("no ceremony")
	}
	raw = raw[i+len(`id="ceremony">`):]
	raw = raw[:strings.Index(raw, "</script>")]
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		t.Fatal(err)
	}
	createCh, err := base64.RawURLEncoding.DecodeString(opts.Create)
	must(t, err)
	assertCh, err := base64.RawURLEncoding.DecodeString(opts.Assert)
	must(t, err)
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	must(t, err)
	created, err := key.Register(opts.RpId, s.origin.URL(), createCh)
	must(t, err)
	asserted, err := key.Assert(opts.RpId, s.origin.URL(), assertCh, false)
	must(t, err)
	form := url.Values{
		"credential_id":      {base64.RawURLEncoding.EncodeToString(created.CredentialID)},
		"create_client_data": {base64.RawURLEncoding.EncodeToString(created.ClientDataJSON)},
		"attestation_object": {base64.RawURLEncoding.EncodeToString(created.AttestationObject)},
		"prf_enabled":        {"true"},
		"assert_client_data": {base64.RawURLEncoding.EncodeToString(asserted.ClientDataJSON)},
		"authenticator_data": {base64.RawURLEncoding.EncodeToString(asserted.AuthenticatorData)},
		"signature":          {base64.RawURLEncoding.EncodeToString(asserted.Signature)},
		"prf_output":         {base64.RawURLEncoding.EncodeToString(bytes32())},
	}
	r = s.request("POST", "/passkeys", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.origin.URL())
	w = request(s, r)
	expectStatus(t, w, http.StatusOK)
	if _, err := os.Lstat(filepath.Join(s.state, "passkeys.json")); err != nil {
		t.Fatal(err)
	}
}

func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = 0x11
	}
	return b
}
