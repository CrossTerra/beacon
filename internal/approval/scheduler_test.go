package approval

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

type recordFunc func(context.Context, []byte) RecordAnswer

func (f recordFunc) PostRecord(ctx context.Context, b []byte) RecordAnswer { return f(ctx, b) }

type waitRequest struct {
	delay time.Duration
	end   chan time.Time
}

func workerHarness(t *testing.T, j *Journal, connect ConnectRecords) (*DeliveryWorker, <-chan waitRequest) {
	t.Helper()
	w := NewDeliveryWorker(j, connect)
	timers := make(chan waitRequest, 8)
	w.timer = func(d time.Duration) (<-chan time.Time, func()) {
		ch := make(chan time.Time, 1)
		timers <- waitRequest{d, ch}
		return ch, func() {}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := next(t, done); !errors.Is(err, context.Canceled) {
			t.Errorf("worker exit: %v", err)
		}
	})
	return w, timers
}

func next[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}

func TestWorkerBackoffDoesNotLetNewWorkOvertake(t *testing.T) {
	j := open(t, scratch(t), t0)
	put(t, j, mergeRequest())
	s := signalFake(t)
	s.script[key(j, 1)] = []refusal{{status: 401}, {status: 429, after: "120"}, {status: 503}}
	posts := make(chan []byte, 8)
	transport := recordFunc(func(ctx context.Context, b []byte) RecordAnswer {
		posts <- b
		return s.transport.PostRecord(ctx, b)
	})
	_, timers := workerHarness(t, j, func(context.Context) (SignalIdentity, RecordTransport, error) { return who, transport, nil })
	first := next(t, posts)
	for i, delay := range []time.Duration{30 * time.Second, 2 * time.Minute, 2 * time.Minute} {
		wait := next(t, timers)
		if wait.delay != delay {
			t.Fatalf("retry %d delay %v, want %v", i, wait.delay, delay)
		}
		put(t, j, mergeRequest())
		select {
		case <-posts:
			t.Fatal("new work bypassed the delay")
		default:
		}
		wait.end <- t0
		if b := next(t, posts); !bytes.Equal(b, first) {
			t.Fatal("the new proposal passed the retry or changed the body's bytes")
		}
	}
	if b := next(t, posts); bytes.Equal(b, first) {
		t.Fatal("did not advance after acknowledgement")
	}
	if backoff(50) != 15*time.Minute {
		t.Fatal("backoff not capped")
	}
}

func TestResumeDuringDrain(t *testing.T) {
	for _, mode := range []string{"stop", "retry"} {
		t.Run(mode, func(t *testing.T) {
			j := open(t, scratch(t), t0)
			put(t, j, mergeRequest())
			put(t, j, mergeRequest())
			s := signalFake(t)
			posts := make(chan []byte, 4)
			release := make(chan struct{})
			first := true
			transport := recordFunc(func(ctx context.Context, b []byte) RecordAnswer {
				posts <- b
				if first {
					first = false
					select {
					case <-ctx.Done():
						return RecordAnswer{Ambiguous: true}
					case <-release:
					}
					if mode == "stop" {
						return RecordAnswer{Stopped: "conflict:transition"}
					}
					return RecordAnswer{RetryAfter: 10 * time.Minute}
				}
				return s.transport.PostRecord(ctx, b)
			})
			w, _ := workerHarness(t, j, func(context.Context) (SignalIdentity, RecordTransport, error) { return who, transport, nil })
			b := next(t, posts)
			w.Resume()
			close(release)
			if !bytes.Equal(b, next(t, posts)) || bytes.Equal(b, next(t, posts)) {
				t.Fatal("resume during a drain was lost or sent out of order")
			}
		})
	}
}

func TestStoppedWorkerRequiresResumeAndWaitsForConnection(t *testing.T) {
	j := open(t, scratch(t), t0)
	put(t, j, mergeRequest())
	s := signalFake(t)
	s.script[key(j, 1)] = []refusal{{status: 409, code: "conflict", reason: "transition"}}
	out, err := j.Drain(t.Context(), who, s.transport)
	must(t, err)
	if out.Stopped != "conflict:transition" {
		t.Fatal("did not stop on transition")
	}
	connected := make(chan struct{}, 4)
	ready := false
	w, timers := workerHarness(t, j, func(context.Context) (SignalIdentity, RecordTransport, error) {
		connected <- struct{}{}
		if !ready {
			return SignalIdentity{}, nil, errors.New("no bound key")
		}
		return who, s.transport, nil
	})
	put(t, j, mergeRequest())
	if j.DeliveryStatus().Stopped != out.Stopped {
		t.Fatal("extra proposals cleared a refusal")
	}
	w.Resume()
	next(t, connected)
	wait := next(t, timers)
	if wait.delay != 30*time.Second || j.DeliveryStatus().Waiting != 2 {
		t.Fatal("missing connection did not pause delivery")
	}
	ready = true
	wait.end <- t0
	next(t, connected)
}
