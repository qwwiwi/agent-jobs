# Issue #110: Test TestStartedDaemonNeverOutlivesItsTest can leak its helper if the PID wait fails

Follow-up from the final Codex review of #105 (commit 994dde2), out of the fix-loop budget.

internal/jobs/testproc_test.go:82-99: the regression test starts helper `c`, then waits for its PID file before registering cleanup. If `waitFor` fails, nothing kills and joins the helper; in `hang` mode the helper and its daemon survive the subtest until the two-minute sleep ends.

Fix: register the helper cleanup right after a successful `c.Start()`, before any wait, coordinated with the existing `c.Wait()` goroutine.

---
## Review task (benchmark)
Review the change on branch `bench/110-haiku-high` (diff `8f6e8f9e5969b205fa9e400b66d0728ba39602cd..HEAD` in this worktree) as the fix for the issue above.
Judge correctness, whether the issue is fully fixed, regressions, test quality. Do not modify files.
End the report with exactly one line in this form, counts of findings by severity:
BENCH-VERDICT: critical=<n> high=<n> medium=<n> low=<n> issue_fixed=<yes|partly|no>
