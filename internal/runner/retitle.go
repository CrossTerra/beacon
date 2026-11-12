package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/CrossTerra/beacon/internal/lease"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/signal"
	"github.com/CrossTerra/beacon/internal/source"
	"github.com/CrossTerra/beacon/internal/titled"
)

// retitlePerPass is how many threads a send pass checks for a retitle. Each check reads Signal twice and the source
// once or more; the rest wait for later passes.
const retitlePerPass = 20

// Titler is a source that can name the thread for one of its subjects now (the Slack source). "" with a nil error
// means it cannot name it now and may later; source.ErrNoTitle means it never will.
type Titler interface {
	ThreadTitle(ctx context.Context, subject string) (string, error)
}

// ThreadReader reads back where one of Beacon's events landed, and a thread as it is now.
type ThreadReader interface {
	EventOpened(ctx context.Context, id string) (signal.Opened, error)
	Thread(ctx context.Context, id string) (signal.Thread, error)
}

// RetitleRecord remembers the subjects whose threads were checked and left alone.
type RetitleRecord interface {
	Checked(subject string) bool
	Mark(subject, outcome string) error
}

// RetitleKeyPart is in the key of every thread title Beacon writes: <source>:retitle:<thread number>:<title hash>.
const RetitleKeyPart = ":retitle:"

// retitle renames threads Beacon opened before its source could name them, while this instance holds the lease. A
// thread is renamed only when every one of these holds:
//
//   - the subject's first event Beacon sent is in the outbox with its decided item, it opened no thread with an
//     object title, and the current rules send that item with a rule that lists title;
//   - a personal subject's prefix is restricted, as for any send;
//   - Signal says that event opened the thread, the thread was not merged since, and its title is still the one
//     Beacon opened it with: nobody renamed it;
//   - the source names it, and the name differs from the title it has.
//
// The thread change is appended to the outbox before it is sent, like any body, under a key for that thread and that
// title. A thread left alone is recorded, so it is not read again.
func (r *Runner) retitle(ctx context.Context, st lease.Status, rep *Report) error {
	if r.Threads == nil || r.Retitled == nil || r.Rules == nil {
		return nil
	}
	titlers := map[string]Titler{}
	for _, s := range r.Sources {
		if t, ok := s.(Titler); ok && !r.Unaccepted[s.Name()] && r.Rules.Lists(s.Name(), project.FieldTitle) {
			titlers[s.Name()] = t
		}
	}
	if len(titlers) == 0 {
		return nil
	}
	first := map[string]outbox.Entry{}
	var order []string
	done := map[string]bool{}
	for _, e := range r.Outbox.Entries() {
		name, _, _ := strings.Cut(e.Key, ":")
		if e.Kind != outbox.Event || titlers[name] == nil {
			continue
		}
		if strings.Contains(e.Key, RetitleKeyPart) {
			done[e.Subject] = true
			continue
		}
		if _, seen := first[e.Subject]; seen || (e.State != outbox.Sent && e.State != outbox.Replayed) || e.EventID == "" {
			continue
		}
		first[e.Subject] = e
		order = append(order, e.Subject)
	}
	var checked, written, left int
	unnamed := map[string]bool{}
	defer func() {
		r.Log.Log(logs.Retitled, logs.Count("checked", checked), logs.Count("titled", written), logs.Count("left", left))
	}()
	for _, subject := range order {
		e := first[subject]
		name, _, _ := strings.Cut(e.Key, ":")
		if done[subject] || unnamed[name] || r.Retitled.Checked(subject) || openedWithTitle(e.Body) {
			continue
		}
		rule, ok := r.retitleRule(e)
		if !ok || r.withholds(subject) != "" {
			continue
		}
		if checked == retitlePerPass || r.stopped() {
			return nil
		}
		if !r.Now().Before(st.Until) {
			r.Log.Log(logs.LeaseExpired, logs.Period(st.Period))
			return nil
		}
		checked++
		// The lease bounds every read and the write: none outlasts it.
		wctx, cancel := context.WithTimeout(ctx, st.Until.Sub(r.Now()))
		outcome, err := r.retitleOne(wctx, titlers[name], e, rule, rep)
		cancel()
		switch {
		case err != nil && ctx.Err() == nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, signal.ErrUnavailable)):
			// Signal or the lease is gone for now. A body already appended stays pending for the next pass.
			return nil
		case err != nil:
			return err
		case outcome == outcomeWritten:
			written++
		case outcome == outcomeLater:
			// The source cannot name threads now (no scope, a failed lookup). Its other threads wait too.
			unnamed[name] = true
		case outcome != "":
			left++
			if err := r.Retitled.Mark(subject, outcome); err != nil {
				return err
			}
		}
	}
	return nil
}

const (
	outcomeWritten = "written"
	outcomeLater   = "later"
)

// retitleRule is the current rule for the stored item, when it sends and lists title. A rule above it that the stored
// item cannot be shown to miss stops the retitle, as it stops a retry.
func (r *Runner) retitleRule(e outbox.Entry) (*rules.Rule, bool) {
	if len(e.Decision) == 0 {
		return nil, false
	}
	var d project.Decision
	if json.Unmarshal(e.Decision, &d) != nil {
		return nil, false
	}
	if r.Seed != nil && r.Seed.Drops(d.Item.Subject) {
		return nil, false
	}
	decided := r.Rules.Decide(d.Item)
	if decided.Tier != rules.Send || decided.Rule == nil || r.Rules.GapsAbove(d.Item, decided.Rule) {
		return nil, false
	}
	if !slices.Contains(decided.Rule.Fields, project.FieldTitle) {
		return nil, false
	}
	return decided.Rule, true
}

// retitleOne checks one thread and renames it when it may. It returns outcomeWritten, outcomeLater, a titled outcome
// code to record, or "" when nothing is recorded (the thread change was appended and refused, or is pending).
func (r *Runner) retitleOne(ctx context.Context, t Titler, e outbox.Entry, rule *rules.Rule, rep *Report) (string, error) {
	opened, err := r.Threads.EventOpened(ctx, e.EventID)
	switch {
	case errors.Is(err, signal.ErrNotFound):
		return titled.NotFound, nil
	case err != nil:
		return "", err
	case opened.Number == 0:
		return titled.NotOpened, nil
	case opened.ThreadID == "":
		return titled.Moved, nil
	}
	th, err := r.Threads.Thread(ctx, opened.ThreadID)
	switch {
	case errors.Is(err, signal.ErrNotFound):
		return titled.NotFound, nil
	case err != nil:
		return "", err
	case th.Number != opened.Number || th.MergedInto != "":
		return titled.Moved, nil
	case th.Title != opened.Title:
		return titled.Renamed, nil
	}
	name, err := t.ThreadTitle(ctx, e.Subject)
	switch {
	case errors.Is(err, source.ErrNoTitle):
		return titled.Unreadable, nil
	case err != nil:
		return "", err
	case name == "":
		return outcomeLater, nil
	}
	sum := sha256.Sum256([]byte(name))
	src, _, _ := strings.Cut(e.Key, ":")
	key := src + RetitleKeyPart + strconv.FormatInt(th.Number, 10) + ":" + hex.EncodeToString(sum[:6])
	body, err := project.ThreadTitled(th.Number, key, name)
	if err != nil {
		return "", err
	}
	var b project.ThreadTitle
	if json.Unmarshal(body, &b) != nil || b.Metadata.Title == th.Title {
		return titled.Same, nil
	}
	if _, exists := r.Outbox.Get(key); exists {
		return "", nil
	}
	// Recorded under the item's subject, so a personal one waits for its restriction like any pending body.
	if err := r.Outbox.Append(outbox.Event, key, e.Subject, rule.Name, body); err != nil {
		return "", err
	}
	if r.AfterAppend != nil {
		r.AfterAppend(key)
	}
	res, err := r.Writer.Write(ctx, body)
	if err != nil {
		r.Log.Log(logs.SendStopped, logs.Why(Classify(err)))
		return "", err
	}
	state := outbox.Rejected
	switch res.Outcome {
	case signal.Written:
		state = outbox.Sent
	case signal.Replayed:
		state = outbox.Replayed
	}
	if err := r.Outbox.Mark(key, state, res.EventID, res.Status, res.Code, res.Message); err != nil {
		return "", err
	}
	r.Log.Log(logs.SendResult, logs.Kind(string(outbox.Event)), logs.Outcome(string(res.Outcome)), logs.Status(res.Status), logs.Code(res.Code))
	rep.Results = append(rep.Results, Sent{Key: key, Outcome: res.Outcome, Status: res.Status, Code: res.Code, EventID: res.EventID})
	if state == outbox.Rejected {
		return "", nil
	}
	rep.Retitled++
	return outcomeWritten, nil
}

// openedWithTitle reports whether a body named the thread's title itself (open.title): it was decided after its
// source could name threads, or from a rule that listed an object title.
func openedWithTitle(body []byte) bool {
	var b struct {
		Open *struct {
			Title string `json:"title"`
		} `json:"open"`
	}
	return json.Unmarshal(body, &b) == nil && b.Open != nil && b.Open.Title != ""
}
