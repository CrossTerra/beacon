//go:build !darwin && !linux

package keychain

import "io"

func noEcho(io.Reader) func() { return func() {} }
