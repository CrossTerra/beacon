package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/approvalserve"
	"github.com/CrossTerra/beacon/internal/supervisor"
	"golang.org/x/sys/unix"
)

const (
	fixtureGrace = 500 * time.Millisecond
	fixtureLimit = 8 * time.Second
	runOne       = "run_01m6a6ft00e81840g2081040g3"
	runTwo       = "run_01m6a6ft00e81840g2081040g4"
)

// All roles re-exec this test binary, not any installed program. Paths, inputs, and the sole
// listener are fixture-local. A watchdog makes even deliberately orphaned fixtures finite.
func supervisionFixture(args []string) int {
	if len(args) < 2 {
		return 90
	}
	role, dir := args[0], args[1]
	time.AfterFunc(60*time.Second, func() { os.Exit(91) })
	if err := runSupervisionFixture(role, dir, args[2:]); err != nil {
		if errors.Is(err, errFixtureExit) {
			return 3
		}
		fmt.Fprintln(os.Stderr, err)
		return 92
	}
	return 0
}

var errFixtureExit = errors.New("fixture nonzero exit")

func fixtureCommand(role, dir string, args ...string) supervisor.Command {
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return supervisor.Command{Path: self, Args: append([]string{"--supervision-fixture", role, dir}, args...)}
}

func fixtureProcess(role, dir string, args ...string) *exec.Cmd {
	c := fixtureCommand(role, dir, args...)
	return exec.Command(c.Path, c.Args...)
}

func fixtureWrite(dir, name, value string) error {
	p := filepath.Join(dir, name)
	tmp := p + "." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, []byte(value), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func fixturePID(dir, role string) error {
	return fixtureWrite(dir, role+".pid", strconv.Itoa(os.Getpid()))
}

func runSupervisionFixture(role, dir string, args []string) error {
	switch role {
	case "service":
		return supervisionService(dir, args[0], args[1])
	case "collector":
		ctx, stop := signal.NotifyContext(context.Background(), unix.SIGTERM)
		defer stop()
		if os.Getenv("BEACON_FIXTURE_PARENT_ONLY") != "" || os.Getenv("HOME") != dir || os.Getenv("BEACON_SUPERVISOR") == "" {
			return errors.New("wrong collector environment")
		}
		fmt.Println("Items: 0 send")
		if err := fixturePID(dir, role); err != nil {
			return err
		}
		<-ctx.Done()
		return fixtureWrite(dir, "collector-term", "TERM")
	case "watch", "leaky-watch":
		p := filepath.Join(dir, "watch-first.pid")
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			if err := fixturePID(dir, "watch-first"); err != nil {
				return err
			}
			if role == "leaky-watch" {
				if err := spawnFixture("watch-detached", dir, true, os.Environ()); err != nil {
					return err
				}
				deadline := time.Now().Add(fixtureLimit)
				for fileText(dir, "watch-detached.addr") == "" && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
			}
			return errFixtureExit
		}
		if err := fixturePID(dir, "watch-next"); err != nil {
			return err
		}
		select {}
	case "tree", "markerless-tree":
		if err := fixturePID(dir, "tree"); err != nil {
			return err
		}
		if err := spawnFixture("child", dir, false, os.Environ(), role); err != nil {
			return err
		}
		select {}
	case "child":
		signal.Ignore(unix.SIGTERM)
		if err := fixturePID(dir, role); err != nil {
			return err
		}
		env := os.Environ()
		if args[0] == "markerless-tree" {
			env = []string{} // ancestry, not the marker, must find this detached process
		}
		if err := spawnFixture("detached", dir, true, env); err != nil {
			return err
		}
		select {}
	case "detached", "watch-detached":
		signal.Ignore(unix.SIGTERM)
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer l.Close()
		if err := fixturePID(dir, role); err != nil {
			return err
		}
		if err := fixtureWrite(dir, role+".addr", l.Addr().String()); err != nil {
			return err
		}
		for {
			c, err := l.Accept()
			if err != nil {
				return err
			}
			c.Close()
		}
	case "probe":
		// This is the very first operation of the target image. The socket must see its run ID.
		ctx, cancel := context.WithTimeout(context.Background(), fixtureLimit)
		defer cancel()
		reply, err := actsocket.Call(ctx, args[0], approval.Object{
			"op": "propose", "type": "slack.post", "params": approval.Object{
				"workspace": "T1", "channel": "C1", "thread_ts": nil, "text": "fixture",
			}, "agent": approval.Object{"name": "fixture"}, "reason": "fixture",
		}, 0)
		if err != nil {
			return err
		}
		return fixtureWrite(dir, "probe.reply", string(reply))
	case "run":
		prompt, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		fmt.Println(cwd, os.Getenv("PWD"), string(prompt), os.Getenv("RUN_VAR"))
		fmt.Fprintf(os.Stderr, "oldpwd=%s marker=%s\n", os.Getenv("OLDPWD"), os.Getenv("BEACON_SUPERVISOR"))
		if err := fixturePID(dir, role); err != nil {
			return err
		}
		return errFixtureExit
	case "exit":
		return nil
	default:
		return errors.New("unknown supervision fixture")
	}
}

func spawnFixture(role, dir string, detach bool, env []string, args ...string) error {
	c := fixtureProcess(role, dir, args...)
	c.Env = env
	c.SysProcAttr = &unix.SysProcAttr{Setsid: detach}
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}

func supervisionService(dir, state, mode string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer stop()
	origin, err := approval.ParseOrigin(approval.LocalOrigin, "")
	if err != nil {
		return err
	}
	collector := fixtureCommand("collector", dir)
	collector.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "LANG=C"}
	collector.Stdout = filepath.Join(dir, "collector.log")
	watch := "watch"
	if mode == "leaky-watch" {
		watch = mode
	}
	var addr string
	return approvalserve.Run(ctx, approvalserve.Options{
		StateDir: state, Origin: origin,
		Supervision: &supervisor.Options{Grace: fixtureGrace, KillWait: 3 * time.Second,
			Jobs: []supervisor.FixedJob{{Command: collector}, {Command: fixtureCommand(watch, dir)}}},
		ListenReview: func() ([]net.Listener, error) {
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			addr = l.Addr().String()
			return []net.Listener{l}, nil
		},
		ConnectJobs: func(s *supervisor.Supervisor) error {
			kind := "tree"
			if mode == "markerless-tree" {
				kind = mode
			}
			r, err := s.PrepareRun(runOne, fixtureCommand(kind, dir))
			if err != nil {
				return err
			}
			if err := r.Release(); err != nil {
				return err
			}
			if mode == "attribution" {
				probe, err := s.PrepareRun(runTwo, fixtureCommand("probe", dir, state))
				if err != nil {
					return err
				}
				if err := probe.Release(); err != nil {
					return err
				}
			}
			return fixtureWrite(dir, "ready", addr)
		},
	})
}

func fileText(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return string(b)
}

func untilFixture(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(fixtureLimit)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func listenAlive(addr string) bool {
	c, err := net.DialTimeout("tcp4", addr, 150*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

type supervisedService struct {
	cmd            *exec.Cmd
	dir, state     string
	addr, detached string
	reaper         int
	done           chan struct{}
	err            error
}

func startSupervised(t *testing.T, root, generation, mode string) *supervisedService {
	t.Helper()
	state := filepath.Join(root, "state")
	dir := filepath.Join(root, generation)
	must(t, os.Mkdir(dir, 0o700))
	c := fixtureProcess("service", dir, state, mode)
	c.Env = []string{"BEACON_FIXTURE_PARENT_ONLY=not-for-jobs"}
	c.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	log, err := os.Create(filepath.Join(dir, "fixture.log"))
	must(t, err)
	c.Stderr = log
	err = c.Start()
	log.Close()
	must(t, err)
	s := &supervisedService{cmd: c, dir: dir, state: state, done: make(chan struct{})}
	go func() {
		s.err = c.Wait()
		close(s.done)
	}()
	t.Cleanup(func() {
		// Do not clean unrelated PIDs. Each fixture also has a self-exit watchdog in case it
		// escaped tracking, e.g. if the test failed halfway through a double crash.
		_ = c.Process.Signal(unix.SIGTERM)
		select {
		case <-s.done:
		case <-time.After(fixtureLimit):
			_ = c.Process.Kill()
			<-s.done
		}
		if t.Failed() {
			t.Logf("fixture %s log: %s", generation, fileText(dir, "fixture.log"))
		}
	})
	untilFixture(t, "service readiness", func() bool {
		select {
		case <-s.done:
			t.Fatalf("service exited during startup: %v: %s", s.err, fileText(dir, "fixture.log"))
		default:
		}
		s.addr = fileText(dir, "ready")
		return s.addr != ""
	})
	untilFixture(t, "collector, watch and detached listener", func() bool {
		s.detached = fileText(dir, "detached.addr")
		return fileText(dir, "collector.pid") != "" && fileText(dir, "watch-first.pid") != "" && s.detached != "" && listenAlive(s.detached)
	})
	// Find the only direct child of the service in its own group. The job leaders belong to it.
	b, err := exec.Command("ps", "-axo", "pid=,ppid=,pgid=").Output()
	must(t, err)
	for _, row := range strings.Split(string(b), "\n") {
		f := strings.Fields(row)
		if len(f) == 3 && f[0] == f[2] && f[1] == strconv.Itoa(c.Process.Pid) {
			s.reaper, _ = strconv.Atoi(f[0])
			break
		}
	}
	if s.reaper <= 1 || s.reaper == c.Process.Pid {
		t.Fatal("reaper does not have its own process group")
	}
	return s
}

func (s *supervisedService) wait(t *testing.T, clean bool) {
	t.Helper()
	select {
	case <-s.done:
		if (s.err == nil) != clean {
			t.Fatalf("unexpected service exit: %v (clean=%v)", s.err, clean)
		}
	case <-time.After(fixtureLimit):
		t.Fatal("service did not exit")
	}
}

func fixturePIDs(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.pid"))
	must(t, err)
	var pids []string
	for _, file := range files {
		pid := fileText(dir, filepath.Base(file))
		if n, err := strconv.Atoi(pid); err != nil || n <= 1 {
			t.Fatalf("bad fixture pid %q", pid)
		}
		pids = append(pids, pid)
	}
	return pids
}

func hasLiveFixture(t *testing.T, pids []string) bool {
	t.Helper()
	b, err := exec.Command("ps", "-p", strings.Join(pids, ","), "-o", "stat=").Output()
	if err != nil {
		var e *exec.ExitError
		if !errors.As(err, &e) || e.ExitCode() != 1 || len(b) != 0 {
			t.Fatal(err)
		}
	}
	for _, stat := range strings.Fields(string(b)) {
		if !strings.HasPrefix(stat, "Z") {
			return true
		}
	}
	return false
}

func assertSupervisedGone(t *testing.T, s *supervisedService) {
	t.Helper()
	untilFixture(t, "all fixture processes and listeners gone", func() bool {
		return !listenAlive(s.detached) && !listenAlive(s.addr) && !hasLiveFixture(t, fixturePIDs(t, s.dir))
	})
}

func assertNoSession(t *testing.T, state string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(state, "supervisor.session")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("supervision session was not removed: %v", err)
	}
}

func TestSupervisionStop(t *testing.T) {
	for _, mode := range []string{"normal", "backoff", "leaky-watch"} {
		t.Run(mode, func(t *testing.T) {
			s := startSupervised(t, previewTemp(t), "first", mode)
			log := filepath.Join(s.dir, "collector.log")
			if st, err := os.Stat(log); err != nil || st.Mode() != 0o600 || fileText(s.dir, "collector.log") != "Items: 0 send\n" {
				t.Fatalf("collector summary log: %v %v", st, err)
			}
			if mode == "backoff" {
				untilFixture(t, "watch leader reaped, restart pending", func() bool {
					pid, _ := strconv.Atoi(fileText(s.dir, "watch-first.pid"))
					return pid > 1 && errors.Is(unix.Kill(pid, 0), unix.ESRCH)
				})
			} else {
				untilFixture(t, "watch restart", func() bool { return fileText(s.dir, "watch-next.pid") != "" })
				if mode == "leaky-watch" {
					leak := fileText(s.dir, "watch-detached.addr")
					if leak == "" || listenAlive(leak) {
						t.Fatal("watch restarted before its detached descendant was stopped")
					}
				}
			}
			// Model systemd's TERM of every group without installing a unit. The reaper must
			// survive the signal and continue through KILL for the detached listener.
			must(t, unix.Kill(-s.reaper, unix.SIGTERM))
			begun := time.Now()
			must(t, s.cmd.Process.Signal(unix.SIGTERM))
			s.wait(t, true)
			if time.Since(begun) < fixtureGrace || fileText(s.dir, "collector-term") != "TERM" {
				t.Fatal("shutdown did not give TERM a grace period")
			}
			assertSupervisedGone(t, s)
			assertNoSession(t, s.state)
			if mode == "backoff" && fileText(s.dir, "watch-next.pid") != "" {
				t.Fatal("watch restarted during shutdown")
			}
		})
	}
}

func TestSupervisionServiceSIGKILL(t *testing.T) {
	s := startSupervised(t, previewTemp(t), "first", "markerless-tree")
	// Give the reaper a tick to observe ancestry before descendants are reparented.
	time.Sleep(250 * time.Millisecond)
	must(t, unix.Kill(-s.cmd.Process.Pid, unix.SIGKILL)) // launchd can kill the whole service group
	s.wait(t, false)
	if lock, err := approval.LockInstance(s.state); !errors.Is(err, approval.ErrLocked) {
		if err == nil {
			must(t, approval.CloseInstance(s.state, lock))
		}
		t.Fatalf("reaper did not retain the instance lock during cleanup: %v", err)
	}
	assertSupervisedGone(t, s)
	untilFixture(t, "reaper cleanup confirmation", func() bool {
		_, err := os.Stat(filepath.Join(s.state, "supervisor.session"))
		return errors.Is(err, os.ErrNotExist)
	})
}

func TestSupervisionReaperSIGKILLAndNextStartSweep(t *testing.T) {
	root := previewTemp(t)
	first := startSupervised(t, root, "first", "normal")
	old := fileText(first.state, "supervisor.session")
	if len(old) != 32 {
		t.Fatal("no crash recovery marker")
	}
	must(t, unix.Kill(first.reaper, unix.SIGKILL))
	first.wait(t, false) // no leader left to supervise: serving stops, but the marker remains
	if listenAlive(first.addr) || !listenAlive(first.detached) || fileText(first.state, "supervisor.session") != old {
		t.Fatal("the killed reaper did not leave an isolated, recoverable session")
	}
	second := startSupervised(t, root, "second", "normal")
	if listenAlive(first.detached) || hasLiveFixture(t, fixturePIDs(t, first.dir)) || fileText(second.state, "supervisor.session") == old {
		t.Fatal("replacement started before sweeping the old tree")
	}
	must(t, second.cmd.Process.Signal(unix.SIGTERM))
	second.wait(t, true)
	assertSupervisedGone(t, second)
	assertNoSession(t, second.state)
}

func TestSupervisionSocketAttribution(t *testing.T) {
	s := startSupervised(t, previewTemp(t), "first", "attribution")
	untilFixture(t, "proposal from the newly released run", func() bool { return fileText(s.dir, "probe.reply") != "" })
	var p struct{ ID string }
	must(t, json.Unmarshal([]byte(fileText(s.dir, "probe.reply")), &p))
	if !approval.ValidID(p.ID, "apr") {
		t.Fatal("run did not create a proposal")
	}
	for _, op := range []string{"get", "wait", "withdraw"} {
		frame := approval.Object{"op": op, "id": p.ID}
		if op == "wait" {
			frame["after"], frame["timeout"] = 0, 1
		}
		b, err := actsocket.Call(t.Context(), s.state, frame, time.Second)
		must(t, err)
		if !strings.Contains(string(b), `"error":"not_found"`) {
			t.Fatalf("unattributed peer could %s run proposal: %s", op, b)
		}
	}
}
