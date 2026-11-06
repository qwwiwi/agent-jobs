//go:build agentjobs_testhooks

package jobs

import (
	"os"
	"time"
)

// Test-only hook, compiled only with -tags agentjobs_testhooks (never in a release build): the wrapper
// under test runs in its own process, so a test shortens its generation lookup through the environment.
//
//	AGENT_JOBS_TEST_GENERATION_WAIT=<go duration>  bound the lookup at the end of an attempt
func init() {
	if d, err := time.ParseDuration(os.Getenv("AGENT_JOBS_TEST_GENERATION_WAIT")); err == nil && d > 0 {
		generationLookupWait = d
		generationRetryDelay = d / 4
	}
}
