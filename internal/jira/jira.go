// Package jira is the Jira source. It reads issues in the configured projects and nothing else. The account email
// and API token are read from the keychain at the start of each pass and leave this process only as a basic
// Authorization header.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/limited"
	"github.com/CrossTerra/beacon/internal/source"
)

// Options configures a Jira source. Creds is read once per pass.
type Options struct {
	Name     string
	Site     string
	Base     string
	Projects []string
	Identity string
	Creds    func() (keychain.Secret, error)
	HTTP     *http.Client
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) error
}

// Source reads Jira.
type Source struct {
	name     string
	site     string
	hostPort string
	base     string
	scope    scope
	identity string
	creds    func() (keychain.Secret, error)
	http     *limited.Client
	now      func() time.Time
	clock    source.Clock
}

// New builds a source. It does not read the credential.
func New(o Options) (*Source, error) {
	name := o.Name
	if name == "" {
		name = "jira"
	}
	if !source.ValidName(name) {
		return nil, fmt.Errorf("jira: name %q is invalid", name)
	}
	if o.Site == "" {
		return nil, errors.New("jira: site is required")
	}
	host := o.Site + ".atlassian.net"
	base := o.Base
	if base == "" {
		base = "https://" + host
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, errors.New("jira: base URL is invalid")
	}
	sc, err := newScope(u.Hostname(), o.Projects)
	if err != nil {
		return nil, errors.New("jira: projects are required")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Source{
		name: name, site: o.Site, hostPort: net.JoinHostPort(host, "443"), base: base,
		scope: sc, identity: o.Identity, creds: o.Creds, now: now,
		http: &limited.Client{HTTP: o.HTTP, Sleep: o.Sleep, Now: now},
	}, nil
}

// Name implements source.Source.
func (s *Source) Name() string { return s.name }

// Fields implements source.Source.
func (s *Source) Fields() []string { return append([]string(nil), Fields...) }

// StateFields implements source.Source.
func (s *Source) StateFields() []string { return append([]string(nil), StateFields...) }

// Hosts implements source.Source. The host is the configured site, not a host taken from a response.
func (s *Source) Hosts() []string { return []string{s.hostPort} }

// SetHTTP sets the client used to collect. The app sets it after the egress list, which is built from Hosts, exists.
func (s *Source) SetHTTP(c *http.Client) {
	if s.http == nil {
		s.http = &limited.Client{Now: s.now}
	}
	s.http.HTTP = c
}

// Collect implements source.Source. A nil cursor reads unresolved issues assigned to the person, not history (DEC-23).
func (s *Source) Collect(ctx context.Context, cursor []byte) (source.Collected, error) {
	if s.creds == nil || s.http == nil || s.http.HTTP == nil {
		return source.Collected{}, errors.New("jira: the source has no credential or client")
	}
	secret, err := s.creds()
	if err != nil {
		return source.Collected{}, err
	}
	s.clock.Reset()
	first := len(cursor) == 0
	var since time.Time
	if !first {
		since, err = time.Parse(time.RFC3339, string(cursor))
		if err != nil {
			return source.Collected{}, errors.New("jira: the cursor is not a time")
		}
		since = since.UTC()
	}
	floor := time.Time{}
	if !since.IsZero() {
		floor = since.Add(-source.Lookback)
	}
	me, loc, err := s.myself(ctx, secret)
	if err != nil {
		return source.Collected{}, err
	}
	if s.identity != "" && s.identity != "jira:account:"+me {
		return source.Collected{}, source.ErrIdentity
	}
	keys, err := s.search(ctx, secret, s.jql(floor, loc, first, me))
	if err != nil {
		return source.Collected{}, err
	}
	var items []source.Item
	dropped := 0
	for _, key := range keys {
		if !s.scope.allowsKey(key) {
			dropped++
			continue
		}
		got, moved, err := s.issueItems(ctx, secret, key, me, loc, floor, first, s.observed())
		if moved {
			dropped++
			continue
		}
		if err != nil {
			return source.Collected{Items: items, Dropped: dropped}, err
		}
		items = append(items, got...)
	}
	var events []time.Time
	for _, it := range items {
		if !it.OccurredAt.IsZero() {
			events = append(events, it.OccurredAt)
		}
	}
	next, err := s.clock.Next(since, s.now(), events)
	if err != nil {
		return source.Collected{Items: items, Dropped: dropped}, err
	}
	return source.Collected{Items: items, Next: next, Dropped: dropped}, nil
}

func (s *Source) observed() time.Time {
	if t := s.clock.First(); !t.IsZero() {
		return t
	}
	return s.now().UTC()
}

func (s *Source) myself(ctx context.Context, secret keychain.Secret) (string, *time.Location, error) {
	body, err := s.get(ctx, secret, s.base+"/rest/api/3/myself")
	if err != nil {
		return "", nil, err
	}
	var me struct {
		AccountID string `json:"accountId"`
		TimeZone  string `json:"timeZone"`
	}
	if err := json.Unmarshal(body, &me); err != nil || me.AccountID == "" {
		return "", nil, errors.New("jira: the account could not be read")
	}
	if strings.ContainsAny(me.AccountID, "\"'\\") {
		return "", nil, errors.New("jira: the account id cannot be used in a query")
	}
	loc := time.UTC
	if me.TimeZone != "" {
		loaded, err := time.LoadLocation(me.TimeZone)
		if err != nil {
			return "", nil, errors.New("jira: the account timezone could not be loaded")
		}
		loc = loaded
	}
	return me.AccountID, loc, nil
}

func (s *Source) jql(since time.Time, loc *time.Location, first bool, account string) string {
	projects := make([]string, 0, len(s.scope.projects))
	for p := range s.scope.projects {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	clause := "project in (" + strings.Join(projects, ", ") + ")"
	if first {
		return clause + " AND resolution = Unresolved AND assignee = currentUser() ORDER BY updated ASC"
	}
	when := since.In(loc).Format("2006-01-02 15:04")
	// text ~ account id is not verified against live ADF mentions. Mentions are also taken from fetched comments.
	mention := ""
	if account != "" && !strings.ContainsAny(account, "\"\\") {
		mention = fmt.Sprintf(` OR text ~ "\"%s\""`, account)
	}
	return clause + fmt.Sprintf(` AND updated >= "%s" AND (assignee = currentUser() OR reporter = currentUser() OR watcher = currentUser()%s) ORDER BY updated ASC`, when, mention)
}

func (s *Source) search(ctx context.Context, secret keychain.Secret, jql string) ([]string, error) {
	var keys []string
	seen := map[string]bool{}
	token := ""
	for pages := 0; pages < 50; pages++ {
		payload := map[string]any{
			"jql":        jql,
			"maxResults": 50,
			"fields":     []string{"summary", "status", "assignee", "priority", "issuetype", "duedate", "resolution", "project"},
		}
		if token != "" {
			payload["nextPageToken"] = token
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		resp, err := s.post(ctx, secret, s.base+"/rest/api/3/search/jql", body)
		if err != nil {
			return nil, err
		}
		var page struct {
			Issues []struct {
				Key string `json:"key"`
			} `json:"issues"`
			NextPageToken string `json:"nextPageToken"`
			IsLast        bool   `json:"isLast"`
		}
		if err := json.Unmarshal(resp, &page); err != nil {
			return nil, errors.New("jira: the search response could not be read")
		}
		for _, iss := range page.Issues {
			if !s.scope.allowsKey(iss.Key) || seen[iss.Key] {
				continue
			}
			seen[iss.Key] = true
			keys = append(keys, iss.Key)
		}
		if page.IsLast || page.NextPageToken == "" {
			return keys, nil
		}
		token = page.NextPageToken
	}
	return nil, errors.New("jira: search pagination did not end")
}

func (s *Source) issueItems(ctx context.Context, secret keychain.Secret, key, person string, loc *time.Location, since time.Time, first bool, observed time.Time) ([]source.Item, bool, error) {
	raw, err := s.get(ctx, secret, s.base+"/rest/api/3/issue/"+url.PathEscape(key)+"?fields=summary,status,assignee,priority,issuetype,duedate,resolution,project")
	if unavailable(err) {
		// The issue itself is gone or not readable. A changelog or comment failure is not this case: that
		// returns the error, and the cursor stays.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	iss, err := decodeIssue(raw, s.browse(key))
	if err != nil {
		return nil, false, err
	}
	if !s.scope.allowsKey(iss.key) {
		// The issue moved out of the configured projects. Drop it; do not fail the pass.
		return nil, true, nil
	}
	histories, err := s.changelog(ctx, secret, key)
	if err != nil {
		return nil, false, err
	}
	var comments []comment
	if !first {
		comments, err = s.comments(ctx, secret, key)
		if err != nil {
			return nil, false, err
		}
	}
	items, err := itemsFrom(s.name, person, iss, histories, comments, since, first, observed, loc)
	return items, false, err
}

func (s *Source) changelog(ctx context.Context, secret keychain.Secret, key string) ([]history, error) {
	var out []history
	start := 0
	for pages := 0; pages < 50; pages++ {
		raw, err := s.get(ctx, secret, fmt.Sprintf("%s/rest/api/3/issue/%s/changelog?startAt=%d&maxResults=100", s.base, url.PathEscape(key), start))
		if err != nil {
			return nil, err
		}
		var page struct {
			Values []struct {
				ID      flexID `json:"id"`
				Created string `json:"created"`
				Author  struct {
					AccountID string `json:"accountId"`
				} `json:"author"`
				Items []struct {
					Field      string `json:"field"`
					FieldID    string `json:"fieldId"`
					From       string `json:"from"`
					To         string `json:"to"`
					FromString string `json:"fromString"`
					ToString   string `json:"toString"`
				} `json:"items"`
			} `json:"values"`
			Total  int  `json:"total"`
			IsLast bool `json:"isLast"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, errors.New("jira: a changelog could not be read")
		}
		for _, h := range page.Values {
			at, err := parseJiraTime(h.Created)
			if err != nil {
				return nil, errors.New("jira: a changelog time could not be read")
			}
			hist := history{id: h.ID.String(), author: h.Author.AccountID, created: at}
			for _, it := range h.Items {
				hist.items = append(hist.items, change{field: jiraField(it.FieldID, it.Field), from: it.From, to: it.To, fromString: it.FromString, toString: it.ToString})
			}
			out = append(out, hist)
		}
		start += len(page.Values)
		if page.IsLast || start >= page.Total || len(page.Values) == 0 {
			sort.Slice(out, func(i, j int) bool { return out[i].created.Before(out[j].created) })
			return out, nil
		}
	}
	return nil, errors.New("jira: changelog pagination did not end")
}

func (s *Source) comments(ctx context.Context, secret keychain.Secret, key string) ([]comment, error) {
	var out []comment
	start := 0
	for pages := 0; pages < 50; pages++ {
		raw, err := s.get(ctx, secret, fmt.Sprintf("%s/rest/api/3/issue/%s/comment?startAt=%d&maxResults=100", s.base, url.PathEscape(key), start))
		if err != nil {
			return nil, err
		}
		var page struct {
			Comments []struct {
				ID      flexID `json:"id"`
				Created string `json:"created"`
				Author  struct {
					AccountID string `json:"accountId"`
				} `json:"author"`
				Body json.RawMessage `json:"body"`
			} `json:"comments"`
			Total int `json:"total"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, errors.New("jira: comments could not be read")
		}
		for _, c := range page.Comments {
			at, err := parseJiraTime(c.Created)
			if err != nil {
				return nil, errors.New("jira: a comment time could not be read")
			}
			text, mentions := commentBody(c.Body)
			out = append(out, comment{id: c.ID.String(), author: c.Author.AccountID, created: at, body: text, mentions: mentions})
		}
		start += len(page.Comments)
		if start >= page.Total || len(page.Comments) == 0 {
			return out, nil
		}
	}
	return nil, errors.New("jira: comment pagination did not end")
}

func commentBody(raw json.RawMessage) (string, map[string]bool) {
	if len(raw) == 0 {
		return "", nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return adfText(v)
}

func decodeIssue(raw []byte, browse string) (issue, error) {
	var resp struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			DueDate string `json:"duedate"`
			Status  *struct {
				Name string `json:"name"`
			} `json:"status"`
			Assignee *struct {
				AccountID   string `json:"accountId"`
				DisplayName string `json:"displayName"`
			} `json:"assignee"`
			Priority *struct {
				Name string `json:"name"`
			} `json:"priority"`
			IssueType *struct {
				Name string `json:"name"`
			} `json:"issuetype"`
			Resolution *struct {
				Name string `json:"name"`
			} `json:"resolution"`
			Project *struct {
				Key string `json:"key"`
			} `json:"project"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Key == "" {
		return issue{}, errors.New("jira: an issue could not be read")
	}
	iss := issue{key: resp.Key, summary: resp.Fields.Summary, url: browse, due: resp.Fields.DueDate}
	if resp.Fields.Project != nil {
		iss.project = resp.Fields.Project.Key
	}
	if resp.Fields.Status != nil {
		iss.status = resp.Fields.Status.Name
	}
	if resp.Fields.Assignee != nil {
		iss.account = resp.Fields.Assignee.AccountID
		iss.assignee = resp.Fields.Assignee.DisplayName
	}
	if resp.Fields.Priority != nil {
		iss.priority = resp.Fields.Priority.Name
	}
	if resp.Fields.IssueType != nil {
		iss.issueType = resp.Fields.IssueType.Name
	}
	if resp.Fields.Resolution != nil {
		iss.resolution = resp.Fields.Resolution.Name
	}
	return iss, nil
}

func (s *Source) browse(key string) string {
	return "https://" + s.site + ".atlassian.net/browse/" + key
}

func (s *Source) get(ctx context.Context, secret keychain.Secret, raw string) ([]byte, error) {
	return s.call(ctx, secret, http.MethodGet, raw, nil)
}

func (s *Source) post(ctx context.Context, secret keychain.Secret, raw string, body []byte) ([]byte, error) {
	return s.call(ctx, secret, http.MethodPost, raw, body)
}

func (s *Source) call(ctx context.Context, secret keychain.Secret, method, raw string, body []byte) ([]byte, error) {
	if err := s.scope.allow(raw); err != nil {
		return nil, err
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, r)
	if err != nil {
		return nil, err
	}
	if err := secret.AuthorizeBasic(req.Header); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "beacon")
	resp, err := s.http.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, source.ErrUnauthorized
	}
	s.clock.Note(resp.Header)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		low := strings.ToLower(string(data))
		if strings.Contains(low, "rate limit") {
			return nil, source.ErrRateLimited
		}
		return nil, &statusError{resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &statusError{resp.StatusCode}
	}
	return data, nil
}

type statusError struct{ status int }

func (e *statusError) Error() string { return fmt.Sprintf("jira: status %d", e.status) }

// unavailable is one issue the token cannot read (gone). A 403 is not this: a rate limit often arrives as a 403
// with no Retry-After, and skipping it would move the cursor past unread changes.
func unavailable(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexID(v)
		return nil
	}
	*f = flexID(s)
	return nil
}

func (f flexID) String() string { return string(f) }

// jiraField is the changelog field id, lowercased and without spaces. Jira sends fieldId ("status") and sometimes
// only the display name ("Due Date").
func jiraField(id, name string) string {
	f := id
	if f == "" {
		f = name
	}
	return strings.ReplaceAll(strings.ToLower(f), " ", "")
}
