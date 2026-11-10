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

	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/runner"
	"github.com/CrossTerra/beacon/internal/service"
)

const serviceUsage = `usage: beacon service install [--config PATH] [--binary PATH] [--force]
       beacon service uninstall [--config PATH]
       beacon service status [--config PATH]
       beacon service logs [--config PATH] [--stdout]
`

const errNotSigned = "refusing to install a binary that is not code-signed with a stable identity; build it with scripts/build-local.sh"

// launcher is launchd. Tests set a fake. The real one is used only on macOS, and only when a person runs the command.
type launcher interface {
	Bootstrap(ctx context.Context, domain, plist string) error
	Bootout(ctx context.Context, target string) error
	Enable(ctx context.Context, target string) error
	Print(ctx context.Context, target string) (string, error)
}

type realLaunchctl struct{}

func (realLaunchctl) Bootstrap(ctx context.Context, domain, plist string) error {
	return runLaunchctl(ctx, "bootstrap", domain, plist)
}

func (realLaunchctl) Bootout(ctx context.Context, target string) error {
	return runLaunchctl(ctx, "bootout", target)
}

func (realLaunchctl) Enable(ctx context.Context, target string) error {
	return runLaunchctl(ctx, "enable", target)
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
		return nil, errors.New("beacon service is a per-user launchd agent and runs only on macOS")
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
		return a.cmdServiceInstall(ctx, args[1:])
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

func (a *app) cmdServiceInstall(ctx context.Context, args []string) error {
	fs, cfgPath := a.flags("service install")
	binary := fs.String("binary", "", "signed binary to run (default: this executable)")
	force := fs.Bool("force", false, "replace an existing agent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: beacon service install [--config PATH] [--binary PATH] [--force]")
	}
	lc, err := a.launcher()
	if err != nil {
		return err
	}
	e, err := a.load(*cfgPath)
	if err != nil {
		return err
	}
	bin, err := resolveBinary(*binary)
	if err != nil {
		return err
	}
	id, err := a.checkIdentity(ctx, bin)
	if err != nil {
		return err
	}
	state, err := filepath.Abs(e.cfg.StatePath())
	if err != nil {
		return err
	}
	spec := service.Spec{
		Binary: bin,
		Config: e.cfg.Path,
		Stdout: filepath.Join(state, service.StdoutName),
		Stderr: filepath.Join(state, service.StderrName),
	}
	plist, err := service.Plist(spec)
	if err != nil {
		return err
	}
	dir, err := a.agentsDirectory()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, service.PlistName)
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s already exists; pass --force to replace it, or beacon service uninstall", path)
	}
	uid := a.userID()
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(state, 0o700); err != nil {
		return err
	}
	if err := service.EnsureLog(spec.Stdout); err != nil {
		return err
	}
	if err := service.EnsureLog(spec.Stderr); err != nil {
		return err
	}
	// Stop a previous job before replacing its definition. A job that is not loaded is not an error. Wait until it
	// is gone before bootstrap, so the new job does not overlap the old one.
	target := service.Target(uid)
	_ = lc.Bootout(ctx, target)
	if err := a.waitUnloaded(ctx, lc, target); err != nil {
		return err
	}
	if err := service.WriteLeaf(state, id.Leaf); err != nil {
		return err
	}
	if err := writePlist(path, plist); err != nil {
		return err
	}
	_ = lc.Enable(ctx, target)
	if err := lc.Bootstrap(ctx, service.Domain(uid), path); err != nil {
		os.Remove(path)
		return err
	}
	fmt.Fprintf(a.stdout, "Installed %s\nPlist: %s\nBinary: %s\nConfig: %s\nLogs: %s\n",
		service.Label, path, bin, e.cfg.Path, spec.Stderr)
	return nil
}

func (a *app) checkIdentity(ctx context.Context, path string) (service.Identity, error) {
	id, err := a.identityOf(ctx, path)
	if err != nil || !id.Stable() || !service.LeafOK(id.Leaf) {
		return service.Identity{}, errors.New(errNotSigned)
	}
	return id, nil
}

// waitUnloaded polls until launchd no longer has the job. The agent may take ExitTimeOut to finish the in-flight
// send after SIGTERM, so the wait is that long, not a few seconds. The last check is after the last sleep: a job
// that exits during that second is not still loaded.
func (a *app) waitUnloaded(ctx context.Context, lc launcher, target string) error {
	for i := 0; i < service.ExitTimeOut; i++ {
		if unloaded, err := a.jobUnloaded(ctx, lc, target); unloaded || err != nil {
			return err
		}
		if err := a.sleepFor(ctx, time.Second); err != nil {
			return err
		}
	}
	unloaded, err := a.jobUnloaded(ctx, lc, target)
	if unloaded || err != nil {
		return err
	}
	return errors.New("the previous agent is still loaded; beacon service uninstall stops it")
}

func (a *app) jobUnloaded(ctx context.Context, lc launcher, target string) (bool, error) {
	out, err := lc.Print(ctx, target)
	if err != nil || !service.ParsePrint(out).Loaded {
		return true, nil
	}
	return false, nil
}

func resolveBinary(path string) (string, error) {
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", errors.New("the binary to install could not be found")
		}
		path = exe
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("the binary to install could not be found")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New("the binary to install could not be found")
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return "", errors.New("the binary to install could not be found")
	}
	return resolved, nil
}

func writePlist(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".plist-tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
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
	_ = lc.Bootout(ctx, service.Target(a.userID()))
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
	useStdout := fs.Bool("stdout", false, "print the standard output log instead of the JSON log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := a.loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	name := service.StderrName
	if *useStdout {
		name = service.StdoutName
	}
	f, err := os.Open(filepath.Join(cfg.StatePath(), name))
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(a.stdout, "no service log yet")
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(a.stdout, io.LimitReader(f, 8<<20))
	return err
}

func (a *app) recordPass(dir, instance string, rep *runner.Report, outcome string) {
	p := service.Pass{At: a.now().UTC(), Outcome: outcome}
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
// wait is one poll interval, doubling up to 30 minutes, so launchd cannot restart the agent every throttle interval.
// Only SIGTERM during the wait is a clean exit. SIGINT still returns the failure. alreadyRecorded means the caller
// wrote pass.json, including the streak.
func (a *app) failLoop(once bool, dir string, poll time.Duration, interrupt context.Context, log *logs.Logger, reason logs.Reason, text string, alreadyRecorded bool) error {
	log.Log(logs.RunFailed, logs.Why(reason))
	if dir != "" && !alreadyRecorded {
		a.recordPass(dir, "", nil, string(reason))
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

func (a *app) serviceStdout(state string) bool {
	return sameFile(filepath.Join(state, service.StdoutName), 1)
}

// refusePinned refuses to run when install recorded a certificate and this binary is not signed with it.
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

// rotateServiceLogs rotates the service logs when this process's standard output or error is that file, which is
// how launchd runs the agent. A terminal run is left alone.
func (a *app) rotateServiceLogs(dir string) {
	a.rotateIfOurs(filepath.Join(dir, service.StdoutName), 1)
	a.rotateIfOurs(filepath.Join(dir, service.StderrName), 2)
}

func (a *app) rotateIfOurs(path string, fd int) {
	if !sameFile(path, fd) {
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
