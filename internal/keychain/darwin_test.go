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
