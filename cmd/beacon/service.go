package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/coverage"
	"github.com/CrossTerra/beacon/internal/localstate"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/runner"
	"github.com/CrossTerra/beacon/internal/service"
)

func (a *app) refuseRoot() error {
	euid := os.Geteuid
	if a.euid != nil {
		euid = a.euid
	}
	if euid() == 0 {
		return errors.New("refusing to run as root")
	}
	return nil
}

// cmdStatus prints the last pass from pass.json and, with --coverage, each source's coverage from coverage.json. It
// never opens credentials. --review also checks the configured Tailscale Serve route.
func (a *app) cmdStatus(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("status")
	cov := fs.Bool("coverage", false, "print each source's coverage: last success, last failure, flags, and Slack's channels")
	review := fs.Bool("review", false, "print the review origin and check the Tailscale Serve route")
	state := fs.String("state-dir", "", "service state directory for --review")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: beacon status [--coverage] [--config PATH]")
	}
	if *review {
		dir, err := localstate.Dir(*state, false)
		if err != nil {
			return err
		}
		o, err := localstate.Load(dir)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "Review origin:", o.URL())
		return localstate.CheckServe(ctx, o, a.tailscale)
	}
	cfg, err := a.loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	pass, passErr := service.ReadPass(cfg.StatePath())
	fmt.Fprint(a.stdout, service.FormatPass(pass, passErr))
	if !*cov {
		return nil
	}
	rec, err := coverage.Read(cfg.StatePath())
	if err != nil {
		fmt.Fprintln(a.stdout, "coverage: unreadable")
		return nil
	}
	fmt.Fprint(a.stdout, coverage.Format(rec, a.now()))
	return nil
}

func (a *app) cmdLogs(args []string) error {
	fs, state := a.reviewFlags("logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: beacon logs [--state-dir DIR]")
	}
	var path string
	var err error
	if *state == "" {
		path, err = a.collectorLogPath()
	} else {
		var dir string
		dir, err = localstate.Dir(*state, false)
		path = filepath.Join(dir, "collector.log")
	}
	if err != nil {
		return err
	}
	f, err := approval.OpenPrivate(path, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(a.stdout, "no collector log yet")
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(a.stdout, io.LimitReader(f, 8<<20))
	return err
}

// recordPass writes pass.json. sources are the sources the pass collected, when it collected only some: a personal
// source another pass withheld stays listed until its own next pass. Nil means every source.
func (a *app) recordPass(dir, instance string, rep *runner.Report, outcome string, sources []string) {
	p := service.Pass{At: a.now().UTC(), Outcome: outcome}
	if sources != nil {
		ours := map[string]bool{}
		for _, s := range sources {
			ours[s] = true
		}
		if prev, err := service.ReadPass(dir); err == nil {
			for _, w := range prev.Withheld {
				if !ours[w.Source] {
					p.Withheld = append(p.Withheld, w)
				}
			}
		}
	}
	if rep != nil && rep.Lease != nil {
		switch {
		case rep.Lease.Held:
			p.Lease = service.LeaseHeld
			p.Holder = instance
			until := rep.Lease.Until.UTC()
			p.LeaseUntil = &until
		case rep.LeaseReason == "":
			p.Lease = service.LeaseOther
		default:
			p.Lease = service.LeaseNotTaken
		}
		p.LeaseReason = string(rep.LeaseReason)
	}
	if rep != nil {
		p.SendReason = string(rep.SendReason)
		for _, w := range rep.Withheld {
			p.Withheld = append(p.Withheld, service.Withheld{Source: w.Source, Prefix: w.Prefix, Reason: string(w.Reason)})
		}
	}
	if service.Failed(outcome) {
		prev, err := service.ReadPass(dir)
		p.Failures = service.NextFailures(prev, err == nil)
	}
	_ = service.WritePass(dir, p)
}

// failLoop records a persistent failure and, unless this is a one-shot run, waits before the non-zero exit. The
// wait is one poll interval, doubling up to 30 minutes, to prevent rapid restarts.
// Only SIGTERM during the wait is a clean exit. SIGINT still returns the failure. alreadyRecorded means the caller
// wrote pass.json, including the streak.
func (a *app) failLoop(once bool, dir string, poll time.Duration, interrupt context.Context, log *logs.Logger, reason logs.Reason, text string, alreadyRecorded bool) error {
	log.Log(logs.RunFailed, logs.Why(reason))
	if dir != "" && !alreadyRecorded {
		a.recordPass(dir, "", nil, string(reason), nil)
	}
	if !once && a.backoff(interrupt, poll, dir) {
		log.Log(logs.RunStopped)
		return nil
	}
	return runFailure{text: text}
}

// backoff waits. It reports whether SIGTERM stopped the wait. Any other interruption is not a clean stop.
func (a *app) backoff(interrupt context.Context, poll time.Duration, dir string) bool {
	n := 1
	if prev, err := service.ReadPass(dir); err == nil && prev.Failures > 0 {
		n = prev.Failures
	}
	if a.sleepFor(interrupt, service.Backoff(poll, n)) == nil {
		return false
	}
	return a.termFired()
}

func (a *app) sleepFor(interrupt context.Context, d time.Duration) error {
	if a.sleep != nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-interrupt.Done():
				cancel()
			case <-a.termDone():
				cancel()
			case <-ctx.Done():
			}
		}()
		return a.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-interrupt.Done():
		return interrupt.Err()
	case <-a.termDone():
		return errors.New("stopped")
	case <-timer.C:
		return nil
	}
}

func (a *app) collectorLogPath() (string, error) {
	if a.collectorLog != nil {
		return a.collectorLog()
	}
	dir := a.vaultDir
	if dir == "" {
		var err error
		dir, err = localstate.DefaultDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "collector.log"), nil
}

// serviceStdout reports whether standard output is the collector log or the retired LaunchAgent's log.
func (a *app) serviceStdout(state string) bool {
	out, ok := a.stdout.(*os.File)
	if !ok {
		return false
	}
	if p, err := a.collectorLogPath(); err == nil && sameFile(p, int(out.Fd())) {
		return true
	}
	return sameFile(filepath.Join(state, service.StdoutName), int(out.Fd()))
}

// Rotate only a descriptor of this process that points to the collector's log, never terminal output.
func (a *app) rotateServiceLogs(dir string) {
	if p, err := a.collectorLogPath(); err == nil {
		a.rotateIfOurs(p, a.stdout)
		a.rotateIfOurs(p, a.stderr)
	}
	a.rotateIfOurs(filepath.Join(dir, service.StdoutName), a.stdout)
	a.rotateIfOurs(filepath.Join(dir, service.StderrName), a.stderr)
}

func (a *app) rotateIfOurs(path string, w io.Writer) {
	f, ok := w.(*os.File)
	if !ok || !sameFile(path, int(f.Fd())) {
		return
	}
	_ = service.Rotate(path, service.LogMaxBytes)
}

func sameFile(path string, fd int) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return false
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return sys.Dev == st.Dev && sys.Ino == st.Ino
}
