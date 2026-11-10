package main

import (
	"context"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeClock is the daemon loop's clock in tests: Now moves only on advance, and every After call is
// handed to the test (afters), which fires it when it wants.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	afters chan fakeAfter
}

type fakeAfter struct {
	d time.Duration
	c chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), afters: make(chan fakeAfter, 64)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	f.afters <- fakeAfter{d: d, c: c}
	return c
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// loopHarness runs daemonLoop in the background with a fake clock, a minute channel and a kick channel
// the test drives; every tick lands in ticks (true: a minute tick).
type loopHarness struct {
	clk    *fakeClock
	minute chan time.Time
	kicks  chan os.Signal
	ticks  chan bool
	cancel context.CancelFunc
	done   chan struct{}
}

func startLoop(t *testing.T) *loopHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &loopHarness{clk: newFakeClock(), minute: make(chan time.Time), kicks: make(chan os.Signal),
		ticks: make(chan bool, 256), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		daemonLoop(ctx, h.clk, h.minute, h.kicks, func(minute bool) { h.ticks <- minute })
	}()
	t.Cleanup(h.stop)
	if !h.nextTick(t) {
		t.Fatal("the first tick at start must be a full (minute) tick")
	}
	return h
}

func (h *loopHarness) stop() {
	h.cancel()
	<-h.done
}

func (h *loopHarness) nextTick(t *testing.T) bool {
	t.Helper()
	select {
	case m := <-h.ticks:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no tick")
	}
	return false
}

func (h *loopHarness) nextAfter(t *testing.T) fakeAfter {
	t.Helper()
	select {
	case a := <-h.clk.afters:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("the loop scheduled no debounce timer")
	}
	return fakeAfter{}
}

// kick hands one SIGUSR1 to the loop; the send returns once the loop has taken it.
func (h *loopHarness) kick(t *testing.T) {
	t.Helper()
	select {
	case h.kicks <- syscall.SIGUSR1:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop does not take kicks")
	}
}

// stopAndCount ends the loop and counts the ticks it ran since the last read.
func (h *loopHarness) stopAndCount() (minute, kick int) {
	h.stop()
	for {
		select {
		case m := <-h.ticks:
			if m {
				minute++
			} else {
				kick++
			}
		default:
			return minute, kick
		}
	}
}

// A kick long after the last tick ticks at once: no debounce wait, no timer (G12).
func TestDaemonLoopKickTicksAtOnce(t *testing.T) {
	h := startLoop(t)
	h.clk.advance(2 * time.Second)
	h.kick(t)
	if h.nextTick(t) {
		t.Fatal("a kick must run a kick tick, not a minute tick")
	}
	if n := len(h.clk.afters); n != 0 {
		t.Fatalf("a kick past the debounce window scheduled %d timer(s)", n)
	}
	if minute, kick := h.stopAndCount(); minute != 0 || kick != 0 {
		t.Fatalf("extra ticks: minute %d, kick %d", minute, kick)
	}
}

// A burst of 20 kicks runs at most 2 ticks: the first at once, one trailing tick when the 1 s
// debounce window ends (G13).
func TestDaemonLoopKickBurstDebounced(t *testing.T) {
	h := startLoop(t)
	h.clk.advance(5 * time.Second)
	for i := 0; i < 20; i++ {
		h.kick(t)
		h.clk.advance(10 * time.Millisecond)
	}
	if h.nextTick(t) {
		t.Fatal("the first kick of the burst must tick at once")
	}
	a := h.nextAfter(t)
	if a.d <= 0 || a.d > kickDebounce {
		t.Fatalf("debounce wait %s, want within (0, %s]", a.d, kickDebounce)
	}
	if n := len(h.clk.afters); n != 0 {
		t.Fatalf("the burst scheduled %d more timer(s)", n)
	}
	h.clk.advance(time.Second)
	a.c <- h.clk.Now()
	if h.nextTick(t) {
		t.Fatal("the trailing tick must be a kick tick")
	}
	if minute, kick := h.stopAndCount(); minute != 0 || kick != 0 {
		t.Fatalf("a burst of 20 kicks ran %d extra kick tick(s) and %d minute tick(s) past 2", kick, minute)
	}
}

// A kick right after a tick is not lost: it ticks once the debounce window ends.
func TestDaemonLoopKickDuringWindowTrails(t *testing.T) {
	h := startLoop(t)
	h.kick(t) // the clock has not moved since the first tick
	a := h.nextAfter(t)
	if a.d != kickDebounce {
		t.Fatalf("debounce wait %s, want %s", a.d, kickDebounce)
	}
	select {
	case <-h.ticks:
		t.Fatal("a kick inside the debounce window must not tick at once")
	default:
	}
	h.clk.advance(kickDebounce)
	a.c <- h.clk.Now()
	if h.nextTick(t) {
		t.Fatal("the trailing tick must be a kick tick")
	}
}

// The minute tick remains; a minute tick covers a kick waiting for its window, so it adds no tick.
func TestDaemonLoopMinuteTickRemains(t *testing.T) {
	h := startLoop(t)
	h.clk.advance(time.Minute)
	h.minute <- h.clk.Now()
	if !h.nextTick(t) {
		t.Fatal("the minute channel must run a minute tick")
	}
	h.kick(t) // inside the window of the minute tick
	a := h.nextAfter(t)
	h.clk.advance(time.Minute)
	h.minute <- h.clk.Now()
	if !h.nextTick(t) {
		t.Fatal("the minute channel must run a minute tick")
	}
	a.c <- h.clk.Now() // the pending kick's timer fires after the minute tick ran
	if minute, kick := h.stopAndCount(); minute != 0 || kick != 0 {
		t.Fatalf("the minute tick did not absorb the pending kick: minute %d, kick %d", minute, kick)
	}
}

// The loop returns when its context ends.
func TestDaemonLoopStopsOnContext(t *testing.T) {
	h := startLoop(t)
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not return after its context ended")
	}
}
