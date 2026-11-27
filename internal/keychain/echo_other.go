//go:build !darwin && !linux

package keychain

import (
	"errors"
	"os"
)

func hideEcho(*os.File) (func(), error) { return nil, errors.New("hidden terminal input unavailable") }
