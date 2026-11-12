package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/limited"
	"github.com/CrossTerra/beacon/internal/source"
)

// Base is the Slack Web API's base URL. Every method is under /api/.
const Base = "https://slack.com"

// Scopes are the user scopes a pass needs. All are read-only. A token missing one fails the pass.
var Scopes = []string{
	"search:read", "channels:history", "groups:history", "im:history", "mpim:history",
	"channels:read", "groups:read", "mpim:read", "usergroups:read",
}

// NameScopes are the user scopes display names need: users.info, and conversations.info on a direct message. A
// token missing one still collects; titles and names stay out of the items, and the pass says so once.
var NameScopes = []string{"users:read", "im:read"}

// The methods Beacon calls, and how far apart it spaces calls to each. Slack limits each method per workspace by tier:
// search.messages and usergroups.list are Tier 2 (20 a minute); conversations.history, conversations.replies,
// conversations.info and users.conversations are Tier 3 (50 a minute for an internal app); users.info and
// conversations.members are Tier 4 (100 a minute). auth.test is called once a pass.
const (
	methodAuth       = "auth.test"
	methodSearch     = "search.messages"
	methodReplies    = "conversations.replies"
	methodHistory    = "conversations.history"
	methodInfo       = "conversations.info"
	methodMembers    = "conversations.members"
	methodGroupDMs   = "users.conversations"
	methodUserGroups = "usergroups.list"
	methodUser       = "users.info"
)

var spacing = map[string]time.Duration{
	methodSearch:     3 * time.Second,
	methodUserGroups: 3 * time.Second,
	methodReplies:    1200 * time.Millisecond,
	methodHistory:    1200 * time.Millisecond,
	methodInfo:       1200 * time.Millisecond,
	methodGroupDMs:   1200 * time.Millisecond,
	methodUser:       600 * time.Millisecond,
	methodMembers:    600 * time.Millisecond,
}

var (
	idPattern      = regexp.MustCompile(`^[A-Z][A-Z0-9]{2,30}$`)
	userPattern    = regexp.MustCompile(`^[UW][A-Z0-9]{2,30}$`)
	channelPattern = regexp.MustCompile(`^[CDG][A-Z0-9]{2,30}$`)
	tsPattern      = regexp.MustCompile(`^[0-9]{9,11}\.[0-9]{6}$`)
	cursorPattern  = regexp.MustCompile(`^[A-Za-z0-9=_:-]{1,200}$`)
	codePattern    = regexp.MustCompile(`^[a-z_]{1,60}$`)
	searchPattern  = regexp.MustCompile(`^(is:dm|<@([UW][A-Z0-9]+)>|from:<@([UW][A-Z0-9]+)>) after:[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// Hosts is the host:port of a Slack API base URL, the one host a Slack source contacts. `beacon egress` and
// `beacon source add slack` both read it.
func Hosts(base string) []string {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" {
		return nil
	}
	return []string{hostPort(u)}
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
		h := u.Hostname()
		if h == "localhost" {
			return true
		}
		ip := net.ParseIP(h)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

// api calls the Slack Web API. Every URL is checked against the methods and query keys Beacon uses before it is
// sent, and calls to each method are spaced by its tier.
type api struct {
	base     string
	host     string
	hostPort string
	http     *limited.Client
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	clock    source.Clock
	// configured reports whether a channel is one the config names; groupDM whether it is a group direct message
	// the person is in. conversations.history reads only those. named reports whether a conversation holds a message
	// this pass made an item of, or a thread Beacon is retitling: conversations.info reads configured and named ones,
	// conversations.members only named ones.
	configured func(string) bool
	groupDM    func(string) bool
	named      func(string) bool
	// user is the token's user. Search queries may name only this user.
	user string
	last map[string]time.Time
}

func newAPI(base string, o Options, now func() time.Time) (*api, error) {
	if base == "" {
		base = Base
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || !secureURL(u) || strings.Trim(u.Path, "/") != "" {
		return nil, errors.New("slack: base URL is invalid")
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	return &api{
		base: base, host: strings.ToLower(u.Hostname()), hostPort: hostPort(u), now: now, sleep: sleep,
		http: &limited.Client{HTTP: o.HTTP, Sleep: o.Sleep, Now: now}, last: map[string]time.Time{},
		configured: func(string) bool { return false }, groupDM: func(string) bool { return false },
		named: func(string) bool { return false },
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// allow refuses a URL outside slack.com, the four methods, and each method's query keys.
func (a *api) allow(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !secureURL(u) || !strings.EqualFold(u.Hostname(), a.host) || hostPort(u) != a.hostPort || u.Fragment != "" {
		return source.ErrScope
	}
	method, ok := strings.CutPrefix(u.EscapedPath(), "/api/")
	if !ok {
		return source.ErrScope
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return source.ErrScope
	}
	for _, vs := range q {
		if len(vs) != 1 {
			return source.ErrScope
		}
	}
	paging := q.Get("cursor") == "" || cursorPattern.MatchString(q.Get("cursor"))
	switch method {
	case methodAuth:
		if onlyKeys(q) {
			return nil
		}
	case methodSearch:
		m := searchPattern.FindStringSubmatch(q.Get("query"))
		if onlyKeys(q, "query", "count", "page", "sort", "sort_dir") && m != nil && (m[2] == "" || m[2] == a.user) && (m[3] == "" || m[3] == a.user) {
			return nil
		}
	case methodReplies:
		if onlyKeys(q, "channel", "ts", "oldest", "inclusive", "limit", "cursor") && channelPattern.MatchString(q.Get("channel")) &&
			tsPattern.MatchString(q.Get("ts")) && optionalTS(q, "oldest") && paging {
			return nil
		}
	case methodHistory:
		c := q.Get("channel")
		if onlyKeys(q, "channel", "oldest", "latest", "limit", "cursor") && (a.configured(c) || a.groupDM(c)) &&
			optionalTS(q, "oldest") && optionalTS(q, "latest") && paging {
			return nil
		}
	case methodInfo:
		c := q.Get("channel")
		if onlyKeys(q, "channel") && (a.configured(c) || a.named(c)) {
			return nil
		}
	case methodMembers:
		if onlyKeys(q, "channel", "limit") && a.named(q.Get("channel")) {
			return nil
		}
	case methodUser:
		if onlyKeys(q, "user") && userPattern.MatchString(q.Get("user")) {
			return nil
		}
	case methodGroupDMs:
		if onlyKeys(q, "types", "exclude_archived", "limit", "cursor") && q.Get("types") == "mpim" && paging {
			return nil
		}
	case methodUserGroups:
		if onlyKeys(q, "include_users") {
			return nil
		}
	}
	return source.ErrScope
}

func optionalTS(q url.Values, key string) bool {
	return q.Get(key) == "" || tsPattern.MatchString(q.Get(key))
}

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

// pace waits until the method's tier allows another call.
func (a *api) pace(ctx context.Context, method string) error {
	gap := spacing[method]
	if gap == 0 {
		return nil
	}
	now := a.now()
	next := a.last[method].Add(gap)
	if now.Before(next) {
		if err := a.sleep(ctx, next.Sub(now)); err != nil {
			return err
		}
		a.last[method] = next
		return nil
	}
	a.last[method] = now
	return nil
}

// envelope is what every Slack method answers with.
type envelope struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Metadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// call makes one GET and decodes the answer into dest. It returns the response header (auth.test's scopes are there).
func (a *api) call(ctx context.Context, token keychain.Secret, method string, q url.Values, dest any) (http.Header, error) {
	raw := a.base + "/api/" + method
	if len(q) > 0 {
		raw += "?" + q.Encode()
	}
	if err := a.allow(raw); err != nil {
		return nil, err
	}
	if a.http.HTTP == nil {
		return nil, errors.New("slack: the source has no client")
	}
	if err := a.pace(ctx, method); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	token.Authorize(req.Header)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "beacon")
	resp, err := a.http.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	a.clock.Note(resp.Header)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, source.ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &statusError{resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("slack: the %s answer could not be read", method)
	}
	if !env.OK {
		return nil, classify(env.Error)
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return nil, fmt.Errorf("slack: the %s answer could not be read", method)
	}
	return resp.Header, nil
}

// errGone is one thread or channel the token cannot read now (deleted, archived away, or the person left). The pass
// goes on without it.
var errGone = errors.New("slack: that conversation cannot be read")

// errMessageGone is a message, or a thread's root, that no longer exists: it was deleted. It is also errGone.
var errMessageGone = fmt.Errorf("%w: the message is gone", errGone)

// classify maps Slack's error codes. Its text is the code, a fixed word from Slack's list, never content.
func classify(code string) error {
	switch code {
	case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "missing_scope",
		"not_allowed_token_type", "no_permission", "ekm_access_denied", "team_access_not_granted":
		return source.ErrUnauthorized
	case "ratelimited":
		return source.ErrRateLimited
	case "thread_not_found", "message_not_found":
		return errMessageGone
	case "channel_not_found", "not_in_channel", "is_archived", "user_not_found", "user_not_visible",
		"method_not_supported_for_channel_type":
		return errGone
	}
	if !codePattern.MatchString(code) {
		code = "unknown"
	}
	return &apiError{code}
}

type apiError struct{ code string }

func (e *apiError) Error() string { return "slack: " + e.code }

type statusError struct{ status int }

func (e *statusError) Error() string { return fmt.Sprintf("slack: status %d", e.status) }

// Identity is what auth.test says about a token.
type Identity struct {
	Workspace string
	User      string
	// URL is the workspace's web address, https://<name>.slack.com/, used to build permalinks. Beacon never requests it.
	URL string
	// Scopes are the token's scopes; Missing are the ones a pass needs that it lacks; MissingNames are the NameScopes
	// it lacks; Extra are the ones Beacon does not use. Missing and MissingNames are empty when Slack did not say.
	Scopes       []string
	Missing      []string
	MissingNames []string
	Extra        []string
}

func (a *api) identity(ctx context.Context, token keychain.Secret) (Identity, error) {
	var resp struct {
		URL    string `json:"url"`
		TeamID string `json:"team_id"`
		UserID string `json:"user_id"`
		BotID  string `json:"bot_id"`
	}
	h, err := a.call(ctx, token, methodAuth, nil, &resp)
	if err != nil {
		return Identity{}, err
	}
	if resp.BotID != "" || !idPattern.MatchString(resp.TeamID) || !idPattern.MatchString(resp.UserID) {
		return Identity{}, source.ErrUnauthorized
	}
	id := Identity{Workspace: resp.TeamID, User: resp.UserID, URL: workspaceURL(resp.URL)}
	have := map[string]bool{}
	for _, s := range strings.Split(h.Get("X-OAuth-Scopes"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			have[s] = true
			id.Scopes = append(id.Scopes, s)
		}
	}
	sort.Strings(id.Scopes)
	need := map[string]bool{}
	for _, s := range Scopes {
		need[s] = true
		if !have[s] {
			id.Missing = append(id.Missing, s)
		}
	}
	for _, s := range NameScopes {
		need[s] = true
		if !have[s] {
			id.MissingNames = append(id.MissingNames, s)
		}
	}
	for _, s := range id.Scopes {
		if !need[s] {
			id.Extra = append(id.Extra, s)
		}
	}
	if h.Get("X-OAuth-Scopes") == "" {
		// Slack did not say. A missing scope then shows up as missing_scope on the method that needs it.
		id.Missing, id.MissingNames = nil, nil
	}
	return id, nil
}

// workspaceURL keeps a workspace address only when it is https on slack.com, so a permalink cannot point elsewhere.
func workspaceURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(h, ".slack.com") {
		return ""
	}
	return "https://" + h
}

// Verify reads a token's identity and scopes with auth.test, through client. `beacon source add slack` uses it before
// it stores the token.
func Verify(ctx context.Context, client *http.Client, base string, token keychain.Secret) (Identity, error) {
	a, err := newAPI(base, Options{HTTP: client}, time.Now)
	if err != nil {
		return Identity{}, err
	}
	return a.identity(ctx, token)
}
