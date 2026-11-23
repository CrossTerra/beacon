package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/CrossTerra/beacon/internal/spike"
)

func (a *app) cmdSpikePasskey(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("beacon spike-passkey", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	data := fs.String("data", "", "credential directory (default: a beacon-spike directory, not Beacon's state directory)")
	login := fs.String("tailscale-login", "", "Tailscale login a ts.net request must carry; empty accepts any non-empty login")
	fs.Usage = func() {
		fmt.Fprint(a.stderr, `usage: beacon spike-passkey [--data DIR] [--tailscale-login LOGIN]

Serves the passkey PRF spike on 127.0.0.1:7317 and [::1]:7317.
It does not read the keychain and does not contact any host.
JSON lines on stdout record Host, Origin and Tailscale-* headers. They do not
include a request body, an assertion or a PRF output.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return exitError{2}
	}
	dir := *data
	if dir == "" {
		var err error
		dir, err = spike.DefaultDataDir()
		if err != nil {
			return err
		}
	}
	lns, err := spike.Listen()
	if err != nil {
		return err
	}
	srv, err := spike.New(spike.Config{DataDir: dir, TailscaleLogin: *login, Log: a.stdout})
	if err != nil {
		for _, ln := range lns {
			_ = ln.Close()
		}
		return err
	}
	fmt.Fprint(a.stderr, "beacon spike-passkey: listening on 127.0.0.1:7317 and [::1]:7317\n")
	fmt.Fprint(a.stderr, "open http://localhost:7317\n")
	fmt.Fprint(a.stderr, "tailscale: tailscale serve --bg --https=443 http://127.0.0.1:7317\n")
	fmt.Fprintf(a.stderr, "data: %s\n", dir)
	if *login == "" {
		fmt.Fprint(a.stderr, "tailscale login: not set; any non-empty Tailscale-User-Login is accepted on the ts.net name\n")
	} else {
		fmt.Fprintf(a.stderr, "tailscale login: %s\n", *login)
	}
	fmt.Fprint(a.stderr, "request log is JSON on stdout; it has no body, assertion or PRF output\n")
	return srv.Serve(ctx, lns)
}
