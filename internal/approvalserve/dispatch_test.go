package approvalserve

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/actionexec"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
)

type stubHandler struct {
	run func(context.Context, *approval.Journal, approval.View, keychain.Secret) error
}

func (stubHandler) Resolve(context.Context, approval.Proposal) (approval.Resolved, error) {
	return approval.Resolved{}, nil
}

func (stubHandler) Unlock(*approval.Frozen, []byte) (keychain.Secret, error) {
	return keychain.Secret{}, nil
}

func (s stubHandler) Run(ctx context.Context, j *approval.Journal, v approval.View, token keychain.Secret) error {
	return s.run(ctx, j, v, token)
}

func TestDispatchPropagatesAFailedStopRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		ret  error
		want error
	}{
		{"fresh journal failure", approval.ErrJournalIO, approval.ErrJournalIO},
		{"already decided", approval.ErrTransition, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "bdp-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			at := time.Unix(1_800_000_000, 0)
			j, err := approval.OpenJournal(dir, at, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { j.Close() })
			p, err := j.Add(approval.Request{Type: approval.Merge, Agent: "fixture", Reason: "fixture", Params: approval.Object{
				"repo": "acme/app", "number": int64(1), "base": "main", "head": strings.Repeat("a", 40), "method": "squash",
			}}, approval.PeerLabel{}, at)
			if err != nil {
				t.Fatal(err)
			}
			f, err := approval.Freeze(p, approval.Binding{Origin: "https://api.github.com", Account: "github:user:1",
				Destination: approval.Object{"repository_id": int64(1), "pr": int64(1), "base": "main"}}, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := j.ApproveAll([]approval.Frozen{f}, []byte("cred"), nil, at); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h := stubHandler{run: func(context.Context, *approval.Journal, approval.View, keychain.Secret) error {
				cancel()
				return tc.ret
			}}
			d := newDispatcher(ctx, j, func() time.Time { return at }, map[approval.Action]actionexec.Handler{approval.Merge: h})
			err = d.dispatch([]readyAction{{id: p.ID, h: h}})
			if tc.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("dispatch = %v, want %v", err, tc.want)
			}
		})
	}
}

type quotaHandler struct {
	budget int
	used   int
	order  []string
}

func (quotaHandler) Resolve(context.Context, approval.Proposal) (approval.Resolved, error) {
	return approval.Resolved{}, nil
}

func (quotaHandler) Unlock(*approval.Frozen, []byte) (keychain.Secret, error) {
	return keychain.Secret{}, nil
}

func (quotaHandler) Run(context.Context, *approval.Journal, approval.View, keychain.Secret) error {
	return nil
}

func (quotaHandler) Reconcile(context.Context, *approval.Journal, string) error { return nil }

func (h *quotaHandler) Due(approval.View) bool { return true }

func (h *quotaHandler) Recover(_ context.Context, _ *approval.Journal, id string) error {
	h.order = append(h.order, id)
	if h.used >= h.budget {
		return actionexec.ErrRecoveryQuota
	}
	h.used++
	return nil
}

func TestQuotaDeniedMergesAreRetriedBeforeOlderUnknowns(t *testing.T) {
	dir, err := os.MkdirTemp("", "bqt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	at := time.Unix(1_800_000_000, 0)
	j, err := approval.OpenJournal(dir, at, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	var ids []string
	for n := int64(1); n <= 3; n++ {
		p, err := j.Add(approval.Request{Type: approval.Merge, Agent: "fixture", Reason: "fixture", Params: approval.Object{
			"repo": "acme/app", "number": n, "base": "main", "head": strings.Repeat("a", 40), "method": "squash",
		}}, approval.PeerLabel{}, at)
		if err != nil {
			t.Fatal(err)
		}
		f, err := approval.Freeze(p, approval.Binding{Origin: "https://api.github.com", Account: "github:user:1",
			Destination: approval.Object{"repository_id": int64(1), "pr": n, "base": "main"}}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := j.Approve(f, at); err != nil {
			t.Fatal(err)
		}
		if _, err := j.Start(p.ID, at); err != nil {
			t.Fatal(err)
		}
		if err := j.Unknown(p.ID, "reply_lost", at); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	if len(j.Unknowns()) != 3 {
		t.Fatal("fixture did not leave three unknown merges", len(j.Unknowns()))
	}
	h := &quotaHandler{budget: 1}
	d := newDispatcher(t.Context(), j, func() time.Time { return at }, map[approval.Action]actionexec.Handler{approval.Merge: h})
	if err := d.recoverPass(); err != nil {
		t.Fatal(err)
	}
	if len(h.order) != 3 || h.order[0] != ids[0] {
		t.Fatalf("first pass order %v, want %v first", h.order, ids[0])
	}
	h.order, h.used = nil, 0
	if err := d.recoverPass(); err != nil {
		t.Fatal(err)
	}
	if len(h.order) < 2 || h.order[0] == ids[0] {
		t.Fatalf("quota-denied merges were not served first: %v", h.order)
	}
}
