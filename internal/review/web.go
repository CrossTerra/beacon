// Package review serves approval pages on the service's canonical loopback origin.
package review

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

//go:embed templates/*.html static/beacon.css static/passkey.js
var assets embed.FS

var pages = template.Must(template.New("review").Funcs(template.FuncMap{
	"visible": visibleText, "action": actionName, "proposalTitle": proposalTitle,
	"shownTime": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
}).ParseFS(assets, "templates/*.html"))

// Read access only. The journal can durably expire stale proposals as part of taking a view.
type proposals interface {
	Get(string, time.Time) (approval.View, <-chan struct{}, error)
	Pending(time.Time) ([]approval.Proposal, error)
}

type Site struct {
	origin      approval.Origin
	journal     proposals
	state       string
	actions     Actions
	now         func() time.Time
	sessions    sessions
	held        map[string]held
	closing     bool // guarded by sessions.mu, alongside held
	diagnostics passkeyDiagnostics
}

func New(origin approval.Origin, j proposals, now func() time.Time) *Site {
	if now == nil {
		now = time.Now
	}
	return &Site{origin: origin, journal: j, now: now, sessions: sessions{
		codes: make(map[string]time.Time), active: make(map[string]session),
	}}
}

// IssueOpen is called only by the admitted Unix socket, not by any HTTP route.
func (s *Site) IssueOpen() (string, bool) { return s.sessions.issue(s.now()) }

// SetState is the preview directory that holds the setup code and passkeys.
func (s *Site) SetState(dir string) { s.state = dir }

const tailLogin = "Tailscale-User-Login"

func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.headers(w.Header())
	// Ignore forwarding headers. Only the kernel's remote address and the configured origin count.
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(peer).IsLoopback() || r.URL.IsAbs() || r.URL.Host != "" ||
		!strings.HasPrefix(r.URL.Path, "/") {
		http.Error(w, "request refused", http.StatusForbidden)
		return
	}
	login := r.Header.Values(tailLogin)
	if s.origin.Secure() && r.Host == approval.LocalHost && r.Method == http.MethodGet && len(login) == 0 {
		// Old bookmarks only. Do not inspect cookies or forward a query (which can contain a code).
		http.Redirect(w, r, s.origin.URL()+r.URL.EscapedPath(), http.StatusFound)
		return
	}
	if r.Host != s.origin.Host() || s.origin.Secure() && (len(login) != 1 || login[0] != s.origin.TailscaleLogin()) ||
		!s.origin.Secure() && len(login) != 0 {
		http.Error(w, "request refused", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/passkeys/new" || r.URL.Path == "/passkeys" || r.URL.Path == "/passkeys/report") {
		if !s.allowOrigin(r) {
			http.Error(w, "form refused", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/passkeys/report" {
			s.registrationReport(w, r)
		} else {
			s.register(w, r)
		}
		return
	}
	if r.Method == http.MethodPost {
		if !s.allowForm(w, r) {
			http.Error(w, "form refused", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/freeze":
			s.freeze(w, r)
			return
		case "/approve":
			s.consume(w, r)
			return
		case "/read-back":
			s.readBack(w, r)
			return
		case "/stop-run":
			s.stopRun(w, r)
			return
		case "/settings/begin", "/settings/save":
			s.changeCredentials(w, r)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	switch r.URL.Path {
	case "/static/passkey.js":
		b, err := assets.ReadFile("static/passkey.js")
		if err != nil {
			http.Error(w, "script unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(b)
		return
	case "/static/beacon.css":
		b, err := assets.ReadFile("static/beacon.css")
		if err != nil {
			http.Error(w, "stylesheet unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(b)
		return
	case "/sign-in":
		render(w, "sign-in", http.StatusOK, nil)
		return
	case "/passkeys/new":
		render(w, "register", http.StatusOK, nil)
		return
	case "/open":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		s.open(w, r)
		return
	}
	at := s.now()
	sess, ok := s.sessions.lookup(r, s.origin, at)
	if !ok {
		render(w, "sign-in", http.StatusUnauthorized, nil)
		return
	}
	switch r.URL.Path {
	case "/":
		s.pendingPage(w, sess, at)
		return
	case "/settings":
		if !s.hasSettings() {
			http.NotFound(w, r)
			return
		}
		s.settingsPage(w, sess, false)
		return
	}
	s.detail(w, r, sess, at)
}

func (s *Site) headers(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	// same-origin, not no-referrer: under no-referrer a browser sends `Origin: null` on a form POST, which
	// allowForm refuses. Referrers still never leave the origin.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	if s.origin.Secure() {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Site) allowOrigin(r *http.Request) bool {
	origin := r.Header.Values("Origin")
	return len(origin) == 1 && origin[0] == s.origin.URL()
}

// A present Origin must match exactly; Fetch Metadata is a fallback only for an absent Origin.
func (s *Site) allowForm(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Values("Origin"); len(origin) != 0 {
		if len(origin) != 1 || origin[0] != s.origin.URL() {
			return false
		}
	} else if site := r.Header.Values("Sec-Fetch-Site"); len(site) != 1 || site[0] != "same-origin" {
		return false
	}
	value, ok := s.sessions.lookup(r, s.origin, s.now())
	if !ok {
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil {
		return false
	}
	csrf := r.PostForm["csrf"]
	return len(csrf) == 1 && subtle.ConstantTimeCompare([]byte(csrf[0]), []byte(value.csrf)) == 1
}

func (s *Site) open(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	codes := q["code"]
	if err != nil || len(q) != 1 || len(codes) != 1 || len(codes[0]) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		render(w, "sign-in", http.StatusUnauthorized, true)
		return
	}
	at := s.now()
	id, status := s.sessions.exchange(codes[0], at)
	if status != http.StatusSeeOther {
		render(w, "sign-in", status, true)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName(s.origin), Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.origin.Secure(), MaxAge: int(sessionLife.Seconds()), Expires: at.Add(sessionLife)})
	w.Header().Set("Location", "/") // remove the now-used code from the address bar
	w.WriteHeader(status)
}

func render(w http.ResponseWriter, name string, status int, model any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, model); err != nil {
		http.Error(w, "page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	io.Copy(w, &buf)
}
