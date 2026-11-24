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
