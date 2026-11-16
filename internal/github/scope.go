package github

import (
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/CrossTerra/beacon/internal/source"
)

// A request may only touch these paths under a configured repository. Anything else is not sent.
var repoPath = regexp.MustCompile(`^(?:pulls/\d+|issues/\d+/timeline|issues/\d+/comments|pulls/\d+/comments|pulls/\d+/reviews|commits/[0-9a-fA-F]+/check-suites)$`)

// GitHub's Link header names a later page of these by the repository's numeric id: /repositories/<id>/<path>. That
// form is allowed only for these paths, and only for an id learned this pass from a configured repository.
var pagedPath = regexp.MustCompile(`^(?:issues/\d+/timeline|pulls/\d+/comments|pulls/\d+/reviews|commits/[0-9a-fA-F]+/check-suites)$`)

var repoID = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

type repoSpec struct {
	owner string
	name  string // "*" or an exact name
}

type scope struct {
	host  string
	repos []repoSpec
	// ids maps a repository's numeric id to the configured repository GitHub said has it, this pass.
	ids map[string]repoSpec
}

func newScope(host string, repos []string) (scope, error) {
	s := scope{host: strings.ToLower(host), ids: map[string]repoSpec{}}
	for _, r := range repos {
		owner, name, ok := strings.Cut(r, "/")
		if !ok || owner == "" || name == "" {
			return scope{}, source.ErrScope
		}
		s.repos = append(s.repos, repoSpec{owner: owner, name: name})
	}
	if s.host == "" || len(s.repos) == 0 {
		return scope{}, source.ErrScope
	}
	return s, nil
}

func (s scope) allowsRepo(owner, name string) bool {
	for _, r := range s.repos {
		if !strings.EqualFold(r.owner, owner) {
			continue
		}
		if r.name == "*" || strings.EqualFold(r.name, name) {
			return true
		}
	}
	return false
}

// learn records a repository's numeric id, from a pull request read at /repos/<owner>/<name>/pulls/<n> whose base
// repository GitHub names as that same, configured, repository. Anything else is not recorded.
func (s scope) learn(id int64, fullName, owner, name string) {
	if id <= 0 || !strings.EqualFold(fullName, owner+"/"+name) || !s.allowsRepo(owner, name) {
		return
	}
	s.ids[strconv.FormatInt(id, 10)] = repoSpec{owner: owner, name: name}
}

// forget drops the ids learned last pass. A repository transferred out keeps its id, so an id is trusted only in the
// pass that learned it.
func (s scope) forget() { clear(s.ids) }

func (s scope) ownerConfigured(owner string) bool {
	for _, r := range s.repos {
		if strings.EqualFold(r.owner, owner) {
			return true
		}
	}
	return false
}

func (s scope) owners() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range s.repos {
		if r.name != "*" || seen[strings.ToLower(r.owner)] {
			continue
		}
		seen[strings.ToLower(r.owner)] = true
		out = append(out, r.owner)
	}
	return out
}

func (s scope) exactRepos() []string {
	var out []string
	for _, r := range s.repos {
		if r.name != "*" {
			out = append(out, r.owner+"/"+r.name)
		}
	}
	return out
}

// allow refuses a URL the configured repositories and host do not cover. The request is not sent. The error names
// the refused path's family, never the URL.
func (s scope) allow(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return denied("url")
	}
	if !strings.EqualFold(u.Hostname(), s.host) {
		return denied("host")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return denied("scheme")
		}
	default:
		return denied("scheme")
	}
	path := strings.TrimPrefix(u.Path, "/api/v3")
	path = strings.Trim(path, "/")
	switch path {
	case "user", "user/teams", "search/issues":
		return nil
	case "user/repos":
		// Public-only and collaborator listings are not this list. affiliation=owner is the private-repo list.
		if u.Query().Get("affiliation") != "owner" {
			return denied("user/repos")
		}
		return nil
	}
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[0] == "users" && s.ownerConfigured(parts[1]) {
		return nil
	}
	if len(parts) == 3 && (parts[0] == "orgs" || parts[0] == "users") && parts[2] == "repos" && s.ownerConfigured(parts[1]) {
		return nil
	}
	if len(parts) >= 4 && parts[0] == "repos" && s.allowsRepo(parts[1], parts[2]) {
		if repoPath.MatchString(strings.Join(parts[3:], "/")) {
			return nil
		}
	}
	if len(parts) >= 3 && parts[0] == "repositories" && repoID.MatchString(parts[1]) {
		if r, ok := s.ids[parts[1]]; ok && s.allowsRepo(r.owner, r.name) && pagedPath.MatchString(strings.Join(parts[2:], "/")) {
			return nil
		}
	}
	return denied(s.family(parts))
}

func denied(family string) error { return &source.ScopeError{Family: family} }

// family is a refused path as a template: fixed GitHub API words, and placeholders for everything else. A
// repository is {owner}/{name} or {id} when it is configured, {unlisted} when not. It holds no name, id or query.
func (s scope) family(parts []string) string {
	if len(parts) == 1 && parts[0] == "" {
		return "root"
	}
	var out []string
	rest := parts
	switch {
	case len(parts) >= 3 && parts[0] == "repos":
		repo := "{unlisted}"
		if s.allowsRepo(parts[1], parts[2]) {
			repo = "{owner}/{name}"
		}
		out, rest = []string{"repos", repo}, parts[3:]
	case len(parts) >= 2 && parts[0] == "repositories":
		repo := "{unlisted}"
		if _, ok := s.ids[parts[1]]; ok {
			repo = "{id}"
		}
		out, rest = []string{"repositories", repo}, parts[2:]
	case len(parts) >= 2 && (parts[0] == "users" || parts[0] == "orgs"):
		out, rest = []string{parts[0], "{owner}"}, parts[2:]
	}
	for _, p := range rest {
		if len(out) >= 7 {
			out = append(out, "{more}")
			break
		}
		out = append(out, segment(p))
	}
	return strings.Join(out, "/")
}

var (
	digits = regexp.MustCompile(`^[0-9]+$`)
	hexSHA = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

// familyWords are GitHub REST path words. A segment that is not one is a placeholder in a logged family.
var familyWords = map[string]bool{
	"actions": true, "branches": true, "check-runs": true, "check-suites": true, "collaborators": true,
	"comments": true, "commits": true, "compare": true, "contents": true, "events": true, "files": true, "git": true,
	"issues": true, "labels": true, "members": true, "merge": true, "organizations": true, "orgs": true,
	"pulls": true, "readme": true, "refs": true, "releases": true, "repos": true, "repositories": true,
	"requested_reviewers": true, "reviews": true, "search": true, "status": true, "statuses": true, "tags": true,
	"teams": true, "timeline": true, "trees": true, "user": true, "users": true,
}

func segment(p string) string {
	switch {
	case familyWords[p]:
		return p
	case digits.MatchString(p):
		return "{n}"
	case hexSHA.MatchString(p):
		return "{sha}"
	}
	return "{x}"
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
