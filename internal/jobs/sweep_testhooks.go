//go:build agentjobs_testhooks

package jobs

import (
	"os"
	"strconv"
	"syscall"
	"time"
)

// Test-only hooks, compiled only with -tags agentjobs_testhooks (never in a release build): they let
// a test run the real wrapper binary against a process the sweep cannot end.
//
//	AGENT_JOBS_TEST_UNKILLABLE=1      every pidfd signal is swallowed (the process "ignores" SIGKILL)
//	AGENT_JOBS_TEST_SWEEP_GRACE_MS=N  SweepGrace in milliseconds
func init() {
	if os.Getenv("AGENT_JOBS_TEST_UNKILLABLE") == "1" {
		pidfdSend = func(int, syscall.Signal) error { return nil }
	}
	if ms, err := strconv.Atoi(os.Getenv("AGENT_JOBS_TEST_SWEEP_GRACE_MS")); err == nil && ms >= 0 {
		SweepGrace = time.Duration(ms) * time.Millisecond
	}
}
