// Command beacon turns activity in a person's services into a few actionable events in Signal, sending nothing a
// rule did not allow.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &app{
		stdin:     os.Stdin,
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		keychain:  keychain.NewSystem,
		transport: egress.Transport,
		now:       time.Now,
	}
	os.Exit(a.main(ctx, os.Args[1:]))
}
