package jira

import (
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/CrossTerra/beacon/internal/source"
)

var (
	issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]+-\d+$`)
	projectPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9]+$`)
)

type scope struct {
	host     string
	projects map[string]bool
}

func newScope(host string, projects []string) (scope, error) {
	s := scope{host: strings.ToLower(host), projects: map[string]bool{}}
	for _, p := range projects {
		if !projectPattern.MatchString(p) {
			return scope{}, source.ErrScope
		}
		s.projects[p] = true
	}
	if s.host == "" || len(s.projects) == 0 {
		return scope{}, source.ErrScope
	}
	return s, nil
}

func (s scope) allowsKey(key string) bool {
	if !issueKeyPattern.MatchString(key) {
		return false
	}
	return s.projects[key[:strings.LastIndex(key, "-")]]
}

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
	path := strings.Trim(u.Path, "/")
	switch path {
	case "rest/api/3/myself", "rest/api/3/search/jql":
		return nil
	}
	const prefix = "rest/api/3/issue/"
	if !strings.HasPrefix(path, prefix) {
		return source.ErrScope
	}
	rest := strings.TrimPrefix(path, prefix)
	key, suffix, _ := strings.Cut(rest, "/")
	if !s.allowsKey(key) {
		return source.ErrScope
	}
	if suffix != "" && suffix != "changelog" && suffix != "comment" {
		return source.ErrScope
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
