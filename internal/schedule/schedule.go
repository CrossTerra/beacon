// Package schedule runs each source on its own loop (DEC-41). A loop makes a pass, waits its own interval, and makes
// the next; a slow or failing source delays no other. When the machine wakes from sleep, every loop makes a pass at
// once, and the wake is reported as a sleep, not as a long pass.
//
// A sleep is told from a long pass by the two clocks Go keeps: the wall clock runs on while the machine sleeps, and the
// monotonic clock, which timers use, does not (macOS and Linux both). When the wall clock has moved further than the
// monotonic clock by more than Threshold between two checks, the machine slept for the difference.
package schedule

import (
	"context"
	"sync"
	"time"
)

// Loop is one source's loop: its name and how long it waits after a pass before the next.
type Loop struct {
	Name  string
	Every time.Duration
}

// Result is how a pass went, for the wait after it.
type Result struct {
	// Failed is a collect that did not finish. Right after a wake the network may not be up yet, so a pass that
	// follows a wake and fails is tried again after Retry rather than a whole interval later.
	Failed bool
}

// Defaults.
const (
	DefaultCheck     = 5 * time.Second
	DefaultThreshold = 30 * time.Second
	DefaultRetry     = 10 * time.Second
)

// Scheduler runs the loops.
type Scheduler struct {
	Loops []Loop
	// Pass makes one pass of the named source. woke is true for the first pass after a wake. An error stops every
	// loop, and Run returns the first.
	Pass func(ctx context.Context, name string, woke bool) (Result, error)
	// Woke is told of each wake, with when the machine went to sleep and when it woke, before the passes it starts.
	// Nil ignores it.
	Woke func(from, to time.Time)
	// Stop, when closed, ends every loop after the pass it is making. No loop starts another.
	Stop <-chan struct{}
	// Check is how often the clocks are compared, Threshold how far apart they must drift to count as a sleep, and
	// Retry the wait after a failed pass that followed a wake. Zero uses the defaults.
	Check, Threshold, Retry time.Duration
	// Clock returns the wall clock, without its monotonic reading, and the monotonic time since some fixed start. Nil
	// uses the process clock. Tests replace it.
	Clock func() (wall time.Time, mono time.Duration)
	// Timer starts a timer. Nil uses time.NewTimer. Tests replace it.
	Timer func(d time.Duration) (<-chan time.Time, func())
}

// Run runs every loop until ctx ends, Stop is closed, or a pass fails. It returns the first pass error, or nil.
func (s *Scheduler) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		first error
	)
	fail := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
		}
		mu.Unlock()
		cancel()
	}
	wakes := make([]chan struct{}, len(s.Loops))
	for i := range wakes {
		wakes[i] = make(chan struct{}, 1)
	}
	var loops sync.WaitGroup
	for i, l := range s.Loops {
		loops.Add(1)
		go func() {
			defer loops.Done()
			s.loop(ctx, l, wakes[i], fail)
		}()
	}
	done := make(chan struct{})
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		s.watch(ctx, done, wakes)
	}()
	loops.Wait()
	close(done)
	<-watching
	mu.Lock()
	defer mu.Unlock()
	return first
}

func (s *Scheduler) loop(ctx context.Context, l Loop, wake chan struct{}, fail func(error)) {
	woke := false
	for {
		if ctx.Err() != nil || s.stopped() {
			return
		}
		// A wake that came during the pass before is this pass's.
		select {
		case <-wake:
			woke = true
		default:
		}
		res, err := s.Pass(ctx, l.Name, woke)
		if err != nil {
			fail(err)
			return
		}
		wait := l.Every
		if woke && res.Failed {
			wait = min(wait, s.retry())
		}
		woke = false
		c, stop := s.timer(wait)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-s.Stop:
			stop()
			return
		case <-wake:
			stop()
			woke = true
		case <-c:
		}
	}
}

// watch compares the clocks every Check and wakes every loop after a sleep.
func (s *Scheduler) watch(ctx context.Context, done <-chan struct{}, wakes []chan struct{}) {
	lastWall, lastMono := s.clock()
	for {
		c, stop := s.timer(s.check())
		select {
		case <-ctx.Done():
			stop()
			return
		case <-done:
			stop()
			return
		case <-c:
		}
		wall, mono := s.clock()
		slept := Slept(lastWall, lastMono, wall, mono, s.threshold())
		lastWall, lastMono = wall, mono
		if slept == 0 {
			continue
		}
		if s.Woke != nil {
			s.Woke(wall.Add(-slept), wall)
		}
		for _, w := range wakes {
			select {
			case w <- struct{}{}:
			default:
			}
		}
	}
}

// Slept is how long the machine slept between two readings of the clocks: how much further the wall clock moved than
// the monotonic clock, when that is at least threshold, else 0. A wall clock set back is not a sleep.
func Slept(wall0 time.Time, mono0 time.Duration, wall1 time.Time, mono1 time.Duration, threshold time.Duration) time.Duration {
	gap := wall1.Sub(wall0) - (mono1 - mono0)
	if gap < threshold {
		return 0
	}
	return gap
}

func (s *Scheduler) stopped() bool {
	select {
	case <-s.Stop:
		return true
	default:
		return false
	}
}

var start = time.Now()

func (s *Scheduler) clock() (time.Time, time.Duration) {
	if s.Clock != nil {
		return s.Clock()
	}
	now := time.Now()
	return now.Round(0), now.Sub(start)
}

func (s *Scheduler) timer(d time.Duration) (<-chan time.Time, func()) {
	if s.Timer != nil {
		return s.Timer(d)
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func (s *Scheduler) check() time.Duration {
	if s.Check > 0 {
		return s.Check
	}
	return DefaultCheck
}

func (s *Scheduler) threshold() time.Duration {
	if s.Threshold > 0 {
		return s.Threshold
	}
	return DefaultThreshold
}

func (s *Scheduler) retry() time.Duration {
	if s.Retry > 0 {
		return s.Retry
	}
	return DefaultRetry
}
