package supervisor

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"golang.org/x/sys/unix"
)

const (
	poll     = 100 * time.Millisecond
	maxRuns  = 256
	maxFixed = 64
)

// FixedJob is a collector or a watch. A failed launch or nonzero exit restarts after cleanup;
// a clean exit does not. An agent run is never a fixed job.
type FixedJob struct{ Command Command }

type fixed struct {
	command Command
	job     *job
	retry   time.Time
	fails   uint
	retired bool
}

type job struct {
	pid, live     int
	tag, ticket   uint64
	runID         string
	fixed         *fixed
	gate          *os.File
	payload       []byte
	preparedUntil time.Time
	started       time.Time
	failure       string
	killAt        time.Time // nonzero means cleaning
	termGroup     bool
	exitSeen      bool // a previous census confirmed the leader had exited
	reported      bool
	life          *os.File // fixed-job stdin writer; closed when the reaper releases the job
	execStatus    *os.File
}

func (j *job) stop(now time.Time, timing timing, failure string) {
	if j.gate != nil {
		j.closeGate() // no permit, even if the bootstrap has not reached the pipe read
		if failure == "" {
			failure = codeStopped
		}
		j.killAt = now // the target has not run; don't give the bootstrap a grace period
	} else if j.killAt.IsZero() {
		j.killAt = now.Add(timing.grace)
	}
	if j.failure == "" {
		j.failure = failure
	}
}

type member struct {
	proc  process
	job   *job
	term  bool
	proof bool // group or ancestry, not just a marker; only these can confer run ownership
}

// The reaper is the sole parent and the sole waiter. All mutations, including launch permits and
// every transition of a job, happen on its event loop. In particular we never Wait before checking
// for the last member. The unreaped PID reserves the PGID even after the leader exits.
type engine struct {
	self, prefix string
	timing       timing
	nextTag      uint64
	jobs         map[int]*job // also indexed by the reserved process group ID
	runs         map[uint64]*job
	tracked      map[int]member
	fixed        []*fixed
	orphans      job // a shutdown sweep also cleans markers of retired jobs
	emit         func(message) error
	list         func() (processTable, error)
	signal       func(int, unix.Signal)
}

func newEngine(self, prefix string, t timing, emit func(message) error) *engine {
	return &engine{self: self, prefix: prefix, timing: t, emit: emit, jobs: make(map[int]*job), runs: make(map[uint64]*job),
		tracked: make(map[int]member), list: processes, signal: func(pid int, sig unix.Signal) { _ = unix.Kill(pid, sig) }}
}

func (e *engine) add(c Command, f *fixed) (*job, error) {
	e.nextTag++
	j, err := spawn(e.self, c, e.prefix+strconv.FormatUint(e.nextTag, 10), f != nil)
	if err != nil {
		return nil, err
	}
	j.tag, j.fixed = e.nextTag, f
	e.jobs[j.pid] = j
	return j, nil
}

func (e *engine) prepare(r request) message {
	if !approval.ValidID(r.RunID, "run") || r.Ticket == 0 || r.Command == nil || e.runs[r.Ticket] != nil {
		return message{Error: codeLaunch}
	}
	for _, j := range e.runs {
		if j.runID == r.RunID {
			return message{Error: codeBusy}
		}
	}
	if len(e.runs) >= maxRuns {
		return message{Error: codeBusy}
	}
	j, err := e.add(*r.Command, nil)
	if err != nil {
		if errors.Is(err, ErrCheckoutChanged) {
			return message{Error: codeCheckout}
		}
		return message{Error: codeLaunch}
	}
	j.ticket, j.runID = r.Ticket, r.RunID
	j.preparedUntil = time.Now().Add(e.timing.prepare)
	e.runs[j.ticket] = j // group ownership is installed BEFORE any permit can be issued
	return message{PID: j.pid}
}

func (e *engine) release(ctx context.Context, ticket uint64) message {
	j := e.runs[ticket]
	now := time.Now()
	if j == nil || j.gate == nil || !j.killAt.IsZero() {
		return message{Error: codeStopped}
	}
	if !now.Before(j.preparedUntil) {
		j.stop(now, e.timing, codeTimeout)
		return message{Error: codeTimeout}
	}
	if err := j.release(ctx); err != nil {
		j.killAt, j.failure = now, codeLaunch
		return message{Error: codeLaunch}
	}
	j.started = now
	return message{}
}

func (e *engine) attribute(pid int) (string, error) {
	// Use the current identity rather than a socket label or a stale PID in the tracker. A new
	// connection can arrive before the next tick, including from a child of a tracked parent.
	// An unreadable or vanished peer is not verified non-membership: the socket must reject it.
	for range 128 {
		p, ok := lookupProcess(pid)
		if !ok {
			return "", errUnreadablePeer
		}
		if p.zombie {
			return "", errUnreadablePeer // a dead peer is not the unattributed identity
		}
		if pid == os.Getpid() {
			return "", nil
		}
		if j := e.group(p); j != nil {
			return j.runID, nil
		}
		if m, ok := e.tracked[pid]; ok && m.proof && m.proc.birth == p.birth {
			return m.job.runID, nil
		}
		if p.parent <= 1 {
			return "", nil
		}
		parent, ok := lookupProcess(p.parent)
		// The peer itself was read and is not in a supervised group. An ancestor the
		// table will not show (another user's process, or one that has gone) ends the
		// walk; that is not an unread peer.
		if !ok || parent.birth > p.birth || parent.pid == pid {
			return "", nil
		}
		pid = parent.pid
	}
	return "", errUnreadablePeer
}

func (e *engine) group(p process) *job {
	if j := e.jobs[p.pid]; j != nil {
		return j
	}
	return e.jobs[p.group]
}

func (e *engine) track(table processTable, sweep bool) {
	for pid, m := range e.tracked {
		p, ok := table[pid]
		if ok && !p.zombie && p.birth == m.proc.birth {
			m.proc = p
			e.tracked[pid] = m
			continue
		}
		// A failed /proc status read omits a live process. Keep the last identity until
		// death or reuse is confirmed; an unconfirmed member blocks cleanup until the deadline.
		if processGone(pid, m.proc.birth) {
			delete(e.tracked, pid)
		}
	}
	for pid, p := range table {
		if p.zombie || pid == os.Getpid() {
			continue
		}
		if j := e.group(p); j != nil {
			m := e.tracked[pid]
			e.tracked[pid] = member{p, j, m.job == j && m.term, true}
		}
	}
	e.adoptChildren(table)
	cleaning := make(map[uint64]*job)
	for _, j := range e.jobs {
		if !j.killAt.IsZero() {
			cleaning[j.tag] = j
		}
	}
	if !sweep && len(cleaning) == 0 {
		return
	}
	// On recovery, a live marked group leader found in this snapshot also identifies the
	// same-user members of its group. Never send a negative-PID signal using such a snapshot:
	// we don't own these leaders and can't keep their PGIDs reserved.
	var anchors map[int]member
	for pid, p := range table {
		if p.zombie || pid == os.Getpid() || e.tracked[pid].job != nil {
			continue
		}
		tag := markerOf(pid, e.prefix)
		if j := cleaning[tag]; j != nil {
			e.tracked[pid] = member{proc: p, job: j}
		} else if sweep && tag != 0 {
			m := member{proc: p, job: &e.orphans}
			e.tracked[pid] = m
			if p.pid == p.group && len(e.jobs) == 0 {
				if anchors == nil {
					anchors = make(map[int]member)
				}
				anchors[p.group] = m
			}
		}
	}
	if len(anchors) != 0 {
		for pid, p := range table {
			if m, ok := anchors[p.group]; ok && !p.zombie && pid != os.Getpid() && e.tracked[pid].job == nil && m.proc.birth <= p.birth {
				e.tracked[pid] = member{proc: p, job: m.job}
			}
		}
	}
	e.adoptChildren(table)
}

func (e *engine) adoptChildren(table processTable) {
	for added := true; added; {
		added = false
		for pid, p := range table {
			if p.zombie || pid == os.Getpid() || e.tracked[pid].job != nil {
				continue
			}
			if parent, ok := e.tracked[p.parent]; ok && parent.proc.birth <= p.birth {
				e.tracked[pid] = member{proc: p, job: parent.job, proof: parent.proof}
				added = true
			}
		}
	}
}

func (e *engine) signalMembers(now time.Time) {
	for _, j := range e.jobs {
		j.live = 0
		if j.killAt.IsZero() {
			continue
		}
		if !now.Before(j.killAt) {
			e.signal(-j.pid, unix.SIGKILL)
		} else if !j.termGroup {
			e.signal(-j.pid, unix.SIGTERM)
		}
	}
	e.orphans.live = 0
	for pid, m := range e.tracked {
		j := m.job
		j.live++
		if j.killAt.IsZero() {
			continue
		}
		if !now.Before(j.killAt) {
			// Signal detached members individually as well as the pinned groups. Recheck birth
			// before sending a PID signal; the kernel can still reuse a PID after this check.
			if p, ok := lookupProcess(pid); ok && !p.zombie && p.birth == m.proc.birth && (p.group != j.pid || j.pid == 0) {
				e.signal(pid, unix.SIGKILL)
			}
		} else if !m.term {
			if p, ok := lookupProcess(pid); ok && !p.zombie && p.birth == m.proc.birth && (p.group != j.pid || j.termGroup) {
				e.signal(pid, unix.SIGTERM)
			}
			m.term = true
			e.tracked[pid] = m
		}
	}
	for _, j := range e.jobs {
		if !j.killAt.IsZero() {
			j.termGroup = true
		}
	}
}

func (e *engine) refresh(sweep bool) (bool, error) {
	now := time.Now()
	for _, j := range e.jobs {
		if j.gate != nil && !now.Before(j.preparedUntil) {
			j.stop(now, e.timing, codeTimeout)
		}
	}
	table, err := e.list()
	if err != nil {
		e.signalMembers(now) // group IDs are pinned even without a fresh process table
		for _, j := range e.jobs {
			if !j.killAt.IsZero() && !now.Before(j.killAt.Add(e.timing.killWait)) {
				return false, ErrUnconfirmed
			}
		}
		return false, nil // not an empty snapshot; no confirmation and no new starts
	}
	confirmed := make(map[int]bool, len(e.jobs))
	for _, j := range e.jobs {
		confirmed[j.pid] = j.exitSeen
		if !leaderDead(j.pid, table) {
			if j.killAt.IsZero() {
				j.exitSeen = false
			}
			continue
		}
		// Remember the exit for the next census. Reaping in this one can miss the child
		// the leader forked as it exited, or reap an exit that happened after waitid.
		j.exitSeen = true
		if j.killAt.IsZero() {
			j.stop(now, e.timing, "")
		}
	}
	e.track(table, sweep)
	e.signalMembers(now)
	var failure error
	for _, j := range e.jobs {
		if j.killAt.IsZero() {
			continue
		}
		if j.live == 0 {
			if !confirmed[j.pid] {
				continue // this census has not yet confirmed the exit
			}
			var status unix.WaitStatus
			if pid, err := unix.Wait4(j.pid, &status, unix.WNOHANG, nil); err == nil && pid == j.pid {
				if err := e.finished(j, status, now); err != nil && !sweep {
					failure = err // loss of the control channel ends the session
				}
				continue
			}
		}
		if !now.Before(j.killAt.Add(e.timing.killWait)) {
			failure = ErrUnconfirmed
		}
	}
	return true, failure
}

func (e *engine) finished(j *job, status unix.WaitStatus, now time.Time) error {
	delete(e.jobs, j.pid)
	j.closeLife()
	if f := j.fixed; f != nil {
		f.job = nil
		if status.Exited() && status.ExitStatus() == 0 && j.failure == "" {
			f.retired = true
		} else {
			if !j.started.IsZero() && now.Sub(j.started) > time.Minute {
				f.fails = 0
			}
			f.backoff(now)
		}
		return nil
	}
	delete(e.runs, j.ticket)
	if j.reported || e.emit == nil {
		return nil
	}
	code := status.ExitStatus()
	if status.Signaled() {
		code = 128 + int(status.Signal())
	}
	if e.emit == nil {
		return nil
	}
	if err := e.emit(message{Finished: j.ticket, Exit: code, Error: j.failure}); err != nil {
		e.emit = nil // at most one reply deadline, not one per finished run, during cleanup
		return err
	}
	return nil
}

func (e *engine) orphanWork() bool {
	if !e.orphans.killAt.IsZero() {
		return true
	}
	for _, m := range e.tracked {
		if m.job == &e.orphans {
			return true
		}
	}
	return false
}

func (e *engine) reportExpired(now time.Time) bool {
	gaveUp := false
	for _, j := range e.jobs {
		if j.ticket == 0 || j.reported || j.killAt.IsZero() || now.Before(j.killAt.Add(e.timing.killWait)) {
			continue
		}
		j.reported = true
		gaveUp = true
		if j.failure == "" {
			j.failure = codeRecovery
		}
		if e.emit == nil {
			continue
		}
		if err := e.emit(message{Finished: j.ticket, Error: j.failure}); err != nil {
			e.emit = nil // one failed reply write; do not keep notifying on a dead pipe
		}
	}
	return gaveUp
}

func (j *job) closeLife() {
	if j.life != nil {
		j.life.Close()
		j.life = nil
	}
}

func (f *fixed) backoff(now time.Time) {
	f.retry = now.Add(min(time.Second<<f.fails, 30*time.Second))
	f.fails = min(f.fails+1, 5)
}

func (e *engine) startFixed(ctx context.Context, live func() bool) {
	for _, f := range e.fixed {
		if !live() {
			return
		}
		now := time.Now()
		if f.retired || f.job != nil || now.Before(f.retry) {
			continue
		}
		j, err := e.add(f.command, f)
		if err != nil {
			f.backoff(now)
			continue
		}
		f.job = j
		if !live() {
			return // still gated; drain will kill it without an exec permit
		}
		if err := j.release(ctx); err != nil {
			j.killAt, j.failure = now, codeLaunch
		} else {
			j.started = now
		}
	}
}

// drain stops all groups concurrently. On startup there are no owned groups to signal by PGID;
// only the previous session's marker and ancestry are trusted (never numeric IDs from disk).
// A job that already used its give-up deadline is reported immediately. The rest keep the
// deadline they already have; drain does not grant another full grace.
func (e *engine) drain(ctx context.Context) error {
	now := time.Now()
	gaveUp := e.reportExpired(now)
	for _, j := range e.jobs {
		if j.killAt.IsZero() {
			j.stop(now, e.timing, "")
		}
	}
	// A startup sweep has no owned groups. Arm the orphan bucket so markers found in the
	// first census are signaled. An already-expired run does not get that grace added:
	// its report has already been sent, and an empty orphan record must not delay it.
	if e.orphans.killAt.IsZero() && (!gaveUp || len(e.jobs) == 0 || e.orphanWork()) {
		e.orphans.stop(now, e.timing, "")
	}
	deadline := now
	consider := func(j *job) {
		if j.killAt.IsZero() {
			return
		}
		if end := j.killAt.Add(e.timing.killWait); end.After(deadline) {
			deadline = end
		}
	}
	for _, j := range e.jobs {
		consider(j)
	}
	if !gaveUp || len(e.jobs) == 0 || e.orphanWork() {
		consider(&e.orphans)
	}
	if gaveUp && !deadline.After(now) {
		return ErrUnconfirmed
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	empty := 0
	for {
		if e.reportExpired(time.Now()) {
			gaveUp = true
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return ErrUnconfirmed
		}
		// One empty census can miss a child that has not appeared yet. Replace the session
		// only after a second fresh census is also empty. Any expiry keeps the marker.
		fresh, _ := e.refresh(true)
		if fresh && len(e.jobs) == 0 && e.orphans.live == 0 {
			empty++
			if empty >= 2 {
				if gaveUp {
					return ErrUnconfirmed
				}
				return nil
			}
		} else {
			empty = 0
		}
		select {
		case <-ctx.Done():
			return ErrUnconfirmed
		case <-ticker.C:
		}
	}
}
