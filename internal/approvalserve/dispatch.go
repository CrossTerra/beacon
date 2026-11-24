package approvalserve

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/actionexec"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/review"
)

type dispatcher struct {
	ctx      context.Context
	j        *approval.Journal
	now      func() time.Time
	handlers map[approval.Action]actionexec.Handler
	inFlight sync.Mutex // one confirmed batch or settings change at a time
	failed   chan error
}

func newDispatcher(ctx context.Context, j *approval.Journal, now func() time.Time, handlers map[approval.Action]actionexec.Handler) *dispatcher {
	return &dispatcher{ctx: ctx, j: j, now: now, handlers: handlers, failed: make(chan error, 1)}
}

func (d *dispatcher) controls(p *actionexec.Providers) review.Actions {
	var targets []review.CredentialTarget
	for _, t := range p.Targets() {
		targets = append(targets, review.CredentialTarget(t))
	}
	return review.Actions{
		Supports: func(a approval.Action) bool { return d.handlers[a] != nil },
		Resolve: func(ctx context.Context, p approval.Proposal) (approval.Resolved, error) {
			if h := d.handlers[p.Type]; h != nil {
				return h.Resolve(ctx, p)
			}
			return approval.Resolved{}, approval.Refusal{Code: "not_configured"}
		},
		Approve: d.submit, ReadBack: d.readBack, Targets: targets,
		Save: func(ctx context.Context, service string, read, write keychain.Secret, key []byte) error {
			if !d.inFlight.TryLock() {
				return approval.Refusal{Code: "busy"}
			}
			defer d.inFlight.Unlock()
			if d.ctx.Err() != nil || d.j.Expire(d.now()) != nil {
				return approval.ErrStopping
			}
			if err := p.Save(ctx, service, read, write, key); err != nil {
				return err
			}
			return d.j.RecordLocal("settings_changed", d.now())
		},
	}
}

// submit is the only live path from a verified assertion to a start. Check every exact binding and open its
// sealed write token before approving anything, then give the worker only scoped credentials, not the vault key.
func (d *dispatcher) submit(items []approval.Frozen, credentialID, challenge, key []byte) error {
	if d.ctx.Err() != nil || len(credentialID) == 0 || len(key) != 32 || len(items) == 0 || len(items) > 10 {
		return approval.ErrTransition
	}
	if !d.inFlight.TryLock() {
		return approval.Refusal{Code: "busy"}
	}
	type dispatchKey struct {
		action                     approval.Action
		origin, account, workspace string
	}
	secrets := make(map[dispatchKey]keychain.Secret)
	work := make([]readyAction, 0, len(items))
	clearAndRelease := func() {
		for _, s := range secrets {
			s.Clear()
		}
		d.inFlight.Unlock()
	}
	submitted := false
	defer func() {
		if !submitted {
			clearAndRelease()
		}
	}()
	for _, f := range items {
		v, _, err := d.j.Get(f.ID, d.now())
		if err != nil {
			return err
		}
		if v.Status != "pending" || v.Version != f.Version || len(items) > 1 && v.Type != approval.Merge && v.Type != approval.Post {
			return approval.ErrTransition
		}
		h := d.handlers[v.Type]
		if h == nil {
			return approval.Refusal{Code: "not_configured"}
		}
		workspace, _ := f.Destination["workspace"].(string)
		b := dispatchKey{v.Type, f.Origin, f.Account, workspace}
		token, ok := secrets[b]
		if !ok {
			token, err = h.Unlock(&f, key)
			if err != nil {
				return err
			}
			secrets[b] = token
		}
		work = append(work, readyAction{f.ID, h, token})
	}
	if err := d.j.ApproveAll(items, credentialID, challenge, d.now()); err != nil {
		return err
	}
	submitted = true
	go func() {
		defer clearAndRelease()
		if err := d.dispatch(work); err != nil {
			select {
			case d.failed <- err:
			default:
			}
		}
	}()
	return nil
}

type readyAction struct {
	id    string
	h     actionexec.Handler
	token keychain.Secret
}

func (d *dispatcher) dispatch(work []readyAction) error {
	for _, action := range work {
		if d.ctx.Err() != nil {
			return nil
		}
		v, err := d.j.StartAsserted(action.id, d.now())
		switch {
		case errors.Is(err, approval.ErrStopping):
			return nil
		case errors.Is(err, approval.ErrTransition): // expired waiting behind an earlier action
			continue
		case err != nil:
			return err
		}
		if err := action.h.Run(d.ctx, d.j, v, action.token); err != nil && d.ctx.Err() == nil && !errors.Is(err, approval.ErrTransition) {
			return err
		}
	}
	return nil
}

func (d *dispatcher) readBack(ctx context.Context, v approval.View) error {
	if h, ok := d.handlers[v.Type].(actionexec.ReadBack); ok {
		return h.Reconcile(ctx, d.j, v.ID)
	}
	return nil
}

func (d *dispatcher) recover() error {
	const tick = 15 * time.Second
	t := time.NewTicker(tick)
	defer t.Stop()
	var lastMerge time.Time
	for d.ctx.Err() == nil {
		at := time.Now()
		mergeDue := at.Sub(lastMerge) >= time.Minute
		if mergeDue {
			lastMerge = at
		}
		for _, v := range d.j.Unknowns() {
			if d.ctx.Err() != nil {
				return nil
			}
			if v.Type == approval.Merge && !mergeDue {
				continue
			}
			if h, ok := d.handlers[v.Type].(actionexec.ReadBack); ok && h.Due(v) {
				if err := h.Reconcile(d.ctx, d.j, v.ID); err != nil {
					return err
				}
			}
		}
		select {
		case <-d.ctx.Done():
		case <-t.C:
		}
	}
	return nil
}

func (d *dispatcher) wait() error {
	d.inFlight.Lock()
	defer d.inFlight.Unlock()
	// A signal or other shutdown cause may have won the select before this failure was reported.
	select {
	case err := <-d.failed:
		return err
	default:
		return nil
	}
}
