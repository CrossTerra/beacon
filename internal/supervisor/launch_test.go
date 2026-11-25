package supervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSilentBootstrapDeathAfterPermitIsNotExec(t *testing.T) {
	for _, exit := range []string{"kill -KILL $$", "exit 4", "exit 5", "exit 0"} {
		t.Run(exit, func(t *testing.T) { testSilentBootstrapDeath(t, exit) })
	}
}

func testSilentBootstrapDeath(t *testing.T, exit string) {
	t.Helper()
	gateR, gateW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gateR.Close()
	defer gateW.Close()
	statusR, statusW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer statusR.Close()
	defer statusW.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	defer readyW.Close()
	// This stand-in models an interrupted bootstrap: read the entire permit, then
	// die without exec or an error write. The kernel still closes its status fd.
	p, err := os.StartProcess("/bin/sh", []string{"sh", "-c", `printf '\001' >&6; exec 6>&-; while IFS= read -r line <&3; do :; done; ` + exit}, &os.ProcAttr{
		Env: []string{}, Files: []*os.File{nil, nil, nil, gateR, statusW, nil, readyW},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Kill(); _, _ = p.Wait() }()
	gateR.Close()
	statusW.Close()
	readyW.Close()
	j := job{pid: p.Pid, gate: gateW, payload: []byte(`{"permit":true}`), execStatus: statusR, ready: readyR, argv: []string{"/bin/cat"}}
	err = j.release(context.Background(), time.Time{})
	if j.events != nil {
		j.events.close()
	}
	if !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("silent bootstrap death reported as %v, want unconfirmed", err)
	}
}

func TestStagePromptReturnsCompleteReadOnlyInput(t *testing.T) {
	want := bytes.Repeat([]byte("approved bytes 👍\n"), 6400)
	f, err := stagePrompt(want)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil || st.Nlink != 0 || st.Size != int64(len(want)) {
		t.Fatalf("prompt was not fully staged in an anonymous file: %v", err)
	}
	if _, err := f.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("the handed-off prompt can be rewritten")
	}
	got, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("did not read the complete prompt from byte zero")
	}
}

func TestMain(m *testing.M) {
	if code, helper := HelperExit(os.Args[1:]); helper {
		os.Exit(code)
	}
	if len(os.Args) == 4 && os.Args[1] == "--fixture-exec-fds" {
		fd, err := strconv.Atoi(os.Args[2])
		if err != nil {
			os.Exit(1)
		}
		_, fdErr := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		// Keep the image observable until the reaper has seen it.
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
			if _, err := os.Stat(os.Args[3]); err == nil {
				if errors.Is(fdErr, unix.EBADF) {
					os.Exit(0)
				}
				os.Exit(2)
			}
			time.Sleep(5 * time.Millisecond)
		}
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestExecMarksUnrelatedInheritedDescriptorsCLOEXEC(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the bootstrap refuses root")
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	finish := filepath.Join(t.TempDir(), "finish")
	j, err := spawn(self, Command{Path: self, Args: []string{"--fixture-exec-fds", strconv.Itoa(fd), finish}}, sessionPrefix("fixture")+"1", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if j.pid != 0 {
			_ = unix.Kill(j.pid, unix.SIGKILL)
			_, _ = unix.Wait4(j.pid, nil, 0, nil)
		}
		if j.events != nil {
			j.events.close()
		}
	}()
	if err := j.release(t.Context(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finish, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(j.pid, &status, 0, nil); err != nil {
		t.Fatal(err)
	}
	j.pid = 0
	if !status.Exited() || status.ExitStatus() != 0 {
		t.Fatalf("target did not have a clean descriptor table (status %d)", status)
	}
}
