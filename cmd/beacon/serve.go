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
	"strings"

	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/approvalserve"
	"github.com/CrossTerra/beacon/internal/cutover"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/notice"
	"github.com/CrossTerra/beacon/internal/supervisor"
)

func (a *app) reviewFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("beacon "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	path := fs.String("state-dir", "", "private service state directory (defaults to the user's Beacon state)")
	return fs, path
}

func (a *app) cmdServe(ctx context.Context, args []string) error {
	fs, path := a.reviewFlags("serve")
	collector := fs.String("config", "", "collector config path (defaults to the user's config.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("serve takes no positional arguments")
	}
	if err := a.refuseRoot(); err != nil {
		return err
	}
	dir, err := localstate.Dir(*path, false)
	if err != nil {
		return err
	}
	o, err := localstate.Load(dir)
	if err != nil {
		return fmt.Errorf("run beacon setup before starting the service: %w", err)
	}
	archive, err := cutover.Load(dir)
	if err != nil {
		return err
	}
	var jobs []supervisor.FixedJob
	var notify func(context.Context, string)
	if a.approvalListeners == nil { // fixtures do not start an installed collector or notify the logged-in user
		notify = notice.Send
		job, err := collectorJob(dir, *collector)
		if err != nil {
			return err
		}
		if job != nil {
			jobs = append(jobs, *job)
		}
	}
	return approvalserve.Run(ctx, approvalserve.Options{StateDir: dir, Origin: o, Now: a.now,
		AfterJournal: archive.Journal, ArchiveSignal: archive.Signal, Notify: notify, RequireHandlers: a.approvalListeners == nil,
		Supervision: &supervisor.Options{Jobs: jobs}, ListenReview: a.approvalListeners})
}

func collectorJob(dir, configFile string) (*supervisor.FixedJob, error) {
	path, err := configPath(configFile)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil // approvals can run before the collector is set up
	} else if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C", "BEACON_SUPERVISED=1"}
	// These are addresses, not secrets. The Linux collector must reach the same Secret Service as serve.
	for _, name := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if s, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+s)
		}
	}
	return &supervisor.FixedJob{Command: supervisor.Command{Path: self,
		Args: []string{"run", "--summary", "--config", path, "--vault-dir", dir}, Env: env,
		Stderr: filepath.Join(dir, "collector.log")}}, nil
}

func (a *app) cmdOpen(ctx context.Context, args []string) error {
	fs, path := a.reviewFlags("open")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("open takes no positional arguments")
	}
	dir, err := localstate.Dir(*path, false)
	if err != nil {
		return err
	}
	if _, err := localstate.Load(dir); err != nil {
		return errors.New("this state directory is not set up for Go Beacon")
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
