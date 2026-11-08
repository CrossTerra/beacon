//go:build !darwin

package keychain

import "io"

func noEcho(io.Reader) func() { return func() {} }
