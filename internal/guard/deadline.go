package guard

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// The hook runner lets a tool call run when its PreToolUse hook times out (fail open), so the guard
// bounds its own work: Decide gives its answer within DecideBudget, refusing the call past it, a Bash
// command longer than maxCommandBytes is refused before it is parsed, and the symlink resolutions of
// one decision are memoized (the same word checked by many rules, against many directories).

// DecideBudget is the most time Decide takes: past it the call is refused as too complex to verify.
// The hook timeout in the job's settings (jobs.GuardHookTimeout) is set well above it.
const DecideBudget = 10 * time.Second

// decideBudget is DecideBudget, a variable for the tests.
var decideBudget = DecideBudget

// maxCommandBytes bounds the Bash command the guard parses and checks.
const maxCommandBytes = 64 << 10

func budgetBlocked() string {
	return "blocked by guard: the call is too complex to verify within the guard's time budget (" +
		decideBudget.String() + "); split it, or write the code to a file in the job's folder and run that."
}

func commandSizeBlocked(n int) string {
	return "blocked by guard: the command is " + strconv.Itoa(n) + " bytes, longer than the " +
		strconv.Itoa(maxCommandBytes) + " bytes the guard verifies; split it, or write the script to a file in the " +
		"job's folder and run that."
}

// resolveMemo caches walkPath results for one decision: the file system does not change while the
// hook runs.
type resolveMemo struct {
	mu sync.Mutex
	m  map[string]walkResult
}

type walkResult struct {
	path string
	ok   bool
}

// activeMemo is the memo of the running decision (nil: none, as for direct Check calls).
var activeMemo atomic.Pointer[resolveMemo]

// startMemo installs a fresh memo and returns the function that removes it, unless a later decision
// installed its own in the meantime (a decision cut off by the deadline may still be running).
func startMemo() func() {
	m := &resolveMemo{m: map[string]walkResult{}}
	activeMemo.Store(m)
	return func() { activeMemo.CompareAndSwap(m, nil) }
}

// memoWalk is walkPath through the active memo.
func memoWalk(abs string) (string, bool) {
	m := activeMemo.Load()
	if m == nil {
		return walkPath(abs)
	}
	m.mu.Lock()
	r, hit := m.m[abs]
	m.mu.Unlock()
	if hit {
		return r.path, r.ok
	}
	p, ok := walkPath(abs)
	m.mu.Lock()
	m.m[abs] = walkResult{p, ok}
	m.mu.Unlock()
	return p, ok
}

// withDeadline runs f and returns its result, or a refusal when it takes longer than decideBudget.
func withDeadline(f func() (string, error)) (string, error) {
	type result struct {
		reason string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		r, err := f()
		done <- result{r, err}
	}()
	timer := time.NewTimer(decideBudget)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.reason, r.err
	case <-timer.C:
		return budgetBlocked(), nil
	}
}
