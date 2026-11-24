package supervisor

import (
	"context"
	"encoding/json"
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
// /dev/null; supplied paths name private files. Fixed jobs append, and runs create output exclusively.
type Command struct {
	Path                  string
	Args, Env             []string
	Dir                   *Directory
	Stdin, Stdout, Stderr string
}

// Directory pins the checkout that was approved. Launch opens it once, checks the pin and enters
// it by descriptor in the child, so renaming or replacing the pathname cannot redirect the run.
type Directory struct {
	Path          string
	Device, Inode uint64
}

func PinDirectory(path string) (Directory, error) {
	f, st, err := openDirectory(path)
	if err != nil {
		return Directory{}, err
	}
	f.Close()
	return Directory{path, deviceBits(st), st.Ino}, nil
}

func openDirectory(path string) (*os.File, unix.Stat_t, error) {
	var st unix.Stat_t
	if !filepath.IsAbs(path) {
		return nil, st, ErrLaunch
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, st, err
	}
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, st, err
	}
	return os.NewFile(uintptr(fd), "checkout"), st, nil
}

type image struct {
	Path      string
	Args, Env []string
	Chdir     bool
}

func (c Command) image(marker string) ([]byte, error) {
	if !filepath.IsAbs(c.Path) || strings.ContainsRune(c.Path, 0) {
		return nil, ErrLaunch
	}
	i := image{Path: c.Path, Args: append([]string{c.Path}, c.Args...), Chdir: c.Dir != nil}
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
	if c.Dir != nil {
		i.Env = append(i.Env, "PWD="+c.Dir.Path)
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
func spawn(self string, c Command, marker string, fixed bool) (*job, error) {
	payload, err := c.image(marker)
	if err != nil {
		return nil, err
	}
	var dir *os.File
	if c.Dir != nil {
		f, st, err := openDirectory(c.Dir.Path)
		if err != nil {
			return nil, ErrLaunch
		}
		defer f.Close()
		if deviceBits(st) != c.Dir.Device || st.Ino != c.Dir.Inode {
			return nil, ErrCheckoutChanged
		}
		dir = f
	}

	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, ErrLaunch
	}
	defer null.Close()
	files := []*os.File{null, null, null}
	output := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if fixed {
		output = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	for i, path := range []string{c.Stdin, c.Stdout, c.Stderr} {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			return nil, ErrLaunch
		}
		flags := output
		if i == 0 {
			flags = os.O_RDONLY
		}
		f, err := approval.OpenPrivate(path, flags)
		if err != nil {
			return nil, ErrLaunch
		}
		defer f.Close()
		files[i] = f
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, ErrLaunch
	}
	defer r.Close()
	files = append(files, r) // fd 3: gate
	if dir != nil {
		files = append(files, dir) // fd 4: pinned directory
	}
	p, err := os.StartProcess(self, []string{self, "serve", "--exec-job"}, &os.ProcAttr{
		Files: files, Env: []string{marker}, Sys: &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		w.Close()
		return nil, ErrLaunch
	}
	pid := p.Pid
	// Only the reaper calls wait4, after group cleanup. Release Go's optional pidfd, not the PID.
	p.Release()
	return &job{pid: pid, gate: w, payload: payload}, nil
}

func (j *job) closeGate() {
	if j.gate != nil {
		j.gate.Close()
		j.gate, j.payload = nil, nil
	}
}

func (j *job) release(ctx context.Context) error {
	defer j.closeGate()
	if err := ctx.Err(); err != nil {
		return ErrStopped
	}
	if err := j.gate.SetWriteDeadline(time.Now().Add(wireTimeout)); err != nil {
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
	return nil
}

func execJob() error {
	gate, err := inheritedPipe(3, "job gate")
	if err != nil {
		return ErrLaunch
	}
	b, err := io.ReadAll(io.LimitReader(gate, maxFrame+2))
	gate.Close()
	if err != nil || len(b) == 0 || len(b) > maxFrame+1 || b[len(b)-1] != '\n' {
		return ErrStopped
	}
	var i image
	if json.Unmarshal(b, &i) != nil || !filepath.IsAbs(i.Path) || len(i.Args) == 0 {
		return ErrLaunch
	}
	if i.Chdir {
		err := unix.Fchdir(4)
		unix.Close(4)
		if err != nil {
			return ErrLaunch
		}
	}
	_ = unix.Exec(i.Path, i.Args, i.Env)
	return ErrLaunch
}
