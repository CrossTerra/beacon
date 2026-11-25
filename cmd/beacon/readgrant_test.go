package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
)

type grantTransport func(*http.Request) (*http.Response, error)

func (rt grantTransport) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

func grantFixture(t *testing.T, host, path, token string, scopes ...string) func([]string) http.RoundTripper {
	t.Helper()
	return func(allowed []string) http.RoundTripper {
		return grantTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.Host != host || r.URL.Path != path || r.Header.Get("Authorization") != "Bearer "+token || len(allowed) != 1 {
				t.Fatal("unexpected request for scope verification")
			}
			h := http.Header{}
			for _, v := range scopes {
				h.Add("X-OAuth-Scopes", v)
			}
			h.Set("X-Accepted-GitHub-Permissions", "metadata=read") // endpoint requirements are not the token's grant
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: h}, nil
		})
	}
}

func TestGitHubUnknownGrantRequiresNewFineGrainedDirectEntry(t *testing.T) {
	for _, tc := range []struct {
		name, token, scope string
		manual, accept     bool
	}{
		{"fine-grained without explicit issuance", "github_pat_fixture", "", false, false},
		{"new read-only fine-grained", "github_pat_fixture", "", true, true},
		{"detectable write even with flag", "github_pat_fixture", "public_repo", true, false},
		{"unverifiable classic", "ghp_fixture", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			must(t, os.WriteFile(path, []byte(`signal:
  url: https://signal.fixture.test
person:
  handle: '@fixture'
rules: rules.yaml
state: state
mode: dry-run
sources: []
`), 0o600))
			var out, stderr strings.Builder
			store := keychain.NewMemory()
			a := &app{stdin: strings.NewReader(tc.token + "\n"), stdout: &out, stderr: &stderr, now: time.Now,
				keychain: func() (keychain.Store, error) { return store, nil }, transport: grantFixture(t, "api.github.com", "/user", tc.token, tc.scope)}
			args := []string{"source", "add", "github", "--config", path, "--repo", "fixture/project"}
			if tc.manual {
				args = append(args, "--new-read-token")
			}
			code := a.main(t.Context(), args)
			if (code == 0) != tc.accept || store.Contains("source:github", tc.token) != tc.accept || strings.Contains(out.String()+stderr.String(), tc.token) {
				t.Fatalf("unexpected grant handling (exit %d)", code)
			}
		})
	}
}
