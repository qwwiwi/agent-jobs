package main

import (
	"context"
	"os"
	"time"
)

// kickDebounce is the shortest gap between two ticks a kick may cause (G13): a kick inside it waits for
// the window to end, and all kicks of the window share that one trailing tick.
const kickDebounce = time.Second

// loopClock is the daemon loop's clock; a variable dependency so tests drive the debounce without sleeping.
type loopClock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// wallClock is the real clock.
type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// daemonLoop runs the daemon's ticks until ctx ends: a minute tick at once and on every value of minute
// (tick(true)), and a kick tick on a kick (tick(false), G12). A kick at least kickDebounce after the last
// tick of either kind ticks at once; a kick inside that window schedules one trailing tick at its end,
// and the kicks that follow it until then add nothing (a burst of kicks: at most 2 ticks). Any tick
// covers a trailing tick still waiting: what the kick asked for, that tick did.
func daemonLoop(ctx context.Context, clk loopClock, minute <-chan time.Time, kicks <-chan os.Signal, tick func(minute bool)) {
	var last time.Time
	var trailing <-chan time.Time // nil: no trailing tick scheduled
	run := func(isMinute bool) {
		trailing = nil
		tick(isMinute)
		last = clk.Now()
	}
	run(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-minute:
			run(true)
		case <-kicks:
			if trailing != nil {
				continue // this window's trailing tick is already scheduled
			}
			if wait := kickDebounce - clk.Now().Sub(last); wait > 0 {
				trailing = clk.After(wait)
				continue
			}
			run(false)
		case <-trailing:
			run(false)
		}
	}
}
