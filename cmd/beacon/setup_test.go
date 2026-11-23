package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestSetupPrintsAOneUseCode(t *testing.T) {
	dir := previewTemp(t)
	a, out, stderr := previewCLI()
	if code := a.main(t.Context(), []string{"setup", "--state-dir", dir}); code != 0 {
		t.Fatalf("setup exited %d: %s", code, stderr)
	}
	printed := strings.TrimSpace(out.String())
	raw, err := base64.RawURLEncoding.Strict().DecodeString(printed)
	if err != nil || len(raw) != 32 {
		t.Fatalf("code %q: %v", printed, err)
	}
	if code := a.main(t.Context(), []string{"setup"}); code == 0 {
		t.Fatal("setup without a directory was accepted")
	}
}
