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
	// SIGINT cancels the in-flight work. SIGTERM does not: the poll loop finishes the current send and exits 0.
	// Other commands still stop on either signal.
	interrupt, stopInt := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopInt()
	term, stopTerm := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stopTerm()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-interrupt.Done():
			cancel()
		case <-term.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	a := &app{
		stdin:     os.Stdin,
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		keychain:  keychain.NewSystem,
		transport: egress.Transport,
		now:       time.Now,
		interrupt: interrupt,
		term:      term,
	}
	os.Exit(a.main(ctx, os.Args[1:]))
}
