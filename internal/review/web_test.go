package review

import (
	"encoding/base64"
	"errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

const tailOrigin = "https://machine.tail123.ts.net"
const login = "person@example.test"

var start = time.Unix(1_800_000_000, 0).UTC()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func makeOrigin(t *testing.T, origin, login string) approval.Origin {
	t.Helper()
	o, err := approval.ParseOrigin(origin, login)
	must(t, err)
	return o
}

func site(t *testing.T, tailnet bool, now func() time.Time) (*Site, *approval.Journal) {
	t.Helper()
	if now == nil {
		now = func() time.Time { return start }
	}
	o := makeOrigin(t, approval.LocalOrigin, "")
	if tailnet {
		o = makeOrigin(t, tailOrigin, login)
	}
	dir := filepath.Join(t.TempDir(), "state")
	j, err := approval.OpenJournal(dir, now(), "")
	must(t, err)
	t.Cleanup(func() { must(t, j.Close()) })
	s := New(o, j, now)
	s.SetState(dir)
	return s, j
}

func (s *Site) request(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = "127.0.0.1:40000"
	r.Host = s.origin.Host()
	if s.origin.Secure() {
		r.Header.Set(tailLogin, s.origin.TailscaleLogin())
	}
	return r
}

func request(s *Site, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func code(t *testing.T, s *Site) string {
	t.Helper()
	c, ok := s.IssueOpen()
	b, err := base64.RawURLEncoding.DecodeString(c)
	if !ok || err != nil || len(b) != 32 {
		t.Fatal("missing sign-in code")
	}
	return c
}

func signIn(t *testing.T, s *Site) *http.Cookie {
	t.Helper()
	c := code(t, s)
	w := request(s, s.request("GET", "/open?code="+c, nil))
	expectStatus(t, w, http.StatusSeeOther)
	if w.Header().Get("Location") != "/" || strings.Contains(w.Body.String(), c) {
		t.Fatal("did not remove code from the link")
	}
	set := w.Result().Cookies()
	if len(set) != 1 {
		t.Fatal("session cookie not set")
	}
	return set[0]
}

func expectStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body)
	}
}

func securityHeaders(t *testing.T, w *httptest.ResponseRecorder, secure bool) {
	t.Helper()
	h := w.Header()
	if h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") ||
		(h.Get("Strict-Transport-Security") != "") != secure || h.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unexpected security headers", h)
	}
}

func TestCanonicalOriginAcceptsOnlyTheConfiguredForms(t *testing.T) {
	for _, v := range []struct{ origin, login string }{
		{approval.LocalOrigin, login}, {"http://127.0.0.1:7317", ""}, {"http://localhost:7318", ""},
		{tailOrigin, ""}, {tailOrigin, "one,two"}, {tailOrigin, "one\r\ntwo"}, {tailOrigin + ":443", login},
		{tailOrigin + "/", login}, {tailOrigin + "?", login}, {tailOrigin + "#", login}, {tailOrigin + ".", login},
		{"http://machine.tail123.ts.net", login}, {"https://machine.tail123.ts.net.example.test", login},
		{"https://name@machine.tail123.ts.net", login}, {"https://MACHINE.tail123.ts.net", login},
		{"https://-machine.tail123.ts.net", login}, {"https://foo.bar.machine.tail123.ts.net", login},
	} {
		if _, err := approval.ParseOrigin(v.origin, v.login); err == nil {
			t.Errorf("accepted invalid origin %q / %q", v.origin, v.login)
		}
	}
	for _, exist := range []approval.Origin{makeOrigin(t, approval.LocalOrigin, ""), makeOrigin(t, tailOrigin, login)} {
		if wantTail := exist.TailscaleLogin() != ""; exist.Secure() != wantTail ||
			wantTail && exist.URL() != tailOrigin || !wantTail && exist.URL() != approval.LocalOrigin {
			t.Fatalf("wrong canonical origin: %+v", exist)
		}
	}
}

func TestAccessTableAndRedirect(t *testing.T) {
	id := "apr_01m6a6ft00e81840g2081040g3"
	for _, tc := range []struct {
		name, host, method string
		tailnet            bool
		logins             []string
		status             int
	}{
		{"local", approval.LocalHost, "GET", false, nil, 401},
		{"ip-literal", "127.0.0.1:7317", "GET", false, nil, 403},
		{"rebound", "page.example:7317", "GET", false, nil, 403},
		{"other-port", "localhost:12345", "GET", false, nil, 403},
		{"tail-host-in-local", "machine.tail123.ts.net", "GET", false, []string{login}, 403},
		{"local-with-header", approval.LocalHost, "GET", false, []string{login}, 403},
		{"local-with-empty-header", approval.LocalHost, "GET", false, []string{""}, 403},
		{"tail", "machine.tail123.ts.net", "GET", true, []string{login}, 401},
		{"tail-explicit-port", "machine.tail123.ts.net:443", "GET", true, []string{login}, 403},
		{"tail-missing-login", "machine.tail123.ts.net", "GET", true, nil, 403},
		{"tail-other-login", "machine.tail123.ts.net", "GET", true, []string{"other@example.test"}, 403},
		{"tail-duplicate-login", "machine.tail123.ts.net", "GET", true, []string{login, login}, 403},
		{"tail-old-link", approval.LocalHost, "GET", true, nil, 302},
		{"tail-local-with-login", approval.LocalHost, "GET", true, []string{login}, 403},
		{"tail-local-post", approval.LocalHost, "POST", true, nil, 403},
		{"tail-local-head", approval.LocalHost, "HEAD", true, nil, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := site(t, tc.tailnet, nil)
			path := "/p/" + id
			r := s.request(tc.method, path+"?code=never-forward", nil)
			r.Host = tc.host
			r.Header.Del(tailLogin)
			for _, value := range tc.logins {
				r.Header.Add(tailLogin, value)
			}
			r.Header.Set("X-Forwarded-Host", s.origin.Host()) // can't change Host
			w := request(s, r)
			expectStatus(t, w, tc.status)
			securityHeaders(t, w, tc.tailnet)
			if tc.status == http.StatusFound && w.Header().Get("Location") != tailOrigin+path {
				t.Fatal("wrong redirect", w.Header())
			}
			if strings.Contains(w.Body.String(), "never-forward") || w.Header().Get("Set-Cookie") != "" {
				t.Fatal("reflected code or issued a session")
			}
		})
	}
}

func TestNoForwardedAddressOrAbsoluteTarget(t *testing.T) {
	s, _ := site(t, false, nil)
	r := s.request("GET", "/sign-in", nil)
	r.RemoteAddr = "192.0.2.1:40000"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	expectStatus(t, request(s, r), http.StatusForbidden)
	r = s.request("GET", approval.LocalOrigin+"/sign-in", nil)
	expectStatus(t, request(s, r), http.StatusForbidden)
	r = s.request("GET", "/sign-in", nil)
	r.RemoteAddr = "[::1]:40000"
	expectStatus(t, request(s, r), http.StatusOK)
}

func TestCodeSingleUseAndSessionLifetime(t *testing.T) {
	for _, tail := range []bool{false, true} {
		at := start
		s, j := site(t, tail, func() time.Time { return at })
		c := code(t, s)
		open := "/open?code=" + c
		for _, suffix := range []string{"&code=" + c, "&next=https://example.test", "&bad=%"} {
			expectStatus(t, request(s, s.request("GET", open+suffix, nil)), http.StatusUnauthorized)
		}
		expectStatus(t, request(s, s.request("HEAD", open, nil)), http.StatusMethodNotAllowed)
		w := request(s, s.request("GET", open, nil))
		expectStatus(t, w, http.StatusSeeOther)
		securityHeaders(t, w, tail)
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatal("no session cookie")
		}
		cookie := cookies[0]
		b, err := base64.RawURLEncoding.DecodeString(cookie.Value)
		must(t, err)
		if len(b) != 32 || cookie.Value == c || cookie.Secure != tail || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode ||
			cookie.Name != cookieName(s.origin) || cookie.Path != "/" || cookie.Domain != "" ||
			!cookie.Expires.Equal(at.Add(sessionLife)) || cookie.MaxAge != 43200 || w.Header().Get("Location") != "/" {
			t.Fatal("incorrect cookie or redirect")
		}
		expectStatus(t, request(s, s.request("GET", open, nil)), http.StatusUnauthorized)
		r := s.request("GET", "/", nil)
		r.AddCookie(cookie)
		r.Header.Set("Sec-Fetch-Site", "cross-site") // e.g. a top-level link from Signal
		expectStatus(t, request(s, r), http.StatusOK)
		restarted := New(s.origin, j, s.now)
		expectStatus(t, request(restarted, r), http.StatusUnauthorized)
		queued := code(t, s)
		expectStatus(t, request(restarted, restarted.request("GET", "/open?code="+queued, nil)), http.StatusUnauthorized)
		at = at.Add(codeLife)
		expectStatus(t, request(s, s.request("GET", "/open?code="+queued, nil)), http.StatusUnauthorized)
		at = start.Add(sessionLife)
		expectStatus(t, request(s, r), http.StatusUnauthorized)
	}
}

func TestOpenExchangesAtMostOnceConcurrentlyAndBoundsCodePool(t *testing.T) {
	at := start
	s, _ := site(t, false, func() time.Time { return at })
	c := code(t, s)
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for range cap(statuses) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- request(s, s.request("GET", "/open?code="+c, nil)).Code
		}()
	}
	wg.Wait()
	close(statuses)
	n := 0
	for v := range statuses {
		switch v {
		case http.StatusSeeOther:
			n++
		case http.StatusUnauthorized:
		default:
			t.Fatal("unexpected status", v)
		}
	}
	if n != 1 {
		t.Fatalf("%d exchanges", n)
	}
	for range maxCodes {
		code(t, s)
	}
	if _, ok := s.IssueOpen(); ok {
		t.Fatal("issued a code beyond the limit")
	}
	at = at.Add(codeLife)
	code(t, s)
}

func TestPostChecksOriginAndCSRFButDoesNotHandleAMutation(t *testing.T) {
	for _, tail := range []bool{false, true} {
		s, _ := site(t, tail, nil)
		cookie := signIn(t, s)
		csrf := s.sessions.active[cookie.Value].csrf
		if len(csrf) != 43 || csrf == cookie.Value {
			t.Fatal("missing distinct CSRF value")
		}
		var canonical = s.origin.URL()
		for _, tc := range []struct {
			origins           []string
			fetch, body, path string
			session           bool
			status            int
		}{
			{[]string{canonical}, "", "csrf=" + csrf, "/approve", true, 405},
			{nil, "same-origin", "csrf=" + csrf, "/freeze", true, 405},
			{nil, "", "csrf=" + csrf, "/freeze", true, 403},
			{nil, "same-site", "csrf=" + csrf, "/freeze", true, 403},
			{[]string{""}, "same-origin", "csrf=" + csrf, "/freeze", true, 403},
			{[]string{"null"}, "same-origin", "csrf=" + csrf, "/freeze", true, 403},
			{[]string{"http://localhost:7318"}, "same-origin", "csrf=" + csrf, "/freeze", true, 403},
			{[]string{"http://127.0.0.1:7317"}, "", "csrf=" + csrf, "/freeze", true, 403},
			{[]string{canonical, canonical}, "", "csrf=" + csrf, "/freeze", true, 403},
			{[]string{canonical}, "", "csrf=" + csrf, "/freeze", false, 403},
			{[]string{canonical}, "", "csrf=wrong", "/freeze", true, 403},
			{[]string{canonical}, "", "", "/freeze?csrf=" + csrf, true, 403},
			{[]string{canonical}, "", "csrf=" + csrf + "&csrf=" + csrf, "/freeze", true, 403},
			{[]string{canonical}, "", "csrf=" + csrf + "&padding=" + strings.Repeat("x", 16<<10), "/freeze", true, 403},
		} {
			r := s.request("POST", tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for _, value := range tc.origins {
				r.Header.Add("Origin", value)
			}
			if tc.fetch != "" {
				r.Header.Set("Sec-Fetch-Site", tc.fetch)
			}
			if tc.session {
				r.AddCookie(cookie)
			}
			w := request(s, r)
			expectStatus(t, w, tc.status)
			securityHeaders(t, w, tail)
		}
	}
}

func TestReadOnlyPagesEscapeAndShowOriginalAndFrozenValues(t *testing.T) {
	s, j := site(t, false, nil)
	first, err := j.Add(approval.Request{Type: approval.Run, Agent: "bosun", Session: "ses_1",
		Reason: "<strong>look</strong>", Context: "<script></script>", Params: approval.Object{
			"checkout": "signal", "prompt": "\n<img src=x onerror=run()>\n\u202Eevil\u200B\x01", "model": "original/model",
		}}, approval.PeerLabel{PID: 34, Executable: "/bin/<proposer>"}, start)
	must(t, err)
	second, err := j.Add(first.Request, approval.PeerLabel{}, start.Add(time.Second))
	must(t, err)
	path := "/p/" + first.ID
	for _, route := range []string{"/", path, path + "/frozen"} {
		w := request(s, s.request("GET", route, nil))
		expectStatus(t, w, http.StatusUnauthorized)
		if strings.Contains(w.Body.String(), first.ID) || strings.Contains(w.Body.String(), "original/model") {
			t.Fatal("unauthenticated disclosure")
		}
	}
	cookie := signIn(t, s)
	get := func(url string) *httptest.ResponseRecorder {
		r := s.request("GET", url, nil)
		r.AddCookie(cookie)
		return request(s, r)
	}
	w := get("/")
	expectStatus(t, w, http.StatusOK)
	if a, b := strings.Index(w.Body.String(), first.ID), strings.Index(w.Body.String(), second.ID); a < 0 || b <= a {
		t.Fatal("pending not ordered oldest first")
	}
	w = get(path)
	expectStatus(t, w, http.StatusOK)
	for _, text := range []string{"&lt;img src=x onerror=run()&gt;", "&lt;strong&gt;look&lt;/strong&gt;",
		"&lt;script&gt;&lt;/script&gt;", "/bin/&lt;proposer&gt;", "Executable (unverified label)", "Show exact parameters", "original/model"} {
		if !strings.Contains(w.Body.String(), text) {
			t.Fatalf("did not render %q", text)
		}
	}
	if !strings.Contains(html.UnescapeString(w.Body.String()), "⟨U+202E⟩evil⟨U+200B⟩⟨U+0001⟩") {
		t.Fatal("did not make hidden characters visible")
	}
	for _, text := range []string{"<script", "<img", "<form", "<button", "https://", "\u202E", "\u200B"} {
		if strings.Contains(w.Body.String(), text) {
			t.Fatalf("unexpected %q in the page", text)
		}
	}
	expectStatus(t, get(path+"/frozen"), http.StatusNotFound)
	f, err := approval.Freeze(first, approval.Binding{Account: "linux:user:person", Destination: approval.Object{
		"checkout_id": "signal-identity", "branch": "main",
	}}, "chosen/model")
	must(t, err)
	must(t, j.Approve(f, start)) // simulates a snapshot already in the journal; no page can create it
	if b := get(path).Body.String(); !strings.Contains(b, "original/model") || strings.Contains(b, "chosen/model") {
		t.Fatal("original parameters were overwritten")
	}
	w = get(path + "/frozen")
	expectStatus(t, w, http.StatusOK)
	for _, text := range []string{f.Hash, f.ParamsHash, "signal-identity", "linux:user:person", "chosen/model", "original/model", "&lt;img"} {
		if !strings.Contains(w.Body.String(), text) {
			t.Fatalf("frozen view missing %q", text)
		}
	}
	if !strings.Contains(html.UnescapeString(w.Body.String()), "⟨U+202E⟩") {
		t.Fatal("frozen parameters hide bidirectional characters")
	}
	if w := get("/"); strings.Contains(w.Body.String(), first.ID) {
		t.Fatal("approved proposal still pending")
	}
	asset := request(s, s.request("GET", "/static/beacon.css", nil))
	expectStatus(t, asset, http.StatusOK)
	securityHeaders(t, asset, false)
	if asset.Header().Get("Content-Type") != "text/css; charset=utf-8" || !strings.Contains(asset.Body.String(), "color-scheme") {
		t.Fatal("embedded stylesheet missing")
	}
}

func TestLoopbackBindsOnlyLiteralAddressesAndRefusesAPortConflict(t *testing.T) {
	s, _ := site(t, false, nil)
	ls, err := listenLoopback("0")
	must(t, err)
	server := s.Server()
	t.Cleanup(func() { server.Close() })
	if len(ls) != 2 {
		t.Fatal("missing listener")
	}
	client := &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for i, l := range ls {
		defer l.Close()
		addr := l.Addr().(*net.TCPAddr)
		if addr.IP.String() != []string{"127.0.0.1", "::1"}[i] {
			t.Fatalf("not the expected loopback address: %s", addr)
		}
		go server.Serve(l)
		r, err := http.NewRequestWithContext(t.Context(), "GET", "http://"+addr.String()+"/sign-in", nil)
		must(t, err)
		r.Host = approval.LocalHost
		res, err := client.Do(r)
		must(t, err)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatal(res.Status)
		}
	}
	_, port, err := net.SplitHostPort(ls[1].Addr().String())
	must(t, err)
	if other, err := listenLoopback(port); !errors.Is(err, syscall.EADDRINUSE) {
		for _, l := range other {
			l.Close()
		}
		t.Fatalf("conflict not reported: %v", err)
	}
	if ls[0].Addr().(*net.TCPAddr).Port != ls[1].Addr().(*net.TCPAddr).Port {
		// If ::1 failed to bind, the first (IPv4) socket was rolled back.
		l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
		must(t, err)
		l.Close()
	}
}

func TestVisibleTextSwiftParity(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ok\u202Eevil\u200B\x01", "ok⟨U+202E⟩evil⟨U+200B⟩⟨U+0001⟩"},
		{"line\nnext — ✅\tend", "line\nnext — ✅\tend"},
		{"\u034F\u180E\uFE0F\U000E0100\u2065", "⟨U+034F⟩⟨U+180E⟩⟨U+FE0F⟩⟨U+E0100⟩⟨U+2065⟩"},
	} {
		if got := visibleText(tc.in); got != tc.want {
			t.Fatalf("visible(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
