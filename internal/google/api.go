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
	// Account is the Google account this source reads. The API must report it as the signed-in account.
	Account string
	// Identity is person.identities for this source, email:<address>. Empty skips that check.
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
	kind     string
	name     string
	account  string
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
	// paths reports whether a URL path, with its query, is one this source calls.
	paths func(string, url.Values) bool
	// pace, when set, waits until the source's quota allows the request. It runs before the access token is read.
	pace func(context.Context, *url.URL) error
}

func newCommon(o Options, kind, defBase string, paths func(string, url.Values) bool) (common, error) {
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
		kind: kind, name: name, account: strings.ToLower(o.Account), person: o.Person, identity: o.Identity, base: base, host: strings.ToLower(u.Hostname()),
		hostPort: hostPort(u), auth: o.Auth, now: now, paths: paths,
		http: &limited.Client{HTTP: o.HTTP, Sleep: o.Sleep, Now: now, Reason: rateReason},
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

// PersonalStem is the subject stem this source's personal subjects start with ("gmail:"). The person's handle
// follows it (DEC-10). Signal keeps such a subject private only when "<stem>*" is restricted.
func (c *common) PersonalStem() string { return c.kind + ":" }

// SetBodies says whether a rule lists body for this source. Bodies are read only then.
func (c *common) SetBodies(b bool) { c.bodies = b }

func (c *common) observed() time.Time {
	if t := c.clock.First(); !t.IsZero() {
		return t
	}
	return c.now().UTC()
}

// checkIdentity compares the configured account and person.identities with the address the API says is the
// person's. A grant for another account is refused.
func (c *common) checkIdentity(email string) error {
	if email == "" {
		return nil
	}
	if c.account != "" && !strings.EqualFold(c.account, email) {
		return source.ErrIdentity
	}
	if c.identity != "" && !strings.EqualFold(c.identity, "email:"+email) {
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
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || u.Fragment != "" || !c.paths(u.EscapedPath(), q) {
		return source.ErrScope
	}
	return nil
}

// onlyKeys reports whether q has no key outside keys. alt, key, and any other parameter a path does not need is
// refused: alt=media on a file would download its content.
func onlyKeys(q url.Values, keys ...string) bool {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range q {
		if !allowed[k] {
			return false
		}
	}
	return true
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
	if c.pace != nil {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, source.ErrScope
		}
		if err := c.pace(ctx, u); err != nil {
			return nil, err
		}
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

// googleError is the part of Google's error body Beacon reads: reason codes, and the message only to compare it
// with a fixed text. Neither is logged as read.
type googleError struct {
	Error struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Errors  []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"errors"`
		Details []struct {
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

func googleReasons(body []byte) (reasons []string, concurrent bool) {
	var e googleError
	_ = json.Unmarshal(body, &e)
	reasons = []string{e.Error.Status}
	msgs := []string{e.Error.Message}
	for _, r := range e.Error.Errors {
		reasons = append(reasons, r.Reason)
		msgs = append(msgs, r.Message)
	}
	for _, r := range e.Error.Details {
		reasons = append(reasons, r.Reason)
	}
	for _, m := range msgs {
		// Gmail's per-user limit on concurrent requests answers with rateLimitExceeded and this message.
		concurrent = concurrent || strings.Contains(strings.ToLower(m), "too many concurrent requests")
	}
	return reasons, concurrent
}

// rateReason is the rate-limit reason code in a Google error body, from a fixed list, or "". A concurrent-request
// limit is "concurrentRequests", which Google does not name as a reason of its own. It never returns body text.
func rateReason(body []byte) string {
	reasons, concurrent := googleReasons(body)
	if concurrent {
		return "concurrentRequests"
	}
	for _, r := range reasons {
		if rateLimitReason(r) {
			return r
		}
	}
	return ""
}

func rateLimitReason(r string) bool {
	switch r {
	case "rateLimitExceeded", "userRateLimitExceeded", "RESOURCE_EXHAUSTED", "quotaExceeded", "RATE_LIMIT_EXCEEDED", "dailyLimitExceeded":
		return true
	}
	return false
}

// classify403 reads Google's error reasons. A rate limit ends the pass; a missing scope is ErrUnauthorized; a
// permission on one object is errPermission (the object is skipped). Any other 403 fails the pass: it is not read
// as "this object is gone".
func classify403(body []byte) error {
	reasons, _ := googleReasons(body)
	if r := rateReason(body); r != "" {
		return &source.RateLimitError{RateLimit: source.RateLimit{Status: http.StatusForbidden, Reason: r, RetryAfter: -1}}
	}
	for _, r := range reasons {
		switch r {
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
