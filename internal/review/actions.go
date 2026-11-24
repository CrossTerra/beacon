package review

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/passkey"
)

// Actions are installed before serving. The default site has none and stays a read-only proposal desk.
type Actions struct {
	Supports func(approval.Action) bool
	Resolve  func(context.Context, approval.Proposal) (approval.Resolved, error)
	// Approve is only called with the exact held snapshots and key from the verified assertion. The
	// callback must not keep the vault key or attach it to work that outlives this request.
	Approve     func(items []approval.Frozen, credentialID, batchChallenge, key []byte, pin io.Closer) error
	ReadBack    func(context.Context, approval.View) error
	CanReadBack func(approval.Action) bool
	Targets     []CredentialTarget
	Save        func(context.Context, string, keychain.Secret, keychain.Secret, []byte) error
	Checkouts   []Checkout
	RunModels   []string
	CanStopRun  func(string) bool
	StopRun     func(string) error
}

func (s *Site) SetActions(actions Actions) { s.actions = actions }

func (s *Site) supports(a approval.Action) bool {
	return s.actions.Resolve != nil && (s.actions.Supports == nil || s.actions.Supports(a))
}

func (s *Site) canReadBack(a approval.Action) bool {
	return s.actions.ReadBack != nil && s.supports(a) && (s.actions.CanReadBack == nil || s.actions.CanReadBack(a))
}

type pendingRow struct {
	approval.Proposal
	Review bool
}

func (s *Site) pendingPage(w http.ResponseWriter, sess session, at time.Time) {
	pending, err := s.journal.Pending(at)
	if err != nil {
		http.Error(w, "journal unavailable", http.StatusServiceUnavailable)
		return
	}
	model := struct {
		Rows                  []pendingRow
		Unknown               []approval.View
		CSRF                  string
		Live, Settings, Batch bool
	}{CSRF: sess.csrf, Live: s.actions.Approve != nil, Settings: s.hasSettings()}
	for _, p := range pending {
		review := s.supports(p.Type)
		if review && (p.Type == approval.Merge || p.Type == approval.Post) {
			model.Batch = true
		}
		model.Rows = append(model.Rows, pendingRow{p, review})
	}
	if j, ok := s.journal.(interface{ Unknowns() []approval.View }); ok && s.actions.ReadBack != nil {
		model.Unknown = j.Unknowns()
	}
	render(w, "pending", http.StatusOK, model)
}

func (s *Site) readBack(w http.ResponseWriter, r *http.Request) {
	if s.actions.ReadBack == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PostForm.Get("id")
	if !approval.ValidID(id, "apr") {
		http.NotFound(w, r)
		return
	}
	v, _, err := s.journal.Get(id, s.now())
	if errors.Is(err, approval.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err == nil && !s.canReadBack(v.Type) {
		http.NotFound(w, r)
		return
	}
	if err == nil && v.Status == "outcome_unknown" {
		err = s.actions.ReadBack(r.Context(), v)
	}
	if err != nil {
		refused(w, "Read outcome", err)
		return
	}
	http.Redirect(w, r, "/p/"+id, http.StatusSeeOther)
}

type CredentialTarget struct{ Service, Origin, Account, Workspace string }
type Checkout struct{ Name, Path string }

func (s *Site) hasSettings() bool {
	return len(s.actions.Targets) != 0 || len(s.actions.Checkouts) != 0
}

func (s *Site) settingsPage(w http.ResponseWriter, sess session, saved bool) {
	render(w, "settings", http.StatusOK, struct {
		Targets   []CredentialTarget
		Checkouts []Checkout
		CSRF      string
		Saved     bool
	}{s.actions.Targets, s.actions.Checkouts, sess.csrf, saved})
}

// A signed-in session may stop an existing run; it cannot start or approve one.
func (s *Site) stopRun(w http.ResponseWriter, r *http.Request) {
	id := r.PostForm.Get("id")
	if s.actions.StopRun == nil || !approval.ValidID(id, "apr") {
		http.NotFound(w, r)
		return
	}
	v, _, err := s.journal.Get(id, s.now())
	if err == nil && (v.Type != approval.Run || v.Status != "executed") {
		err = approval.ErrTransition
	}
	if err == nil {
		err = s.actions.StopRun(id)
	}
	if err != nil {
		refused(w, "Stop run", err)
		return
	}
	http.Redirect(w, r, "/p/"+id, http.StatusSeeOther)
}

func (s *Site) changeCredentials(w http.ResponseWriter, r *http.Request) {
	if s.actions.Save == nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/settings/begin" {
		selection, ok := s.beginSelection(r)
		if !ok {
			http.Error(w, "passkey unavailable", http.StatusUnauthorized)
			return
		}
		var target *CredentialTarget
		for _, t := range s.actions.Targets {
			if t.Service == r.PostForm.Get("service") {
				target = &t
				break
			}
		}
		if target == nil {
			http.NotFound(w, r)
			return
		}
		creds, err := passkey.List(s.state)
		if err != nil || len(creds) == 0 {
			refused(w, "Save credentials", approval.Refusal{Code: "no_passkey"})
			return
		}
		page := ceremonyPage{Action: "/settings/save", Title: "Save provider credentials", Button: "Save with passkey", Target: target}
		s.challenge(w, creds[0], selection, held{target: target.Service}, page, func(instance string, nonce []byte) ([]byte, error) {
			return passkey.SettingsChallenge(s.origin.URL(), instance, nonce, target.Service, target.Origin, target.Account, target.Workspace)
		})
		return
	}
	item, key := s.openAssertion(w, r, true)
	if key == nil {
		return
	}
	defer item.close()
	defer clear(key)
	read, write, err := keychain.ActionTokens(r.PostForm)
	delete(r.Form, "read_token")
	delete(r.Form, "write_token")
	defer read.Clear()
	defer write.Clear()
	if err != nil {
		refused(w, "Save credentials", approval.Refusal{Code: "credential_unavailable"})
		return
	}
	if err := s.actions.Save(r.Context(), item.target, read, write, key); err != nil {
		refused(w, "Save credentials", err)
		return
	}
	sess, _ := s.sessions.lookup(r, s.origin, s.now())
	s.settingsPage(w, sess, true)
}
