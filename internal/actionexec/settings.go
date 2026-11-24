// Package actionexec implements the Go service's approval handlers. The collector does not use these clients.
package actionexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/vault"
)

const settingsName = "action-providers.json"

type Settings struct {
	GitHub *GitHubSettings `json:"github"`
	Slack  *SlackSettings  `json:"slack"`
}

type GitHubSettings struct {
	Origin string `json:"origin"`
	UserID int64  `json:"user_id"`
}

const slackOrigin = "https://slack.com"

type SlackSettings struct {
	Workspace string `json:"workspace"`
	UserID    string `json:"user_id"`
}

func (s SlackSettings) binding() (vault.Binding, error) {
	if !slackID.MatchString(s.Workspace) || !slackUserID.MatchString(s.UserID) {
		return vault.Binding{}, errSettings
	}
	return vault.Binding{Service: "slack", Origin: slackOrigin, Account: "slack:user:" + s.UserID, Workspace: s.Workspace}, nil
}

var (
	errSettings  = errors.New("invalid action-providers.json")
	originSyntax = regexp.MustCompile(`^https://[a-z0-9.-]{1,253}(:[0-9]{1,5})?$`)
)

func (s GitHubSettings) binding() (vault.Binding, error) {
	u, err := url.Parse(s.Origin)
	if err != nil || !originSyntax.MatchString(s.Origin) || s.UserID <= 0 || s.UserID > 1<<53-1 {
		return vault.Binding{}, errSettings
	}
	if u.Port() != "" {
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 || strconv.FormatUint(port, 10) != u.Port() {
			return vault.Binding{}, errSettings
		}
	}
	for _, part := range strings.Split(u.Hostname(), ".") {
		if part == "" || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return vault.Binding{}, errSettings
		}
	}
	return vault.Binding{Service: "github", Origin: s.Origin, Account: "github:user:" + strconv.FormatInt(s.UserID, 10)}, nil
}

func LoadSettings(dir string) (Settings, error) {
	f, err := approval.OpenPrivate(filepath.Join(dir, settingsName), os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, errSettings
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return Settings{}, errSettings
	}
	// Reject duplicate keys and malformed JSON before decoding the settings schema.
	if _, err := approval.ParseObject(b); err != nil {
		return Settings{}, errSettings
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	var s Settings
	if d.Decode(&s) != nil {
		return Settings{}, errSettings
	}
	return s, s.check()
}

func (s Settings) check() error {
	if s.GitHub != nil {
		if _, err := s.GitHub.binding(); err != nil {
			return err
		}
	}
	if s.Slack != nil {
		_, err := s.Slack.binding()
		return err
	}
	return nil
}

// Hosts is the same list used to guard each client. It reads no credentials.
func (s Settings) Hosts() []egress.Host {
	var hosts []egress.Host
	if s.GitHub != nil {
		u, _ := url.Parse(s.GitHub.Origin)
		hosts = append(hosts, egress.Host{Address: egress.Address(u),
			Purpose: "Go approvals: GitHub account validation, github.merge reads and one approved merge"})
	}
	if s.Slack != nil {
		u, _ := url.Parse(slackOrigin)
		hosts = append(hosts, egress.Host{Address: egress.Address(u),
			Purpose: "Go approvals: Slack account validation, slack.post reads and one approved post"})
	}
	return hosts
}

// Handler is the seam at a durable passkey-approved start. A future local handler can use the same seam
// without opening provider credentials or implementing provider read-back.
type Handler interface {
	Resolve(context.Context, approval.Proposal) (approval.Resolved, error)
	// Unlock is called only with the data key from a verified assertion, before anything is started.
	// A future local handler can return an empty secret and use its own passkey-approved start path.
	Unlock(*approval.Frozen, []byte) (keychain.Secret, error)
	Run(context.Context, *approval.Journal, approval.View, keychain.Secret) error
}

type ReadBack interface {
	Reconcile(context.Context, *approval.Journal, string) error
	Due(approval.View) bool
}

// Providers is immutable after construction. No request can change the configured origins or accounts.
type Providers struct {
	github *mergeHandler
	slack  *postHandler
}

// New constructs guarded clients without making a request. transport is an injection point for fakes.
func New(s Settings, dir string, now func() time.Time, transport func([]string) http.RoundTripper) (*Providers, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	preflight := newBudget(120, 2)
	p := &Providers{}
	if s.GitHub != nil {
		b, _ := s.GitHub.binding()
		p.github = &mergeHandler{provider: newProvider(b, dir, now, transport, preflight), user: s.GitHub.UserID}
	}
	if s.Slack != nil {
		b, _ := s.Slack.binding()
		p.slack = &postHandler{provider: newProvider(b, dir, now, transport, preflight), user: s.Slack.UserID,
			intake: newBudget(120, 2), intakePerPeer: 30}
	}
	return p, nil
}

func (p *Providers) Handlers() map[approval.Action]Handler {
	h := make(map[approval.Action]Handler)
	if p != nil && p.github != nil {
		h[approval.Merge] = p.github
	}
	if p != nil && p.slack != nil {
		h[approval.Post] = p.slack
	}
	return h
}

func (p *Providers) SlackIntake() func(context.Context, approval.Request, string) (approval.SlackPlacement, error) {
	if p == nil || p.slack == nil {
		return nil
	}
	return p.slack.place
}

type CredentialTarget struct {
	Service, Origin, Account, Workspace string
}

func (p *Providers) Targets() []CredentialTarget {
	var out []CredentialTarget
	if p != nil && p.github != nil {
		b := p.github.binding
		out = append(out, CredentialTarget{Service: b.Service, Origin: b.Origin, Account: b.Account})
	}
	if p != nil && p.slack != nil {
		b := p.slack.binding
		out = append(out, CredentialTarget{Service: b.Service, Origin: b.Origin, Account: b.Account, Workspace: b.Workspace})
	}
	return out
}

// Save verifies both credentials before touching disk. The supplied data key comes from the separate settings
// assertion, never from the request's form fields.
func (p *Providers) Save(ctx context.Context, service string, read, write keychain.Secret, key []byte) error {
	if !keychain.ActionToken(read) || !keychain.ActionToken(write) || len(key) != 32 {
		return refusal("credential_unavailable")
	}
	if read.Same(write) {
		return refusal("same_credential")
	}
	if p != nil && p.github != nil && service == "github" {
		return p.github.save(ctx, read, write, key)
	}
	if p != nil && p.slack != nil && service == "slack" {
		return p.slack.save(ctx, read, write, key)
	}
	return refusal("not_configured")
}
