package runner

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/CrossTerra/beacon/internal/lease"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/signal"
)

// RetryReport is what an explicit retry of refused events did. The counts are the only thing logged.
type RetryReport struct {
	Sent     int
	Replayed int
	Refused  int
	// Conflict is rejected entries Signal answered with 409. They are not sent.
	Conflict int
	// Missing is rejected entries with no stored decision. They cannot be rebuilt.
	Missing int
	// Failed is entries that could not be projected. They stay rejected.
	Failed int
	// Held and Never are refusals the current rules no longer send. They were not sent.
	Held    int
	Never   int
	Pending int
	Lease   *lease.Status
	// LeaseReason is set when the lease could not be taken, or the pass stopped because it ran out.
	LeaseReason logs.Reason
}

// RetryRejected re-decides rejected events from the stored item and the current rules, and sends only what those
// rules still send. It never sends an entry Signal accepted, and it never touches a 409. A field leaves only when
// both the stored decision and the current rule list it. An entry the rules now hold or drop is recorded as
// superseded and not sent. An entry with no stored item is counted and left as it is. Sending still requires the
// lease. A body is appended and synced before it is sent; if the lease runs out after that, the body stays pending
// for the next pass, which sends those exact bytes.
func (r *Runner) RetryRejected(ctx context.Context) (*RetryReport, error) {
	rep := &RetryReport{}
	if r.Outbox == nil || r.Writer == nil || r.Lease == nil || r.Rules == nil {
		return rep, errors.New("runner: retrying needs an outbox, a writer, a lease and the rules")
	}
	st, err := r.Lease.Ensure(ctx)
	rep.Lease = &st
	if err != nil {
		rep.LeaseReason = Classify(err)
		r.Log.Log(logs.LeaseFailed, logs.Why(rep.LeaseReason))
		r.logRetry(rep)
		if !transient(err) || ctx.Err() != nil {
			return rep, err
		}
		if !st.Held {
			return rep, nil
		}
	}
	if !st.Held {
		r.Log.Log(logs.LeaseNotHeld, logs.Period(st.Period))
		r.logRetry(rep)
		return rep, nil
	}
	r.Log.Log(logs.LeaseHeld, logs.Period(st.Period), logs.Until(st.Until))
	view := &asks{ob: r.Outbox, added: map[string][]string{}, closed: map[string]bool{}}
	for _, e := range r.Outbox.Entries() {
		if e.Kind != outbox.Event || e.State != outbox.Rejected {
			continue
		}
		if e.Status == 409 || e.Code == "idempotency_conflict" {
			rep.Conflict++
			continue
		}
		if len(e.Decision) == 0 {
			rep.Missing++
			continue
		}
		if !r.Now().Before(st.Until) {
			r.Log.Log(logs.LeaseExpired, logs.Period(st.Period))
			rep.LeaseReason = logs.ReasonLeaseExpired
			break
		}
		_, body, tier, perr := r.reproject(e.Decision, view)
		if perr != nil {
			rep.Failed++
			continue
		}
		if tier != rules.Send {
			if err := r.Outbox.Supersede(e.Key, string(tier)); err != nil {
				r.logRetry(rep)
				return rep, err
			}
			if tier == rules.Hold {
				rep.Held++
			} else {
				rep.Never++
			}
			continue
		}
		if err := r.Outbox.Requeue(e.Key, body); err != nil {
			r.logRetry(rep)
			return rep, err
		}
		stopped, err := r.sendRequeued(ctx, st, e.Key, rep)
		if err != nil {
			r.logRetry(rep)
			return rep, err
		}
		if stopped {
			break
		}
	}
	rep.Pending = len(r.Outbox.PendingEvents())
	r.logRetry(rep)
	return rep, nil
}

func (r *Runner) logRetry(rep *RetryReport) {
	r.Log.Log(logs.Retry,
		logs.Count("sent", rep.Sent),
		logs.Count("replayed", rep.Replayed),
		logs.Count("refused", rep.Refused),
		logs.Count("conflict", rep.Conflict),
		logs.Count("missing", rep.Missing),
		logs.Count("failed", rep.Failed),
		logs.Count("hold", rep.Held),
		logs.Count("never", rep.Never),
		logs.Count("pending", rep.Pending),
	)
}

// reproject runs the current rules on the stored item. A send uses only fields both the stored decision and the
// current rule list. Hold and never are returned without a body.
func (r *Runner) reproject(raw []byte, view *asks) (*project.Body, []byte, rules.Tier, error) {
	var d project.Decision
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, nil, "", err
	}
	if r.Seed != nil && r.Seed.Drops(d.Item.Subject) {
		return nil, nil, rules.Never, nil
	}
	decided := r.Rules.Decide(d.Item)
	// A rule above the match that needs a field or actor the stored item lacks cannot be proved. Do not send
	// past it: a send rule below must not win because the higher hold or never could not see the field.
	if r.Rules.GapsAbove(d.Item, decided.Rule) {
		return nil, nil, rules.Never, nil
	}
	if decided.Tier != rules.Send {
		if decided.Tier == rules.Hold {
			return nil, nil, rules.Hold, nil
		}
		return nil, nil, rules.Never, nil
	}
	rule := d.Narrow(decided.Rule)
	if rule == nil {
		return nil, nil, rules.Never, nil
	}
	b, body, _, err := r.project(d.Item, rule, view)
	if err != nil {
		return nil, nil, "", err
	}
	return b, body, rules.Send, nil
}

// sendRequeued writes one requeued body and records Signal's answer. stopped is true when the pass should end and
// leave the body pending, matching a send pass that loses Signal or the lease.
func (r *Runner) sendRequeued(ctx context.Context, st lease.Status, key string, rep *RetryReport) (stopped bool, err error) {
	e, ok := r.Outbox.Get(key)
	if !ok || e.State != outbox.Pending {
		return false, errors.New("runner: requeued key is not pending")
	}
	wctx, cancel := context.WithTimeout(ctx, st.Until.Sub(r.Now()))
	res, err := r.Writer.Write(wctx, e.Body)
	cancel()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			rep.LeaseReason = logs.ReasonLeaseExpired
			r.Log.Log(logs.LeaseExpired, logs.Period(st.Period))
			return true, nil
		}
		r.Log.Log(logs.SendStopped, logs.Why(Classify(err)))
		if errors.Is(err, signal.ErrUnavailable) {
			rep.LeaseReason = logs.ReasonUnavailable
			return true, nil
		}
		return false, err
	}
	state := outbox.Rejected
	switch res.Outcome {
	case signal.Written:
		state = outbox.Sent
		rep.Sent++
	case signal.Replayed:
		state = outbox.Replayed
		rep.Replayed++
	default:
		rep.Refused++
	}
	return false, r.Outbox.Mark(key, state, res.EventID, res.Status, res.Code, res.Message)
}
