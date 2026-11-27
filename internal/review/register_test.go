package review

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestExpiredOrUsedSetupCodeNamesTheReissueCommand(t *testing.T) {
	for _, expired := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "state")
		code, err := passkey.Issue(dir, start)
		must(t, err)
		at := start
		if expired {
			at = start.Add(10 * time.Minute)
		}
		j, err := approval.OpenJournal(dir, at, "")
		must(t, err)
		s := New(makeOrigin(t, approval.LocalOrigin, ""), j, func() time.Time { return at })
		s.SetState(dir)
		t.Cleanup(func() { must(t, j.Close()) })
		var logged bytes.Buffer
		s.SetDiagnostics(&logged)
		post := func() *httptest.ResponseRecorder {
			r := s.request("POST", "/passkeys/new", strings.NewReader(url.Values{"code": {code}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", s.origin.URL())
			return request(s, r)
		}
		if !expired {
			expectStatus(t, post(), http.StatusOK)
		}
		w := post()
		expectStatus(t, w, http.StatusUnauthorized)
		body := w.Body.String()
		if !strings.Contains(body, "./scripts/install.sh --reissue-code") || !strings.Contains(body, "expired, or already used") ||
			strings.Contains(body, code) || logged.String() != "passkey.register.failed reason=setup_code_refused\n" {
			t.Fatal("setup refusal did not provide safe recovery guidance")
		}
	}
}

func TestRegistrationReportOnlyLogsFixedCodes(t *testing.T) {
	s, _ := site(t, true, nil)
	var logged bytes.Buffer
	s.SetDiagnostics(&logged)
	post := func(body string) *http.Request {
		r := s.request("POST", "/passkeys/report", strings.NewReader(body))
		r.Header.Set("Origin", s.origin.URL())
		r.Header.Set("Content-Type", "text/plain; charset=UTF-8")
		r.Header.Set("User-Agent", "fixture-UA-must-not-log")
		return r
	}
	r := post("SecurityError")
	r.Header.Set("Origin", "https://elsewhere.example.test")
	expectStatus(t, request(s, r), http.StatusForbidden)
	r = post("SecurityError")
	r.Header.Del(tailLogin)
	expectStatus(t, request(s, r), http.StatusForbidden)
	expectStatus(t, request(s, post(strings.Repeat("x", 65))), http.StatusBadRequest)
	if logged.Len() != 0 {
		t.Fatal("a refused diagnostic was logged")
	}
	for _, body := range []string{"NotAllowedError", "NotAllowedError", "bad\nfixture-secret-not-for-log", "prf_missing"} {
		w := request(s, post(body))
		expectStatus(t, w, http.StatusNoContent)
		if w.Body.Len() != 0 || w.Header().Get("Set-Cookie") != "" {
			t.Fatal("diagnostics created a session or reflected request data")
		}
	}
	const want = "passkey.register.failed reason=NotAllowedError\npasskey.register.failed reason=other\npasskey.register.failed reason=prf_missing\n"
	if logged.String() != want {
		t.Fatal("expected fixed, rate-limited reason codes only")
	}
}

func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = 0x11
	}
	return b
}
