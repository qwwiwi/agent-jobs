# Issue #110: Test TestStartedDaemonNeverOutlivesItsTest can leak its helper if the PID wait fails

Follow-up from the final Codex review of #105 (commit 994dde2), out of the fix-loop budget.

internal/jobs/testproc_test.go:82-99: the regression test starts helper `c`, then waits for its PID file before registering cleanup. If `waitFor` fails, nothing kills and joins the helper; in `hang` mode the helper and its daemon survive the subtest until the two-minute sleep ends.

Fix: register the helper cleanup right after a successful `c.Start()`, before any wait, coordinated with the existing `c.Wait()` goroutine.
