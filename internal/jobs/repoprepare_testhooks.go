//go:build agentjobs_testhooks

package jobs

import (
	"os"
	"strconv"
	"time"
)

// Test-only hooks, compiled only with -tags agentjobs_testhooks (never in a release build): the wrapper
// fails to record the named prepare snapshot (the fail-closed tests of the prepare step), and the
// prepare step's time limit is milliseconds instead of the config's minutes (its timeout test).
//
//	AGENT_JOBS_TEST_PREPARE_RECORD_FAULT=unfinished|passed
//	AGENT_JOBS_TEST_PREPARE_TIMEOUT_MS=N
func init() {
	prepareRecordFault = os.Getenv("AGENT_JOBS_TEST_PREPARE_RECORD_FAULT")
	if ms, err := strconv.Atoi(os.Getenv("AGENT_JOBS_TEST_PREPARE_TIMEOUT_MS")); err == nil && ms > 0 {
		prepareTimeoutOverride = time.Duration(ms) * time.Millisecond
	}
}
