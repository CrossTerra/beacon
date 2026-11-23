package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/approvalserve"
)

func (a *app) reviewFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("beacon "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	path := fs.String("state-dir", "", "private directory for the isolated Go review preview (required)")
	return fs, path
}

func (a *app) cmdServe(ctx context.Context, args []string) error {
	fs, path := a.reviewFlags("serve")
	origin := fs.String("origin", approval.LocalOrigin, "canonical browser origin")
	login := fs.String("tailscale-login", "", "expected Tailscale-User-Login for a ts.net origin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("serve takes no positional arguments")
	}
	if err := a.refuseRoot(); err != nil {
		return err
	}
	o, err := approval.ParseOrigin(*origin, *login)
	if err != nil {
		return err
	}
	dir, err := reviewStateDir(*path)
	if err != nil {
		return err
	}
	return approvalserve.Run(ctx, approvalserve.Options{StateDir: dir, Origin: o, Now: a.now, ListenReview: a.approvalListeners})
}

// This is a sandbox, not cutover. Require a separate path and never probe the live proposal socket
// or read the live trust file. Normalize parent aliases but let the private-path check reject a leaf symlink.
func reviewStateDir(path string) (string, error) {
	if path == "" {
		return "", errors.New("pass --state-dir DIR for the isolated Go review preview")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	dir := filepath.Join(parent, filepath.Base(abs))
	if runtime.GOOS == "darwin" {
		config, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		if resolved, err := filepath.EvalSymlinks(config); err == nil {
			config = resolved
		}
		// Preserve the live path even on a case-insensitive filesystem and before it has been created.
		live, folded := strings.ToLower(filepath.Join(config, "beacon")), strings.ToLower(dir)
		if folded == live || strings.HasPrefix(folded, live+string(filepath.Separator)) {
			return "", errors.New("the Go preview cannot use Beacon.app's state directory")
		}
	}
	if _, err := approval.CheckPrivate(dir, os.ModeDir|0o700); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dir, nil
		}
		return "", err
	}
	// Also refuse copied or nonstandard signed-client state. Do not open or parse the file.
	if _, err := os.Lstat(filepath.Join(dir, "act.json")); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("the Go preview cannot use a directory with signed-client settings")
	}
	return dir, nil
}

func (a *app) cmdOpen(ctx context.Context, args []string) error {
	fs, path := a.reviewFlags("open")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("open takes no positional arguments")
	}
	dir, err := reviewStateDir(*path)
	if err != nil {
		return err
	}
	b, err := actsocket.Call(ctx, dir, approval.Object{"op": "open"}, 0)
	if err != nil {
		return err
	}
	link, err := openLink(b)
	if err != nil {
		return err
	}
	// Print for the person to open. Do not hand the bearer code to an opener in its process arguments.
	_, err = fmt.Fprintln(a.stdout, link)
	return err
}

func openLink(b []byte) (string, error) {
	var reply struct{ Code, URL, Error string }
	if err := json.Unmarshal(b, &reply); err != nil {
		return "", errors.New("unexpected Go proposal socket reply")
	}
	if reply.Error != "" {
		return "", errors.New("the Go proposal socket could not issue a review link")
	}
	origin, code, ok := strings.Cut(reply.URL, "/open?code=")
	if !ok || !approval.ValidOriginURL(origin) || code != reply.Code || len(code) != base64.RawURLEncoding.EncodedLen(32) {
		return "", errors.New("invalid review link")
	}
	if b, err := base64.RawURLEncoding.Strict().DecodeString(code); err != nil || len(b) != 32 {
		return "", errors.New("invalid review link")
	}
	return reply.URL, nil
}
