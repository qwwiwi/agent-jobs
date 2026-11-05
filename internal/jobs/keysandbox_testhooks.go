//go:build agentjobs_testhooks

package jobs

import "os"

// Test-only hook, compiled only with -tags agentjobs_testhooks (never in a release build): the tests'
// OpenRouter key files live in their temp dirs, which wrapperKeySandbox refuses as a placement.
// Landlock itself still applies.
//
//	AGENT_JOBS_TEST_KEY_PLACEMENT_ANY=1  skip the key directory placement check
func init() {
	if os.Getenv("AGENT_JOBS_TEST_KEY_PLACEMENT_ANY") == "1" {
		keyPlacementCheck = false
	}
}
