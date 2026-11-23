package approvalserve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
)

var proposedAt = time.Unix(1_800_000_000, 0).UTC()

func options(t *testing.T) Options {
	t.Helper()
	dir, err := os.MkdirTemp("", "bsv-") // leave room for run/act.sock on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	origin, err := approval.ParseOrigin(approval.LocalOrigin, "")
	if err != nil {
		t.Fatal(err)
	}
	return Options{StateDir: dir, Origin: origin, Now: func() time.Time { return proposedAt },
		ListenReview: func() ([]net.Listener, error) {
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			return []net.Listener{l}, nil
		}}
}

func fixtureProposal(t *testing.T, j *approval.Journal) approval.Proposal {
	t.Helper()
	p, err := j.Add(approval.Request{Type: approval.Post, Agent: "fixture", Reason: "Fixture draft.",
		Params: approval.Object{"workspace": "T1", "channel": "C1", "thread_ts": nil, "text": "Synthetic message."}},
		approval.PeerLabel{}, proposedAt)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func start(t *testing.T, opts Options) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("service did not stop")
		}
	})
	t.Cleanup(stop)
	return stop
}

func TestServiceResumesInterruptedRecordAndDeliversExpiryWithoutARead(t *testing.T) {
	opts := options(t)
	var advance atomic.Bool
	opts.Now = func() time.Time {
		if advance.Load() {
			return proposedAt.Add(approval.Lifetime)
		}
		return proposedAt
	}
	j, err := approval.OpenJournal(opts.StateDir, proposedAt, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	p, journalID := fixtureProposal(t, j), j.ID()
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	posts := make(chan []byte, 4)
	var calls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/app-records" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("unexpected request to fake Signal")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		n := calls.Add(1)
		select {
		case posts <- b:
		case <-r.Context().Done():
			return
		}
		if n == 1 {
			<-r.Context().Done() // fake stored it, but never answers before shutdown
			return
		}
		if n > 2 {
			w.WriteHeader(http.StatusCreated)
		}
		fmt.Fprintf(w, `{"event_id":"evt_01m6a6ft00e40r2081040g2081","replayed":%t}`, n == 2)
	}))
	t.Cleanup(fake.Close)
	who := approval.SignalIdentity{Person: "act_01m6a6ft00e40r2081040g2081", Handle: "winton"}
	transport, err := approval.NewSignalRecords(fake.URL, func(h http.Header) { h.Set("Authorization", "Bearer fixture") })
	if err != nil {
		t.Fatal(err)
	}
	opts.ConnectRecords = func(context.Context) (approval.SignalIdentity, approval.RecordTransport, error) {
		return who, transport, nil
	}
	expectPost := func(seq, kind string) []byte {
		t.Helper()
		select {
		case b := <-posts:
			r, err := approval.ParseObject(b)
			m, _ := r["metadata"].(approval.Object)
			if err != nil || r["key"] != "beacon:"+journalID+":"+seq || r["type"] != "app.action."+kind ||
				r["subject"] != "signal:actor:"+who.Person || m["proposal"] != p.ID {
				t.Fatalf("unexpected fixture record: %s (%v)", b, err)
			}
			return b
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s record delivered", kind)
			return nil
		}
	}

	stop := start(t, opts)
	first := expectPost("1", "proposed")
	other := opts
	other.ListenReview = func() ([]net.Listener, error) { panic("a second instance must not bind") }
	if err := Run(t.Context(), other); !errors.Is(err, approval.ErrLocked) {
		t.Fatalf("second instance: %v", err)
	}
	stop()
	stop = start(t, opts)
	if replay := expectPost("1", "proposed"); !bytes.Equal(first, replay) {
		t.Fatal("restart changed an ambiguous POST")
	}
	advance.Store(true)
	if b := expectPost("2", "expired"); !bytes.Contains(b, []byte(`"reason":"proposal_expired"`)) {
		t.Fatal("expiry record has wrong reason")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	b, err := actsocket.Call(ctx, opts.StateDir, approval.Object{"op": "wait", "id": p.ID, "after": p.Version, "timeout": 300}, 300*time.Second)
	if err != nil || !strings.Contains(string(b), `"status":"expired","version":2`) {
		t.Fatalf("wait after expiry: %s, %v", b, err)
	}
	stop()
}

func TestExpirySweepNotifiesRegisteredWaiters(t *testing.T) {
	opts := options(t)
	j, err := approval.OpenJournal(opts.StateDir, proposedAt, "")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	p := fixtureProposal(t, j)
	_, changed, err := j.Get(p.ID, proposedAt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- expire(ctx, j, func() time.Time { return p.ExpiresAt }) }()
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Error("expiry did not wake a registered waiter")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Use the old time so this read can't expire the proposal itself.
	v, _, err := j.Get(p.ID, proposedAt)
	if err != nil || v.Status != "expired" || v.Version != p.Version+1 {
		t.Fatalf("sweep did not commit expiry: %s, %v", v.Status, err)
	}
}

func TestSocketStartupFailureClosesReviewAndReleasesLock(t *testing.T) {
	opts := options(t)
	run := filepath.Join(opts.StateDir, "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(run, "act.sock")
	if err := os.WriteFile(sock, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	opts.ListenReview = func() ([]net.Listener, error) { return []net.Listener{l}, nil }
	if err := Run(t.Context(), opts); !errors.Is(err, approval.ErrPrivate) {
		t.Fatalf("attached to unsafe socket: %v", err)
	}
	if err := l.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("startup left an open review listener: %v", err)
	}
	if b, err := os.ReadFile(sock); err != nil || string(b) != "not a socket" {
		t.Fatalf("startup replaced the refused file: %v", err)
	}
	lock, err := approval.LockInstance(opts.StateDir)
	if err != nil {
		t.Fatal("startup leaked the instance lock:", err)
	}
	approval.CloseInstance(opts.StateDir, lock)
}
