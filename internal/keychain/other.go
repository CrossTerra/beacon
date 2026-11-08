//go:build !(darwin && cgo)

package keychain

import "errors"

// NewSystem returns an error: only the macOS keychain is supported so far.
func NewSystem() (Store, error) {
	return nil, errors.New("keychain: no system keychain on this platform; Beacon supports the macOS keychain")
}
