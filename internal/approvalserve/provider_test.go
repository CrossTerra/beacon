package approvalserve

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/passkey"
	"github.com/CrossTerra/beacon/internal/webauthn"
	"github.com/CrossTerra/beacon/internal/webauthntest"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func registerFixturePasskey(t *testing.T, dir string) (*webauthntest.Key, []byte) {
	t.Helper()
	code, err := passkey.Issue(dir, proposedAt)
	must(t, err)
	j, err := approval.OpenJournal(dir, proposedAt, "")
	must(t, err)
	defer j.Close()
	rp := webauthn.RelyingParty{ID: "localhost", Origin: approval.LocalOrigin}
	create, get, _, err := passkey.Begin(dir, rp, code, proposedAt)
	must(t, err)
	key, err := webauthntest.Generate(webauthntest.AlgES256)
	must(t, err)
	a, err := key.Register(rp.ID, rp.Origin, create)
	must(t, err)
	b, err := key.Assert(rp.ID, rp.Origin, get, false)
	must(t, err)
	prf := bytes.Repeat([]byte{73}, 32)
	must(t, passkey.Finish(dir, rp, passkey.Ceremony{
		ClientDataJSON: a.ClientDataJSON, AttestationObject: a.AttestationObject, PRFEnabled: true,
	}, passkey.Ceremony{
		ClientDataJSON: b.ClientDataJSON, AuthenticatorData: b.AuthenticatorData, CredentialID: b.CredentialID,
		Signature: b.Signature, PRFOutput: bytes.Clone(prf),
	}, proposedAt))
	return key, prf
}

type browser struct {
	t       *testing.T
	client  *http.Client
	address string
	cookie  *http.Cookie
	csrf    string
}

func launchBrowser(t *testing.T, opts Options) (browser, func()) {
	t.Helper()
	b, stop := signInBrowser(t, opts)
	_, page := b.request(http.MethodGet, "/settings", nil, 200)
	b.csrf = capture(t, page, `name="csrf" value="([a-zA-Z0-9_-]+)"`)
	return b, stop
}

func signInBrowser(t *testing.T, opts Options) (browser, func()) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	must(t, err)
	opts.ListenReview = func() ([]net.Listener, error) { return []net.Listener{l}, nil }
	stop := start(t, opts)
	b := browser{t: t, address: "http://" + l.Addr().String(), client: &http.Client{Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	t.Cleanup(b.client.CloseIdleConnections)
	var wire []byte
	for deadline := time.Now().Add(5 * time.Second); ; {
		wire, err = actsocket.Call(t.Context(), opts.StateDir, approval.Object{"op": "open"}, 0)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket not ready:", err)
		}
		time.Sleep(time.Millisecond)
	}
	var link struct{ Code string }
	must(t, json.Unmarshal(wire, &link))
	res, _ := b.request(http.MethodGet, "/open?code="+url.QueryEscape(link.Code), nil, 303)
	b.cookie = res.Cookies()[0]
	return b, stop
}

func capture(t *testing.T, page, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(page)
	if len(m) != 2 {
		t.Fatalf("missing expected element %q", pattern)
	}
	return m[1]
}

func (b browser) request(method, path string, v url.Values, want int) (*http.Response, string) {
	b.t.Helper()
	r, err := http.NewRequestWithContext(b.t.Context(), method, b.address+path, strings.NewReader(v.Encode()))
	must(b.t, err)
	r.Host = approval.LocalHost
	if b.cookie != nil {
		r.AddCookie(b.cookie)
	}
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", approval.LocalOrigin)
	}
	res, err := b.client.Do(r)
	must(b.t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	must(b.t, err)
	if res.StatusCode != want {
		b.t.Fatalf("%s %s returned HTTP %d, want %d", method, r.URL.Path, res.StatusCode, want)
	}
	return res, string(body)
}

func (b browser) form(key *webauthntest.Key, prf []byte, page string) url.Values {
	b.t.Helper()
	var attempt struct{ Challenge, Nonce string }
	must(b.t, json.Unmarshal([]byte(capture(b.t, page, `id="ceremony">(.*?)</script>`)), &attempt))
	b64 := base64.RawURLEncoding
	challenge, err := b64.DecodeString(attempt.Challenge)
	must(b.t, err)
	a, err := key.Assert("localhost", approval.LocalOrigin, challenge, false)
	must(b.t, err)
	return url.Values{"csrf": {b.csrf}, "nonce": {attempt.Nonce}, "prf_output": {b64.EncodeToString(prf)},
		"client_data_json": {b64.EncodeToString(a.ClientDataJSON)}, "authenticator_data": {b64.EncodeToString(a.AuthenticatorData)},
		"credential_id": {b64.EncodeToString(a.CredentialID)}, "signature": {b64.EncodeToString(a.Signature)}}
}

func fixtureTransportAnswer(status int, data string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data))}, nil
}

type gitProviderFixture struct {
	wrongUser atomic.Bool
	merged    atomic.Bool
	puts      atomic.Int32
	users     atomic.Int32
}

const (
	mergeHead  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mergeSHA   = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	mergeRead  = "fixture-read-credential"
	mergeWrite = "fixture-write-credential"
)

func (f *gitProviderFixture) transport(t *testing.T) http.RoundTripper {
	return transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "github.fixture.test" {
			t.Error("contacted an unconfigured provider")
			return fixtureTransportAnswer(500, "{}")
		}
		auth := r.Header.Get("Authorization")
		read, write := auth == "Bearer "+mergeRead, auth == "Bearer "+mergeWrite
		if !read && !write {
			t.Error("wrong test token")
			return fixtureTransportAnswer(401, "{}")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /user":
			f.users.Add(1)
			id := 123
			if write && f.wrongUser.Load() {
				id = 124
			}
			return fixtureTransportAnswer(200, fmt.Sprintf(`{"id":%d,"login":"fixture-user","type":"User"}`, id))
		case "GET /repos/Fixture/repo/pulls/1":
			state, merged := "open", f.merged.Load()
			if merged {
				state = "closed"
			}
			return fixtureTransportAnswer(200, fmt.Sprintf(`{"state":%q,"merged":%t,"mergeable":true,"mergeable_state":"clean",`+
				`"merge_commit_sha":%q,"merged_by":{"id":123},"base":{"ref":"main","repo":{"id":456}},"head":{"sha":%q}}`, state, merged, mergeSHA, mergeHead))
		case "GET /repos/Fixture/repo/pulls/1/reviews", "GET /repos/Fixture/repo/rules/branches/main":
			return fixtureTransportAnswer(200, "[]")
		case "GET /repos/Fixture/repo/branches/main":
			return fixtureTransportAnswer(200, "{}")
		case "GET /repos/Fixture/repo/commits/" + mergeHead + "/check-runs":
			return fixtureTransportAnswer(200, `{"check_runs":[]}`)
		case "GET /repos/Fixture/repo/commits/" + mergeHead + "/status":
			return fixtureTransportAnswer(200, `{"statuses":[]}`)
		case "PUT /repos/Fixture/repo/pulls/1/merge":
			f.puts.Add(1)
			var body map[string]string
			if !write || json.NewDecoder(r.Body).Decode(&body) != nil || body["sha"] != mergeHead || body["merge_method"] != "rebase" {
				t.Error("provider did not receive exactly the approved merge")
			}
			r.Body.Close()
			f.merged.Store(true)
			return nil, io.ErrUnexpectedEOF
		}
		t.Errorf("unexpected fixture call: %s %s", r.Method, r.URL.Path)
		return fixtureTransportAnswer(500, "{}")
	})
}

func proposeMerge(t *testing.T, dir string) string {
	t.Helper()
	wire, err := actsocket.Call(t.Context(), dir, approval.Object{"op": "propose", "type": "github.merge", "reason": "fixture",
		"agent": approval.Object{"name": "fixture"}, "params": approval.Object{"repo": "Fixture/repo", "number": int64(1),
			"base": "main", "head": mergeHead, "method": "rebase"}}, 0)
	must(t, err)
	var p struct{ ID string }
	must(t, json.Unmarshal(wire, &p))
	if !approval.ValidID(p.ID, "apr") {
		t.Fatal("proposal was refused")
	}
	return p.ID
}

func (b browser) waitFor(id, marker string) {
	b.t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for {
		_, page := b.request(http.MethodGet, "/p/"+id, nil, 200)
		if strings.Contains(page, marker) {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("proposal did not show %q", marker)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServeOnlyMergesAfterAssertionAndUnsealing(t *testing.T) {
	opts := options(t)
	must(t, os.WriteFile(filepath.Join(opts.StateDir, "action-providers.json"), []byte(`{"github":{"origin":"https://github.fixture.test","user_id":123}}`), 0o600))
	key, prf := registerFixturePasskey(t, opts.StateDir)
	f := new(gitProviderFixture)
	opts.ProviderTransport = func([]string) http.RoundTripper { return f.transport(t) }
	b, stop := launchBrowser(t, opts)
	setup := func(read, write string) url.Values {
		_, page := b.request(http.MethodPost, "/settings/begin", url.Values{"csrf": {b.csrf}, "service": {"github"}}, 200)
		form := b.form(key, prf, page)
		form.Set("read_token", read)
		form.Set("write_token", write)
		return form
	}
	_, page := b.request(http.MethodPost, "/settings/save", setup(mergeRead, mergeRead), 409)
	if !strings.Contains(page, "same_credential") || f.users.Load() != 0 {
		t.Fatal("duplicate token was not refused before network")
	}
	f.wrongUser.Store(true)
	_, page = b.request(http.MethodPost, "/settings/save", setup(mergeRead, mergeWrite), 409)
	if !strings.Contains(page, "account_mismatch") {
		t.Fatal("accepted tokens for different accounts")
	}
	if _, err := os.Stat(filepath.Join(opts.StateDir, "unattended.kdbx")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote a vault before validating both tokens")
	}
	f.wrongUser.Store(false)
	b.request(http.MethodPost, "/settings/save", setup(mergeRead, mergeWrite), 200)
	id := proposeMerge(t, opts.StateDir)
	freeze := func() string {
		t.Helper()
		_, page := b.request(http.MethodPost, "/freeze", url.Values{"csrf": {b.csrf}, "id": {id}}, 200)
		for _, fact := range []string{"fixture-user", "github:user:123", "Fixture/repo", mergeHead, "rebase", "456"} {
			if !strings.Contains(page, fact) {
				t.Fatal("missing frozen fact", fact)
			}
		}
		if f.puts.Load() != 0 {
			t.Fatal("freeze dispatched a write")
		}
		return page
	}
	form := b.form(key, prf, freeze())
	form.Set("signature", "AA")
	b.request(http.MethodPost, "/approve", form, 401)
	b.request(http.MethodPost, "/approve", b.form(key, make([]byte, 32), freeze()), 401)
	sealed := filepath.Join(opts.StateDir, "write-github.sealed")
	good, err := os.ReadFile(sealed)
	must(t, err)
	must(t, os.WriteFile(sealed, []byte("not the authenticated ciphertext"), 0o600))
	form = b.form(key, prf, freeze())
	_, page = b.request(http.MethodPost, "/approve", form, 409)
	if !strings.Contains(page, "credential_unavailable") {
		t.Fatal("wrong refusal for unavailable credential")
	}
	b.waitFor(id, "pending (version 1)")
	if f.puts.Load() != 0 {
		t.Fatal("invalid approvals reached the provider")
	}
	must(t, os.WriteFile(sealed, good, 0o600))
	form = b.form(key, prf, freeze())
	b.request(http.MethodPost, "/approve", form, 200)
	b.waitFor(id, "github:merge:"+mergeSHA)
	b.waitFor(id, "read_back")
	b.request(http.MethodPost, "/approve", form, 409) // even the same signed assertion has been consumed
	if f.puts.Load() != 1 {
		t.Fatalf("want one merge, got %d", f.puts.Load())
	}
	stop()
	for _, name := range []string{"journal.log", "action-providers.json", "unattended.kdbx", "unattended.keyx", "passkeys.json", "write-vault.json", "write-github.sealed"} {
		body, err := os.ReadFile(filepath.Join(opts.StateDir, name))
		must(t, err)
		for _, needle := range [][]byte{[]byte(mergeRead), []byte(mergeWrite), prf, []byte(base64.StdEncoding.EncodeToString(prf)), []byte(base64.RawURLEncoding.EncodeToString(prf))} {
			if bytes.Contains(body, needle) {
				t.Fatalf("%s stored a raw credential or PRF output", name)
			}
		}
	}
}
