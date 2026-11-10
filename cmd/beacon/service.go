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
	if err := a.checkIdentity(ctx, bin); err != nil {
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
	// Stop a previous job before replacing its definition. A job that is not loaded is not an error.
	_ = lc.Bootout(ctx, service.Target(uid))
	if err := writePlist(path, plist); err != nil {
		return err
	}
	_ = lc.Enable(ctx, service.Target(uid))
	if err := lc.Bootstrap(ctx, service.Domain(uid), path); err != nil {
		os.Remove(path)
		return err
	}
	fmt.Fprintf(a.stdout, "Installed %s\nPlist: %s\nBinary: %s\nConfig: %s\nLogs: %s\n",
		service.Label, path, bin, e.cfg.Path, spec.Stderr)
	return nil
}

func (a *app) checkIdentity(ctx context.Context, path string) error {
	var id service.Identity
	var err error
	if a.identify != nil {
		id, err = a.identify(ctx, path)
	} else {
		id, err = service.CodesignIdentity(ctx, path)
	}
	if err != nil || !id.Stable() {
		return errors.New(errNotSigned)
	}
	return nil
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
	if e, err := a.load(*cfgPath); err == nil {
		pass, passErr = service.ReadPass(e.cfg.StatePath())
	}
	fmt.Fprint(a.stdout, service.FormatStatus(job, pass, passErr))
	return nil
}

func (a *app) cmdServiceLogs(args []string) error {
	fs, cfgPath := a.flags("service logs")
	useStdout := fs.Bool("stdout", false, "print the standard output log instead of the JSON log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := a.load(*cfgPath)
	if err != nil {
		return err
	}
	name := service.StderrName
	if *useStdout {
		name = service.StdoutName
	}
	f, err := os.Open(filepath.Join(e.cfg.StatePath(), name))
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
	_ = service.WritePass(dir, p)
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
