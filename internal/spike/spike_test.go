package spike

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CrossTerra/beacon/internal/webauthntest"
)

func TestOriginChecks(t *testing.T) {
	s := newServer(t, "winton@example.com")
	h := s.Handler()

	t.Run("localhost", func(t *testing.T) {
		rec := get(t, h, "localhost:7317", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Beacon passkey spike") {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("headers %v", rec.Header())
		}
		if rec.Header().Get("Strict-Transport-Security") != "" {
			t.Fatal("HSTS on localhost")
		}
	})
	t.Run("wrong host", func(t *testing.T) {
		rec := get(t, h, "127.0.0.1:7317", "")
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), `id="reason"`) || !strings.Contains(rec.Body.String(), "Host") {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("localhost with tailscale login", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:7317/", nil)
		req.Host = "localhost:7317"
		req.Header.Set("Tailscale-User-Login", "winton@example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "must not carry") {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("ts.net", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://mac.tailnet.ts.net/", nil)
		req.Host = "mac.tailnet.ts.net"
		req.Header.Set("Tailscale-User-Login", "winton@example.com")
		req.Header.Set("Tailscale-User-Name", "Winton")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "winton@example.com") || !strings.Contains(rec.Body.String(), "Winton") {
			t.Fatal("headers not shown")
		}
		if rec.Header().Get("Strict-Transport-Security") == "" {
			t.Fatal("missing HSTS")
		}
		if !strings.Contains(s.log.String(), "Tailscale-User-Login") || !strings.Contains(s.log.String(), "winton@example.com") {
			t.Fatal("stdout missing headers")
		}
	})
	t.Run("funnel", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://mac.tailnet.ts.net/", nil)
		req.Host = "mac.tailnet.ts.net"
		req.Header.Set("Tailscale-Funnel-Request", "?1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Funnel") {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("tagged", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://mac.tailnet.ts.net/", nil)
		req.Host = "mac.tailnet.ts.net"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Tailscale-User-Login") {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("other login", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://mac.tailnet.ts.net/", nil)
		req.Host = "mac.tailnet.ts.net"
		req.Header.Set("Tailscale-User-Login", "other@example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "not the login") {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("post without origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:7317/register", strings.NewReader("ceremony=x"))
		req.Host = "localhost:7317"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("post wrong origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:7317/register", strings.NewReader("ceremony=x"))
		req.Host = "localhost:7317"
		req.Header.Set("Origin", "http://127.0.0.1:7317")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Origin") {
			t.Fatalf("status %d", rec.Code)
		}
	})
}

func TestRegisterAssertAndPRFHash(t *testing.T) {
	s := newServer(t, "")
	h := s.Handler()
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	key.Backup(true)

	rec := get(t, h, "localhost:7317", "/register")
	id, challenge := ceremony(t, rec.Body.String())
	reg, err := key.Register("localhost", "http://localhost:7317", challenge)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("ceremony", id)
	form.Set("credential_id", base64.RawURLEncoding.EncodeToString(reg.CredentialID))
	form.Set("client_data_json", base64.RawURLEncoding.EncodeToString(reg.ClientDataJSON))
	form.Set("attestation_object", base64.RawURLEncoding.EncodeToString(reg.AttestationObject))
	form.Set("prf_enabled", "true")
	rec = post(t, h, "localhost:7317", "http://localhost:7317", "/register", form)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "PRF is enabled") {
		t.Fatalf("register status %d body %s", rec.Code, rec.Body.String())
	}

	// A credential that does not report PRF is not stored.
	rec = get(t, h, "localhost:7317", "/register?prf=0")
	id, challenge = ceremony(t, rec.Body.String())
	other, err := webauthntest.Generate(webauthntest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	reg2, err := other.Register("localhost", "http://localhost:7317", challenge)
	if err != nil {
		t.Fatal(err)
	}
	form = url.Values{}
	form.Set("ceremony", id)
	form.Set("credential_id", base64.RawURLEncoding.EncodeToString(reg2.CredentialID))
	form.Set("client_data_json", base64.RawURLEncoding.EncodeToString(reg2.ClientDataJSON))
	form.Set("attestation_object", base64.RawURLEncoding.EncodeToString(reg2.AttestationObject))
	form.Set("prf_enabled", "false")
	rec = post(t, h, "localhost:7317", "http://localhost:7317", "/register", form)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "did not enable the PRF") {
		t.Fatalf("no prf status %d", rec.Code)
	}
	home := get(t, h, "localhost:7317", "/")
	if strings.Count(home.Body.String(), "<tr><td><code>") != 1 {
		t.Fatalf("stored more than the PRF credential:\n%s", home.Body.String())
	}

	rec = get(t, h, "localhost:7317", "/assert")
	id, challenge = ceremony(t, rec.Body.String())
	assertion, err := key.Assert("localhost", "http://localhost:7317", challenge, false)
	if err != nil {
		t.Fatal(err)
	}
	prf := bytes.Repeat([]byte{0x11}, 32)
	form = url.Values{}
	form.Set("ceremony", id)
	form.Set("credential_id", base64.RawURLEncoding.EncodeToString(assertion.CredentialID))
	form.Set("client_data_json", base64.RawURLEncoding.EncodeToString(assertion.ClientDataJSON))
	form.Set("authenticator_data", base64.RawURLEncoding.EncodeToString(assertion.AuthenticatorData))
	form.Set("signature", base64.RawURLEncoding.EncodeToString(assertion.Signature))
	form.Set("prf_output", base64.RawURLEncoding.EncodeToString(prf))
	rec = post(t, h, "localhost:7317", "http://localhost:7317", "/assert", form)
	sum := sha256.Sum256(prf)
	want := hex.EncodeToString(sum[:])
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("assert status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), form.Get("prf_output")) || strings.Contains(s.log.String(), form.Get("prf_output")) {
		t.Fatal("raw PRF output was shown or logged")
	}
	if strings.Contains(s.log.String(), "attestation_object") || strings.Contains(s.log.String(), "client_data_json") {
		t.Fatal("request body logged")
	}

	rec = get(t, h, "localhost:7317", "/assert")
	id, challenge = ceremony(t, rec.Body.String())
	assertion, err = key.Assert("localhost", "http://localhost:7317", challenge, false)
	if err != nil {
		t.Fatal(err)
	}
	form.Set("ceremony", id)
	form.Set("client_data_json", base64.RawURLEncoding.EncodeToString(assertion.ClientDataJSON))
	form.Set("authenticator_data", base64.RawURLEncoding.EncodeToString(assertion.AuthenticatorData))
	form.Set("signature", base64.RawURLEncoding.EncodeToString(assertion.Signature))
	rec = post(t, h, "localhost:7317", "http://localhost:7317", "/assert", form)
	if !strings.Contains(rec.Body.String(), "Matches the PRF hash") || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("second assert body %s", rec.Body.String())
	}
}

func TestWrongWebAuthnOrigin(t *testing.T) {
	s := newServer(t, "")
	h := s.Handler()
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, "localhost:7317", "/register")
	id, challenge := ceremony(t, rec.Body.String())
	// Signed for a different origin. The HTTP Origin header is correct, so the
	// refusal is the verifier's, not the Host check.
	reg, err := key.Register("localhost", "http://127.0.0.1:7317", challenge)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("ceremony", id)
	form.Set("credential_id", base64.RawURLEncoding.EncodeToString(reg.CredentialID))
	form.Set("client_data_json", base64.RawURLEncoding.EncodeToString(reg.ClientDataJSON))
	form.Set("attestation_object", base64.RawURLEncoding.EncodeToString(reg.AttestationObject))
	form.Set("prf_enabled", "true")
	rec = post(t, h, "localhost:7317", "http://localhost:7317", "/register", form)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Origin is not the canonical") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestListenLoopback(t *testing.T) {
	lns, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, ln := range lns {
			_ = ln.Close()
		}
	})
	if len(lns) != 2 {
		t.Fatalf("listeners %d", len(lns))
	}
	for _, ln := range lns {
		tcp, ok := ln.Addr().(*net.TCPAddr)
		if !ok || !tcp.IP.IsLoopback() || tcp.Port != 7317 {
			t.Fatalf("addr %v", ln.Addr())
		}
	}
	if _, err := Listen(); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("second listen: %v", err)
	}
}

func TestSymlinkDataDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{DataDir: link}); err == nil {
		t.Fatal("symlink data dir accepted")
	}
}

type testServer struct {
	*Server
	log *bytes.Buffer
}

func newServer(t *testing.T, login string) *testServer {
	t.Helper()
	var buf bytes.Buffer
	s, err := New(Config{DataDir: t.TempDir(), TailscaleLogin: login, Log: &buf})
	if err != nil {
		t.Fatal(err)
	}
	return &testServer{Server: s, log: &buf}
}

func get(t *testing.T, h http.Handler, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	if path == "" {
		path = "/"
	}
	req := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(t *testing.T, h http.Handler, host, origin, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://"+host+path, strings.NewReader(form.Encode()))
	req.Host = host
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ceremony(t *testing.T, body string) (id string, challenge []byte) {
	t.Helper()
	const mark = `name="ceremony" value="`
	i := strings.Index(body, mark)
	if i < 0 {
		t.Fatalf("no ceremony in %s", body)
	}
	rest := body[i+len(mark):]
	id = rest[:strings.Index(rest, `"`)]
	const cmark = `"challenge":"`
	j := strings.Index(body, cmark)
	if j < 0 {
		t.Fatalf("no challenge in:\n%s", body)
	}
	rest = body[j+len(cmark):]
	enc := rest[:strings.Index(rest, `"`)]
	challenge, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	return id, challenge
}

func TestLogIsJSON(t *testing.T) {
	s := newServer(t, "")
	get(t, s.Handler(), "localhost:7317", "/")
	line := s.log.String()
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &got); err != nil {
		t.Fatal(err)
	}
	if got["event"] != "spike_request" || got["host"] != "localhost:7317" {
		t.Fatalf("%v", got)
	}
	if _, err := io.ReadAll(strings.NewReader(line)); err != nil {
		t.Fatal(err)
	}
}
