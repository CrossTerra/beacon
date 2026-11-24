// Package approvalserve connects the Go approval journal to the socket and the review pages.
package approvalserve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/actionexec"
	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/review"
)

type Options struct {
	StateDir string
	Origin   approval.Origin
	Now      func() time.Time
	// Only tests supply a Signal connection. The CLI does not have a credential source.
	ConnectRecords approval.ConnectRecords
	// The future supervisor can supply run attribution. Nil means all peers are unattributed.
	Attribute func(pid int) string
	// Test seam for ephemeral loopback listeners. The CLI uses both fixed D4 addresses.
	ListenReview func() ([]net.Listener, error)
	// ProviderTransport is a test seam. Production uses a guarded TLS transport, without proxy or redirects.
	ProviderTransport func([]string) http.RoundTripper
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
	j, err := approval.OpenJournal(opts.StateDir, opts.Now(), "")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, j.Close()) }()
	configured, err := actionexec.LoadSettings(opts.StateDir)
	if err != nil {
		return err
	}
	providers, err := actionexec.New(configured, opts.StateDir, opts.Now, opts.ProviderTransport)
	if err != nil {
		return err
	}
	// The parent requests shutdown. Mark the journal stopping before closing listeners or canceling work.
	work, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
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
	var dispatch *dispatcher
	var dispatchFailed <-chan error
	if handlers := providers.Handlers(); len(handlers) != 0 {
		dispatch = newDispatcher(work, j, opts.Now, handlers)
		dispatchFailed = dispatch.failed
		desk.SetActions(dispatch.controls(providers))
	}
	socket, err := actsocket.Listen(opts.StateDir, j, actsocket.Options{Origin: opts.Origin,
		Enabled: []approval.Action{approval.Merge, approval.Post, approval.Run}, Now: opts.Now,
		Attribute: opts.Attribute, IssueOpen: desk.IssueOpen, PlaceSlack: providers.SlackIntake()})
	if err != nil {
		return err
	}

	httpd := desk.Server()
	httpd.BaseContext = func(net.Listener) context.Context { return work }
	var tasks sync.WaitGroup
	exits := make(chan error, len(listeners)+4)
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
	if dispatch != nil {
		start(dispatch.recover)
	}
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
	case err = <-dispatchFailed:
	case err = <-exits:
		if err == nil {
			err = errors.New("review service stopped unexpectedly")
		}
	}

	err = errors.Join(err, j.Shutdown(opts.Now()))
	cancel()
	err = errors.Join(err, socket.Close())
	deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if httpErr := httpd.Shutdown(deadline); httpErr != nil {
		err = errors.Join(err, httpErr, httpd.Close())
	}
	if dispatch != nil {
		err = errors.Join(err, dispatch.wait())
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
