package supervisor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"golang.org/x/sys/unix"
)

// Command is built by a trusted caller after approval (or from a fixed job definition), never from
// a socket request. It inherits no environment or open file from the service. Unset stdio goes to
// /dev/null. A run's Input is sent over the private control channel and fed through an
// anonymous pipe. Output files are private and exclusive for runs; fixed jobs append.
// A fixed job instead gets a lifetime pipe: EOF means the reaper died.
type Command struct {
	Path           string
	Args, Env      []string
	Dir            *Directory
	Input          []byte `json:",omitempty"`
	Stdout, Stderr string
}

// Directory retains the open checkout from review to handoff. Its wire description is
// paired with the actual descriptor, never used to recreate a pin by opening a path.
type Directory struct {
	Path          string
	Device, Inode uint64
	file          *os.File
}

func PinDirectory(path string) (*Directory, error) {
	if !filepath.IsAbs(path) {
		return nil, ErrLaunch
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &Directory{path, deviceBits(st), st.Ino, os.NewFile(uintptr(fd), "checkout")}, nil
}

func (d *Directory) Close() error { return d.file.Close() }

func (d *Directory) Dup() (*Directory, error) {
	fd, err := unix.FcntlInt(d.file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &Directory{d.Path, d.Device, d.Inode, os.NewFile(uintptr(fd), "checkout")}, nil
}

// OpenAt lets review read the Git metadata relative to the retained checkout.
func (d *Directory) OpenAt(path string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(d.file.Fd()), path, flags|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "checkout metadata"), nil
}

func (d *Directory) MatchesPath() bool { return d.matches(int(d.file.Fd())) }

func (d *Directory) matches(fd int) bool {
	var held, named unix.Stat_t
	return filepath.IsAbs(d.Path) && unix.Fstat(fd, &held) == nil && held.Mode&unix.S_IFMT == unix.S_IFDIR &&
		deviceBits(held) == d.Device && held.Ino == d.Inode && unix.Stat(d.Path, &named) == nil &&
		deviceBits(named) == d.Device && named.Ino == d.Inode
}

type image struct {
	Path      string
	Args, Env []string
	Dir       *Directory
}

func (c Command) image(marker string) ([]byte, error) {
	if !filepath.IsAbs(c.Path) || strings.ContainsRune(c.Path, 0) {
		return nil, ErrLaunch
	}
	i := image{Path: c.Path, Args: append([]string{c.Path}, c.Args...), Dir: c.Dir}
	for _, arg := range i.Args {
		if strings.ContainsRune(arg, 0) {
			return nil, ErrLaunch
		}
	}
	for _, entry := range c.Env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsRune(entry, 0) {
			return nil, ErrLaunch
		}
		if name != markerKey && !(c.Dir != nil && (name == "PWD" || name == "OLDPWD")) {
			i.Env = append(i.Env, entry)
		}
	}
	i.Env = append(i.Env, marker)
	b, err := json.Marshal(i)
	if err != nil || len(b) > maxFrame {
		return nil, ErrLaunch
	}
	return b, nil
}

// spawn starts only our trusted bootstrap as group leader. The target image, arguments and env
// (including loader variables) do not reach it until release. EOF alone never authorizes exec.
func spawn(self string, c Command, marker string, fixed bool) (out *job, err error) {
	payload, err := c.image(marker)
	if err != nil {
		return nil, err
	}
	var dir *os.File
	if c.Dir != nil {
		if c.Dir.file == nil || !c.Dir.MatchesPath() {
			return nil, ErrCheckoutChanged
		}
		dir = c.Dir.file
	}

	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, ErrLaunch
	}
	defer null.Close()
	files := []*os.File{null, null, null}
	var life, stdin *os.File
	defer func() {
		if err != nil {
			closeLife(life)
			closeLife(stdin)
		}
	}()
	if fixed || len(c.Input) != 0 {
		// The collector treats stdin EOF as reaper death, the same way C tether does.
		// /dev/null would be an immediate EOF and make a tethered collector exit at start.
		readEnd, writeEnd, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, ErrLaunch
		}
		defer readEnd.Close()
		files[0] = readEnd
		if fixed {
			life = writeEnd
		} else {
			stdin = writeEnd
		}
	}
	output := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if fixed {
		output = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	for i, path := range []string{c.Stdout, c.Stderr} {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			return nil, ErrLaunch
		}
		f, err := approval.OpenPrivate(path, output)
		if err != nil {
			return nil, ErrLaunch
		}
		defer f.Close()
		files[i+1] = f
	}
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		return nil, ErrLaunch
	}
	defer r.Close()
	statusR, statusW, pipeErr := os.Pipe()
	if pipeErr != nil {
		w.Close()
		return nil, ErrLaunch
	}
	defer statusW.Close()
	readyR, readyW, pipeErr := os.Pipe()
	if pipeErr != nil {
		w.Close()
		statusR.Close()
		return nil, ErrLaunch
	}
	defer readyW.Close()
	// fd 3 is the gate, 4 the exec-status writer, 5 the optional pin, and 6 readiness.
	files = append(files, r, statusW, dir, readyW)
	p, err := os.StartProcess(self, []string{self, "serve", "--exec-job"}, &os.ProcAttr{
		Files: files, Env: []string{marker}, Sys: &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		w.Close()
		statusR.Close()
		readyR.Close()
		return nil, ErrLaunch
	}
	pid := p.Pid
	// Only the reaper calls wait4, after group cleanup. Release Go's optional pidfd, not the PID.
	p.Release()
	return &job{pid: pid, gate: w, payload: payload, life: life, stdin: stdin, input: c.Input, execStatus: statusR, ready: readyR}, nil
}

func closeLife(f *os.File) {
	if f != nil {
		f.Close()
	}
}

func (j *job) closeGate() {
	if j.gate != nil {
		j.gate.Close()
		j.gate = nil
	}
	j.payload = nil
	if j.execStatus != nil {
		j.execStatus.Close()
		j.execStatus = nil
	}
	if j.ready != nil {
		j.ready.Close()
		j.ready = nil
	}
}

func (j *job) release(ctx context.Context, until time.Time) error {
	defer j.closeGate()
	if err := ctx.Err(); err != nil {
		return ErrStopped
	}
	// One deadline for the permit and the exec result. A release request carries the
	// service's deadline, a poll inside its wait. Do not start a fresh timeout past that.
	deadline := releaseDeadline(time.Now(), until)
	if !time.Now().Before(deadline) {
		return ErrLaunch
	}
	// Observe exec only after the trusted bootstrap is executing; otherwise an
	// event for starting the bootstrap itself could be mistaken for the target.
	var ready [1]byte
	if j.ready == nil || j.ready.SetReadDeadline(deadline) != nil {
		return ErrLaunch
	}
	if _, err := io.ReadFull(j.ready, ready[:]); err != nil || ready[0] != 1 {
		return ErrLaunch
	}
	if ctx.Err() != nil || !time.Now().Before(deadline) {
		return ErrStopped
	}
	events, err := watchChild(j.pid)
	if err != nil {
		return ErrLaunch
	}
	j.events = events
	if err := j.gate.SetWriteDeadline(deadline); err != nil {
		return ErrLaunch
	}
	// An interrupted write cannot be confused with a complete permit. The bootstrap requires
	// one complete JSON frame ending in a newline. Closing on cancellation gives it EOF.
	gate := j.gate
	stop := context.AfterFunc(ctx, func() { gate.Close() })
	defer stop()
	if _, err := j.gate.Write(append(j.payload, '\n')); err != nil {
		return ErrLaunch
	}
	// The bootstrap reads until EOF. Close the permit before waiting for exec, or it
	// never reaches the status pipe. Drop the permit bytes either way.
	j.gate.Close()
	j.gate = nil
	j.payload = nil
	if j.stdin != nil {
		// The child may take time to read stdin. Do not block exec confirmation or
		// truncate input to the pipe capacity. Cleanup closes the pollable writer.
		stdin, input := j.stdin, j.input
		j.input = nil
		go func() {
			defer stdin.Close()
			defer clear(input)
			_, _ = stdin.Write(input)
		}()
	}
	return j.execResult(deadline)
}

// until is the caller's absolute deadline. Zero means no caller is waiting. The result is
// never later than wireTimeout from now, and never later than until, so a delayed start
// cannot extend the exec read past the request timeout.
func releaseDeadline(now, until time.Time) time.Time {
	deadline := now.Add(wireTimeout)
	if !until.IsZero() && until.Before(deadline) {
		deadline = until
	}
	return deadline
}

func (j *job) execResult(deadline time.Time) error {
	if j.execStatus == nil || !time.Now().Before(deadline) {
		return ErrUnconfirmed // the permit was sent; exec may have happened
	}
	if err := j.execStatus.SetReadDeadline(deadline); err != nil {
		return ErrUnconfirmed
	}
	var buf [4]byte
	n, err := io.ReadFull(j.execStatus, buf[:])
	if n == len(buf) {
		if binary.LittleEndian.Uint32(buf[:]) == uint32(unix.ESTALE) {
			return ErrCheckoutChanged
		}
		return ErrLaunch // the bootstrap explicitly refused
	}
	if errors.Is(err, io.EOF) && j.events != nil && j.events.confirmExec(deadline) {
		return nil // EOF alone could also mean the bootstrap died
	}
	return ErrUnconfirmed
}

func execJob() {
	status, err := inheritedPipe(4, "exec status")
	if err != nil {
		return
	}
	fail := func(err error) {
		var code uint32 = 1
		if errno, ok := err.(syscall.Errno); ok && errno != 0 {
			code = uint32(errno)
		}
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], code)
		_, _ = status.Write(buf[:])
	}
	gate, err := inheritedPipe(3, "job gate")
	if err != nil {
		fail(ErrLaunch)
		return
	}
	ready, err := inheritedPipe(6, "job ready")
	if err != nil {
		fail(ErrLaunch)
		return
	}
	_, err = ready.Write([]byte{1})
	ready.Close()
	if err != nil {
		fail(ErrLaunch)
		return
	}
	b, err := io.ReadAll(io.LimitReader(gate, maxFrame+2))
	gate.Close()
	if err != nil || len(b) == 0 || len(b) > maxFrame+1 || b[len(b)-1] != '\n' {
		fail(ErrStopped)
		return
	}
	var i image
	if json.Unmarshal(b, &i) != nil || !filepath.IsAbs(i.Path) || len(i.Args) == 0 {
		fail(ErrLaunch)
		return
	}
	if i.Dir != nil {
		unix.CloseOnExec(5)
		if unix.Fchdir(5) != nil || !i.Dir.matches(5) {
			fail(unix.ESTALE)
			return
		}
		i.Env = append(i.Env, "PWD="+i.Dir.Path)
	}
	// The status pipe is already close-on-exec. A failed exec writes a definite refusal;
	// the reaper checks kernel evidence before accepting a silent EOF.
	fail(unix.Exec(i.Path, i.Args, i.Env))
}
