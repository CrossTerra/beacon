// Package supervisor manages the lifetime of jobs started by the Go service. A separate reaper
// owns the processes so it can finish cleanup even if the service cannot run any Go code.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Supervisor struct {
	life, input *os.File
	descriptors *os.File
	cmd         *exec.Cmd
	replies     chan message
	stopping    chan struct{}
	done        chan struct{}
	err         error // published by closing done
	stopOnce    sync.Once
	wireMu      sync.Mutex
	stopLimit   time.Duration
	next        atomic.Uint64
	runsMu      sync.Mutex
	runs        map[uint64]*Run
}

// Open starts the reaper and waits for the previous session's sweep. The service already owns
// instance.lock. The reaper inherits a duplicate of the *same* locked file description, so no new
// instance can race its cleanup after service death. No jobs start until StartFixed or PrepareRun.
func Open(ctx context.Context, dir string, lock *os.File, opts Options) (*Supervisor, error) {
	self, err := os.Executable()
	if err != nil || lock == nil || os.Geteuid() == 0 {
		return nil, ErrReaper
	}
	lifeIn, lifeOut, err := os.Pipe()
	if err != nil {
		return nil, ErrReaper
	}
	defer lifeIn.Close()
	inputR, inputW, err := os.Pipe()
	if err != nil {
		lifeOut.Close()
		return nil, ErrReaper
	}
	defer inputR.Close()
	replyR, replyW, err := os.Pipe()
	if err != nil {
		lifeOut.Close()
		inputW.Close()
		return nil, ErrReaper
	}
	defer replyW.Close()
	sendFD, recvFD, err := descriptorPair()
	if err != nil {
		lifeOut.Close()
		inputW.Close()
		replyR.Close()
		return nil, ErrReaper
	}
	defer recvFD.Close()
	cmd := exec.Command(self, "serve", "--reaper", dir)
	cmd.Env = []string{}
	cmd.Stdin, cmd.ExtraFiles = inputR, []*os.File{lock, lifeIn, replyW, recvFD}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		lifeOut.Close()
		inputW.Close()
		replyR.Close()
		sendFD.Close()
		return nil, ErrReaper
	}
	// No service-side writer of replies (otherwise EOF would not report reaper death).
	replyW.Close()
	recvFD.Close()
	p := periods{opts.Grace, opts.KillWait, opts.PrepareWait}
	t := p.timing()
	s := &Supervisor{life: lifeOut, input: inputW, descriptors: sendFD, cmd: cmd, stopLimit: t.grace + t.killWait + reaperSlack,
		replies: make(chan message, 1), stopping: make(chan struct{}), done: make(chan struct{}), runs: make(map[uint64]*Run)}
	go s.watch(replyR)
	if _, err := s.call(ctx, request{Op: "open", Periods: &p}, s.stopLimit); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

func (s *Supervisor) watch(reply *os.File) {
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(s.replies)
		defer reply.Close()
		scan := wireScanner(reply)
		for scan.Scan() {
			var m message
			if json.Unmarshal(scan.Bytes(), &m) != nil {
				break
			}
			if m.Finished != 0 {
				s.finishRun(m.Finished, m.Exit, m.err(), m.Cleaned)
				continue
			}
			if m.Exited != 0 {
				s.reportExit(m.Exited, LeaderExit{Code: m.Exit, Reason: m.Reason})
				continue
			}
			select {
			case s.replies <- m:
			default:
				s.BeginShutdown()
				return
			}
		}
		s.BeginShutdown()
	}()
	if err := s.cmd.Wait(); err != nil {
		s.err = ErrReaper
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == unconfirmed {
			s.err = ErrUnconfirmed
		}
	}
	<-readerDone // deliver completions written before clean exit, then fail any unresolved runs
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	for _, r := range s.runs {
		r.err = ErrUnconfirmed
		close(r.done)
	}
	clear(s.runs)
	close(s.done)
}

func (s *Supervisor) call(ctx context.Context, r request, wait time.Duration) (message, error) {
	b, err := json.Marshal(r)
	if err != nil || len(b) > maxFrame {
		return message{}, ErrLaunch
	}
	s.wireMu.Lock()
	defer s.wireMu.Unlock()
	select {
	case <-s.stopping:
		return message{}, ErrStopped
	case <-ctx.Done():
		return message{}, ctx.Err()
	default:
	}
	deadline := time.Now().Add(wait)
	if r.Op == "prepare" && r.Command != nil && r.Command.descriptorCount() != 0 {
		if err := sendCommandFiles(s.descriptors, r.Ticket, r.Command); err != nil {
			s.BeginShutdown()
			return message{}, ErrReaper
		}
	}
	if err := wireWrite(s.input, b, deadline); err != nil {
		s.BeginShutdown()
		return message{}, ErrReaper
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case m, ok := <-s.replies:
		if ok {
			return m, m.err()
		}
	case <-ctx.Done():
		s.BeginShutdown()
		return message{}, ctx.Err()
	case <-timer.C:
	}
	s.BeginShutdown() // an unanswered request cannot leave a reply for the next request
	return message{}, ErrReaper
}

func (s *Supervisor) request(r request) (message, error) {
	return s.call(context.Background(), r, wireTimeout+poll)
}

// StartFixed supplies the fixed collector and watch list once, after startup recovery.
func (s *Supervisor) StartFixed(jobs []FixedJob) error {
	_, err := s.request(request{Op: "fixed", Jobs: jobs})
	return err
}

// Attribute resolves a connect-time kernel PID. A failed lookup through the reaper isn't the
// unattributed identity: the socket must refuse that connection rather than lose run ownership.
func (s *Supervisor) Attribute(pid int) (string, error) {
	m, err := s.request(request{Op: "attribute", PID: pid})
	return m.RunID, err
}

// BeginShutdown closes the only writer of the lifetime pipe. No new run or restart is authorized.
// It does not wait for TERM/KILL cleanup, so the service can close its listeners in parallel.
func (s *Supervisor) BeginShutdown() {
	s.stopOnce.Do(func() {
		close(s.stopping)
		s.life.Close()
		s.input.Close()
		s.descriptors.Close()
	})
}

func (s *Supervisor) Done() <-chan struct{} { return s.done }

func (s *Supervisor) Ready() bool {
	select {
	case <-s.stopping:
		return false
	default:
		return true
	}
}

// Close waits for confirmation. If the reaper itself is stuck, leave the marker for the next start.
func (s *Supervisor) Close() error {
	s.BeginShutdown()
	timer := time.NewTimer(s.stopLimit)
	defer timer.Stop()
	select {
	case <-s.done:
		return s.err
	case <-timer.C:
		_ = s.cmd.Process.Kill()
		return ErrUnconfirmed
	}
}

// Run is a prepared process group, not yet an executed image. Its unique ticket survives a PID
// reuse: a late Release or Stop can never address another run. Wait confirms exit of the whole tree.
type Run struct {
	s          *Supervisor
	ticket     uint64
	pgid       int
	done       chan struct{}
	exit       int
	err        error
	cleaned    bool // published by closing done
	leaderDone chan struct{}
	leader     LeaderExit // published by closing leaderDone, before cleanup is acknowledged
}

type LeaderExit struct {
	Code   int
	Reason string // fixed stop code only if the reaper began stopping a live leader
}

// PrepareRun installs attribution before returning. A handler can durably record start using
// PGID(), then call Release. If it doesn't, the reaper times out and kills the bootstrap unopened.
func (s *Supervisor) PrepareRun(id string, c Command) (*Run, error) {
	r := &Run{s: s, ticket: s.next.Add(1), done: make(chan struct{}), leaderDone: make(chan struct{})}
	s.runsMu.Lock()
	select {
	case <-s.stopping:
		s.runsMu.Unlock()
		return nil, ErrStopped
	default:
	}
	s.runs[r.ticket] = r
	s.runsMu.Unlock()
	m, err := s.request(request{Op: "prepare", RunID: id, Ticket: r.ticket, Command: &c})
	if err != nil {
		s.finishRun(r.ticket, 0, err, false)
		return nil, err
	}
	r.pgid = m.PID
	return r, nil
}

func (s *Supervisor) reportExit(ticket uint64, exit LeaderExit) {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	if r := s.runs[ticket]; r != nil {
		select {
		case <-r.leaderDone:
		default:
			r.leader = exit
			close(r.leaderDone)
		}
	}
}

func (s *Supervisor) finishRun(ticket uint64, exit int, err error, cleaned bool) {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	if r := s.runs[ticket]; r != nil {
		r.exit, r.err, r.cleaned = exit, err, cleaned
		close(r.done)
		delete(s.runs, ticket)
	}
}

func (r *Run) PGID() int { return r.pgid }

func (r *Run) Done() <-chan struct{} { return r.done }

func (r *Run) CleanupConfirmed() bool {
	select {
	case <-r.done:
		return r.cleaned
	default:
		return false
	}
}

func (r *Run) LeaderDone() <-chan struct{} { return r.leaderDone }

func (r *Run) LeaderExit() (LeaderExit, bool) {
	select {
	case <-r.leaderDone:
		return r.leader, true
	default:
		return LeaderExit{}, false
	}
}

func (r *Run) Release() error { return r.ReleaseWithReceipt("") }

// ReleaseWithReceipt acknowledges exec only after the reaper syncs the matching receipt.
func (r *Run) ReleaseWithReceipt(permit string) error {
	// The wait is wireTimeout+poll. The reaper is told to finish a poll sooner, so the
	// exec read cannot outlast this request and the reply is still observable.
	req, wait := releaseRequest(r.ticket, time.Now())
	req.Receipt = permit
	_, err := r.s.call(context.Background(), req, wait)
	return err
}

func releaseRequest(ticket uint64, now time.Time) (request, time.Duration) {
	return request{Op: "release", Ticket: ticket, Until: now.Add(wireTimeout).UnixNano()}, wireTimeout + poll
}

func (r *Run) Stop() error { return r.StopWithReason("stopped") }

func (r *Run) StopWithReason(reason string) error {
	_, err := r.s.request(request{Op: "stop", Ticket: r.ticket, Reason: reason})
	return err
}

func (r *Run) Wait(ctx context.Context) (exit int, err error) {
	select {
	case <-r.done:
		return r.exit, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
