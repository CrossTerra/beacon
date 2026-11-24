package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/localstate"
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
`), 0o600))
	o, _ := approval.ParseOrigin(approval.LocalOrigin, "")
	must(t, localstate.Save(dir, o))
	must(t, vault.Create(dir))
	a, out, stderr := previewCLI()
	cfg, err := a.loadConfig(path)
	must(t, err)
	b := collectorBindings(cfg)
	var asked []string
	a.legacyRead = func(_ context.Context, account string) (keychain.Secret, error) {
		asked = append(asked, account)
		if _, allowed := b[account]; !allowed {
			t.Fatal("requested an unrelated item")
		}
		return keychain.FromString("fixture-never-print-" + account)
	}
	args := []string{"vault", "import-keychain", "--config", path, "--state-dir", dir}
	if code := a.main(t.Context(), args); code != 0 {
		t.Fatalf("import exited %d: %s", code, stderr)
	}
	if len(asked) != 2 || strings.Contains(out.String()+stderr.String(), "fixture-never-print-") {
		t.Fatal("unexpected import scope or leaked credential")
	}
	for slot, binding := range b {
		got, err := keychain.ReadBound(dir, binding)
		must(t, err)
		want, _ := keychain.FromString("fixture-never-print-" + slot)
		if !got.Same(want) {
			t.Fatal("credential not stored at the collector binding")
		}
		binding.Origin = "https://another.fixture.test"
		if _, err := keychain.ReadBound(dir, binding); !errors.Is(err, keychain.ErrNotFound) {
			t.Fatalf("different origin did not fail closed: %v", err)
		}
	}
	if code := a.main(t.Context(), args); code != 0 || len(asked) != 2 {
		t.Fatal("rerun read over the imported entries")
	}
	if _, err := os.Lstat(filepath.Join(dir, "write-github.sealed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("import installed a provider write credential")
	}
}
