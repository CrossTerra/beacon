package approval

import (
	"context"
	"time"
)

// ConnectRecords acquires the current Signal connection from the credential source. A failure
// (including a locked vault) leaves the outbox pending. The source must honor cancellation.
type ConnectRecords func(context.Context) (SignalIdentity, RecordTransport, error)

// DeliveryWorker serializes draining with timed retries and explicit resume. Only the worker calls Drain
// in a running service. A resume requested during a POST is applied after that drain, even if it stops.
type DeliveryWorker struct {
	journal *Journal
	connect ConnectRecords
	resume  chan struct{}
	timer   func(time.Duration) (<-chan time.Time, func())
}

func NewDeliveryWorker(j *Journal, connect ConnectRecords) *DeliveryWorker {
	return &DeliveryWorker{journal: j, connect: connect, resume: make(chan struct{}, 1), timer: retryTimer}
}

// Resume requests a new drain without waiting out backoff. It coalesces concurrent requests.
func (w *DeliveryWorker) Resume() {
	select {
	case w.resume <- struct{}{}:
	default:
	}
}

// Run is the worker's one loop; run it once per service instance and join it before closing the journal.
func (w *DeliveryWorker) Run(ctx context.Context) error {
	failures := 0
	restarting := false
	for ctx.Err() == nil {
		if restarting {
			if err := w.journal.ResumeDelivery(); err != nil {
				return err
			}
			failures, restarting = 0, false
		}
		changes := w.journal.Changes() // register before reading status so no new work is lost
		if s := w.journal.DeliveryStatus(); s.Waiting == 0 || s.Stopped != "" {
			select {
			case <-ctx.Done():
			case <-changes:
			case <-w.resume:
				restarting = true
			}
			continue
		}

		who, transport, err := w.connect(ctx)
		var out DrainResult
		if err != nil || transport == nil {
			out.Retry = true // there was no POST
		} else {
			out, err = w.journal.Drain(ctx, who, transport)
			if err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			break
		}
		if !out.Retry {
			failures = 0
			continue
		}
		// Incoming proposals do not bypass the deadline, even when they arrive during the drain.
		wait, stop := w.timer(max(out.RetryAfter, backoff(failures)))
		failures = min(5, failures+1)
		select {
		case <-ctx.Done():
		case <-wait:
		case <-w.resume:
			restarting = true
		}
		stop()
	}
	return ctx.Err()
}

func retryTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func backoff(failures int) time.Duration {
	return min(30*time.Second<<min(failures, 5), 15*time.Minute)
}
