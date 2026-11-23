//go:build darwin && cgo

package keychain

import (
	"errors"
	"testing"
)

// A test binary is signed ad hoc, like go build and go run, so it must not reach the login keychain: no prompt, and
// no build hash added to an item's partition list.
func TestSystemRefusesAnAdHocBuild(t *testing.T) {
	t.Setenv(TestKeychainEnv, "")
	if _, err := NewSystem(); !errors.Is(err, ErrAdHocBuild) {
		t.Fatalf("NewSystem in an ad-hoc test binary: %v", err)
	}
}

// The statuses readRefused names are the Security framework's, so a refused prompt is not reported as a missing item.
func TestReadRefusedMatchesTheSecurityFramework(t *testing.T) {
	for _, s := range securityRefusals() {
		if !readRefused(s) {
			t.Errorf("status %d is a refusal", s)
		}
	}
	if readRefused(securityNotFound()) || readRefused(0) {
		t.Fatal("a missing item or success is not a refusal")
	}
}
