//go:build !linux && !darwin

package jobs

import "testing"

// processGone has no process state to read on this system: the test that needs it is skipped rather
// than passing on a check that cannot fail.
func processGone(t *testing.T, pid int) bool {
	t.Helper()
	t.Skipf("no process state check on this system (pid %d)", pid)
	return false
}
