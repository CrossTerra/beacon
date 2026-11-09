package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/keychain"
)

func TestSourceAddDoesNotWriteTheToken(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte(`
signal:
  url: https://signal.example.com
person:
  handle: "@winton"
rules: rules.yaml
state: state
mode: dry-run
sources: []
`), 0o600))
	store := keychain.NewMemory()
	const tok = "ghp_PHASE2_SECRET_TOKEN"
	var out, errOut strings.Builder
	a := &app{
		stdin: strings.NewReader(""), stdout: &out, stderr: &errOut, now: time.Now,
		keychain: func() (keychain.Store, error) { return store, nil },
		ghToken:  func(context.Context) (string, error) { return tok, nil },
	}
	if code := a.main(context.Background(), []string{"source", "add", "github", "--config", cfg, "--repo", "acme/app", "--from-gh"}); code == 0 {
		t.Fatal("import without --yes succeeded")
	}
	if _, err := store.Get("source:github"); err == nil {
		t.Fatal("token stored without confirmation")
	}
	out.Reset()
	errOut.Reset()
	if code := a.main(context.Background(), []string{"source", "add", "github", "--config", cfg, "--repo", "acme/app", "--project", "ENG", "--from-gh", "--yes"}); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
	}
	text := out.String() + errOut.String()
	if strings.Contains(text, tok) {
		t.Fatal("the token was printed")
	}
	data, err := os.ReadFile(cfg)
	if err != nil || strings.Contains(string(data), tok) {
		t.Fatalf("config holds a credential:\n%s", data)
	}
	if !strings.Contains(string(data), "kind: github") || !strings.Contains(string(data), "acme/app") {
		t.Fatalf("source not enabled:\n%s", data)
	}
	secret, err := store.Get("source:github")
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	secret.Authorize(h)
	if h.Get("Authorization") != "Bearer "+tok {
		t.Fatal("stored token does not authorize")
	}
}

func TestEgressListsSourceHosts(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte(`
signal:
  url: https://signal.example.com
person:
  handle: "@winton"
rules: rules.yaml
state: state
mode: dry-run
sources:
  - kind: github
    repos: ["acme/app"]
    host: ghe.example.com
  - kind: jira
    site: acme
    projects: ["ENG"]
`), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte("rules: []\n"), 0o600))
	var out strings.Builder
	a := &app{stdout: &out, stderr: &out}
	if code := a.main(context.Background(), []string{"egress", "--config", cfg}); code != 0 {
		t.Fatal(out.String())
	}
	got := out.String()
	for _, host := range []string{"ghe.example.com:443", "acme.atlassian.net:443", "signal.example.com:443"} {
		if !strings.Contains(got, host) {
			t.Fatalf("missing %s in\n%s", host, got)
		}
	}
}
