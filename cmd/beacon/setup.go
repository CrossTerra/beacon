package main

import (
	"errors"
	"fmt"

	"github.com/CrossTerra/beacon/internal/passkey"
)

func (a *app) cmdSetup(args []string) error {
	fs, path := a.reviewFlags("setup")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("setup takes no positional arguments")
	}
	if err := a.refuseRoot(); err != nil {
		return err
	}
	dir, err := reviewStateDir(*path)
	if err != nil {
		return err
	}
	code, err := passkey.Issue(dir, a.now())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.stdout, code)
	return err
}
