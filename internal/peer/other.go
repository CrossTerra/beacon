//go:build !(darwin && cgo)

package peer

import "errors"

func check(int, string) error {
	return errors.New("peer: checking a socket's peer is only supported on macOS")
}

// SelfRequirement is only available on macOS with cgo.
func SelfRequirement() (string, error) {
	return "", errors.New("peer: only supported on macOS")
}
