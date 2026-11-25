package main

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/slack/slacktest"
	"github.com/CrossTerra/beacon/internal/vault"
)

func TestImportKeychainOnlyReadsConfiguredCollectorItems(t *testing.T) {
	dir := previewTemp(t)
	path := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(path, []byte(`signal:
  url: https://signal.fixture.test
person:
  handle: '@winton'
rules: rules.yaml
state: state
mode: dry-run
sources:
  - kind: github
    repos: [fixture/check]
  - kind: jira
    site: fixture
    projects: [FIX]
`), 0o600))
	o, _ := approval.ParseOrigin(approval.LocalOrigin, "")
	must(t, localstate.Save(dir, o))
	must(t, vault.Create(dir))
	a, out, stderr := previewCLI()
	const email, jiraToken = "jira@example.test", "fixture-never-print-jira:token "
	legacyPair := strings.ToUpper(hex.EncodeToString([]byte(email + "\n" + jiraToken)))
	a.transport = grantFixture(t, "api.github.com", "/user", "fixture-never-print-source-github", "read:user")
	cfg, err := a.loadConfig(path)
	must(t, err)
	b := collectorBindings(cfg)
	var asked []string
	a.legacyRead = func(_ context.Context, account string) (keychain.Secret, error) {
		asked = append(asked, account)
		if _, allowed := b[account]; !allowed {
			t.Fatal("requested an unrelated item")
		}
		if account == "source:jira" {
			return keychain.FromString(legacyPair)
		}
		return keychain.FromString("fixture-never-print-" + strings.ReplaceAll(account, ":", "-"))
	}
	args := []string{"vault", "import-keychain", "--config", path, "--state-dir", dir}
	if code := a.main(t.Context(), args); code != 0 {
		t.Fatalf("import exited %d: %s", code, stderr)
	}
	text := out.String() + stderr.String()
	if len(asked) != 3 || strings.Contains(text, "fixture-never-print-") || strings.Contains(text, legacyPair) || strings.Contains(text, email) {
		t.Fatal("unexpected import scope or leaked credential")
	}
	for slot, binding := range b {
		got, err := keychain.ReadBound(dir, binding)
		must(t, err)
		if slot == "source:jira" {
			r := &http.Request{Header: make(http.Header)}
			must(t, got.AuthorizeBasic(r.Header))
			if user, pass, ok := r.BasicAuth(); !ok || user != email || pass != jiraToken {
				t.Fatal("hex-encoded Jira import did not preserve the Basic auth bytes")
			}
		} else {
			want, _ := keychain.FromString("fixture-never-print-" + strings.ReplaceAll(slot, ":", "-"))
			if !got.Same(want) {
				t.Fatal("credential not stored at the collector binding")
			}
		}
		binding.Origin = "https://another.fixture.test"
		if _, err := keychain.ReadBound(dir, binding); !errors.Is(err, keychain.ErrNotFound) {
			t.Fatalf("different origin did not fail closed: %v", err)
		}
	}
	if code := a.main(t.Context(), args); code != 0 || len(asked) != 3 {
		t.Fatal("rerun read over the imported entries")
	}
	if _, err := os.Lstat(filepath.Join(dir, "write-github.sealed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("import installed a provider write credential")
	}
}

func TestKeychainImportRejectsWriteAndUnverifiedGrants(t *testing.T) {
	for _, tc := range []struct{ name, githubScopes, slackScopes string }{
		{"write scopes", "read:user,public_repo", slacktest.AllScopes + ",chat:write"},
		{"not reported", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := previewTemp(t)
			path := filepath.Join(dir, "config.yaml")
			must(t, os.WriteFile(path, []byte(slackConfig+slackSource+"  - kind: github\n    repos: [fixture/check]\n"), 0o600))
			o, _ := approval.ParseOrigin(approval.LocalOrigin, "")
			must(t, localstate.Save(dir, o))
			must(t, vault.Create(dir))
			a, out, stderr := previewCLI()
			srv := slacktest.New(t)
			srv.Scopes = tc.slackScopes
			a.slackBase = srv.URL
			const githubToken = "github_pat_fixture_never_print"
			a.transport = func(allowed []string) http.RoundTripper {
				if len(allowed) == 1 && allowed[0] == "api.github.com:443" {
					return grantFixture(t, "api.github.com", "/user", githubToken, tc.githubScopes)(allowed)
				}
				if len(allowed) != 1 || "http://"+allowed[0] != srv.URL {
					t.Fatal("unexpected grant verification host")
				}
				return http.DefaultTransport
			}
			cfg, err := a.loadConfig(path)
			must(t, err)
			a.legacyRead = func(_ context.Context, account string) (keychain.Secret, error) {
				switch account {
				case "source:github":
					return keychain.FromString(githubToken)
				case keychain.SlackAccount(slacktest.Workspace, slacktest.User):
					return keychain.FromString(slacktest.Token)
				case cfg.KeychainAccount():
					return keychain.FromString("sig_fixture")
				}
				return keychain.Secret{}, keychain.ErrNotFound
			}
			if code := a.main(t.Context(), []string{"vault", "import-keychain", "--config", path, "--state-dir", dir}); code == 0 ||
				!strings.Contains(stderr.String(), "2 collector items could not be imported") {
				t.Fatalf("import exited %d without refusing both grants", code)
			}
			text := out.String() + stderr.String()
			if strings.Contains(text, githubToken) || strings.Contains(text, slacktest.Token) {
				t.Fatal("grant refusal disclosed a token")
			}
			for slot, binding := range collectorBindings(cfg) {
				if slot != cfg.KeychainAccount() {
					if _, err := keychain.ReadBound(dir, binding); !errors.Is(err, keychain.ErrNotFound) {
						t.Fatalf("rejected provider grant reached the vault: %v", err)
					}
				}
			}
		})
	}
}
