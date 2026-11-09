// Package github is the GitHub source. It reads pull requests in the configured repositories and nothing else.
// The token is read from the keychain at the start of each pass and is set only as an Authorization header.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/limited"
	"github.com/CrossTerra/beacon/internal/source"
)

const apiVersion = "2022-11-28"

// Options configures a GitHub source. Token is read once per pass.
type Options struct {
	Name     string
	Host     string
	Base     string
	Repos    []string
	Projects []string
	Identity string
	Token    func() (keychain.Secret, error)
	HTTP     *http.Client
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) error
}

// Source reads GitHub.
type Source struct {
	name       string
	host       string
	hostPort   string
	base       string
	scope      scope
	projects   map[string]bool
	identity   string
	token      func() (keychain.Secret, error)
	http       *limited.Client
	now        func() time.Time
	serverDate time.Time
	teams      map[string]bool
}

// New builds a source. It does not read the token.
func New(o Options) (*Source, error) {
	name := o.Name
	if name == "" {
		name = "github"
	}
	if !source.ValidName(name) {
		return nil, fmt.Errorf("github: name %q is invalid", name)
	}
	host := o.Host
	if host == "" {
		host = "api.github.com"
	}
	hostName := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostName = h
	}
	base := o.Base
	if base == "" {
		if strings.EqualFold(hostName, "api.github.com") {
			base = "https://api.github.com"
		} else {
			base = "https://" + host + "/api/v3"
		}
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("github: base URL is invalid")
	}
	sc, err := newScope(u.Hostname(), o.Repos)
	if err != nil {
		return nil, fmt.Errorf("github: repos are required")
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	projects := map[string]bool{}
	for _, p := range o.Projects {
		projects[p] = true
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Source{
		name: name, host: u.Hostname(), hostPort: net.JoinHostPort(u.Hostname(), port), base: base,
		scope: sc, projects: projects, identity: o.Identity, token: o.Token, now: now,
		http: &limited.Client{HTTP: o.HTTP, Sleep: o.Sleep, Now: now},
	}, nil
}

// Name implements source.Source.
func (s *Source) Name() string { return s.name }

// Fields implements source.Source.
func (s *Source) Fields() []string { return append([]string(nil), Fields...) }

// StateFields implements source.Source.
func (s *Source) StateFields() []string { return append([]string(nil), StateFields...) }

// Hosts implements source.Source.
func (s *Source) Hosts() []string { return []string{s.hostPort} }

// SetHTTP sets the client used to collect. The app sets it after the egress list, which is built from Hosts, exists.
func (s *Source) SetHTTP(c *http.Client) {
	if s.http == nil {
		s.http = &limited.Client{Now: s.now}
	}
	s.http.HTTP = c
}

// Collect implements source.Source. A nil cursor reads open pull requests, not history (DEC-23).
func (s *Source) Collect(ctx context.Context, cursor []byte) (source.Collected, error) {
	if s.token == nil || s.http == nil || s.http.HTTP == nil {
		return source.Collected{}, errors.New("github: the source has no token or client")
	}
	secret, err := s.token()
	if err != nil {
		return source.Collected{}, err
	}
	first := len(cursor) == 0
	var since time.Time
	if !first {
		since, err = time.Parse(time.RFC3339, string(cursor))
		if err != nil {
			return source.Collected{}, errors.New("github: the cursor is not a time")
		}
		since = since.UTC()
	}
	// Read a window before the cursor. The next cursor is the server's time or the newest event, never the
	// local clock, so a late index or a skewed clock is collected again. Keys make that harmless.
	floor := time.Time{}
	if !since.IsZero() {
		floor = since.Add(-source.Lookback)
	}
	login, err := s.currentLogin(ctx, secret)
	if err != nil {
		return source.Collected{}, err
	}
	// Logins are case-insensitive. The configured spelling is what `by` sends; this only checks it is the same user.
	if s.identity != "" && !strings.EqualFold(s.identity, "github:user:"+login) {
		return source.Collected{}, source.ErrIdentity
	}
	s.teams, err = s.userTeams(ctx, secret)
	if err != nil {
		return source.Collected{}, err
	}
	quals, err := s.qualifiers(ctx, secret)
	if err != nil {
		return source.Collected{}, err
	}
	hits, err := s.search(ctx, secret, s.queries(floor, first, quals), floor)
	if err != nil {
		return source.Collected{}, err
	}
	var openErr error
	if !first {
		// Check suites change without bumping a pull request's updated time, so own open pull requests are read
		// even when the updated search did not return them. A failure here does not discard the updated search:
		// those items are still returned, and the cursor stays so the next pass tries again.
		open, err := s.search(ctx, secret, s.queries(floor, true, quals), time.Time{})
		if err != nil {
			openErr = err
		} else {
			seen := map[string]bool{}
			for _, h := range hits {
				seen[h.key()] = true
			}
			for _, h := range open {
				if !strings.EqualFold(h.author, login) || seen[h.key()] {
					continue
				}
				hits = append(hits, h)
			}
		}
	}
	var items []source.Item
	for _, hit := range hits {
		if !s.scope.allowsRepo(hit.owner, hit.name) {
			continue
		}
		got, err := s.pull(ctx, secret, hit, login, floor, first, s.serverNow())
		if err != nil {
			return source.Collected{Items: items}, err
		}
		items = append(items, got...)
	}
	if openErr != nil {
		return source.Collected{Items: items}, openErr
	}
	next, err := cursorFrom(s.serverDate, items)
	if err != nil {
		return source.Collected{Items: items}, err
	}
	return source.Collected{Items: items, Next: next}, nil
}

// serverNow is the latest Date header seen this pass, or the local clock only as a timestamp on items when the
// server sent none. The cursor itself never uses the local clock.
func (s *Source) serverNow() time.Time {
	if !s.serverDate.IsZero() {
		return s.serverDate
	}
	return s.now().UTC()
}

func cursorFrom(server time.Time, items []source.Item) ([]byte, error) {
	// A server Date caps the cursor. A git author date can be ahead of GitHub; advancing to it would skip the
	// window until that date. Events already collected are deduped by key. With no Date, the newest event time
	// is the only clock the server gave us.
	t := server
	if t.IsZero() {
		for _, it := range items {
			if it.OccurredAt.After(t) {
				t = it.OccurredAt
			}
		}
	}
	if t.IsZero() {
		return nil, errors.New("github: the server sent no Date and there is no event time")
	}
	return []byte(t.UTC().Format(time.RFC3339)), nil
}

type hit struct {
	owner  string
	name   string
	n      int
	author string
}

func (h hit) key() string {
	return strings.ToLower(h.owner) + "/" + strings.ToLower(h.name) + "#" + fmt.Sprint(h.n)
}

func (s *Source) userTeams(ctx context.Context, secret keychain.Secret) (map[string]bool, error) {
	out := map[string]bool{}
	err := s.each(ctx, secret, s.base+"/user/teams?per_page=100", func(body []byte) error {
		var page []struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return errors.New("github: teams could not be read")
		}
		for _, t := range page {
			if t.Slug != "" {
				out[strings.ToLower(t.Slug)] = true
			}
		}
		return nil
	})
	if unavailable(err) {
		return map[string]bool{}, nil
	}
	return out, err
}

func (s *Source) currentLogin(ctx context.Context, secret keychain.Secret) (string, error) {
	body, _, err := s.get(ctx, secret, s.base+"/user")
	if err != nil {
		return "", err
	}
	var u ghUser
	if err := json.Unmarshal(body, &u); err != nil || u.Login == "" {
		return "", errors.New("github: the token's user has no login")
	}
	if strings.ContainsAny(u.Login, " \t\"'\\") {
		return "", errors.New("github: the token's login cannot be used in a query")
	}
	return u.Login, nil
}

func (s *Source) qualifiers(ctx context.Context, secret keychain.Secret) ([]string, error) {
	var quals []string
	for _, r := range s.scope.exactRepos() {
		quals = append(quals, "repo:"+r)
	}
	for _, owner := range s.scope.owners() {
		q, err := s.ownerQualifier(ctx, secret, owner)
		if err != nil {
			return nil, err
		}
		quals = append(quals, q)
	}
	if len(quals) == 0 {
		return nil, source.ErrScope
	}
	return quals, nil
}

func (s *Source) ownerQualifier(ctx context.Context, secret keychain.Secret, owner string) (string, error) {
	body, _, err := s.get(ctx, secret, s.base+"/users/"+url.PathEscape(owner))
	if err != nil {
		return "", err
	}
	var u ghUser
	if err := json.Unmarshal(body, &u); err != nil {
		return "", errors.New("github: the owner could not be read")
	}
	if strings.EqualFold(u.Type, "Organization") {
		return "org:" + owner, nil
	}
	return "user:" + owner, nil
}

func (s *Source) queries(since time.Time, first bool, quals []string) []string {
	var head strings.Builder
	head.WriteString("is:pr ")
	// No login qualifier. Some tokens can read the configured repositories and still cannot search a user
	// (422). The search is repo-scoped; involvement is checked on each pull request before an item is yielded.
	if first {
		head.WriteString("is:open ")
	} else if !since.IsZero() {
		fmt.Fprintf(&head, "updated:>=%s ", since.UTC().Format(time.RFC3339))
	}
	// One qualifier per query. GitHub's search treats an OR of repo: qualifiers as matching nothing.
	prefix := head.String()
	queries := make([]string, 0, len(quals))
	for _, q := range quals {
		queries = append(queries, prefix+q)
	}
	return queries
}

func (s *Source) search(ctx context.Context, secret keychain.Secret, queries []string, floor time.Time) ([]hit, error) {
	seen := map[string]bool{}
	var hits []hit
	for _, q := range queries {
		page := s.base + "/search/issues?" + url.Values{"q": {q}, "per_page": {"100"}, "sort": {"updated"}, "order": {"asc"}}.Encode()
		for pages := 0; page != "" && pages < 20; pages++ {
			body, next, err := s.get(ctx, secret, page)
			if err != nil {
				return nil, err
			}
			var resp struct {
				Items []struct {
					Number        int    `json:"number"`
					RepositoryURL string `json:"repository_url"`
					UpdatedAt     string `json:"updated_at"`
					User          ghUser `json:"user"`
					PullRequest   *struct {
						URL string `json:"url"`
					} `json:"pull_request"`
				} `json:"items"`
				Incomplete bool `json:"incomplete_results"`
				Total      int  `json:"total_count"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				return nil, errors.New("github: the search response could not be read")
			}
			if resp.Incomplete || resp.Total > 1000 {
				return nil, errors.New("github: the search is incomplete; the newest results would be cut off")
			}
			for _, item := range resp.Items {
				if item.PullRequest == nil {
					continue
				}
				if updated, err := parseTime(item.UpdatedAt); err == nil && !floor.IsZero() && updated.Before(floor) {
					continue
				}
				owner, name, ok := repoFromURL(item.RepositoryURL)
				if !ok || !s.scope.allowsRepo(owner, name) {
					continue
				}
				h := hit{owner: owner, name: name, n: item.Number, author: item.User.Login}
				if seen[h.key()] {
					continue
				}
				seen[h.key()] = true
				hits = append(hits, h)
			}
			page = next
		}
		if page != "" {
			return nil, errors.New("github: search pagination did not end")
		}
	}
	return hits, nil
}

func (s *Source) pull(ctx context.Context, secret keychain.Secret, h hit, login string, since time.Time, first bool, observed time.Time) ([]source.Item, error) {
	raw := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", s.base, url.PathEscape(h.owner), url.PathEscape(h.name), h.n)
	body, _, err := s.get(ctx, secret, raw)
	if unavailable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pr pull
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, errors.New("github: a pull request could not be read")
	}
	pr.owner, pr.name = h.owner, h.name
	// First run is open pull requests the person wrote or was asked to review, not every open pull request
	// in the repository (DEC-23).
	if first && !strings.EqualFold(pr.author(), login) && !reviewRequested(pr, login, s.teams) {
		return nil, nil
	}
	// A timeline or reviews failure is not "this pull request is gone". It fails the pass and the cursor stays,
	// so the events are read again. Skipping them and committing would drop them.
	timeline, err := s.timeline(ctx, secret, h)
	if err != nil {
		return nil, err
	}
	reviews, err := s.reviews(ctx, secret, h)
	if err != nil {
		return nil, err
	}
	var comments []reviewComment
	if !first {
		comments, err = s.reviewComments(ctx, secret, h, since)
		if unavailable(err) {
			// Line comments are also absent from a token that cannot read them. Issue comments still come from
			// the timeline. Omitting them does not stall every other event.
			comments, err = nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	var checks []checkSuite
	fetched := false
	if strings.EqualFold(pr.author(), login) && strings.EqualFold(pr.State, "open") && !pr.Merged && pr.headSHA() != "" {
		checks, fetched, err = s.checks(ctx, secret, h, pr.headSHA())
		if err != nil {
			return nil, err
		}
	}
	if !first && !involved(pr, timeline, reviews, comments, login, s.teams) {
		return nil, nil
	}
	return build(buildInput{
		Name: s.name, PR: pr, Timeline: timeline, Reviews: reviews, Comments: comments,
		Checks: checks, Fetched: fetched, Login: login, Since: since, First: first,
		Observed: observed, Projects: s.projects, Teams: s.teams,
	})
}

func (s *Source) timeline(ctx context.Context, secret keychain.Secret, h hit) ([]timelineEvent, error) {
	var out []timelineEvent
	err := s.each(ctx, secret, fmt.Sprintf("%s/repos/%s/%s/issues/%d/timeline?per_page=100", s.base, url.PathEscape(h.owner), url.PathEscape(h.name), h.n), func(body []byte) error {
		var page []timelineEvent
		if err := json.Unmarshal(body, &page); err != nil {
			return errors.New("github: a timeline could not be read")
		}
		out = append(out, page...)
		return nil
	})
	return out, err
}

func (s *Source) reviews(ctx context.Context, secret keychain.Secret, h hit) ([]review, error) {
	var out []review
	err := s.each(ctx, secret, fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=100", s.base, url.PathEscape(h.owner), url.PathEscape(h.name), h.n), func(body []byte) error {
		var page []review
		if err := json.Unmarshal(body, &page); err != nil {
			return errors.New("github: reviews could not be read")
		}
		out = append(out, page...)
		return nil
	})
	return out, err
}

func (s *Source) reviewComments(ctx context.Context, secret keychain.Secret, h hit, since time.Time) ([]reviewComment, error) {
	q := url.Values{"per_page": {"100"}, "since": {since.Format(time.RFC3339)}}
	var out []reviewComment
	err := s.each(ctx, secret, fmt.Sprintf("%s/repos/%s/%s/pulls/%d/comments?%s", s.base, url.PathEscape(h.owner), url.PathEscape(h.name), h.n, q.Encode()), func(body []byte) error {
		var page []reviewComment
		if err := json.Unmarshal(body, &page); err != nil {
			return errors.New("github: review comments could not be read")
		}
		out = append(out, page...)
		return nil
	})
	return out, err
}

func (s *Source) checks(ctx context.Context, secret keychain.Secret, h hit, sha string) ([]checkSuite, bool, error) {
	raw := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-suites?per_page=100", s.base, url.PathEscape(h.owner), url.PathEscape(h.name), url.PathEscape(sha))
	var out []checkSuite
	page := raw
	for n := 0; page != "" && n < 10; n++ {
		body, status, next, err := s.do(ctx, secret, page)
		if err != nil {
			return nil, false, err
		}
		if status == http.StatusNotFound {
			// No check app. That must not stall every other event.
			return nil, false, nil
		}
		if status == http.StatusForbidden {
			classified := classify403(body)
			if unavailable(classified) {
				// The token cannot read checks. Omitting them does not stall every other event. A bare 403 is not
				// this: a secondary rate limit often arrives that way, and skipping it would move the cursor.
				return nil, false, nil
			}
			return nil, false, classified
		}
		if status < 200 || status >= 300 {
			return nil, false, &statusError{status}
		}
		var resp struct {
			CheckSuites []checkSuite `json:"check_suites"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, false, errors.New("github: check suites could not be read")
		}
		out = append(out, resp.CheckSuites...)
		page = next
	}
	return out, true, nil
}

func (s *Source) each(ctx context.Context, secret keychain.Secret, first string, handle func([]byte) error) error {
	page := first
	for n := 0; page != "" && n < 50; n++ {
		body, next, err := s.get(ctx, secret, page)
		if err != nil {
			return err
		}
		if err := handle(body); err != nil {
			return err
		}
		page = next
	}
	if page != "" {
		return errors.New("github: pagination did not end")
	}
	return nil
}

func (s *Source) get(ctx context.Context, secret keychain.Secret, raw string) ([]byte, string, error) {
	body, status, next, err := s.do(ctx, secret, raw)
	if err != nil {
		return nil, "", err
	}
	if status == http.StatusForbidden {
		return nil, "", classify403(body)
	}
	if status < 200 || status >= 300 {
		return nil, "", &statusError{status}
	}
	return body, next, nil
}

// errPermission is a 403 that names a permission failure. The object is skipped. A bare 403 is not this: it fails
// the pass, because a secondary rate limit often arrives as a 403 with no Retry-After.
var errPermission = errors.New("github: the token cannot read this resource")

func classify403(body []byte) error {
	low := strings.ToLower(string(body))
	switch {
	case strings.Contains(low, "rate limit"):
		return source.ErrRateLimited
	case strings.Contains(low, "not accessible"), strings.Contains(low, "must have"), strings.Contains(low, "permission"):
		return errPermission
	default:
		return &statusError{http.StatusForbidden}
	}
}

// statusError is an HTTP status from a source API. The text is the status only.
type statusError struct{ status int }

func (e *statusError) Error() string { return fmt.Sprintf("github: status %d", e.status) }

// unavailable is a single object the token cannot read (gone, or not permitted). The pass continues so one such
// object cannot freeze the cursor.
func unavailable(err error) bool {
	if errors.Is(err, errPermission) {
		return true
	}
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

func (s *Source) do(ctx context.Context, secret keychain.Secret, raw string) ([]byte, int, string, error) {
	if err := s.scope.allow(raw); err != nil {
		return nil, 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, 0, "", err
	}
	secret.Authorize(req.Header)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "beacon")
	resp, err := s.http.Do(ctx, req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, resp.StatusCode, "", source.ErrUnauthorized
	}
	s.noteDate(resp.Header)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, "", err
	}
	return body, resp.StatusCode, nextLink(resp.Header), nil
}

func (s *Source) noteDate(h http.Header) {
	t, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return
	}
	t = t.UTC()
	if t.After(s.serverDate) {
		s.serverDate = t
	}
}

func nextLink(h http.Header) string {
	for _, part := range strings.Split(h.Get("Link"), ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		i, j := strings.Index(part, "<"), strings.Index(part, ">")
		if i >= 0 && j > i {
			return part[i+1 : j]
		}
	}
	return ""
}

func repoFromURL(raw string) (string, string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "repos" {
			return parts[i+1], parts[i+2], true
		}
	}
	return "", "", false
}
