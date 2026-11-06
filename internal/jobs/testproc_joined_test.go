package jobs

import (
	"os/exec"
	"testing"
	"time"
)

// #110: a child started with startJoined is killed and joined when its test ends early, before the
// test reaches its own kill or wait (a waitFor that fails right after the start).
func TestStartJoinedEndsChildOnEarlyExit(t *testing.T) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	var exited <-chan struct{}
	var werr func() error
	t.Run("early-exit", func(t *testing.T) {
		exited, werr = startJoined(t, exec.Command(sleepBin, "120"))
		// The subtest returns here without a kill or a wait of its own, as a failed waitFor
		// (t.Fatal, runtime.Goexit) would; only the cleanups run.
	})
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the child outlived its test: no cleanup killed and joined it")
	}
	if werr() == nil {
		t.Fatal("the child exited cleanly; it must have been killed")
	}
}
