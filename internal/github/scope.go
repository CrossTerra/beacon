package github

import (
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/CrossTerra/beacon/internal/source"
)

// A request may only touch these paths under a configured repository. Anything else is not sent.
var repoPath = regexp.MustCompile(`^(?:pulls/\d+|issues/\d+/timeline|issues/\d+/comments|pulls/\d+/comments|pulls/\d+/reviews|commits/[0-9a-fA-F]+/check-suites)$`)

type repoSpec struct {
	owner string
	name  string // "*" or an exact name
}

type scope struct {
	host  string
	repos []repoSpec
}

func newScope(host string, repos []string) (scope, error) {
	s := scope{host: strings.ToLower(host)}
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

// allow refuses a URL the configured repositories and host do not cover. The request is not sent.
func (s scope) allow(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return source.ErrScope
	}
	if !strings.EqualFold(u.Hostname(), s.host) {
		return source.ErrScope
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback(u.Hostname()) {
			return source.ErrScope
		}
	default:
		return source.ErrScope
	}
	path := strings.TrimPrefix(u.Path, "/api/v3")
	path = strings.Trim(path, "/")
	switch path {
	case "user", "user/teams", "user/repos", "search/issues":
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
	return source.ErrScope
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
