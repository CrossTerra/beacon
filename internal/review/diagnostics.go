package review

import (
	"io"
	"log"
	"mime"
	"net/http"
	"sync"
	"time"
)

type passkeyDiagnostics struct {
	log  *log.Logger
	mu   sync.Mutex
	next map[string]time.Time // only fixed reason codes are keys
}

// SetDiagnostics configures the local service log before serving requests.
func (s *Site) SetDiagnostics(w io.Writer) {
	if w != nil {
		s.diagnostics = passkeyDiagnostics{log: log.New(w, "", 0), next: make(map[string]time.Time)}
	}
}

// Browser reports are hints, not proof of what happened. They have no session, do not
// touch enrollment, and are checked against the canonical origin just like registration.
func (s *Site) registrationReport(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "text/plain" || r.URL.RawQuery != "" {
		http.Error(w, "report refused", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64))
	if err != nil || len(body) == 0 {
		http.Error(w, "report refused", http.StatusBadRequest)
		return
	}
	reason := "other"
	switch string(body) {
	case "NotAllowedError", "InvalidStateError", "SecurityError", "NotSupportedError",
		"AbortError", "ConstraintError", "UnknownError", "TypeError", "prf_unavailable", "prf_missing", "webauthn_unavailable":
		reason = string(body)
	}
	s.registrationFailure(reason)
	w.WriteHeader(http.StatusNoContent)
}

// Only fixed codes from registration or the allow-list above reach this writer.
func (s *Site) registrationFailure(reason string) {
	d := &s.diagnostics
	if d.log == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	at := s.now()
	if !at.Before(d.next[reason]) {
		d.next[reason] = at.Add(time.Second)
		d.log.Printf("passkey.register.failed reason=%s", reason)
	}
}
