package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"golang.org/x/sys/unix"
)

// HelperExit is called before the CLI (and by test binaries) to enter one of two private exec modes.
// They require the appropriate inherited descriptors, not the person's config or environment.
func HelperExit(args []string) (int, bool) {
	if len(args) < 2 || args[0] != "serve" {
		return 0, false
	}
	switch args[1] {
	case "--reaper":
		if len(args) != 3 || os.Geteuid() == 0 {
			return 2, true
		}
		if err := reap(args[2]); err != nil {
			if errors.Is(err, ErrUnconfirmed) {
				return unconfirmed, true
			}
			return 1, true
		}
		return 0, true
	case "--exec-job":
		if len(args) != 2 || os.Geteuid() == 0 {
			return 2, true
		}
		execJob()
		return bootstrapFailed, true // successful exec never returns to this image
	default:
		return 0, false
	}
}

func reap(dir string) error {
	// launchd can clean the service's group, not this one. systemd sends TERM to the entire
	// cgroup; the reaper stays alive for the pipe's EOF in either case. Catch (don't ignore) these
	// signals so that exec resets their dispositions for the actual jobs. In particular a broken
	// reply pipe must never SIGPIPE the reaper before it has stopped the groups.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, unix.SIGTERM, unix.SIGINT, unix.SIGPIPE)
	defer signal.Stop(signals)
	if unix.Getpgrp() != os.Getpid() || !filepath.IsAbs(dir) {
		return ErrReaper
	}
	lock := os.NewFile(3, "instance lock")
	defer lock.Close()
	unix.CloseOnExec(3)
	if err := checkLock(dir, lock); err != nil {
		return err
	}
	life, err := inheritedPipe(4, "lifetime pipe")
	if err != nil {
		return err
	}
	defer life.Close()
	if st, err := os.Stdin.Stat(); err != nil || st.Mode()&os.ModeNamedPipe == 0 {
		return ErrReaper
	}
	// Inherited pipes are blocking by default. Make a pollable reply fd for write deadlines.
	if unix.SetNonblock(5, true) != nil {
		return ErrReaper
	}
	reply, err := inheritedPipe(5, "reply pipe")
	if err != nil {
		return err
	}
	defer reply.Close()
	descriptors := os.NewFile(6, "checkout receiver")
	defer descriptors.Close()
	if kind, err := unix.GetsockoptInt(6, unix.SOL_SOCKET, unix.SO_TYPE); err != nil || kind != unix.SOCK_DGRAM {
		return ErrReaper
	}
	unix.CloseOnExec(6)
	return reaperLoop(dir, lock, life, os.Stdin, reply, descriptors)
}

func checkLock(dir string, lock *os.File) error {
	if _, err := approval.CheckPrivate(dir, os.ModeDir|0o700); err != nil {
		return ErrReaper
	}
	expected, err := approval.CheckPrivate(filepath.Join(dir, "instance.lock"), 0o600)
	if err != nil {
		return ErrReaper
	}
	actual, err := lock.Stat()
	if err != nil || !os.SameFile(expected, actual) || unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return ErrReaper
	}
	return nil
}

func reaperLoop(dir string, lock, life, input, reply, descriptors *os.File) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lifeFD := int32(life.Fd())
	live := func() bool {
		if ctx.Err() != nil {
			return false
		}
		// Also check synchronously before executing a queued permit or restart. EOF can be
		// visible to the kernel before the reader goroutine has been scheduled.
		fd := [1]unix.PollFd{{Fd: lifeFD, Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(fd[:], 0)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if n != 0 || err != nil {
				cancel()
				return false
			}
			return true
		}
	}
	go func() {
		var b [1]byte
		_, _ = life.Read(b[:]) // never a byte written; EOF is independent of pending commands
		cancel()
	}()
	requests := make(chan request)
	go func() {
		defer cancel()
		s := wireScanner(input)
		for s.Scan() {
			var r request
			if json.Unmarshal(s.Bytes(), &r) != nil {
				return
			}
			select {
			case requests <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	emit := func(m message) error {
		b, _ := json.Marshal(m) // only simple values, never errors or spec strings
		return wireWrite(reply, b, time.Now().Add(wireTimeout))
	}
	var t timing
	select {
	case <-ctx.Done():
		return nil // no commands accepted and no new session created
	case r := <-requests:
		if r.Op != "open" || r.Periods == nil {
			return ErrReaper
		}
		t = r.Periods.timing()
	}
	// Finish the bounded sweep even if the recovering service dies during TERM grace.
	// Service liveness is checked before a new session is opened, not before KILL.
	if err := sweepPrevious(dir, lock, t); err != nil {
		_ = emit(message{Error: codeRecovery})
		return err
	}
	if !live() {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return ErrReaper
	}
	prefix, err := newSession(dir, lock)
	if err != nil {
		_ = emit(message{Error: codeSession})
		return err
	}
	e := newEngine(self, prefix, t, emit)
	e.recordExec = func(id string, pid int, permit string) error { return approval.PersistRunExec(dir, id, pid, permit) }
	if emit(message{}) == nil {
		err = e.serve(ctx, requests, live, descriptors)
	} else {
		err = ErrReaper
	}
	if e.nextTag != 0 {
		if drainErr := e.drain(context.Background()); drainErr != nil {
			return drainErr // marker stays on disk for the next start
		}
	}
	return errors.Join(err, clearSession(dir))
}

// sweepPrevious drains a prior session to its own deadline. It does not take the service
// lifetime: cancelling that context must not abandon TERM before KILL.
func sweepPrevious(dir string, lock *os.File, t timing) error {
	old, err := oldSession(dir, lock)
	if err != nil || old == "" {
		return err
	}
	return newEngine("", old, t, nil).drain(context.Background())
}

func (e *engine) serve(ctx context.Context, requests <-chan request, live func() bool, descriptors *os.File) error {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	fixedSet := false
	for live() {
		select {
		case <-ctx.Done():
			return nil
		case r := <-requests:
			if !live() {
				return nil
			}
			var m message
			switch r.Op {
			case "fixed":
				if fixedSet || len(r.Jobs) > maxFixed {
					m.Error = codeLaunch
					break
				}
				for _, j := range r.Jobs {
					e.fixed = append(e.fixed, &fixed{command: j.Command})
				}
				fixedSet = true
			case "prepare":
				m = e.prepareWithDirectory(r, descriptors)
				if m.Error == codeReaper {
					return ErrReaper // cannot keep reading after losing descriptor/frame synchronization
				}
			case "release":
				var until time.Time
				if r.Until != 0 {
					until = time.Unix(0, r.Until)
				}
				m = e.release(ctx, r.Ticket, until, r.Receipt)
			case "stop":
				if j := e.runs[r.Ticket]; j != nil {
					if err := e.observeExit(j); err != nil {
						return err
					}
					if r.Reason != "output_too_large" {
						r.Reason = "stopped"
					}
					j.stopRun(time.Now(), e.timing, r.Reason)
				}
			case "attribute":
				id, attrErr := e.attribute(r.PID)
				m.RunID = id
				if attrErr != nil {
					m.Error = codeAttribute
				}
			default:
				return ErrReaper
			}
			if err := e.emit(m); err != nil {
				return ErrReaper
			}
		case <-ticker.C:
			if len(e.jobs) == 0 && len(e.fixed) == 0 {
				continue
			}
			fresh, err := e.refresh(false)
			if err != nil {
				return err
			}
			if fresh {
				e.startFixed(ctx, live)
			}
		}
	}
	return nil
}

func (e *engine) prepareWithDirectory(r request, descriptors *os.File) message {
	if r.Command != nil && r.Command.Dir != nil {
		file, err := receiveDirectory(descriptors, r.Ticket)
		if err != nil {
			return message{Error: codeReaper}
		}
		defer file.Close() // spawn passes its own reference to the gated bootstrap
		r.Command.Dir.file = file
	}
	return e.prepare(r)
}
