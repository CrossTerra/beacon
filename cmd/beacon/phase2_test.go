package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/cursor"
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
		ghToken:  func(context.Context, string) (string, error) { return tok, nil },
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

func TestFromGHUsesHost(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte("signal:\n  url: https://signal.example.com\nperson:\n  handle: \"@winton\"\nrules: rules.yaml\nstate: state\nmode: dry-run\nsources: []\n"), 0o600))
	store := keychain.NewMemory()
	var host string
	var out, errOut strings.Builder
	a := &app{
		stdin: strings.NewReader(""), stdout: &out, stderr: &errOut, now: time.Now,
		keychain: func() (keychain.Store, error) { return store, nil },
		ghToken: func(_ context.Context, h string) (string, error) {
			host = h
			return "ghe-token", nil
		},
	}
	if code := a.main(context.Background(), []string{"source", "add", "github", "--config", cfg, "--repo", "acme/app", "--host", "ghe.example.com", "--from-gh", "--yes"}); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
	}
	if host != "ghe.example.com" {
		t.Fatalf("gh auth token host %q", host)
	}
}

func TestPipedJiraKeepsTheToken(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte("signal:\n  url: https://signal.example.com\nperson:\n  handle: \"@winton\"\nrules: rules.yaml\nstate: state\nmode: dry-run\nsources: []\n"), 0o600))
	store := keychain.NewMemory()
	const email, tok = "winton@example.com", "jira-piped-secret"
	var out, errOut strings.Builder
	a := &app{
		stdin: strings.NewReader(email + "\n" + tok + "\n"), stdout: &out, stderr: &errOut, now: time.Now,
		keychain: func() (keychain.Store, error) { return store, nil },
	}
	if code := a.main(context.Background(), []string{"source", "add", "jira", "--config", cfg, "--site", "acme", "--project", "ENG"}); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), tok) {
		t.Fatal("the token was printed")
	}
	secret, err := store.Get("source:jira")
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	if err := secret.AuthorizeBasic(h); err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") == "" {
		t.Fatal("piped token was not stored")
	}
}

func TestAcceptFirstRequiresCompletedFirst(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte("signal:\n  url: https://signal.example.com\nperson:\n  handle: \"@winton\"\nrules: rules.yaml\nstate: state\nmode: dry-run\nsources:\n  - kind: github\n    repos: [\"acme/app\"]\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte("rules: []\n"), 0o600))
	store := keychain.NewMemory()
	a := &app{
		stdout: &strings.Builder{}, stderr: &strings.Builder{}, now: time.Now,
		keychain: func() (keychain.Store, error) { return store, nil },
	}
	var errOut strings.Builder
	a.stderr = &errOut
	if code := a.main(context.Background(), []string{"accept-first", "--config", cfg}); code == 0 || !strings.Contains(errOut.String(), "no completed beacon run --first") {
		t.Fatalf("exit %d\n%s", code, errOut.String())
	}
	errOut.Reset()
	if code := a.main(context.Background(), []string{"run", "--config", cfg, "--first", "--once"}); code != 0 {
		t.Fatalf("first run exit %d\n%s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "first.json")); err == nil {
		t.Fatal("a failed collect was marked as a completed first run")
	}
	st, err := cursor.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFirst([]string{"github"}); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if code := a.main(context.Background(), []string{"accept-first", "--config", cfg}); code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut.String())
	}
}
