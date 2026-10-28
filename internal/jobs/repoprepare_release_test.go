//go:build !agentjobs_testhooks

package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const prepareHooksChildEnv = "AGENT_JOBS_TEST_PREPARE_HOOKS_CHILD"

// A build without the agentjobs_testhooks tag (a release build, and this test binary) ignores the
// prepare step's test hooks: with AGENT_JOBS_TEST_PREPARE_RECORD_FAULT and
// AGENT_JOBS_TEST_PREPARE_TIMEOUT_MS set when it starts, no record fails and the step keeps its own
// time limit. The hooks are read at init, so a child of this test binary starts with them set.
func TestPrepareTestHooksIgnoredWithoutTag(t *testing.T) {
	if os.Getenv(prepareHooksChildEnv) == "1" {
		if prepareRecordFault != "" || prepareTimeoutOverride != 0 ||
			injectedPrepareRecordFault(PrepareResult{Failure: prepareUnfinished}) != nil {
			fmt.Printf("prepare hooks set: fault %q, timeout %s\n", prepareRecordFault, prepareTimeoutOverride)
			os.Exit(3)
		}
		fmt.Println("prepare hooks unset")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrepareTestHooksIgnoredWithoutTag$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), prepareHooksChildEnv+"=1",
		"AGENT_JOBS_TEST_PREPARE_RECORD_FAULT=all", "AGENT_JOBS_TEST_PREPARE_TIMEOUT_MS=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "prepare hooks unset") {
		t.Fatalf("untagged build with the prepare hooks' env: %v\n%s", err, out)
	}
}
