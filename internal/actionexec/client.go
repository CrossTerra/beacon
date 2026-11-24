package actionexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/vault"
)

const (
	maxResponse   = 4 << 20
	readDeadline  = 12 * time.Second // deadline per complete preflight, not per page
	actionTimeout = 25 * time.Second
)

func refusal(code string) approval.Refusal { return approval.Refusal{Code: code} }

// oneLine bounds display-only provider labels. The review template also reveals controls and escapes HTML.
func oneLine(s string, maxRunes int) string {
	s = strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s))
	seen := 0
	for i := range s {
		if seen == maxRunes {
			return s[:i]
		}
		seen++
	}
	return s
}

type budget struct {
	mu      sync.Mutex
	reads   []time.Time
	perHour int
	slots   chan struct{}
}

func newBudget(perHour, concurrent int) *budget {
	return &budget{perHour: perHour, slots: make(chan struct{}, concurrent)}
}

func (b *budget) enter() (func(), error) {
	select {
	case b.slots <- struct{}{}:
		return func() { <-b.slots }, nil
	default:
		return nil, refusal("busy")
	}
}

func (b *budget) take(at time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads = slices.DeleteFunc(b.reads, func(read time.Time) bool { return at.Sub(read) >= time.Hour })
	if len(b.reads) >= b.perHour {
		return refusal("busy")
	}
	b.reads = append(b.reads, at)
	return nil
}

type provider struct {
	dir                        string
	binding                    vault.Binding
	now                        func() time.Time
	client                     *http.Client
	preflight, final, recovery *budget
	read                       func() (keychain.Secret, error)
	write                      func([]byte) (keychain.Secret, error)
	saving                     sync.Mutex
	recovering                 sync.Map // only holds IDs of read-backs in flight
}

func newProvider(b vault.Binding, dir string, now func() time.Time, transport func([]string) http.RoundTripper, preflight *budget) *provider {
	u, _ := url.Parse(b.Origin)
	allowed := []string{egress.Address(u)}
	var rt http.RoundTripper
	if transport == nil {
		rt = egress.Transport(allowed)
	} else {
		rt = egress.Guard(allowed, transport(allowed))
	}
	return &provider{dir: dir, binding: b, now: now,
		preflight: preflight, final: newBudget(120, 2), recovery: newBudget(60, 2),
		client: &http.Client{Transport: rt, Timeout: actionTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		read:  func() (keychain.Secret, error) { return keychain.ReadAction(dir, b) },
		write: func(k []byte) (keychain.Secret, error) { return keychain.UnsealActionWrite(dir, b, k) },
	}
}

func (p *provider) store(read, write keychain.Secret, key []byte) error {
	p.saving.Lock()
	defer p.saving.Unlock()
	// Don't move an existing sealed credential into the unattended vault during rotation.
	if old, err := keychain.UnsealActionWrite(p.dir, p.binding, key); err == nil {
		defer old.Clear()
		if old.Same(read) {
			return refusal("same_credential")
		}
	}
	if err := keychain.StoreActionRead(p.dir, p.binding, read); err != nil {
		return refusal("credential_unavailable")
	}
	if err := keychain.StoreActionWrite(p.dir, p.binding, write, key); err != nil {
		return refusal("credential_unavailable")
	}
	return nil
}

func (p *provider) Unlock(f *approval.Frozen, key []byte) (keychain.Secret, error) {
	if !p.bound(f) {
		return keychain.Secret{}, refusal("destination_changed")
	}
	if len(key) != 32 {
		return keychain.Secret{}, refusal("credential_unavailable")
	}
	write, err := p.write(key)
	if err != nil || !keychain.ActionToken(write) {
		write.Clear()
		return keychain.Secret{}, refusal("credential_unavailable")
	}
	read, err := p.read()
	defer read.Clear()
	if err != nil || !keychain.ActionToken(read) || write.Same(read) {
		write.Clear()
		return keychain.Secret{}, refusal("credential_unavailable")
	}
	return write, nil
}

func (p *provider) bound(f *approval.Frozen) bool {
	return f != nil && f.Origin == p.binding.Origin && f.Account == p.binding.Account &&
		(p.binding.Workspace == "" || f.Destination["workspace"] == p.binding.Workspace)
}

func (p *provider) reader() (keychain.Secret, error) {
	token, err := p.read()
	if err != nil || !keychain.ActionToken(token) {
		token.Clear()
		return keychain.Secret{}, refusal("credential_unavailable")
	}
	return token, nil
}

type answer struct {
	status int
	header http.Header
	body   []byte
}

// call's path and query are built only by the typed clients. No pagination or redirect URL from a response
// is requested. A write body has no GetBody, so the transport cannot replay it on a broken connection.
func (p *provider) call(ctx context.Context, token keychain.Secret, b *budget, method, path string, query url.Values, body any) (answer, error) {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return answer{}, refusal("unavailable")
		}
	}
	u := p.binding.Origin + path
	if len(query) != 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(data))
	if err != nil || ctx.Err() != nil {
		return answer{}, refusal("unavailable")
	}
	if b != nil {
		if err := b.take(p.now()); err != nil {
			return answer{}, err
		}
	}
	req.Header.Set("User-Agent", "beacon")
	if p.binding.Service == "github" {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	if body != nil {
		req.GetBody = nil
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	token.Authorize(req.Header)
	res, err := p.client.Do(req)
	if err != nil {
		return answer{}, refusal("unavailable")
	}
	defer res.Body.Close()
	a := answer{status: res.StatusCode, header: res.Header}
	a.body, err = io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil || len(a.body) > maxResponse {
		a.body = nil
		return a, refusal("unavailable")
	}
	return a, nil
}

func (a answer) decode(out any) error {
	if a.status < 200 || a.status >= 300 || json.Unmarshal(a.body, out) != nil {
		return refusal("unavailable")
	}
	return nil
}

func (p *provider) fail(j *approval.Journal, id, by string, err error) error {
	r := approval.Refusal{Code: "unavailable"}
	_ = errors.As(err, &r)
	return j.SetResult(id, approval.Result{ConfirmedBy: by, Error: r.Code}, p.now())
}

func (p *provider) readView(ctx context.Context, j *approval.Journal, id string, action approval.Action) (approval.View, func(), bool) {
	if _, busy := p.recovering.LoadOrStore(id, struct{}{}); busy {
		return approval.View{}, nil, false
	}
	leave := func() { p.recovering.Delete(id) }
	v, _, err := j.Get(id, p.now())
	if err != nil || ctx.Err() != nil || v.Type != action || v.Status != "outcome_unknown" ||
		!p.bound(v.Frozen) {
		leave()
		return approval.View{}, nil, false
	}
	return v, leave, true
}
