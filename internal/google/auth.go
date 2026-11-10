// Package google holds the Gmail, Calendar and Drive sources and the OAuth grant they share. The person brings their
// own OAuth client (DEC-2). Its id and secret, and the refresh token, are in the keychain; an access token is held in
// memory for one pass at a time and leaves this process only as an Authorization header. Every scope is read-only.
package google

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/limited"
	"github.com/CrossTerra/beacon/internal/source"
)

// Read-only scopes. Each source asks for the fewest that work; see docs/sources.md for why each is needed.
const (
	ScopeGmailMetadata  = "https://www.googleapis.com/auth/gmail.metadata"
	ScopeGmailReadonly  = "https://www.googleapis.com/auth/gmail.readonly"
	ScopeCalendarEvents = "https://www.googleapis.com/auth/calendar.events.readonly"
	ScopeCalendarRead   = "https://www.googleapis.com/auth/calendar.readonly"
	ScopeDriveReadonly  = "https://www.googleapis.com/auth/drive.readonly"
)

// Google's endpoints. Tests replace them with httptest servers.
const (
	TokenURL = "https://oauth2.googleapis.com/token"
	AuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	// GmailBase serves the Gmail API.
	GmailBase = "https://gmail.googleapis.com"
	// APIBase serves the Calendar and Drive APIs.
	APIBase = "https://www.googleapis.com"
)

// Scopes returns the scopes to request for the given source kinds. bodies asks for gmail.readonly instead of
// gmail.metadata: a grant that holds gmail.metadata cannot read a body even beside another Gmail scope.
func Scopes(kinds []string, bodies bool) []string {
	set := map[string]bool{}
	for _, k := range kinds {
		switch k {
		case "gmail":
			if bodies {
				set[ScopeGmailReadonly] = true
			} else {
				set[ScopeGmailMetadata] = true
			}
		case "calendar":
			set[ScopeCalendarEvents] = true
		case "drive":
			set[ScopeDriveReadonly] = true
			// Meeting notes are found through the events the person attended.
			set[ScopeCalendarEvents] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// AuthOptions configures the shared grant.
type AuthOptions struct {
	// TokenURL is the token endpoint. Empty is Google's.
	TokenURL string
	// Client and Refresh read the OAuth client and the refresh token from the keychain.
	Client  func() (keychain.Secret, error)
	Refresh func() (keychain.Secret, error)
	HTTP    *http.Client
	Now     func() time.Time
}

// Auth is one OAuth grant shared by the Google sources. It refreshes an access token when it needs one, and keeps
// it in memory until a minute before it expires.
type Auth struct {
	tokenURL string
	hostPort string
	client   func() (keychain.Secret, error)
	refresh  func() (keychain.Secret, error)
	now      func() time.Time

	mu     sync.Mutex
	http   *limited.Client
	access keychain.Secret
	expiry time.Time
	scopes map[string]bool
}

// NewAuth builds a grant. It reads no credential.
func NewAuth(o AuthOptions) (*Auth, error) {
	tu := o.TokenURL
	if tu == "" {
		tu = TokenURL
	}
	u, err := url.Parse(tu)
	if err != nil || u.Host == "" || !secureURL(u) {
		return nil, errors.New("google: the token URL is invalid")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Auth{
		tokenURL: tu, hostPort: hostPort(u), client: o.Client, refresh: o.Refresh, now: now,
		http: &limited.Client{HTTP: o.HTTP, Now: now},
	}, nil
}

// Host is the token endpoint's host:port.
func (a *Auth) Host() string { return a.hostPort }

// SetHTTP sets the client used for the token endpoint.
func (a *Auth) SetHTTP(c *http.Client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.http.HTTP = c
}

// token returns an access token and the granted scopes.
func (a *Auth) token(ctx context.Context) (keychain.Secret, map[string]bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.access.Empty() && a.now().Before(a.expiry) {
		return a.access, a.scopes, nil
	}
	if a.client == nil || a.refresh == nil || a.http.HTTP == nil {
		return keychain.Secret{}, nil, errors.New("google: the source has no credential or client")
	}
	client, err := a.client()
	if err != nil {
		return keychain.Secret{}, nil, err
	}
	refresh, err := a.refresh()
	if err != nil {
		return keychain.Secret{}, nil, err
	}
	req, err := keychain.RefreshRequest(ctx, a.tokenURL, client, refresh)
	if err != nil {
		return keychain.Secret{}, nil, err
	}
	resp, err := a.http.Do(ctx, req)
	if err != nil {
		return keychain.Secret{}, nil, err
	}
	defer resp.Body.Close()
	tok, err := keychain.ReadToken(resp)
	if errors.Is(err, keychain.ErrGrantRefused) {
		return keychain.Secret{}, nil, source.ErrUnauthorized
	}
	if err != nil {
		return keychain.Secret{}, nil, err
	}
	life := tok.ExpiresIn
	if life <= 0 {
		life = time.Hour
	}
	a.access = tok.Access
	a.expiry = a.now().Add(life - time.Minute)
	a.scopes = map[string]bool{}
	for _, s := range tok.Scopes {
		a.scopes[s] = true
	}
	return a.access, a.scopes, nil
}

// forget drops the access token after the API refused it, so the next pass refreshes.
func (a *Auth) forget() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.access = keychain.Secret{}
	a.expiry = time.Time{}
}

func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// secureURL is https, or http to a loopback address (tests).
func secureURL(u *url.URL) bool {
	if u.User != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return loopback(u.Hostname())
	}
	return false
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
