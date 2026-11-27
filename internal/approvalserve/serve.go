// Package approvalserve connects the Go approval journal to the socket and the review pages.
package approvalserve

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/actionexec"
	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/cutover"
	"github.com/CrossTerra/beacon/internal/review"
	"github.com/CrossTerra/beacon/internal/supervisor"
)

type Options struct {
	StateDir      string
	AfterJournal  string // D16: a completed archive's journal id, never imported
	ArchiveSignal *cutover.SignalActor
	Origin        approval.Origin
	Now           func() time.Time
	Notify        func(context.Context, string)
	Diagnostics   io.Writer // fixed-code registration diagnostics, never request content
	// Only list types the live service has configured. Review-only fixtures opt out.
	RequireHandlers bool
	// ConnectRecords can replace the configured delivery connection in tests.
	ConnectRecords approval.ConnectRecords
	// The CLI supplies supervision for the collector and configured runs. Library tests may leave it nil.
	// ConnectJobs hands the supervisor to trusted service code, never to a request.
	Supervision *supervisor.Options
	ConnectJobs func(*supervisor.Supervisor) error
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
	var sup *supervisor.Supervisor
	var supClosed bool
	closeReaper := func() error {
		if sup == nil || supClosed {
			return nil
		}
		supClosed = true
		return sup.Close()
	}
	var startup func(*os.File) error
	if opts.Supervision != nil {
		startup = func(lock *os.File) error {
			var err error
			sup, err = supervisor.Open(ctx, opts.StateDir, lock, *opts.Supervision)
			return err
		}
	}
	j, err := approval.OpenJournalBeforeReplay(opts.StateDir, opts.Now(), opts.AfterJournal, startup)
	defer func() {
		err = errors.Join(err, closeReaper())
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
	var runs *actionexec.RunHandler
	var runFailed <-chan error
	runSettings, err := actionexec.LoadRunSettings(opts.StateDir)
	if err != nil {
		return err
	}
	if sup != nil {
		if err := actionexec.RecoverRunFiles(opts.StateDir); err != nil {
			return err
		}
		if err := j.RecoverRuns(opts.Now()); err != nil {
			return err
		}
		if runSettings != nil {
			runs, err = actionexec.NewRunHandler(ctx, *runSettings, opts.StateDir, sup, opts.Now)
			if err != nil {
				return err
			}
			runFailed = runs.Failed()
		}
	}
	configured, err := actionexec.LoadSettings(opts.StateDir)
	if err != nil {
		return err
	}
	if old, next := opts.ArchiveSignal, configured.Signal; old != nil && next != nil &&
		(old.Actor != next.Actor || old.Origin != next.Origin || old.Person != next.Person) {
		return errors.New("use the same Signal approval-app actor and person as the Swift archive")
	}
	providers, err := actionexec.New(configured, opts.StateDir, opts.Now, opts.ProviderTransport)
	if err != nil {
		return err
	}
	var attribute func(int) (string, error)
	var reaperDone <-chan struct{}
	stopJobs := func() {
		if runs != nil && ctx.Err() != nil {
			runs.BeginShutdown()
		}
		if sup != nil {
			sup.BeginShutdown()
		}
	}
	if sup != nil {
		attribute, reaperDone = sup.Attribute, sup.Done()
		stop := context.AfterFunc(ctx, stopJobs)
		defer stop()
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
	defer desk.CloseChallenges()
	desk.SetState(opts.StateDir)
	desk.SetDiagnostics(opts.Diagnostics)
	var dispatch *dispatcher
	var dispatchFailed <-chan error
	handlers := providers.Handlers()
	if runs != nil {
		handlers[approval.Run] = runs
	}
	if len(handlers) != 0 {
		dispatch = newDispatcher(work, j, opts.Now, handlers)
		dispatchFailed = dispatch.failed
		controls := dispatch.controls(providers)
		if runs != nil {
			controls.CanStopRun, controls.StopRun = runs.CanStop, runs.Stop
			controls.RunModels = runSettings.Models
			for _, c := range runSettings.Checkouts {
				controls.Checkouts = append(controls.Checkouts, review.Checkout(c))
			}
		}
		desk.SetActions(controls)
	}
	connectRecords := opts.ConnectRecords
	if connectRecords == nil && configured.Signal != nil {
		connectRecords = configured.Signal.RecordConnection(opts.StateDir)
	}
	var worker *approval.DeliveryWorker
	var resumeDelivery func()
	if connectRecords != nil {
		worker = approval.NewDeliveryWorker(j, connectRecords)
		resumeDelivery = worker.Resume
	}
	enabled := []approval.Action{approval.Merge, approval.Post, approval.Run}
	if opts.RequireHandlers {
		types := enabled[:0]
		if opts.ArchiveSignal == nil || configured.Signal != nil {
			for _, kind := range enabled {
				if handlers[kind] != nil {
					types = append(types, kind)
				}
			}
		}
		enabled = types
	}
	changes := make(chan struct{}, 1)
	var proposed func()
	if opts.Notify != nil {
		proposed = func() {
			select {
			case changes <- struct{}{}:
			default: // the pending count is read after commits; do not delay the socket
			}
		}
	}
	socket, err := actsocket.Listen(opts.StateDir, j, actsocket.Options{Origin: opts.Origin,
		Enabled: enabled, Now: opts.Now, Proposed: proposed, ResumeDelivery: resumeDelivery,
		Attribute: attribute, IssueOpen: desk.IssueOpen, PlaceSlack: providers.SlackIntake()})
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

	httpd := desk.Server()
	httpd.BaseContext = func(net.Listener) context.Context { return work }
	var tasks sync.WaitGroup
	exits := make(chan error, len(listeners)+5)
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
	start(func() error { return expire(work, j, opts.Now, desk.ExpireChallenges) })
	if opts.Notify != nil {
		start(func() error { return noticeLoop(work, changes, opts.Notify, j, opts.Now) })
	}
	if dispatch != nil {
		start(dispatch.recover)
	}
	if worker != nil {
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
	case err = <-dispatchFailed:
	case err = <-runFailed:
	case err = <-exits:
		if err == nil {
			err = errors.New("review service stopped unexpectedly")
		}
	}

	stopJobs()
	desk.CloseChallenges()
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
	if runs != nil {
		err = errors.Join(err, closeReaper(), runs.Join())
	}
	tasks.Wait()
	close(exits)
	for e := range exits {
		err = errors.Join(err, e)
	}
	return err
}

func expire(ctx context.Context, j *approval.Journal, now func() time.Time, cleanups ...func(time.Time)) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			at := now()
			for _, cleanup := range cleanups {
				cleanup(at)
			}
			if err := j.Expire(at); err != nil {
				return err
			}
		}
	}
}
