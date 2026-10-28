//go:build agentjobs_testhooks

package jobs

import "os"

// Test-only hook, compiled only with -tags agentjobs_testhooks (never in a release build): the wrapper
// fails to record the named prepare snapshot, for the fail-closed tests of the prepare step.
//
//	AGENT_JOBS_TEST_PREPARE_RECORD_FAULT=unfinished|passed
func init() {
	prepareRecordFault = os.Getenv("AGENT_JOBS_TEST_PREPARE_RECORD_FAULT")
}
