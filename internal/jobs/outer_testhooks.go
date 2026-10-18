//go:build agentjobs_testhooks

package jobs

import (
	"os"
	"strconv"
	"strings"
)

// Test-only hook, compiled only with -tags agentjobs_testhooks (never in a release build): the binary
// under test ignores the wrapper of the job that runs the test suite, so this repository's own tests
// can run inside a job working on it.
//
//	AGENT_JOBS_TEST_OUTER_WRAPPER=PID[,PID…]  wrappers (and their sessions and scopes) not counted
func init() {
	for _, f := range strings.Split(os.Getenv("AGENT_JOBS_TEST_OUTER_WRAPPER"), ",") {
		if pid, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && pid > 1 {
			outerWrappers = append(outerWrappers, pid)
		}
	}
}
