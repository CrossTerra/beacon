package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CrossTerra/beacon/internal/source"
)

// prRepo is pr9 with the base repository GitHub returns: its numeric id and full name.
const prRepo = `{
  "id": 900, "number": 9, "title": "Own open", "html_url": "https://github.com/acme/app/pull/9",
  "state": "open", "merged": false, "user": {"login": "winton"},
  "created_at": "2026-10-01T00:00:00Z", "mergeable": true,
  "base": {"ref": "main", "repo": {"id": 4242, "full_name": "acme/app"}},
  "head": {"ref": "checks", "sha": "def456"}
}`

// pagedServer is a fake GitHub whose second page of a pull request's timeline, reviews or check suites is named, as
// GitHub names it, by the repository's numeric id: /repositories/<id>/... rather than /repos/<owner>/<name>/....
func pagedServer(t *testing.T, next func(base string) string, prBody string) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Sun, 08 Nov 2026 12:00:00 GMT")
		paths = append(paths, r.URL.Path)
		switch p := r.URL.Path; {
		case p == "/user":
			io.WriteString(w, `{"login":"winton"}`)
		case p == "/user/teams":
			io.WriteString(w, `[]`)
		case p == "/search/issues":
			io.WriteString(w, `{"items":[{"number":9,"user":{"login":"winton"},"repository_url":"https://api.github.com/repos/acme/app","pull_request":{"url":"x"}}]}`)
		case p == "/repos/acme/app/pulls/9":
			io.WriteString(w, prBody)
		case p == "/repos/acme/app/issues/9/timeline":
			if link := next(srv.URL); link != "" {
				w.Header().Set("Link", `<`+link+`>; rel="next", <`+link+`>; rel="last"`)
			}
			io.WriteString(w, `[{"id":1,"event":"commented","user":{"login":"dana"},"body":"one","created_at":"2026-11-02T00:00:00Z"}]`)
		case p == "/repositories/4242/issues/9/timeline":
			io.WriteString(w, `[{"id":2,"event":"commented","user":{"login":"dana"},"body":"two @winton","created_at":"2026-11-03T00:00:00Z"}]`)
		case strings.HasSuffix(p, "/check-suites"):
			io.WriteString(w, `{"check_suites":[]}`)
		case strings.HasPrefix(p, "/repos/acme/app/"):
			io.WriteString(w, `[]`)
		default:
			t.Errorf("request outside scope: %s", p)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

// GitHub's Link header for a later page of a repository resource names the repository by id. The id is the configured
// repository's, learned from the pull request in the same pass: the page is read.
func TestNextPageByRepositoryID(t *testing.T) {
	srv, paths := pagedServer(t, func(base string) string {
		return base + "/repositories/4242/issues/9/timeline?per_page=100&page=2"
	}, prRepo)
	s := newTest(t, srv, []string{"acme/app"}, nil)
	got, err := s.Collect(context.Background(), []byte("2026-11-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	found := false
	for _, it := range got.Items {
		if it.Key == "github:acme/app#9:comment:2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the second timeline page was not read: %v", *paths)
	}
}

// A next link naming any other id is refused and not sent: an id the pass did not learn, or one whose pull request
// GitHub did not place in the configured repository.
func TestNextPageByOtherRepositoryIDRefused(t *testing.T) {
	for name, c := range map[string]struct {
		link string
		pr   string
	}{
		"unlearned id":     {"/repositories/9999/issues/9/timeline?page=2", prRepo},
		"base elsewhere":   {"/repositories/4242/issues/9/timeline?page=2", strings.Replace(prRepo, `"acme/app"`, `"other/secret"`, 1)},
		"no base repo":     {"/repositories/4242/issues/9/timeline?page=2", pr9},
		"not a paged path": {"/repositories/4242/contents/README", prRepo},
	} {
		t.Run(name, func(t *testing.T) {
			srv, paths := pagedServer(t, func(base string) string { return base + c.link }, c.pr)
			s := newTest(t, srv, []string{"acme/app"}, nil)
			_, err := s.Collect(context.Background(), []byte("2026-11-01T00:00:00Z"))
			if !errors.Is(err, source.ErrScope) {
				t.Fatalf("err %v", err)
			}
			for _, p := range *paths {
				if strings.HasPrefix(p, "/repositories/") {
					t.Fatalf("sent %s", p)
				}
			}
			fam := source.ScopeFamily(err)
			if !strings.HasPrefix(fam, "repositories/") || strings.ContainsAny(fam, "0123456789") || strings.Contains(fam, "acme") {
				t.Fatalf("family %q", fam)
			}
		})
	}
}

func TestRepositoryIDScope(t *testing.T) {
	sc, err := newScope("api.github.com", []string{"acme/app", "acme/*"})
	if err != nil {
		t.Fatal(err)
	}
	sc.learn(4242, "acme/app", "acme", "app")
	sc.learn(77, "Acme/Lib", "acme", "lib")
	sc.learn(500, "other/secret", "acme", "app") // GitHub named another repository: not recorded.
	sc.learn(501, "other/secret", "other", "secret")
	sc.learn(0, "acme/app", "acme", "app")
	for _, raw := range []string{
		"https://api.github.com/repositories/4242/issues/9/timeline?per_page=100&page=2",
		"https://api.github.com/repositories/4242/pulls/9/reviews?per_page=100&page=2",
		"https://api.github.com/repositories/4242/pulls/9/comments?per_page=100&since=x&page=2",
		"https://api.github.com/repositories/77/commits/abc123/check-suites?per_page=100&page=2",
	} {
		if err := sc.allow(raw); err != nil {
			t.Errorf("refused %s: %v", raw, err)
		}
	}
	for raw, fam := range map[string]string{
		"https://api.github.com/repositories/500/issues/9/timeline":            "repositories/{unlisted}/issues/{n}/timeline",
		"https://api.github.com/repositories/501/issues/9/timeline":            "repositories/{unlisted}/issues/{n}/timeline",
		"https://api.github.com/repositories/0/issues/9/timeline":              "repositories/{unlisted}/issues/{n}/timeline",
		"https://api.github.com/repositories/4242/pulls/9":                     "repositories/{id}/pulls/{n}",
		"https://api.github.com/repositories/4242/issues/9/comments":           "repositories/{id}/issues/{n}/comments",
		"https://api.github.com/repositories/4242/contents/secret.txt":         "repositories/{id}/contents/{x}",
		"https://api.github.com/repositories/4242/issues/9/../../../pulls/1":   "repositories/{id}/issues/{n}/{x}/{x}/{x}/{more}",
		"https://api.github.com/repositories/4242":                             "repositories/{id}",
		"https://api.github.com/repositories/abc/issues/9/timeline":            "repositories/{unlisted}/issues/{n}/timeline",
		"https://api.github.com/repos/acme/app/commits/abc1234/check-runs?x=1": "repos/{owner}/{name}/commits/{sha}/check-runs",
		"https://api.github.com/repos/other/secret/pulls/1":                    "repos/{unlisted}/pulls/{n}",
		"https://api.github.com/organizations/12/repos?page=2":                 "organizations/{n}/repos",
		"https://api.github.com/users/someone/repos":                           "users/{owner}/repos",
		"https://evil.example/repositories/4242/issues/9/timeline":             "host",
		"http://api.github.com/user":                                           "scheme",
		"https://api.github.com/user/repos?affiliation=collaborator":           "user/repos",
		"https://api.github.com/":                                              "root",
		"https://api.github.com/repos/acme/app/git/refs/heads/feature-private": "repos/{owner}/{name}/git/refs/{x}/{x}",
	} {
		err := sc.allow(raw)
		if !errors.Is(err, source.ErrScope) {
			t.Errorf("allowed %s", raw)
			continue
		}
		if got := source.ScopeFamily(err); got != fam {
			t.Errorf("%s: family %q, want %q", raw, got, fam)
		}
	}
	// An id is trusted only in the pass that learned it: a repository transferred out keeps its id.
	sc.forget()
	if err := sc.allow("https://api.github.com/repositories/4242/issues/9/timeline?page=2"); err == nil {
		t.Fatal("an id from an earlier pass was trusted")
	}
}
