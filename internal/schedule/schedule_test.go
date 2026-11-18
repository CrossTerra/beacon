package schedule

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSlept(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		wall      time.Duration
		mono      time.Duration
		want      time.Duration
		threshold time.Duration
	}{
		{"awake", 5 * time.Second, 5 * time.Second, 0, 30 * time.Second},
		{"a long pass is not a sleep", 10 * time.Minute, 10 * time.Minute, 0, 30 * time.Second},
		{"asleep for eight hours", 8*time.Hour + 5*time.Second, 5 * time.Second, 8 * time.Hour, 30 * time.Second},
		{"a short nap under the threshold", 25 * time.Second, 5 * time.Second, 0, 30 * time.Second},
		{"the wall clock set back", -time.Hour, 5 * time.Second, 0, 30 * time.Second},
	}
	for _, c := range cases {
		if got := Slept(t0, time.Minute, t0.Add(c.wall), time.Minute+c.mono, c.threshold); got != c.want {
			t.Errorf("%s: slept %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeTimers hands out timers that fire only when the test says so.
type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
	added  chan struct{}
}

type fakeTimer struct {
	d    time.Duration
	c    chan time.Time
	done bool
}

func newFakeTimers() *fakeTimers { return &fakeTimers{added: make(chan struct{}, 1000)} }

func (f *fakeTimers) timer(d time.Duration) (<-chan time.Time, func()) {
	t := &fakeTimer{d: d, c: make(chan time.Time, 1)}
	f.mu.Lock()
	f.timers = append(f.timers, t)
	f.mu.Unlock()
	f.added <- struct{}{}
	return t.c, func() {
		f.mu.Lock()
		t.done = true
		f.mu.Unlock()
	}
}

// fire fires every live timer of duration d.
func (f *fakeTimers) fire(d time.Duration) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.timers {
		if !t.done && t.d == d {
			t.done = true
			t.c <- time.Time{}
			n++
		}
	}
	return n
}

// waitLive waits until n live timers of duration d exist.
func (f *fakeTimers) waitLive(t *testing.T, d time.Duration, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		live := 0
		for _, tm := range f.timers {
			if !tm.done && tm.d == d {
				live++
			}
		}
		f.mu.Unlock()
		if live >= n {
			return
		}
		select {
		case <-f.added:
		case <-deadline:
			t.Fatalf("%d live timers of %v, want %d", live, d, n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type passLog struct {
	mu     sync.Mutex
	passes []string
	woke   map[string]int
}

func (p *passLog) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.passes {
		if s == name {
			n++
		}
	}
	return n
}

// A slow source does not delay another: each loop waits only its own interval.
func TestLoopsAreIndependent(t *testing.T) {
	ft := newFakeTimers()
	log := &passLog{woke: map[string]int{}}
	release := make(chan struct{})
	stop := make(chan struct{})
	s := &Scheduler{
		Loops: []Loop{{"slack", 15 * time.Second}, {"github", 60 * time.Second}},
		Pass: func(ctx context.Context, name string, woke bool) (Result, error) {
			log.mu.Lock()
			log.passes = append(log.passes, name)
			log.mu.Unlock()
			if name == "github" {
				// GitHub's first pass hangs until the test lets it go.
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return Result{}, nil
		},
		Stop:  stop,
		Timer: ft.timer,
		Clock: func() (time.Time, time.Duration) { return time.Time{}, 0 },
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	for i := 1; i <= 3; i++ {
		ft.waitLive(t, 15*time.Second, 1)
		ft.fire(15 * time.Second)
		deadline := time.Now().Add(5 * time.Second)
		for log.count("slack") < i+1 {
			if time.Now().After(deadline) {
				t.Fatalf("slack made %d passes while github was slow", log.count("slack"))
			}
			time.Sleep(time.Millisecond)
		}
	}
	if log.count("github") != 1 {
		t.Fatalf("github passes %d", log.count("github"))
	}
	close(release)
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// After a sleep every loop makes a pass at once, says it followed a wake, and the wake is reported once.
func TestWakeRunsEverySource(t *testing.T) {
	ft := newFakeTimers()
	var clockMu sync.Mutex
	wall := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	var mono time.Duration
	log := &passLog{woke: map[string]int{}}
	var wakes [][2]time.Time
	stop := make(chan struct{})
	s := &Scheduler{
		Loops: []Loop{{"slack", 15 * time.Second}, {"calendar", 120 * time.Second}},
		Pass: func(_ context.Context, name string, woke bool) (Result, error) {
			log.mu.Lock()
			log.passes = append(log.passes, name)
			if woke {
				log.woke[name]++
			}
			log.mu.Unlock()
			return Result{}, nil
		},
		Woke: func(from, to time.Time) {
			log.mu.Lock()
			wakes = append(wakes, [2]time.Time{from, to})
			log.mu.Unlock()
		},
		Stop:  stop,
		Timer: ft.timer,
		Clock: func() (time.Time, time.Duration) {
			clockMu.Lock()
			defer clockMu.Unlock()
			return wall, mono
		},
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	ft.waitLive(t, 15*time.Second, 1)
	ft.waitLive(t, 120*time.Second, 1)
	ft.waitLive(t, DefaultCheck, 1)
	// Eight hours asleep: the wall clock moves, the monotonic clock five seconds.
	clockMu.Lock()
	wall = wall.Add(8*time.Hour + 5*time.Second)
	mono += 5 * time.Second
	clockMu.Unlock()
	ft.fire(DefaultCheck)
	deadline := time.Now().Add(5 * time.Second)
	for {
		log.mu.Lock()
		ok := log.woke["slack"] == 1 && log.woke["calendar"] == 1
		log.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("passes %v woke %v", log.passes, log.woke)
		}
		time.Sleep(time.Millisecond)
	}
	if log.count("calendar") != 2 || log.count("slack") != 2 {
		t.Fatalf("passes %v", log.passes)
	}
	log.mu.Lock()
	if len(wakes) != 1 || wakes[0][1].Sub(wakes[0][0]) != 8*time.Hour {
		t.Fatalf("wakes %v", wakes)
	}
	log.mu.Unlock()
	// An ordinary check after that is not another wake.
	clockMu.Lock()
	wall = wall.Add(5 * time.Second)
	mono += 5 * time.Second
	clockMu.Unlock()
	ft.waitLive(t, DefaultCheck, 1)
	ft.fire(DefaultCheck)
	ft.waitLive(t, DefaultCheck, 1)
	log.mu.Lock()
	if len(wakes) != 1 {
		t.Fatalf("wakes %v", wakes)
	}
	log.mu.Unlock()
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A pass that fails right after a wake is tried again soon, not a whole interval later.
func TestFailedPassAfterWakeRetriesSoon(t *testing.T) {
	ft := newFakeTimers()
	var clockMu sync.Mutex
	wall := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	var mono time.Duration
	log := &passLog{woke: map[string]int{}}
	stop := make(chan struct{})
	s := &Scheduler{
		Loops: []Loop{{"calendar", 120 * time.Second}},
		Pass: func(_ context.Context, name string, woke bool) (Result, error) {
			log.mu.Lock()
			log.passes = append(log.passes, name)
			log.mu.Unlock()
			return Result{Failed: woke}, nil
		},
		Stop:  stop,
		Timer: ft.timer,
		Clock: func() (time.Time, time.Duration) {
			clockMu.Lock()
			defer clockMu.Unlock()
			return wall, mono
		},
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	ft.waitLive(t, 120*time.Second, 1)
	ft.waitLive(t, DefaultCheck, 1)
	clockMu.Lock()
	wall = wall.Add(time.Hour)
	clockMu.Unlock()
	ft.fire(DefaultCheck)
	ft.waitLive(t, DefaultRetry, 1)
	ft.fire(DefaultRetry)
	ft.waitLive(t, 120*time.Second, 1)
	if n := log.count("calendar"); n != 3 {
		t.Fatalf("passes %d", n)
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A pass error stops every loop and is what Run returns.
func TestPassErrorStopsEveryLoop(t *testing.T) {
	boom := errors.New("boom")
	ft := newFakeTimers()
	s := &Scheduler{
		Loops: []Loop{{"slack", 15 * time.Second}, {"gmail", 30 * time.Second}},
		Pass: func(ctx context.Context, name string, _ bool) (Result, error) {
			if name == "gmail" {
				return Result{}, boom
			}
			<-ctx.Done()
			return Result{}, nil
		},
		Timer: ft.timer,
		Clock: func() (time.Time, time.Duration) { return time.Time{}, 0 },
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// Stop ends every loop after the pass it is making, and none starts another.
func TestStopWaitsForThePassUnderWay(t *testing.T) {
	ft := newFakeTimers()
	stop := make(chan struct{})
	started := make(chan struct{})
	release := make(chan struct{})
	var passes int
	var mu sync.Mutex
	s := &Scheduler{
		Loops: []Loop{{"slack", 15 * time.Second}},
		Pass: func(context.Context, string, bool) (Result, error) {
			mu.Lock()
			passes++
			mu.Unlock()
			close(started)
			<-release
			return Result{}, nil
		},
		Stop:  stop,
		Timer: ft.timer,
		Clock: func() (time.Time, time.Duration) { return time.Time{}, 0 },
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	<-started
	close(stop)
	select {
	case <-done:
		t.Fatal("Run returned before the pass finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if passes != 1 {
		t.Fatalf("passes %d", passes)
	}
}
