// Package runner is the path from a collected item to the network: rules, projection, outbox, lease, send. It is
// the only code that sends item content, and it sends only bodies the projection built from a send rule.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/egress"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/lease"
	"github.com/CrossTerra/beacon/internal/logs"
	"github.com/CrossTerra/beacon/internal/outbox"
	"github.com/CrossTerra/beacon/internal/project"
	"github.com/CrossTerra/beacon/internal/queue"
	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/seed"
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

	// Stop, when set, is checked before each event send. A true result ends the pass without an error after the
	// in-flight send, and leaves the rest pending. It does not cancel the write already in progress: the caller
	// keeps that context alive. A lease renewal already inside Ensure finishes; no further event send starts after it.
	Stop func() bool

	// Cursors holds committed cursors. Nil means the sources have no cursor and none is stored. A cursor is stored
	// only after every item of that source's pass has been decided, and only when sending (DEC-23).
	Cursors Cursors
	// Unaccepted names sources whose first run has not been accepted. A send pass decides their items and reports
	// them, but does not record them and does not commit their cursor.
	Unaccepted map[string]bool
	// Seed drops subjects a seed file marks never, before the rules file (DEC-24). Nil means no seed.
	Seed Seeder

	// Personal maps an enabled source to its stem, for the withheld report. The check itself uses source.PersonalStems,
	// which does not depend on the config: a pending body stays personal after its source is removed.
	Personal map[string]string
	// Restrictions reads Signal's restricted prefixes. A send pass with Personal set needs it.
	Restrictions RestrictionReader

	// restricted is this pass's restricted prefixes; restrictErr is why they could not be read.
	restricted   []string
	restrictErr  error
	restrictRead bool
	deferred     map[string]bool
	gaps         []Gap
	partial      []string
	continued    []string
}

// Gap is a source that could not read its whole window: its change history expired (Expired), or a read stopped at
// its limit (Cut), or both.
type Gap struct {
	Source  string
	Expired bool
	Cut     bool
}

// RestrictionReader reads the restricted subject prefixes from Signal.
type RestrictionReader interface {
	Restrictions(ctx context.Context) ([]string, error)
}

// Withheld is a personal source whose send items this pass did not send, and why. Reason is
// prefix_unrestricted (Signal answered, and the stem is not restricted: send items are held) or
// restrictions_unknown (the restrictions could not be read: send items are not recorded, and the cursor stays).
type Withheld struct {
	Source string
	// Prefix is the restriction that is missing, "gmail:*".
	Prefix string
	Reason logs.Reason
	Items  int
}

// Cursors is the committed cursor of each source. Reset removes one, so the next collect reads open state.
type Cursors interface {
	Get(name string) ([]byte, bool)
	Commit(name string, next []byte) error
	Reset(name string) (bool, error)
}

// Seeder is the seed file's never-list.
type Seeder interface {
	Drops(subject string) bool
}

// Planned is what a pass decided for one item.
type Planned struct {
	Item source.Item
	Tier rules.Tier
	// Rule is the matching rule's name, or "" for the default.
	Rule string
	// Body is the request body the item would be, or was, sent as: for send and hold items.
	Body []byte
	// Skipped says why an item was not decided again: it is already in the outbox or the hold queue. A 400 with no
	// stored item does not count.
	Skipped string
	// Unaccepted is true when the source's first run has not been accepted, so a send pass did not record the item.
	Unaccepted bool
	// Withheld says why a send item of a personal source was not sent: prefix_unrestricted (it was held instead)
	// or restrictions_unknown (it was not recorded).
	Withheld logs.Reason
	// Err is a projection error; such an item is neither appended nor sent.
	Err error
}

// SourceFailure is a source that could not be collected. Reason is a fixed code; the error text is not kept.
type SourceFailure struct {
	Name   string
	Reason logs.Reason
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
	// Failed are sources that could not be collected. Their cursors were not moved.
	Failed []SourceFailure
	// Withheld are personal sources whose send items were not sent because their stem is not restricted in Signal.
	Withheld []Withheld
	// WithheldPending is how many pending bodies on personal subjects were not sent for the same reason.
	WithheldPending int
	// RestrictionsReason is why the restricted prefixes could not be read, when they could not.
	RestrictionsReason logs.Reason
	// Gaps are sources that could not read their whole window: an expired history, or a read cut at its limit.
	Gaps []Gap
	// Partial are sources whose read stopped at a rate limit. The next pass continues; they are not failed, and
	// not a completed first run.
	Partial []string
	// Continued are sources that finished a scan an earlier pass had stopped. The items are only the remainder,
	// so this is not a completed first-run summary.
	Continued []string
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

// passSource is one source's collect, and whether its cursor may move.
type passSource struct {
	name   string
	next   []byte
	commit bool
	// resume stores next even when the pass does not commit a completed cursor: a read stopped at a rate limit.
	resume bool
	// clear drops a resume cursor when this collect finished that read and is not committing a completed cursor.
	clear bool
}

// Collect gathers items from every source, validated and sorted by when they happened, then by key.
func (r *Runner) Collect(ctx context.Context) []source.Item {
	items, _, _ := r.collect(ctx)
	return items
}

// collect gathers items and the cursors that may be stored after they are decided.
func (r *Runner) collect(ctx context.Context) ([]source.Item, []passSource, []SourceFailure) {
	var items []source.Item
	var passes []passSource
	var failed []SourceFailure
	r.gaps = nil
	r.partial = nil
	r.continued = nil
	for _, s := range r.Sources {
		var cursor []byte
		if r.Cursors != nil {
			cursor, _ = r.Cursors.Get(s.Name())
		}
		got, err := s.Collect(ctx, cursor)
		// A limited or failed pass does not move the cursor. Items already read are still decided: a later pass
		// collects them again, and keys make that harmless.
		commit := err == nil && r.Send && !r.Unaccepted[s.Name()] && got.Next != nil && r.Cursors != nil && !got.Partial
		// A resume cursor is stored only on a send pass, and only when the items already read are recorded.
		// A dry run writes no cursor: a later send must not skip threads the dry run merely printed.
		resume := err == nil && r.Send && !r.Unaccepted[s.Name()] && got.Partial && got.Next != nil && r.Cursors != nil
		clear := err == nil && r.Send && got.ClearResume && !resume && !commit && r.Cursors != nil
		limit := got.Limit
		if err != nil {
			limit = source.LimitOf(err)
		}
		if limit != nil {
			r.Log.Log(logs.RateLimited, logs.Source(s.Name()), logs.Status(limit.Status), logs.LimitReason(limit.Reason),
				logs.RetryAfter(limit.RetryAfter), logs.Count("units", limit.Units))
		}
		if err == nil && got.Partial {
			r.Log.Log(logs.ReadPaused, logs.Source(s.Name()), logs.Why(logs.ReasonRateLimit))
			r.partial = append(r.partial, s.Name())
		}
		if err == nil && got.ClearResume {
			r.continued = append(r.continued, s.Name())
		}
		if err != nil {
			reason := Classify(err)
			if reason == "" || reason == logs.ReasonInternal {
				reason = logs.ReasonSource
			}
			r.Log.Log(logs.CollectFailed, logs.Source(s.Name()), logs.Why(reason))
			failed = append(failed, SourceFailure{Name: s.Name(), Reason: reason})
			commit = false
		}
		fields, stateFields := s.Fields(), s.StateFields()
		invalid := 0
		kept := 0
		for _, it := range got.Items {
			if it.Source != s.Name() || source.Validate(it, fields, stateFields) != nil {
				invalid++
				continue
			}
			kept++
			items = append(items, it)
		}
		// Dropped items were never in Items. Count them apart so the collected count cannot go negative.
		if invalid+got.Dropped > 0 {
			r.Log.Log(logs.ItemInvalid, logs.Source(s.Name()), logs.Count("items", invalid+got.Dropped), logs.Why(logs.ReasonItem))
		}
		r.Log.Log(logs.Collected, logs.Source(s.Name()), logs.Count("items", kept))
		if got.Gap || got.Cut {
			cut := 0
			if got.Cut {
				cut = 1
			}
			if got.Gap {
				r.Log.Log(logs.HistoryGap, logs.Source(s.Name()), logs.Count("cut", cut))
			} else {
				r.Log.Log(logs.ReadCut, logs.Source(s.Name()))
			}
			r.gaps = append(r.gaps, Gap{Source: s.Name(), Expired: got.Gap, Cut: got.Cut})
		}
		passes = append(passes, passSource{name: s.Name(), next: got.Next, commit: commit, resume: resume, clear: clear})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].OccurredAt.Equal(items[j].OccurredAt) {
			return items[i].OccurredAt.Before(items[j].OccurredAt)
		}
		return items[i].Key < items[j].Key
	})
	return items, passes, failed
}

// asks tracks the person's open asks per subject: the outbox's, plus those a dry run would add.
type asks struct {
	ob     *outbox.Outbox
	added  map[string][]string
	closed map[string]bool
}

// forItem is the open asks an item's projection may close: on its subject, and for an own act bounded by
// CloseSiblingsBefore, the earlier asks that opened its sibling subjects. CloseOnly narrows them to the asks it names.
func (a *asks) forItem(it source.Item) []string {
	before := ""
	if it.Own {
		before = it.CloseSiblingsBefore
	}
	open := a.openOn(it.Subject, before)
	if len(it.CloseOnly) == 0 {
		return open
	}
	// Only the named asks, and only while they are open on this subject.
	named := map[string]bool{}
	for _, k := range it.CloseOnly {
		named[k] = true
	}
	var out []string
	for _, k := range open {
		if named[k] {
			out = append(out, k)
		}
	}
	return out
}

func (a *asks) openOn(subject, before string) []string {
	var base []string
	if a.ob != nil {
		if before != "" {
			base = a.ob.OpenAsksSiblings(subject, before)
		} else {
			base = a.ob.OpenAsks(subject)
		}
	}
	added := a.added[subject]
	if before != "" {
		// Sorted by subject, the asks this pass added stay in a fixed order.
		var subjects []string
		for s := range a.added {
			if s != subject && source.IsSibling(subject, s) {
				subjects = append(subjects, s)
			}
		}
		sort.Strings(subjects)
		for _, s := range subjects {
			for _, k := range a.added[s] {
				if source.ClosesSibling(subject, before, s, k) {
					added = append(added, k)
				}
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, k := range append(base, added...) {
		if a.closed[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
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

	items, passes, failed := r.collect(ctx)
	rep.Failed, rep.Gaps, rep.Partial, rep.Continued = failed, r.gaps, r.partial, r.continued
	if err := r.readRestrictions(ctx, r.Send); err != nil {
		return rep, err
	}
	if r.restrictErr != nil {
		rep.RestrictionsReason = Classify(r.restrictErr)
	}
	projected := map[string]int{}
	withheld := map[string]*Withheld{}
	for _, it := range items {
		p, err := r.decide(it, view)
		if err != nil {
			return rep, err
		}
		if p.Err != nil {
			projected[it.Source]++
		}
		if p.Withheld != "" {
			w := withheld[it.Source]
			if w == nil {
				w = &Withheld{Source: it.Source, Prefix: r.Personal[it.Source] + "*", Reason: p.Withheld}
				withheld[it.Source] = w
			}
			w.Items++
		}
		rep.Items = append(rep.Items, p)
	}
	for _, name := range sortedKeys(withheld) {
		w := withheld[name]
		rep.Withheld = append(rep.Withheld, *w)
		r.Log.Log(logs.Withheld, logs.Source(name), logs.Count("items", w.Items), logs.Why(w.Reason))
	}
	// A projection error is dropped and logged by count. It must not freeze the source, and the error text is
	// not logged: it can name a field.
	var names []string
	for name := range projected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r.Log.Log(logs.ProjectFailed, logs.Source(name), logs.Count("items", projected[name]), logs.Why(logs.ReasonProject))
	}
	// The cursor moves only after every item of the pass is decided. A crash before this collects the same items
	// again; keys make that harmless.
	for _, p := range passes {
		// A source whose send items could not be recorded keeps its cursor, so they are collected again.
		// A resume cursor is stored only when those items were recorded. A dry run writes nothing.
		switch {
		case p.commit && !r.deferred[p.name]:
			if err := r.Cursors.Commit(p.name, p.next); err != nil {
				return rep, err
			}
		case p.resume && !r.deferred[p.name]:
			if err := r.Cursors.Commit(p.name, p.next); err != nil {
				return rep, err
			}
		case p.clear:
			if _, err := r.Cursors.Reset(p.name); err != nil {
				return rep, err
			}
		}
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
		if e, ok := r.Outbox.Get(it.Key); ok && !e.Undecided() {
			p.Tier, p.Rule, p.Body, p.Skipped = rules.Send, e.Rule, e.Body, "in the outbox, "+string(e.State)
			return p, nil
		}
	}
	if r.Queue != nil && r.Queue.IsHeld(it.Key) {
		p.Tier, p.Skipped = rules.Hold, "in the hold queue"
		return p, nil
	}
	if r.Seed != nil && r.Seed.Drops(it.Subject) {
		p.Tier = rules.Never
		p.Rule = seed.RuleName
		return r.finishNever(p)
	}
	d := r.Rules.Decide(it)
	p.Tier = d.Tier
	if d.Rule != nil {
		p.Rule = d.Rule.Name
	}
	if r.Unaccepted[it.Source] {
		p.Unaccepted = true
	}
	if d.Tier == rules.Send && r.record(it) {
		switch p.Withheld = r.withholds(it.Subject); p.Withheld {
		case logs.ReasonUnrestricted:
			// Signal would show it to every reader. Hold it locally instead.
			p.Tier = rules.Hold
			_, body, _, err := r.project(it, d.Rule, view)
			p.Body, p.Err = body, err
			return p, r.Queue.Hold(queue.Held{Key: it.Key, Rule: p.Rule, HeldAt: r.Now().UTC(), Item: it})
		case logs.ReasonRestrictionsUnknown:
			// Not recorded, and the cursor stays: a later pass that can read the restrictions decides it again.
			r.deferred[it.Source] = true
			_, body, _, err := r.project(it, d.Rule, view)
			p.Body, p.Err = body, err
			return p, nil
		}
	}
	switch d.Tier {
	case rules.Never:
		// Every pass decides dropped items again, since the rules may have changed. A key the same rule already
		// dropped is an earlier decision: it is reported as skipped and not recorded again. A key another rule
		// dropped, or a new key, is a new decision.
		return r.finishNever(p)
	case rules.Hold:
		// The body shown for a held item is a preview; it is built again when the person approves it.
		_, body, _, err := r.project(it, d.Rule, view)
		p.Body = body
		if err != nil {
			p.Err = err
		}
		if r.record(it) {
			return p, r.Queue.Hold(queue.Held{Key: it.Key, Rule: p.Rule, HeldAt: r.Now().UTC(), Item: it})
		}
		return p, nil
	}
	b, body, truncated, err := r.project(it, d.Rule, view)
	if err != nil {
		// A body or request over Signal's limit is not dropped. Hold it so the person can still see it.
		p.Err = err
		p.Tier = rules.Hold
		if r.record(it) && r.Queue != nil {
			return p, r.Queue.Hold(queue.Held{Key: it.Key, Rule: p.Rule, HeldAt: r.Now().UTC(), Item: it})
		}
		return p, nil
	}
	p.Body = body
	if !r.record(it) {
		view.record(it.Subject, it.Key, b)
		return p, nil
	}
	decided, err := json.Marshal(project.Decide(it, d.Rule, truncated))
	if err != nil {
		return p, err
	}
	if _, exists := r.Outbox.Get(it.Key); exists {
		// A 400 Signal never stored, with no item kept. The refused lines stay; this attempt stores the item.
		err = r.Outbox.AppendAttempt(it.Key, it.Subject, p.Rule, body, decided)
	} else {
		err = r.Outbox.AppendDecided(outbox.Event, it.Key, it.Subject, p.Rule, body, decided)
	}
	if err != nil {
		return p, err
	}
	if r.AfterAppend != nil {
		r.AfterAppend(it.Key)
	}
	return p, nil
}

// project builds the body. If the body or the request is over Signal's limit, it tries again without the body and
// marks the event truncated. The marker is a fixed flag, not item content.
func (r *Runner) project(it source.Item, rule *rules.Rule, view *asks) (*project.Body, []byte, bool, error) {
	in := project.Input{Item: it, Rule: rule, Person: r.Person, OpenAsks: view.forItem(it)}
	b, body, err := project.Project(in)
	if err == nil {
		return b, body, false, nil
	}
	stripped := it
	if it.Fields != nil {
		stripped.Fields = map[string]string{}
		for k, v := range it.Fields {
			if k != project.FieldBody {
				stripped.Fields[k] = v
			}
		}
	}
	in.Item = stripped
	in.Truncated = true
	b, body, err = project.Project(in)
	return b, body, true, err
}

// record reports whether this pass writes the item. A dry run writes nothing. A source whose first run has not been
// accepted is reported and not written (DEC-23).
func (r *Runner) record(it source.Item) bool {
	return r.Send && !r.Unaccepted[it.Source]
}

func (r *Runner) finishNever(p Planned) (Planned, error) {
	if r.Queue != nil {
		if rule, ok := r.Queue.DroppedBy(p.Item.Key); ok && rule == p.Rule {
			p.Skipped = "dropped earlier"
			return p, nil
		}
	}
	if !r.record(p.Item) {
		return p, nil
	}
	return p, r.Queue.Drop(queue.Dropped{Key: p.Item.Key, Rule: p.Rule, DroppedAt: r.Now().UTC()})
}

// readRestrictions reads Signal's restricted prefixes for a send pass that might send a personal subject. A failure
// is kept, not returned: personal sends wait and everything else goes on. Only an interrupt ends the pass.
func (r *Runner) readRestrictions(ctx context.Context, sending bool) error {
	r.restricted, r.restrictErr, r.restrictRead, r.deferred = nil, nil, false, map[string]bool{}
	if !sending || !r.needsRestrictions() {
		return nil
	}
	r.restrictRead = true
	if r.Restrictions == nil {
		r.restrictErr = errors.New("runner: no restriction reader")
		return nil
	}
	r.restricted, r.restrictErr = r.Restrictions.Restrictions(ctx)
	if r.restrictErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.Log.Log(logs.RestrictionsUnread, logs.Why(Classify(r.restrictErr)))
	}
	return nil
}

// needsRestrictions reports whether this pass might send a personal subject: a personal source is enabled, or the
// outbox still holds a pending or rejected event under a personal stem. The stem list is fixed, so removing the
// source does not skip the check.
func (r *Runner) needsRestrictions() bool {
	if len(r.Personal) > 0 {
		return true
	}
	if r.Outbox == nil {
		return false
	}
	for _, e := range r.Outbox.Entries() {
		if e.Kind == outbox.Event && (e.State == outbox.Pending || e.State == outbox.Rejected) && source.PersonalStem(e.Subject) != "" {
			return true
		}
	}
	return false
}

// withholds says why a body on subject may not be sent now: the subject is personal and Signal does not, or cannot
// be shown to, keep it private. Signal reads a subject's owner after the longest restricted stem it is under, so that
// stem must be exactly the personal stem. The stems are fixed (source.PersonalStems), not the enabled sources.
func (r *Runner) withholds(subject string) logs.Reason {
	stem := source.PersonalStem(subject)
	if stem == "" {
		return ""
	}
	if !r.restrictRead || r.restrictErr != nil {
		return logs.ReasonRestrictionsUnknown
	}
	if RestrictingStem(r.restricted, subject) != stem {
		return logs.ReasonUnrestricted
	}
	return ""
}

// RestrictingStem is the longest restricted stem a subject is under, without its "*", or "" when none is. It is
// Signal's rule (restrictingStem in Signal's domain/visibility.ts).
func RestrictingStem(prefixes []string, subject string) string {
	found := ""
	for _, p := range prefixes {
		if !strings.HasSuffix(p, ":*") {
			continue
		}
		stem := strings.TrimSuffix(p, "*")
		if strings.HasPrefix(subject, stem) && len(stem) > len(found) {
			found = stem
		}
	}
	return found
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
	case errors.Is(err, source.ErrRateLimited):
		return logs.ReasonRateLimit
	case errors.Is(err, source.ErrUnauthorized):
		return logs.ReasonSourceAuth
	case errors.Is(err, source.ErrScope):
		return logs.ReasonScope
	case errors.Is(err, source.ErrIdentity):
		return logs.ReasonIdentity
	case errors.Is(err, signal.ErrNoRead):
		return logs.ReasonNoRead
	case errors.As(err, &pathErr):
		return logs.ReasonState
	}
	return logs.ReasonInternal
}

// transient reports whether a pass can end normally after err, leaving bodies pending for the next pass.
func transient(err error) bool {
	return errors.Is(err, signal.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded)
}

func (r *Runner) stopped() bool { return r.Stop != nil && r.Stop() }

func (r *Runner) sendPending(ctx context.Context, rep *Report) error {
	if r.stopped() {
		return nil
	}
	// Again, immediately before anything pending leaves. A restriction lifted while this pass was collecting is
	// honoured; one that cannot be re-read keeps personal bodies pending.
	if err := r.readRestrictions(ctx, true); err != nil {
		return err
	}
	if r.restrictErr != nil {
		rep.RestrictionsReason = Classify(r.restrictErr)
	}
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
		if r.stopped() {
			return nil
		}
		if r.withholds(e.Subject) != "" {
			// Appended while its stem was restricted, and it is not now (or cannot be shown to be). It stays pending.
			rep.WithheldPending++
			continue
		}
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
	// Seed is true when a seed file dropped the subject before the rules file.
	Seed bool
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
		if r.Seed != nil && r.Seed.Drops(it.Subject) {
			ex.Tier = rules.Never
			ex.Seed = true
			break
		}
		d := r.Rules.Decide(it)
		ex.Tier, ex.Rule = d.Tier, d.Rule
		if ex.Entry == nil && d.Tier != rules.Never {
			open := (&asks{ob: r.Outbox, added: map[string][]string{}, closed: map[string]bool{}}).forItem(it)
			_, ex.Body, ex.Err = project.Project(project.Input{Item: it, Rule: d.Rule, Person: r.Person, OpenAsks: open})
		}
		break
	}
	if ex.Item == nil && ex.Entry == nil && !ex.Held {
		return nil, fmt.Errorf("no item, outbox entry or held item has key %q", key)
	}
	return ex, nil
}
