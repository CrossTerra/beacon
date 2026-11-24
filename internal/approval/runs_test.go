package approval

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunPermitAndLifecycleReplay(t *testing.T) {
	dir := scratch(t)
	j, err := OpenJournal(dir, t0, "")
	must(t, err)
	t.Cleanup(func() { j.Close() })
	start := func() string {
		p, err := j.Add(Request{Type: Run, Agent: "fixture", Reason: "fixture", Params: Object{
			"checkout": "repo", "prompt": "fixture", "model": "test/model",
		}}, PeerLabel{}, t0)
		must(t, err)
		f, err := Freeze(p, Binding{Account: "linux:user:fixture", Destination: Object{"checkout_id": "vol:1/inode:2", "branch": "main"}}, "")
		must(t, err)
		must(t, j.ApproveAll([]Frozen{f}, []byte("fixture-asserted"), nil, t0))
		_, err = j.StartAsserted(p.ID, t0)
		must(t, err)
		return p.ID
	}
	unreleased, uncertain, launched, lostReply := start(), start(), start(), start()
	if j.ClaimDispatch(uncertain) {
		t.Fatal("a run used the provider's non-durable claim")
	}
	for _, id := range []string{uncertain, launched, lostReply} {
		permit, err := j.ClaimRunDispatch(id, 42, t0)
		must(t, err)
		if _, err := j.ClaimRunDispatch(id, 43, t0); !errors.Is(err, ErrTransition) {
			t.Fatalf("second permit: %v", err)
		}
		if id == lostReply || id == uncertain {
			v, _, err := j.Get(id, t0)
			must(t, err)
			must(t, os.MkdirAll(filepath.Join(dir, "runs", v.RunnerID), 0o700))
			if id == uncertain {
				permit = nonce() // a receipt for a different permit tells us nothing
			}
			must(t, PersistRunExec(dir, v.RunnerID, 42, permit))
		}
	}
	if err := j.RecordRunner(uncertain, RunnerEvent{Kind: RunnerFinished, Text: "report"}, t0); !errors.Is(err, ErrTransition) {
		t.Fatalf("report before confirmed launch: %v", err)
	}
	must(t, j.SetResult(launched, Result{ConfirmedBy: "reply"}, t0))
	if err := j.RecordRunner(launched, RunnerEvent{Kind: RunnerClosed}, t0); !errors.Is(err, ErrTransition) {
		t.Fatalf("closed without outcome: %v", err)
	}
	must(t, j.RecordRunner(launched, RunnerEvent{Kind: RunnerWorking}, t0))
	must(t, j.Close())
	at := t0.Add(time.Second)
	j, err = OpenJournal(dir, at, "")
	must(t, err)
	must(t, j.RecoverRuns(at))
	for id, want := range map[string]string{unreleased: "failed", uncertain: "outcome_unknown", launched: "executed", lostReply: "executed"} {
		v, _, err := j.Get(id, at)
		must(t, err)
		if v.Status != want || j.MayDispatch(id) {
			t.Fatalf("recovered %s, want %s and no dispatch", v.Status, want)
		}
		if (id == launched || id == lostReply) && (v.RunnerState != RunnerClosed || v.RunOutcome.Error != "interrupted") {
			t.Fatalf("recovered runner: %+v", v.RunOutcome)
		}
		if id == lostReply && (v.Outcome == nil || v.Outcome.ConfirmedBy != "read_back") {
			t.Fatal("the durable receipt did not settle the lost reply by read-back")
		}
	}
	if err := j.RecordRunner(launched, RunnerEvent{Kind: RunnerWorking}, at); !errors.Is(err, ErrTransition) {
		t.Fatalf("changed a closed run: %v", err)
	}
}
