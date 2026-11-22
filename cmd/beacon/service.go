package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/CrossTerra/beacon/internal/coverage"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/runner"
	"github.com/CrossTerra/beacon/internal/service"
)

const serviceUsage = `usage: beacon service uninstall [--config PATH]
       beacon service status [--config PATH]
       beacon service logs [--legacy [--config PATH]]
`

// launcher inspects and retires the legacy collector job. It cannot start one.
type launcher interface {
	Bootout(ctx context.Context, target string) error
	Print(ctx context.Context, target string) (string, error)
}

type realLaunchctl struct{}

func (realLaunchctl) Bootout(ctx context.Context, target string) error {
	return runLaunchctl(ctx, "bootout", target)
}

func (realLaunchctl) Print(ctx context.Context, target string) (string, error) {
	cmd := exec.CommandContext(ctx, "launchctl", "print", target)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runLaunchctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "launchctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.Join(strings.Fields(string(out)), " ")
		if msg == "" {
			return errors.New("launchd did not accept the agent")
		}
		return fmt.Errorf("launchd did not accept the agent: %s", msg)
	}
	return nil
}

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

func (a *app) userID() int {
	if a.uid != nil {
		return a.uid()
	}
	return os.Getuid()
}

func (a *app) agentsDirectory() (string, error) {
	if a.agentsDir != nil {
		return a.agentsDir()
	}
	return service.AgentsDir()
}

func (a *app) launcher() (launcher, error) {
	if a.launchctl != nil {
		return a.launchctl, nil
	}
	if runtime.GOOS != "darwin" {
		return nil, errors.New("the legacy collector LaunchAgent only exists on macOS")
	}
	return realLaunchctl{}, nil
}

func (a *app) cmdService(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, serviceUsage)
		return exitError{2}
	}
	if err := a.refuseRoot(); err != nil {
		return err
	}
	switch args[0] {
	case "install":
		return errors.New("the collector LaunchAgent installer has been retired; see docs/tether.md")
	case "uninstall":
		return a.cmdServiceUninstall(ctx, args[1:])
	case "status":
		return a.cmdServiceStatus(ctx, args[1:])
	case "logs":
		return a.cmdServiceLogs(args[1:])
	default:
		fmt.Fprint(a.stderr, serviceUsage)
		return exitError{2}
	}
}

func (a *app) cmdServiceUninstall(ctx context.Context, args []string) error {
	fs, _ := a.flags("service uninstall")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: beacon service uninstall [--config PATH]")
	}
	lc, err := a.launcher()
	if err != nil {
		return err
	}
	target := service.Target(a.userID())
	if bootErr := lc.Bootout(ctx, target); bootErr != nil {
		// Do not remove the plist and report success if launchd still reports a loaded job.
		if out, err := lc.Print(ctx, target); err == nil || service.ParsePrint(out).Loaded {
			return bootErr
		}
	}
	dir, err := a.agentsDirectory()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, service.PlistName))
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(a.stdout, service.Label+" is not installed")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "Stopped and removed "+service.Label)
	return nil
}

func (a *app) cmdServiceStatus(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("service status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	lc, err := a.launcher()
	if err != nil {
		return err
	}
	var job service.Job
	if out, err := lc.Print(ctx, service.Target(a.userID())); err == nil {
		job = service.ParsePrint(out)
	}
	var pass service.Pass
	passErr := os.ErrNotExist
	identity := "none"
	// Status only needs the state directory. A rules or source error must not hide a pass that was recorded.
	if cfg, err := a.loadConfig(*cfgPath); err == nil {
		pass, passErr = service.ReadPass(cfg.StatePath())
		identity = a.identityStatus(ctx, cfg.StatePath())
	}
	fmt.Fprint(a.stdout, service.FormatStatus(job, pass, passErr, identity))
	return nil
}

// cmdStatus prints the last pass from pass.json and, with --coverage, each source's coverage from coverage.json. It
// reads the state directory only: no keychain, no launchd, no Signal.
func (a *app) cmdStatus(args []string) error {
	fs, cfgPath := a.flags("status")
	cov := fs.Bool("coverage", false, "print each source's coverage: last success, last failure, flags, and Slack's channels")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: beacon status [--coverage] [--config PATH]")
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

// identityStatus compares the plist binary with the leaf hash install recorded. It does not echo either.
func (a *app) identityStatus(ctx context.Context, state string) string {
	want, err := service.ReadLeaf(state)
	if errors.Is(err, os.ErrNotExist) {
		return "none"
	}
	if err != nil {
		return "unreadable"
	}
	dir, err := a.agentsDirectory()
	if err != nil {
		return "mismatch"
	}
	data, err := os.ReadFile(filepath.Join(dir, service.PlistName))
	if err != nil {
		return "mismatch"
	}
	bin := service.BinaryFromPlist(data)
	if bin == "" {
		return "mismatch"
	}
	id, err := a.identityOf(ctx, bin)
	if err != nil || !strings.EqualFold(id.Leaf, want) {
		return "mismatch"
	}
	return "ok"
}

func (a *app) cmdServiceLogs(args []string) error {
	fs, cfgPath := a.flags("service logs")
	legacy := fs.Bool("legacy", false, "print the retired LaunchAgent's summary log from the state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: beacon service logs [--legacy [--config PATH]]")
	}
	var path string
	if *legacy {
		cfg, err := a.loadConfig(*cfgPath)
		if err != nil {
			return err
		}
		path = filepath.Join(cfg.StatePath(), service.StdoutName)
	} else {
		p, err := a.collectorLogPath()
		if err != nil {
			return err
		}
		path = p
	}
	f, err := os.Open(path)
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
		case rep.LeaseReason == "":
			p.Lease = service.LeaseOther
		default:
			p.Lease = service.LeaseNotTaken
		}
	}
	if rep != nil {
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
	return service.CollectorLog()
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

// refusePinned refuses to run when the retired installer recorded a certificate and this binary is not signed with it.
func (a *app) refusePinned(ctx context.Context, dir string) error {
	want, err := service.ReadLeaf(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	id, err := a.identityOf(ctx, exe)
	if err != nil || !strings.EqualFold(id.Leaf, want) {
		return errors.New("signing identity does not match")
	}
	return nil
}

func (a *app) identityOf(ctx context.Context, path string) (service.Identity, error) {
	if a.identify != nil {
		return a.identify(ctx, path)
	}
	return service.CodesignIdentity(ctx, path)
}

// rotateServiceLogs rotates a log when this process's standard output or error is that file, which is how tether
// (and the retired LaunchAgent) runs the collector. A terminal run is left alone.
func (a *app) rotateServiceLogs(dir string) {
	if p, err := a.collectorLogPath(); err == nil {
		a.rotateIfOurs(p, a.stdout)
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
