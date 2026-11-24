// Package approvalserve connects the Go approval journal to the socket and the review pages.
package approvalserve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/review"
	"github.com/CrossTerra/beacon/internal/supervisor"
)

type Options struct {
	StateDir string
	Origin   approval.Origin
	Now      func() time.Time
	// Only tests supply a Signal connection. The CLI does not have a credential source.
	ConnectRecords approval.ConnectRecords
	// The CLI always supplies supervision, but no live jobs in the preview. Library tests may
	// leave it nil. ConnectJobs hands the supervisor to trusted service code, never to a request.
	Supervision *supervisor.Options
	ConnectJobs func(*supervisor.Supervisor) error
	// Test seam for ephemeral loopback listeners. The CLI uses both fixed D4 addresses.
	ListenReview func() ([]net.Listener, error)
}

func Run(ctx context.Context, opts Options) (err error) {
	if ctx.Err() != nil {
		return nil
	}
	if opts.Origin.Host() == "" {
		return errors.New("no canonical review origin")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ListenReview == nil {
		opts.ListenReview = review.ListenLoopback
	}
	var sup *supervisor.Supervisor
	var startup func(*os.File) error
	if opts.Supervision != nil {
		startup = func(lock *os.File) error {
			var err error
			sup, err = supervisor.Open(ctx, opts.StateDir, lock, *opts.Supervision)
			return err
		}
	}
	j, err := approval.OpenJournalBeforeReplay(opts.StateDir, opts.Now(), "", startup)
	defer func() {
		if sup != nil {
			err = errors.Join(err, sup.Close())
		}
		if j != nil {
			err = errors.Join(err, j.Close())
		}
	}()
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	var attribute func(int) (string, error)
	var reaperDone <-chan struct{}
	if sup != nil {
		attribute, reaperDone = sup.Attribute, sup.Done()
		stopJobs := context.AfterFunc(ctx, sup.BeginShutdown)
		defer stopJobs()
	}
	listeners, err := opts.ListenReview()
	if err != nil {
		return err
	}
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	desk := review.New(opts.Origin, j, opts.Now)
	desk.SetState(opts.StateDir)
	socket, err := actsocket.Listen(opts.StateDir, j, actsocket.Options{Origin: opts.Origin,
		Enabled: []approval.Action{approval.Merge, approval.Post, approval.Run}, Now: opts.Now,
		Attribute: attribute, IssueOpen: desk.IssueOpen})
	if err != nil {
		return err
	}
	defer socket.Close()
	if sup != nil {
		if err := sup.StartFixed(opts.Supervision.Jobs); err != nil {
			return err
		}
		if opts.ConnectJobs != nil {
			if err := opts.ConnectJobs(sup); err != nil {
				return err
			}
		}
	}

	// The parent requests shutdown. Mark the journal stopping before closing listeners or canceling work.
	work, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	httpd := desk.Server()
	httpd.BaseContext = func(net.Listener) context.Context { return work }
	var tasks sync.WaitGroup
	exits := make(chan error, len(listeners)+3)
	start := func(f func() error) {
		tasks.Add(1)
		go func() { defer tasks.Done(); exits <- f() }()
	}
	for _, l := range listeners {
		start(func() error {
			if err := httpd.Serve(l); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}
	start(func() error { return socket.Serve(work) })
	start(func() error { return expire(work, j, opts.Now) })
	if opts.ConnectRecords != nil {
		worker := approval.NewDeliveryWorker(j, opts.ConnectRecords)
		start(func() error {
			err := worker.Run(work)
			if errors.Is(err, context.Canceled) && work.Err() != nil {
				return nil
			}
			return err
		})
	}
	select {
	case <-ctx.Done():
	case <-reaperDone:
		if ctx.Err() == nil {
			err = supervisor.ErrReaper
		}
	case err = <-exits:
		if err == nil {
			err = errors.New("review service stopped unexpectedly")
		}
	}

	if sup != nil {
		sup.BeginShutdown()
	}
	err = errors.Join(err, j.Shutdown(opts.Now()))
	cancel()
	err = errors.Join(err, socket.Close())
	deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if httpErr := httpd.Shutdown(deadline); httpErr != nil {
		err = errors.Join(err, httpErr, httpd.Close())
	}
	tasks.Wait()
	close(exits)
	for e := range exits {
		err = errors.Join(err, e)
	}
	return err
}

func expire(ctx context.Context, j *approval.Journal, now func() time.Time) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := j.Expire(now()); err != nil {
				return err
			}
		}
	}
}
