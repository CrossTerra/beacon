// Package runner is the path from a collected item to the network: rules, projection, outbox, lease, send. It is
// the only code that sends item content, and it sends only bodies the projection built from a send rule.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/lease"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/queue"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/source"
)

// Runner runs one pass at a time.
type Runner struct {
	Sources []source.Source
	Rules   *rules.File
	Person  project.Person
	// Send is false for a dry run: nothing is written to the state directory and nothing is sent.
	Send bool
	// Outbox is the outbox. A dry run may pass one opened with outbox.Read, or nil.
	Outbox *outbox.Outbox
	// Queue is the hold queue and dropped record. Unused in a dry run.
	Queue *queue.Queue
	// Writer and Lease are used only when Send is true.
	Writer lease.Writer
	Lease  *lease.Lease
	Log    *logs.Logger
	Now    func() time.Time

	// AfterAppend, when set, is called after a body is appended and synced, before it is sent. Tests use it to stop
	// the process at that point.
	AfterAppend func(key string)
}

// Planned is what a pass decided for one item.
type Planned struct {
	Item source.Item
	Tier rules.Tier
	// Rule is the matching rule's name, or "" for the default.
	Rule string
	// Body is the request body the item would be, or was, sent as: for send and hold items.
	Body []byte
	// Skipped says why an item was not decided again: it is already in the outbox or the hold queue.
	Skipped string
	// Err is a projection error; such an item is neither appended nor sent.
	Err error
}

// Sent is Signal's answer for one outbox key in this pass.
type Sent struct {
	Key     string
	Outcome signal.Outcome
	Status  int
	Code    string
	EventID string
}

// Report is the result of one pass.
type Report struct {
	Send    bool
	Items   []Planned
	Lease   *lease.Status
	Results []Sent
	// LeaseReason says why the lease could not be checked, when it could not; empty when Signal answered.
	LeaseReason logs.Reason
	// Pending is how many event bodies are still waiting to be sent after the pass. Lease claims are not counted.
	Pending int
}

// Counts returns how many items landed in each tier, and how many were skipped.
func (r *Report) Counts() (send, hold, never, skipped int) {
	for _, p := range r.Items {
		switch {
		case p.Skipped != "":
			skipped++
		case p.Tier == rules.Send:
			send++
		case p.Tier == rules.Hold:
			hold++
		default:
			never++
		}
	}
	return
}

// Collect gathers items from every source, validated and sorted by when they happened, then by key.
func (r *Runner) Collect(ctx context.Context) []source.Item {
	var items []source.Item
	for _, s := range r.Sources {
		got, err := s.Collect(ctx)
		if err != nil {
			reason := logs.ReasonSource
			if Classify(err) == logs.ReasonEgress {
				reason = logs.ReasonEgress
			}
			r.Log.Log(logs.CollectFailed, logs.Source(s.Name()), logs.Why(reason))
			continue
		}
		fields, stateFields := s.Fields(), s.StateFields()
		invalid := 0
		for _, it := range got {
			if it.Source != s.Name() || source.Validate(it, fields, stateFields) != nil {
				invalid++
				continue
			}
			items = append(items, it)
		}
		if invalid > 0 {
			r.Log.Log(logs.ItemInvalid, logs.Source(s.Name()), logs.Count("items", invalid))
		}
		r.Log.Log(logs.Collected, logs.Source(s.Name()), logs.Count("items", len(got)-invalid))
	}
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].OccurredAt.Equal(items[j].OccurredAt) {
			return items[i].OccurredAt.Before(items[j].OccurredAt)
		}
		return items[i].Key < items[j].Key
	})
	return items
}

// asks tracks the person's open asks per subject: the outbox's, plus those a dry run would add.
type asks struct {
	ob     *outbox.Outbox
	added  map[string][]string
	closed map[string]bool
}

func (a *asks) open(subject string) []string {
	var base []string
	if a.ob != nil {
		base = a.ob.OpenAsks(subject)
	}
	var out []string
	for _, k := range append(base, a.added[subject]...) {
		if !a.closed[k] {
			out = append(out, k)
		}
	}
	return out
}

func (a *asks) record(subject, key string, b *project.Body) {
	if project.AsksOf(b) {
		a.added[subject] = append(a.added[subject], key)
	}
	for _, k := range project.ClosesOf(b) {
		a.closed[k] = true
	}
}

// Run makes one pass: collect, decide, record, and, when sending, send while this instance holds the lease.
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	mode := "dry-run"
	if r.Send {
		mode = "send"
		if r.Outbox == nil || r.Queue == nil || r.Writer == nil || r.Lease == nil {
			return nil, errors.New("runner: sending needs an outbox, a queue, a writer and a lease")
		}
	}
	r.Log.Log(logs.RunStarted, logs.Mode(mode))
	rep := &Report{Send: r.Send}
	view := &asks{ob: r.Outbox, added: map[string][]string{}, closed: map[string]bool{}}

	for _, it := range r.Collect(ctx) {
		p, err := r.decide(it, view)
		if err != nil {
			return rep, err
		}
		rep.Items = append(rep.Items, p)
	}
	s, h, n, k := rep.Counts()
	r.Log.Log(logs.Decided, logs.Count("send", s), logs.Count("hold", h), logs.Count("never", n), logs.Count("skipped", k))

	if !r.Send {
		r.Log.Log(logs.DryRunNoSending)
		r.Log.Log(logs.RunFinished, logs.Mode(mode))
		return rep, nil
	}
	err := r.sendPending(ctx, rep)
	rep.Pending = len(r.Outbox.PendingEvents())
	r.Log.Log(logs.RunFinished, logs.Mode(mode), logs.Count("results", len(rep.Results)), logs.Count("pending", rep.Pending))
	return rep, err
}

func (r *Runner) decide(it source.Item, view *asks) (Planned, error) {
	p := Planned{Item: it}
	if r.Outbox != nil {
		if e, ok := r.Outbox.Get(it.Key); ok {
			p.Tier, p.Rule, p.Body, p.Skipped = rules.Send, e.Rule, e.Body, "in the outbox, "+string(e.State)
			return p, nil
		}
	}
	if r.Queue != nil && r.Queue.IsHeld(it.Key) {
		p.Tier, p.Skipped = rules.Hold, "in the hold queue"
		return p, nil
	}
	d := r.Rules.Decide(it)
	p.Tier = d.Tier
	if d.Rule != nil {
		p.Rule = d.Rule.Name
	}
	switch d.Tier {
	case rules.Never:
		// Every pass decides dropped items again, since the rules may have changed. A key the same rule already
		// dropped is an earlier decision: it is reported as skipped and not recorded again. A key another rule
		// dropped, or a new key, is a new decision.
		if r.Queue != nil {
			if rule, ok := r.Queue.DroppedBy(it.Key); ok && rule == p.Rule {
				p.Skipped = "dropped earlier"
				return p, nil
			}
		}
		if !r.Send {
			return p, nil
		}
		return p, r.Queue.Drop(queue.Dropped{Key: it.Key, Rule: p.Rule, DroppedAt: r.Now().UTC()})
	case rules.Hold:
		// The body shown for a held item is a preview; it is built again when the person approves it.
		_, body, err := project.Project(project.Input{Item: it, Rule: d.Rule, Person: r.Person, OpenAsks: view.open(it.Subject)})
		p.Body, p.Err = body, err
		if r.Send {
			return p, r.Queue.Hold(queue.Held{Key: it.Key, Rule: p.Rule, HeldAt: r.Now().UTC(), Item: it})
		}
		return p, nil
	}
	b, body, err := project.Project(project.Input{Item: it, Rule: d.Rule, Person: r.Person, OpenAsks: view.open(it.Subject)})
	if err != nil {
		p.Err = err
		r.Log.Log(logs.ProjectFailed, logs.Source(it.Source))
		return p, nil
	}
	p.Body = body
	if !r.Send {
		view.record(it.Subject, it.Key, b)
		return p, nil
	}
	if err := r.Outbox.Append(outbox.Event, it.Key, it.Subject, p.Rule, body); err != nil {
		return p, err
	}
	if r.AfterAppend != nil {
		r.AfterAppend(it.Key)
	}
	return p, nil
}

// Classify maps an error to a fixed reason code, for logs and for what `beacon run` prints.
func Classify(err error) logs.Reason {
	var denied *egress.DeniedError
	var pathErr *fs.PathError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, signal.ErrUnauthorized):
		return logs.ReasonUnauthorized
	case errors.Is(err, signal.ErrUnavailable):
		return logs.ReasonUnavailable
	case errors.Is(err, lease.ErrRefused):
		return logs.ReasonLeaseRefused
	case errors.Is(err, context.DeadlineExceeded):
		return logs.ReasonLeaseExpired
	case errors.Is(err, context.Canceled):
		return logs.ReasonCanceled
	case errors.As(err, &denied):
		return logs.ReasonEgress
	case errors.Is(err, outbox.ErrLocked):
		return logs.ReasonLocked
	case errors.Is(err, keychain.ErrNotFound):
		return logs.ReasonKeychain
	case errors.As(err, &pathErr):
		return logs.ReasonState
	}
	return logs.ReasonInternal
}

// transient reports whether a pass can end normally after err, leaving bodies pending for the next pass.
func transient(err error) bool {
	return errors.Is(err, signal.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded)
}

func (r *Runner) sendPending(ctx context.Context, rep *Report) error {
	st, err := r.Lease.Ensure(ctx)
	rep.Lease = &st
	if err != nil {
		rep.LeaseReason = Classify(err)
		r.Log.Log(logs.LeaseFailed, logs.Why(rep.LeaseReason))
		if !transient(err) || ctx.Err() != nil {
			return err
		}
		if !st.Held {
			return nil
		}
	}
	if !st.Held {
		r.Log.Log(logs.LeaseNotHeld, logs.Period(st.Period))
		return nil
	}
	r.Log.Log(logs.LeaseHeld, logs.Period(st.Period), logs.Until(st.Until))
	for _, e := range r.Outbox.PendingEvents() {
		if !r.Now().Before(st.Until) {
			r.Log.Log(logs.LeaseExpired, logs.Period(st.Period))
			return nil
		}
		// The lease bounds the send: no write, with its retries and waits, outlasts it.
		wctx, cancel := context.WithTimeout(ctx, st.Until.Sub(r.Now()))
		res, err := r.Writer.Write(wctx, e.Body)
		cancel()
		if err != nil {
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				r.Log.Log(logs.LeaseExpired, logs.Period(st.Period))
				return nil
			}
			r.Log.Log(logs.SendStopped, logs.Why(Classify(err)))
			if errors.Is(err, signal.ErrUnavailable) {
				// Later bodies may close asks this one opens, so they wait for it.
				return nil
			}
			return err
		}
		state := outbox.Rejected
		switch res.Outcome {
		case signal.Written:
			state = outbox.Sent
		case signal.Replayed:
			state = outbox.Replayed
		}
		if err := r.Outbox.Mark(e.Key, state, res.EventID, res.Status, res.Code, res.Message); err != nil {
			return err
		}
		r.Log.Log(logs.SendResult, logs.Kind(string(e.Kind)), logs.Outcome(string(res.Outcome)), logs.Status(res.Status), logs.Code(res.Code))
		rep.Results = append(rep.Results, Sent{Key: e.Key, Outcome: res.Outcome, Status: res.Status, Code: res.Code, EventID: res.EventID})
	}
	return nil
}

// Explanation is what `beacon explain` shows for one key.
type Explanation struct {
	Key string
	// Item is the collected item, when a source still yields it.
	Item *source.Item
	Tier rules.Tier
	// Rule is the matching rule, or nil for the default.
	Rule *rules.Rule
	// Body is the stored body when the key is in the outbox, else the body the item would be sent as now.
	Body []byte
	// Entry is the outbox entry, when there is one.
	Entry *outbox.Entry
	// Held is true when the item is in the hold queue.
	Held bool
	Err  error
}

// Explain finds a key among the collected items and the outbox and says what Beacon does with it, and why.
func (r *Runner) Explain(ctx context.Context, key string) (*Explanation, error) {
	ex := &Explanation{Key: key}
	if r.Outbox != nil {
		if e, ok := r.Outbox.Get(key); ok {
			ex.Entry, ex.Body = &e, e.Body
		}
	}
	if r.Queue != nil {
		ex.Held = r.Queue.IsHeld(key)
	}
	for _, it := range r.Collect(ctx) {
		if it.Key != key {
			continue
		}
		it := it
		ex.Item = &it
		d := r.Rules.Decide(it)
		ex.Tier, ex.Rule = d.Tier, d.Rule
		if ex.Entry == nil && d.Tier != rules.Never {
			var open []string
			if r.Outbox != nil {
				open = r.Outbox.OpenAsks(it.Subject)
			}
			_, ex.Body, ex.Err = project.Project(project.Input{Item: it, Rule: d.Rule, Person: r.Person, OpenAsks: open})
		}
		break
	}
	if ex.Item == nil && ex.Entry == nil && !ex.Held {
		return nil, fmt.Errorf("no item, outbox entry or held item has key %q", key)
	}
	return ex, nil
}
