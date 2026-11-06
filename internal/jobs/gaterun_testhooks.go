//go:build agentjobs_testhooks

package jobs

import "os"

// Test-only hook, compiled only with -tags agentjobs_testhooks (never in a release build): the tests run
// `agent-jobs gate run` against a lock in their temp dir, so they never wait for (or block) a real gate
// on the box; go test itself may run under one.
//
//	AGENT_JOBS_TEST_GATE_LOCK_DIR=DIR  the box-wide gate lock is DIR/agent-jobs-gate.lock
func init() {
	if d := os.Getenv("AGENT_JOBS_TEST_GATE_LOCK_DIR"); d != "" {
		gateLockDirs = []string{d}
	}
}
