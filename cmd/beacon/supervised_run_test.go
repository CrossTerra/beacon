package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/approvalserve"
	"github.com/CrossTerra/beacon/internal/supervisor"
	"golang.org/x/sys/unix"
)

func testSupervisor(t *testing.T, dir string, prepareWait time.Duration) *supervisor.Supervisor {
	t.Helper()
	lock, err := approval.LockInstance(dir)
	must(t, err)
	t.Cleanup(func() { must(t, approval.CloseInstance(dir, lock)) })
	s, err := supervisor.Open(t.Context(), dir, lock, supervisor.Options{
		Grace: fixtureGrace, KillWait: 3 * time.Second, PrepareWait: prepareWait,
	})
	must(t, err)
	t.Cleanup(func() {
		must(t, s.Close())
		assertNoSession(t, dir)
	})
	return s
}

func waitRun(t *testing.T, run *supervisor.Run) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), fixtureLimit)
	defer cancel()
	return run.Wait(ctx)
}

func TestSupervisionPreparedRun(t *testing.T) {
	dir := previewTemp(t)
	dir, err := filepath.EvalSymlinks(dir)
	must(t, err)
	s := testSupervisor(t, dir, 0)
	checkout := filepath.Join(dir, "checkout")
	moved := filepath.Join(dir, "renamed-checkout")
	must(t, os.Mkdir(checkout, 0o700))
	pin, err := supervisor.PinDirectory(checkout)
	must(t, err)
	t.Cleanup(func() { pin.Close() })
	c := fixtureCommand("run", dir)
	c.Dir = pin
	c.Env = []string{"RUN_VAR=fixture-env", "BEACON_SUPERVISOR=forged", "PWD=/wrong", "OLDPWD=/wrong"}
	c.Input = []byte("fixture-prompt")
	c.Stdout, c.Stderr = filepath.Join(dir, "output"), filepath.Join(dir, "errors")
	run, err := s.PrepareRun(runOne, c)
	must(t, err)
	if owner, err := s.Attribute(run.PGID()); err != nil || owner != runOne {
		t.Fatalf("prepared run was not registered: %q %v", owner, err)
	}
	if _, err := s.PrepareRun(runOne, c); !errors.Is(err, supervisor.ErrBusy) {
		t.Fatalf("duplicate live run: %v", err)
	}
	if fileText(dir, "run.pid") != "" || fileText(dir, "output") != "" || fileText(dir, "errors") != "" {
		t.Fatal("unreleased target executed")
	}
	// Before exec the approved name must still resolve to the pinned descriptor.
	must(t, os.Rename(checkout, moved))
	must(t, os.Mkdir(checkout, 0o700))
	if err := run.Release(); !errors.Is(err, supervisor.ErrCheckoutChanged) {
		t.Fatalf("renamed checkout was not refused at release: %v", err)
	}
	if _, err := waitRun(t, run); !errors.Is(err, supervisor.ErrCheckoutChanged) {
		t.Fatalf("renamed checkout result: %v", err)
	}
	if fileText(dir, "run.pid") != "" || fileText(dir, "output") != "" {
		t.Fatal("the target ran after the approved path was replaced")
	}
	if _, err := s.PrepareRun(runOne, c); !errors.Is(err, supervisor.ErrCheckoutChanged) {
		t.Fatalf("swapped checkout: %v", err)
	}
	// Restore the directory and prepare another run. Close the service's copy after
	// handoff: the reaper/child must have received the actual descriptor.
	must(t, os.Remove(checkout))
	must(t, os.Rename(moved, checkout))
	c.Stdout, c.Stderr = filepath.Join(dir, "ok-output"), filepath.Join(dir, "ok-errors")
	run, err = s.PrepareRun(runOne, c)
	must(t, err)
	must(t, pin.Close())
	must(t, run.Release())
	if code, err := waitRun(t, run); code != 3 || err != nil {
		t.Fatalf("run result: %d %v", code, err)
	}
	if got := fileText(dir, "ok-output"); got != checkout+" "+checkout+" fixture-prompt fixture-env\n" {
		t.Fatalf("wrong run cwd, environment or input: %q", got)
	}
	var session struct {
		Token string `json:"token"`
	}
	must(t, json.Unmarshal([]byte(fileText(dir, "supervisor.session")), &session))
	if got := fileText(dir, "ok-errors"); session.Token == "" || !strings.HasPrefix(got, "oldpwd= marker="+session.Token+":") || strings.Contains(got, "forged") {
		t.Fatalf("wrong run marker or OLDPWD: %q", got)
	}
	for _, path := range []string{c.Stdout, c.Stderr} {
		st, err := os.Stat(path)
		if err != nil || st.Mode() != 0o600 {
			t.Fatalf("unsafe output file: %v %v", st, err)
		}
	}
	c.Dir, err = supervisor.PinDirectory(checkout)
	must(t, err)
	defer c.Dir.Close()
	if _, err := s.PrepareRun(runOne, c); !errors.Is(err, supervisor.ErrLaunch) {
		t.Fatalf("overwrote existing run output: %v", err)
	}
	if err := run.Release(); !errors.Is(err, supervisor.ErrStopped) {
		t.Fatalf("released a completed launch again: %v", err)
	}
}

func TestSupervisionUnreleasedRunNeverActs(t *testing.T) {
	for _, reason := range []string{"timeout", "stop run", "shutdown"} {
		t.Run(reason, func(t *testing.T) {
			dir := previewTemp(t)
			s := testSupervisor(t, dir, 650*time.Millisecond)
			c := fixtureCommand("run", dir)
			c.Stdout = filepath.Join(dir, "output")
			run, err := s.PrepareRun(runOne, c)
			must(t, err)
			want := supervisor.ErrStopped
			switch reason {
			case "timeout":
				want = supervisor.ErrPrepareTimeout
			case "stop run":
				must(t, run.Stop())
			case "shutdown":
				s.BeginShutdown()
			}
			if _, err := waitRun(t, run); !errors.Is(err, want) {
				t.Fatalf("prepared cancellation: got %v, want %v", err, want)
			}
			if fileText(dir, "run.pid") != "" || fileText(dir, "output") != "" || !errors.Is(unix.Kill(run.PGID(), 0), unix.ESRCH) {
				t.Fatal("cancelled target ran or was not reaped")
			}
		})
	}
}

func TestSupervisionStopOneOfTwoRuns(t *testing.T) {
	root := previewTemp(t)
	s := testSupervisor(t, root, 0)
	start := func(id string) (string, *supervisor.Run) {
		dir := filepath.Join(root, id)
		must(t, os.Mkdir(dir, 0o700))
		r, err := s.PrepareRun(id, fixtureCommand("markerless-tree", dir))
		must(t, err)
		must(t, r.Release())
		untilFixture(t, "runner's detached listener", func() bool { return listenAlive(fileText(dir, "detached.addr")) })
		return dir, r
	}
	dirA, a := start(runOne)
	dirB, b := start(runTwo)
	if a.PGID() == b.PGID() {
		t.Fatal("runs shared a group")
	}
	must(t, a.Stop())
	if code, err := waitRun(t, a); code != 128+int(unix.SIGTERM) || err != nil {
		t.Fatalf("stopped run's result: %d %v", code, err)
	}
	if listenAlive(fileText(dirA, "detached.addr")) || !listenAlive(fileText(dirB, "detached.addr")) {
		t.Fatal("stop didn't isolate a single run tree")
	}
	must(t, s.Close())
	if listenAlive(fileText(dirB, "detached.addr")) {
		t.Fatal("parallel run survived shutdown")
	}
}

func TestSupervisionExecErrorIsLaunchFailure(t *testing.T) {
	dir := previewTemp(t)
	s := testSupervisor(t, dir, 0)
	pin, err := supervisor.PinDirectory(dir)
	must(t, err)
	defer pin.Close()
	c := fixtureCommand("run", dir)
	c.Dir = pin
	c.Path = filepath.Join(dir, "missing-binary")
	run, err := s.PrepareRun(runOne, c)
	must(t, err)
	if err := run.Release(); !errors.Is(err, supervisor.ErrLaunch) {
		t.Fatalf("exec failure reported as success: %v", err)
	}
}

func TestSupervisionExecReceiptSurvivesAnUnrecordedReply(t *testing.T) {
	dir := previewTemp(t)
	at := time.Now()
	var sup *supervisor.Supervisor
	open := func() *approval.Journal {
		j, err := approval.OpenJournalBeforeReplay(dir, at, "", func(lock *os.File) error {
			var err error
			sup, err = supervisor.Open(t.Context(), dir, lock, supervisor.Options{Grace: fixtureGrace, KillWait: time.Second})
			return err
		})
		must(t, err)
		opened := sup
		t.Cleanup(func() { must(t, errors.Join(opened.Close(), j.Close())) })
		return j
	}
	j := open()
	p, err := j.Add(approval.Request{Type: approval.Run, Agent: "fixture", Reason: "fixture", Params: approval.Object{
		"checkout": "a", "model": "test/model", "prompt": "fixture",
	}}, approval.PeerLabel{}, at)
	must(t, err)
	f, err := approval.Freeze(p, approval.Binding{Account: "linux:user:fixture", Destination: approval.Object{"checkout_id": "vol:1/inode:2", "branch": "none"}}, "")
	must(t, err)
	must(t, j.ApproveAll([]approval.Frozen{f}, []byte("fixture-asserted"), nil, at))
	_, err = j.StartAsserted(p.ID, at)
	must(t, err)
	dest := filepath.Join(dir, "runs", p.RunnerID)
	must(t, os.MkdirAll(dest, 0o700))
	r, err := sup.PrepareRun(p.RunnerID, fixtureCommand("exit", dir))
	must(t, err)
	token, err := j.ClaimRunDispatch(p.ID, r.PGID(), at)
	must(t, err)
	must(t, r.ReleaseWithReceipt(token))
	if _, err := approval.CheckPrivate(filepath.Join(dest, "exec.receipt"), 0o600); err != nil {
		t.Fatalf("reaper replied before persisting exec: %v", err)
	}
	// Simulate loss of the service-side result after the reaper's reply.
	must(t, sup.Close())
	must(t, j.Close())
	j = open()
	must(t, j.RecoverRuns(at))
	v, _, err := j.Get(p.ID, at)
	must(t, err)
	if v.Outcome == nil || v.Outcome.ConfirmedBy != "read_back" || v.Status != "executed" || v.RunnerState != approval.RunnerClosed {
		t.Fatal("recovery did not settle and close the confirmed run")
	}
}

func TestSupervisionRefusesUnconfirmedRecoveryBeforeReplay(t *testing.T) {
	dir := previewTemp(t)
	marker := filepath.Join(dir, "supervisor.session")
	must(t, os.WriteFile(marker, []byte("malformed marker"), 0o600))
	origin, err := approval.ParseOrigin(approval.LocalOrigin, "")
	must(t, err)
	err = approvalserve.Run(t.Context(), approvalserve.Options{
		StateDir: dir, Origin: origin, Supervision: &supervisor.Options{},
		ListenReview: func() ([]net.Listener, error) {
			t.Error("review bound before recovery was confirmed")
			return nil, errors.New("unexpected bind")
		},
	})
	if !errors.Is(err, supervisor.ErrUnconfirmed) || fileText(dir, "supervisor.session") != "malformed marker" {
		t.Fatalf("unsafe recovery: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "journal.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal was opened before recovery: %v", err)
	}
}
