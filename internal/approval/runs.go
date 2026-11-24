package approval

import "time"

type RunnerKind string

const (
	RunnerWorking  RunnerKind = "runner_working"
	RunnerWaiting  RunnerKind = "runner_waiting"
	RunnerStalled  RunnerKind = "runner_stalled"
	RunnerFinished RunnerKind = "runner_finished"
	RunnerFailed   RunnerKind = "runner_failed"
	RunnerClosed   RunnerKind = "runner_closed"
)

func (k RunnerKind) terminal() bool { return k == RunnerFinished || k == RunnerFailed }

// RunnerEvent follows a confirmed launch. Text is untrusted runner output, not an instruction.
type RunnerEvent struct {
	Kind  RunnerKind `json:"kind"`
	Text  string     `json:"text,omitempty"`
	Error string     `json:"error,omitempty"`
}

func (r RunnerEvent) check() error {
	words := field{max: maxText}
	switch r.Kind {
	case RunnerWorking, RunnerClosed:
		if r.Text == "" && r.Error == "" {
			return nil
		}
	case RunnerWaiting, RunnerStalled, RunnerFinished:
		if r.Error == "" && words.matches(r.Text) {
			return nil
		}
	case RunnerFailed:
		if statusCode.matches(r.Error) && (r.Text == "" || words.matches(r.Text)) {
			return nil
		}
	}
	return Invalid("/runner")
}

func runnerTransition(s *proposalState, kind RunnerKind) bool {
	return s != nil && s.Type == Run && s.Status == "executed" && ValidID(s.RunnerID, "run") &&
		s.RunnerState != RunnerClosed && (kind == RunnerClosed) == s.RunnerState.terminal()
}

// ClaimRunDispatch records the attribution installed by PrepareRun and the one-shot permit to
// release it. No target image may execute until this local frame is durable. Recovery never uses
// the PID as authority to signal or as evidence that the image actually executed.
func (j *Journal) ClaimRunDispatch(id string, pgid int, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.healthy() {
		return ErrJournalUnhealthy
	}
	if j.stopping {
		return ErrStopping
	}
	s := j.states[id]
	if !j.dispatchable(s) || s.Type != Run || pgid <= 1 {
		return ErrTransition
	}
	return j.commit(frame{Kind: "run_permit", ID: id, PGID: pgid, At: at.UnixMilli()})
}

func (j *Journal) RecordRunner(id string, event RunnerEvent, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.recordRunner(j.states[id], event, at)
}

func (j *Journal) recordRunner(s *proposalState, event RunnerEvent, at time.Time) error {
	if err := event.check(); err != nil {
		return err
	}
	if !runnerTransition(s, event.Kind) {
		return ErrTransition
	}
	return j.action(frame{ID: s.ID, Kind: string(event.Kind), Runner: &event}, at)
}

// RecoverRuns is called only after the reaper's startup sweep. A missing permit proves a target
// never ran. A permit alone does not prove launch, and an unknown launch is never dispatched again.
func (j *Journal) RecoverRuns(at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range j.order {
		if s.Type != Run {
			continue
		}
		switch {
		case s.Status == "outcome_unknown" && s.RunPGID == 0:
			if err := j.action(frame{ID: s.ID, Kind: "failed", Result: &Result{ConfirmedBy: "read_back", Error: "not_launched"}}, at); err != nil {
				return err
			}
		case s.Status == "executed" && s.RunnerState != RunnerClosed:
			if !s.RunnerState.terminal() {
				if err := j.recordRunner(s, RunnerEvent{Kind: RunnerFailed, Error: "interrupted"}, at); err != nil {
					return err
				}
			}
			if err := j.recordRunner(s, RunnerEvent{Kind: RunnerClosed}, at); err != nil {
				return err
			}
		}
	}
	return nil
}
