package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/limited"
	"github.com/CrossTerra/beacon/internal/source"
)

var (
	personPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)
)

// Options configures one Google source.
type Options struct {
	Name string
	// Person is the person's Signal handle without "@". Personal subjects carry it (DEC-10).
	Person string
	// Identity is person.identities for this source, email:<address>. Empty skips the check.
	Identity string
	Auth     *Auth
	// Base replaces the API base URL (tests). Gmail's is GmailBase; Calendar's and Drive's is APIBase.
	Base  string
	HTTP  *http.Client
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// common is what every Google source holds.
type common struct {
	name     string
	person   string
	identity string
	base     string
	host     string
	hostPort string
	auth     *Auth
	http     *limited.Client
	now      func() time.Time
	clock    source.Clock
	bodies   bool
	// paths reports whether a URL path is one this source calls.
	paths func(string) bool
}

func newCommon(o Options, kind, defBase string, paths func(string) bool) (common, error) {
	name := o.Name
	if name == "" {
		name = kind
	}
	if !source.ValidName(name) {
		return common{}, fmt.Errorf("%s: name %q is invalid", kind, name)
	}
	if !personPattern.MatchString(o.Person) || strings.HasPrefix(o.Person, "act_") {
		return common{}, fmt.Errorf("%s: person.handle must be an @handle; personal subjects carry it", kind)
	}
	if o.Auth == nil {
		return common{}, fmt.Errorf("%s: the source has no Google grant", kind)
	}
	base := o.Base
	if base == "" {
		base = defBase
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || !secureURL(u) {
		return common{}, fmt.Errorf("%s: base URL is invalid", kind)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return common{
		name: name, person: o.Person, identity: o.Identity, base: base, host: strings.ToLower(u.Hostname()),
		hostPort: hostPort(u), auth: o.Auth, now: now, paths: paths,
		http: &limited.Client{HTTP: o.HTTP, Sleep: o.Sleep, Now: now},
	}, nil
}

// Name implements source.Source.
func (c *common) Name() string { return c.name }

// Hosts implements source.Source: the API host and the token endpoint.
func (c *common) Hosts() []string {
	if c.auth.Host() == c.hostPort {
		return []string{c.hostPort}
	}
	return []string{c.hostPort, c.auth.Host()}
}

// SetHTTP sets the client used to collect and to refresh the grant. The app sets it after the egress list, which is
// built from Hosts, exists.
func (c *common) SetHTTP(h *http.Client) {
	c.http.HTTP = h
	c.auth.SetHTTP(h)
}

// SetBodies says whether a rule lists body for this source. Bodies are read only then.
func (c *common) SetBodies(b bool) { c.bodies = b }

func (c *common) observed() time.Time {
	if t := c.clock.First(); !t.IsZero() {
		return t
	}
	return c.now().UTC()
}

// checkIdentity compares person.identities with the address the API says is the person's.
func (c *common) checkIdentity(email string) error {
	if c.identity == "" || email == "" {
		return nil
	}
	if !strings.EqualFold(c.identity, "email:"+email) {
		return source.ErrIdentity
	}
	return nil
}

// grant returns the granted scopes, refreshing the access token if needed, and checks that one of each group of
// scopes was granted. A missing scope is ErrUnauthorized: the person authorises again.
func (c *common) grant(ctx context.Context, groups ...[]string) (map[string]bool, error) {
	_, scopes, err := c.auth.token(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		ok := false
		for _, s := range g {
			ok = ok || scopes[s]
		}
		if !ok {
			return nil, source.ErrUnauthorized
		}
	}
	return scopes, nil
}

// allow checks a URL against the host and the API paths this source calls. Nothing else is requested.
func (c *common) allow(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !secureURL(u) || !strings.EqualFold(u.Hostname(), c.host) || hostPort(u) != c.hostPort {
		return source.ErrScope
	}
	if !c.paths(u.EscapedPath()) {
		return source.ErrScope
	}
	return nil
}

func (c *common) get(ctx context.Context, raw string) ([]byte, error) {
	return c.call(ctx, http.MethodGet, raw, 8<<20, "application/json")
}

func (c *common) call(ctx context.Context, method, raw string, limit int64, accept string) ([]byte, error) {
	if err := c.allow(raw); err != nil {
		return nil, err
	}
	if c.http.HTTP == nil {
		return nil, errors.New("google: the source has no client")
	}
	tok, _, err := c.auth.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, nil)
	if err != nil {
		return nil, err
	}
	tok.Authorize(req.Header)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "beacon")
	resp, err := c.http.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		c.auth.forget()
		return nil, source.ErrUnauthorized
	}
	c.clock.Note(resp.Header)
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, classify403(data)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &statusError{resp.StatusCode}
	}
	return data, nil
}

// classify403 reads Google's error reasons. A rate limit ends the pass; a missing scope is ErrUnauthorized; a
// permission on one object is errPermission (the object is skipped). Any other 403 fails the pass: it is not read
// as "this object is gone".
func classify403(body []byte) error {
	var e struct {
		Error struct {
			Status string `json:"status"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	reasons := []string{e.Error.Status}
	for _, r := range e.Error.Errors {
		reasons = append(reasons, r.Reason)
	}
	for _, r := range e.Error.Details {
		reasons = append(reasons, r.Reason)
	}
	for _, r := range reasons {
		switch r {
		case "rateLimitExceeded", "userRateLimitExceeded", "RESOURCE_EXHAUSTED", "quotaExceeded", "RATE_LIMIT_EXCEEDED":
			return source.ErrRateLimited
		case "insufficientPermissions", "ACCESS_TOKEN_SCOPE_INSUFFICIENT":
			return source.ErrUnauthorized
		}
	}
	for _, r := range reasons {
		switch r {
		case "forbidden", "insufficientFilePermissions", "appNotAuthorizedToFile", "cannotDownloadFile", "exportSizeLimitExceeded":
			return errPermission
		}
	}
	return &statusError{http.StatusForbidden}
}

var errPermission = errors.New("google: the grant cannot read this object")

type statusError struct{ status int }

func (e *statusError) Error() string { return fmt.Sprintf("google: status %d", e.status) }

func isStatus(err error, status int) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == status
}

// unavailable is one object the grant cannot read now (gone, or not permitted). The pass goes on.
func unavailable(err error) bool {
	return errors.Is(err, errPermission) || isStatus(err, http.StatusNotFound)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
