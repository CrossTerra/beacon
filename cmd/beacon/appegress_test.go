package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEgressListsBeaconAppHosts(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := writeSetup(t, dir, h.server.URL, "dry-run", "appegress")
	settings := filepath.Join(dir, "settings.json")
	a := h.app(nil)
	a.appSettingsFile = settings
	if code := a.main(context.Background(), []string{"egress", "--config", cfg}); code != 0 || strings.Contains(h.stdout.String(), "Beacon.app") {
		t.Fatalf("no settings: %d\n%s%s", code, h.stdout.String(), h.stderr.String())
	}
	must(t, os.WriteFile(settings, []byte(`{"version":1,"github":{"origin":"https://github.example.com:8443","account":"github:user:1","login":"x"}}`), 0o600))
	a = h.app(nil)
	a.appSettingsFile = settings
	if code := a.main(context.Background(), []string{"egress", "--config", cfg}); code != 0 ||
		!strings.Contains(h.stdout.String(), "github.example.com:8443") || !strings.Contains(h.stdout.String(), "Beacon.app: github.merge") {
		t.Fatalf("with settings: %d\n%s%s", code, h.stdout.String(), h.stderr.String())
	}
	must(t, os.WriteFile(settings, []byte(`{"github":{"origin":"http://github.example.com"}}`), 0o600))
	a = h.app(nil)
	a.appSettingsFile = settings
	if code := a.main(context.Background(), []string{"egress", "--config", cfg}); code == 0 {
		t.Fatal("a non-https origin was listed")
	}
}
