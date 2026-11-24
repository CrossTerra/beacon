//go:build linux

package supervisor

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStatusUID(t *testing.T) {
	uid, ok := statusUID("Name:\tbeacon\nUid:\t1000\t1000\t1000\t1000\nGid:\t1000\n")
	if !ok || uid != 1000 {
		t.Fatalf("uid: %d %v", uid, ok)
	}
	if _, ok := statusUID("Name:\tbeacon\n"); ok {
		t.Fatal("missing Uid line was accepted")
	}
}

func TestConfirmedExitDoesNotReap(t *testing.T) {
	p, err := os.StartProcess("/bin/true", []string{"true"}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var status unix.WaitStatus
		_, _ = unix.Wait4(p.Pid, &status, unix.WNOHANG, nil)
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !confirmedExit(p.Pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if !confirmedExit(p.Pid) {
		t.Fatal("zombie child was not confirmed exited")
	}
	var status unix.WaitStatus
	wpid, err := unix.Wait4(p.Pid, &status, unix.WNOHANG, nil)
	if err != nil || wpid != p.Pid {
		t.Fatalf("WNOWAIT reaped the child or it was not a zombie: %d %v", wpid, err)
	}
}

func TestMissingTableEntryDoesNotKillLiveChild(t *testing.T) {
	p, err := os.StartProcess("/bin/sleep", []string{"sleep", "30"}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Kill(p.Pid, unix.SIGKILL)
		var status unix.WaitStatus
		_, _ = unix.Wait4(p.Pid, &status, 0, nil)
	})
	e := newEngine("", sessionPrefix("fixture"), timing{grace: time.Second, killWait: time.Second}, nil)
	e.list = func() (processTable, error) { return processTable{}, nil }
	e.jobs[p.Pid] = &job{pid: p.Pid, tag: 1}
	var signals []int
	e.signal = func(pid int, _ unix.Signal) { signals = append(signals, pid) }
	if _, err := e.refresh(false); err != nil {
		t.Fatal(err)
	}
	if len(signals) != 0 || !e.jobs[p.Pid].killAt.IsZero() {
		t.Fatalf("lookup failure stopped a live child: signals=%v killAt=%v", signals, e.jobs[p.Pid].killAt)
	}
}

func TestLookupKeepsNonDumpableSameUser(t *testing.T) {
	if os.Getenv("BEACON_NONDUMPABLE") == "1" {
		if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
			os.Exit(2)
		}
		time.Sleep(30 * time.Second)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLookupKeepsNonDumpableSameUser$")
	cmd.Env = append(os.Environ(), "BEACON_NONDUMPABLE=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	path := "/proc/" + strconv.Itoa(pid) + "/stat"
	deadline := time.Now().Add(2 * time.Second)
	for {
		var st unix.Stat_t
		if unix.Stat(path, &st) == nil && st.Uid == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stat did not become root-owned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p, ok := lookupProcess(pid)
	if !ok || p.pid != pid {
		t.Fatal("dropped a same-user non-dumpable process")
	}
}
