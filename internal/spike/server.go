package spike

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/CrossTerra/beacon/internal/webauthn"
)

// Config is how the spike is started. It contacts no host and reads no credential.
type Config struct {
	// DataDir holds credentials.json. It is created 0700.
	DataDir string
	// TailscaleLogin, when set, is the only Tailscale-User-Login a ts.net request may carry.
	// Empty still refuses a missing login (Funnel, a tagged device).
	TailscaleLogin string
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
	// Log receives one JSON line per request and per result. Nil discards.
	// Lines never contain a request body, an assertion or a PRF output.
	Log io.Writer
}

// Server is the spike. It listens only on loopback port 7317.
type Server struct {
	dir   string
	login string
	now   func() time.Time
	logw  io.Writer

	mu         sync.Mutex
	state      diskState
	ceremonies map[string]issued
	recent     []arrival
	logMu      sync.Mutex
}

type issued struct {
	kind      string
	rpID      string
	origin    string
	challenge []byte
	expires   time.Time
}

type arrival struct {
	When      time.Time
	Host      string
	Origin    string
	Tailscale []headerValue
	Method    string
	Path      string
	Reason    string
	Remote    string
}

type ctxKey struct{}

// Listen binds 127.0.0.1:7317 and [::1]:7317 and refuses anything else.
// If the port is in use it returns an error and does not choose another port.
func Listen() ([]net.Listener, error) {
	var opened []net.Listener
	closeAll := func() {
		for _, ln := range opened {
			_ = ln.Close()
		}
	}
	for _, addr := range []string{"127.0.0.1:" + Port, "[::1]:" + Port} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			closeAll()
			if errors.Is(err, syscall.EADDRINUSE) {
				return nil, errors.New("port 7317 is already in use; the spike does not choose another port")
			}
			return nil, err
		}
		tcp, ok := ln.Addr().(*net.TCPAddr)
		if !ok || tcp == nil || !tcp.IP.IsLoopback() || tcp.Port != 7317 {
			_ = ln.Close()
			closeAll()
			return nil, errors.New("listener is not loopback port 7317")
		}
		opened = append(opened, ln)
	}
	return opened, nil
}

// New loads the credential file. It does not listen.
func New(cfg Config) (*Server, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("spike: data directory is required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	st, err := loadState(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logw := cfg.Log
	if logw == nil {
		logw = io.Discard
	}
	return &Server{
		dir:        cfg.DataDir,
		login:      cfg.TailscaleLogin,
		now:        now,
		logw:       logw,
		state:      st,
		ceremonies: map[string]issued{},
	}, nil
}

// Serve accepts connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, lns []net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	errc := make(chan error, len(lns))
	for _, ln := range lns {
		go func(ln net.Listener) {
			err := srv.Serve(ln)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
				return
			}
			errc <- nil
		}(ln)
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
	return err
}

// Handler is the spike's HTTP handler, including the origin checks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /register", s.registerPage)
	mux.HandleFunc("POST /register", s.register)
	mux.HandleFunc("GET /assert", s.assertPage)
	mux.HandleFunc("POST /assert", s.assert)
	mux.HandleFunc("GET /passkey.js", s.script)
	mux.HandleFunc("GET /spike.css", s.css)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.protect(mux, w, r)
	})
}

func (s *Server) protect(next http.Handler, w http.ResponseWriter, r *http.Request) {
	o, reason := classify(r, s.login)
	headers := tailscaleHeaders(r)
	s.note(r, headers, reason)
	s.logRequest(r, headers, reason)
	w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	if o.Mode == "ts.net" {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	if reason != "" {
		s.render(w, http.StatusForbidden, s.page(r, o, headers, page{
			Title:      "Refused",
			Reason:     reason,
			ReasonText: textFor(reason),
		}))
		return
	}
	ctx := context.WithValue(r.Context(), ctxKey{}, o)
	next.ServeHTTP(w, r.WithContext(ctx))
}

func originOf(r *http.Request) Origin {
	o, _ := r.Context().Value(ctxKey{}).(Origin)
	return o
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, originOf(r), tailscaleHeaders(r), page{Title: "Beacon passkey spike", Nav: true})
	s.render(w, http.StatusOK, p)
}

func (s *Server) registerPage(w http.ResponseWriter, r *http.Request) {
	o := originOf(r)
	id, challenge, err := s.issue("create", o)
	if err != nil {
		s.fail(w, r, "ceremony")
		return
	}
	body, err := json.Marshal(map[string]any{
		"kind":      "create",
		"rpId":      o.RPID,
		"challenge": base64.RawURLEncoding.EncodeToString(challenge),
		"userId":    s.userID(),
		"prf":       r.URL.Query().Get("prf") != "0",
	})
	if err != nil {
		s.fail(w, r, "malformed")
		return
	}
	p := s.page(r, o, tailscaleHeaders(r), page{
		Title:      "Register a passkey",
		Ceremony:   templateHTML(body),
		CeremonyID: id,
		Kind:       "create",
		Action:     "/register",
	})
	s.render(w, http.StatusOK, p)
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	o := originOf(r)
	if err := readForm(w, r); err != nil {
		s.fail(w, r, "malformed")
		return
	}
	challenge, err := s.take(r.FormValue("ceremony"), "create", o)
	if err != nil {
		s.fail(w, r, "ceremony")
		return
	}
	if r.FormValue("prf_enabled") != "true" {
		s.logResult("register", "no_prf", false, "")
		s.fail(w, r, "no_prf")
		return
	}
	clientData, err := b64(r.FormValue("client_data_json"))
	if err != nil {
		s.fail(w, r, "malformed")
		return
	}
	attestation, err := b64(r.FormValue("attestation_object"))
	if err != nil {
		s.fail(w, r, "malformed")
		return
	}
	credID, err := b64(r.FormValue("credential_id"))
	if err != nil {
		s.fail(w, r, "malformed")
		return
	}
	reg, err := webauthn.VerifyRegistration(webauthn.RelyingParty{ID: o.RPID, Origin: o.Origin}, challenge, clientData, attestation)
	if err != nil {
		s.fail(w, r, codeOf(err))
		return
	}
	if subtleDiff(reg.CredentialID, credID) {
		s.fail(w, r, "credential")
		return
	}
	if err := s.add(o.RPID, reg); err != nil {
		s.fail(w, r, storeCode(err))
		return
	}
	s.logResult("register", "", true, "")
	p := s.page(r, o, tailscaleHeaders(r), page{
		Title:  "Registered",
		Result: "Registered. PRF is enabled. Assert with this passkey to record the PRF hash. The hash is stable for this credential's salt, so the same passkey on another device can be compared.",
		Nav:    true,
	})
	s.render(w, http.StatusOK, p)
}

func (s *Server) assertPage(w http.ResponseWriter, r *http.Request) {
	o := originOf(r)
	creds := s.credsFor(o.RPID)
	if len(creds) == 0 {
		p := s.page(r, o, tailscaleHeaders(r), page{
			Title:  "Assert",
			Result: "No passkey is registered for this origin yet.",
			Nav:    true,
		})
		s.render(w, http.StatusOK, p)
		return
	}
	id, challenge, err := s.issue("get", o)
	if err != nil {
		s.fail(w, r, "ceremony")
		return
	}
	var listed []map[string]string
	for _, c := range creds {
		listed = append(listed, map[string]string{"id": c.ID, "salt": c.PRFSalt})
	}
	body, err := json.Marshal(map[string]any{
		"kind":        "get",
		"rpId":        o.RPID,
		"challenge":   base64.RawURLEncoding.EncodeToString(challenge),
		"credentials": listed,
	})
	if err != nil {
		s.fail(w, r, "malformed")
		return
	}
	p := s.page(r, o, tailscaleHeaders(r), page{
		Title:      "Assert",
		Ceremony:   templateHTML(body),
		CeremonyID: id,
		Kind:       "get",
		Action:     "/assert",
	})
	s.render(w, http.StatusOK, p)
}

func (s *Server) assert(w http.ResponseWriter, r *http.Request) {
	o := originOf(r)
	if err := readForm(w, r); err != nil {
		s.fail(w, r, "malformed")
		return
	}
	challenge, err := s.take(r.FormValue("ceremony"), "get", o)
	if err != nil {
		s.fail(w, r, "ceremony")
		return
	}
	credID, err := b64(r.FormValue("credential_id"))
	if err != nil {
		s.fail(w, r, "malformed")
		return
	}
	stored, ok := s.cred(o.RPID, credID)
	if !ok {
		s.fail(w, r, "credential")
		return
	}
	cose, err := b64(stored.COSEKey)
	if err != nil {
		s.fail(w, r, "credential")
		return
	}
	clientData, err := b64(r.FormValue("client_data_json"))
	if err != nil {
		s.fail(w, r, "malformed")
		return
	}
	authData, err := b64(r.FormValue("authenticator_data"))
	if err != nil {
		s.fail(w, r, "malformed")
		return
	}
	sig, err := b64(r.FormValue("signature"))
	if err != nil {
		s.fail(w, r, "malformed")
		return
	}
	assertion, err := webauthn.VerifyAssertion(
		webauthn.RelyingParty{ID: o.RPID, Origin: o.Origin},
		challenge,
		webauthn.Credential{ID: credID, COSEKey: cose, SignCount: stored.SignCount},
		credID,
		clientData,
		authData,
		sig,
	)
	if err != nil {
		s.fail(w, r, codeOf(err))
		return
	}
	prfRaw, err := b64(r.FormValue("prf_output"))
	if err != nil || len(prfRaw) != 32 {
		// The signature was valid. Keep the sign count so a counter does not stick.
		_ = s.update(o.RPID, credID, assertion.SignCount, "")
		s.logResult("assert", "no_prf_output", false, "")
		s.fail(w, r, "no_prf_output")
		return
	}
	sum := sha256.Sum256(prfRaw)
	for i := range prfRaw {
		prfRaw[i] = 0
	}
	hash := hex.EncodeToString(sum[:])
	match := s.rememberHash(o.RPID, credID, assertion.SignCount, hash)
	s.logResult("assert", "", true, hash)
	p := s.page(r, o, tailscaleHeaders(r), page{
		Title:   "Asserted",
		Result:  "Assertion verified with user verification. Compare this hash with the same passkey on another device. It should not change.",
		PRFHash: hash,
		Match:   match,
		Nav:     true,
	})
	s.render(w, http.StatusOK, p)
}

func (s *Server) script(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/passkey.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) css(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/spike.css")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, code string) {
	o := originOf(r)
	p := s.page(r, o, tailscaleHeaders(r), page{
		Title:      "Refused",
		Reason:     code,
		ReasonText: textFor(code),
		Nav:        true,
	})
	s.render(w, http.StatusBadRequest, p)
}

func (s *Server) page(r *http.Request, o Origin, headers []headerValue, p page) page {
	if p.Title == "" {
		p.Title = "Beacon passkey spike"
	}
	p.Host = r.Host
	p.OriginHeader = r.Header.Get("Origin")
	p.Headers = headers
	p.Recent = s.recentRows()
	p.Creds = s.credRows(o.RPID)
	if o.Mode == "ts.net" && s.login == "" && p.Reason == "" {
		p.LoginNote = "No --tailscale-login was set, so any non-empty Tailscale-User-Login is accepted. Restart with the flag to refuse another user's device."
	}
	return p
}

func (s *Server) issue(kind string, o Origin) (string, []byte, error) {
	idb := make([]byte, 16)
	ch := make([]byte, 32)
	if _, err := rand.Read(idb); err != nil {
		return "", nil, err
	}
	if _, err := rand.Read(ch); err != nil {
		return "", nil, err
	}
	id := base64.RawURLEncoding.EncodeToString(idb)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, c := range s.ceremonies {
		if !now.Before(c.expires) {
			delete(s.ceremonies, k)
		}
	}
	if len(s.ceremonies) >= 32 {
		return "", nil, errCeremony
	}
	s.ceremonies[id] = issued{kind: kind, rpID: o.RPID, origin: o.Origin, challenge: ch, expires: now.Add(2 * time.Minute)}
	return id, ch, nil
}

func (s *Server) take(id, kind string, o Origin) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.ceremonies[id]
	delete(s.ceremonies, id)
	if !ok || c.kind != kind || c.rpID != o.RPID || c.origin != o.Origin || !s.now().Before(c.expires) {
		return nil, errCeremony
	}
	return append([]byte(nil), c.challenge...), nil
}

func (s *Server) userID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.UserID
}

func (s *Server) add(rpID string, reg webauthn.Registration) error {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := base64.RawURLEncoding.EncodeToString(reg.CredentialID)
	for _, c := range s.state.Credentials {
		if c.ID == id {
			return errExists
		}
	}
	if len(s.state.Credentials) >= maxCredentials {
		return errFull
	}
	next := s.state
	next.Credentials = append(slices.Clone(next.Credentials), credential{
		ID:        id,
		RPID:      rpID,
		Algorithm: int64(reg.Algorithm),
		COSEKey:   base64.RawURLEncoding.EncodeToString(reg.COSEKey),
		SignCount: reg.SignCount,
		PRFSalt:   base64.RawURLEncoding.EncodeToString(salt),
		AAGUID:    hex.EncodeToString(reg.AAGUID[:]),
		Added:     s.now().UTC(),
	})
	if err := saveState(s.dir, next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *Server) credsFor(rpID string) []credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []credential
	for _, c := range s.state.Credentials {
		if c.RPID == rpID {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) cred(rpID string, id []byte) (credential, bool) {
	want := base64.RawURLEncoding.EncodeToString(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.state.Credentials {
		if c.RPID == rpID && c.ID == want {
			return c, true
		}
	}
	return credential{}, false
}

func (s *Server) update(rpID string, id []byte, count uint32, hash string) error {
	want := base64.RawURLEncoding.EncodeToString(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Credentials {
		c := &s.state.Credentials[i]
		if c.RPID == rpID && c.ID == want {
			c.SignCount = count
			if hash != "" && c.PRFHash == "" {
				c.PRFHash = hash
			}
			return saveState(s.dir, s.state)
		}
	}
	return errMissing
}

// rememberHash stores the first hash and reports how this one compares.
func (s *Server) rememberHash(rpID string, id []byte, count uint32, hash string) string {
	want := base64.RawURLEncoding.EncodeToString(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	match := "First PRF hash for this credential. Assert again, including from another device, and compare."
	for i := range s.state.Credentials {
		c := &s.state.Credentials[i]
		if c.RPID != rpID || c.ID != want {
			continue
		}
		c.SignCount = count
		switch {
		case c.PRFHash == "":
			c.PRFHash = hash
		case c.PRFHash == hash:
			match = "Matches the PRF hash already recorded for this credential."
		default:
			match = "Differs from the PRF hash already recorded for this credential. A synced passkey should not do that."
		}
		_ = saveState(s.dir, s.state)
		return match
	}
	return match
}

func (s *Server) credRows(rpID string) []credRow {
	creds := s.credsFor(rpID)
	var rows []credRow
	for _, c := range creds {
		hash := c.PRFHash
		if hash == "" {
			hash = "(not yet asserted)"
		}
		rows = append(rows, credRow{ID: c.ID, Alg: webauthn.Algorithm(c.Algorithm).String(), Hash: hash, Count: c.SignCount})
	}
	return rows
}

func (s *Server) note(r *http.Request, headers []headerValue, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = append(s.recent, arrival{
		When:      s.now().UTC(),
		Host:      r.Host,
		Origin:    r.Header.Get("Origin"),
		Tailscale: headers,
		Method:    r.Method,
		Path:      r.URL.Path,
		Reason:    reason,
		Remote:    r.RemoteAddr,
	})
	if len(s.recent) > 40 {
		s.recent = s.recent[len(s.recent)-40:]
	}
}

func (s *Server) recentRows() []recentRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]recentRow, 0, len(s.recent))
	for i := len(s.recent) - 1; i >= 0; i-- {
		a := s.recent[i]
		login := ""
		for _, h := range a.Tailscale {
			if http.CanonicalHeaderKey(h.Name) == "Tailscale-User-Login" {
				login = h.Value
			}
		}
		rows = append(rows, recentRow{
			When: a.When.Format(time.RFC3339), Method: a.Method, Path: a.Path,
			Host: a.Host, Origin: a.Origin, Login: login, Reason: a.Reason,
		})
	}
	return rows
}

func (s *Server) logRequest(r *http.Request, headers []headerValue, reason string) {
	s.writeLog(map[string]any{
		"event":     "spike_request",
		"host":      r.Host,
		"origin":    r.Header.Get("Origin"),
		"tailscale": headers,
		"remote":    r.RemoteAddr,
		"method":    r.Method,
		"path":      r.URL.Path,
		"reason":    reason,
	})
}

func (s *Server) logResult(kind, reason string, prf bool, hash string) {
	line := map[string]any{
		"event":  "spike_result",
		"kind":   kind,
		"reason": reason,
		"prf":    prf,
	}
	if hash != "" {
		line["prf_hash"] = hash
	}
	s.writeLog(line)
}

func (s *Server) writeLog(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	_, _ = s.logw.Write(append(b, '\n'))
}

func readForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	return r.ParseForm()
}

func b64(s string) ([]byte, error) {
	if s == "" {
		return nil, errMissing
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func codeOf(err error) string {
	var e *webauthn.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "malformed"
}

func subtleDiff(a, b []byte) bool {
	if len(a) != len(b) {
		return true
	}
	diff := byte(0)
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff != 0
}

func templateHTML(b []byte) template.JS {
	// json.Marshal escapes <, >, and &. template.JS inserts the object as a
	// script expression, which is what <script type="application/json"> needs.
	// A string would be quoted again.
	return template.JS(b)
}

func storeCode(err error) string {
	switch {
	case errors.Is(err, errExists):
		return "exists"
	case errors.Is(err, errFull):
		return "full"
	default:
		return "malformed"
	}
}

var (
	errCeremony = errors.New("ceremony")
	errExists   = errors.New("exists")
	errFull     = errors.New("full")
	errMissing  = errors.New("malformed")
)
