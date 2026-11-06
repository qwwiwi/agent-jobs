[jobs:job-done] job 261106-134543-b6f6 BENCH-REVIEW S #110 opus-high succeeded · 21 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain
Runtime tests and race detection were not run. The read-only sandbox prevents Go build artifacts and the temporary files created by this package’s `TestMain`; the daemon regression also exercises sockets unavailable here.

Recommended outside this sandbox: run both `TestStartJoinedEndsChildOnEarlyExit` and `TestStartedDaemonNeverOutlivesItsTest` repeatedly with `-race`, followed by `scripts/check.sh`. No files were changed or committed.

## Progress
- 17:15 UTC Inspected the requested diff and identified process cleanup and synchronization as the risky areas.
- 17:15 UTC Traced helper startup, cleanup ordering, wait ownership, and daemon parent-death protection.
- 17:16 UTC Checked regression coverage, formatting, diff whitespace, and worktree cleanliness; completed the review.

BENCH-VERDICT: critical=0 high=0 medium=0 low=0 issue_fixed=yes

## Summary
Verdict: SHIP
Risky areas: concurrency and child-process cleanup.
Reviewed `8f6e8f9..00192e6` for issue #110. The fix closes the helper leak when PID waiting fails. No findings.

## Findings / Changes

### Delivered
- **Closed:** Cleanup is registered before PID waiting begins: `internal/jobs/testproc_test.go:104` calls `startJoined`, which registers kill-and-join cleanup at `:55`, before `waitFor` at `:107`.
- **Closed:** Early termination kills and joins the helper. Cleanup waits on the same completion channel used by the normal path (`internal/jobs/testproc_test.go:55–59`).
- **Closed:** The hanging helper’s daemon retains its existing Linux parent-death protection (`internal/jobs/testproc_test.go:80`, `internal/jobs/testproc_linux_test.go:17`).

### Design
Commit `00192e6` follows the requested approach: immediate cleanup registration after successful startup, coordinated with the existing asynchronous wait. Changes remain confined to test code.

### Code
No verified defects. `cmd.Wait()` has one caller (`internal/jobs/testproc_test.go:52`). Closing `done` synchronizes publication of the wait error; cleanup and the normal path can both observe completion safely.

The added regression at `internal/jobs/testproc_joined_test.go:11` exercises early subtest return without an explicit kill or wait, then checks completion and abnormal exit. This covers the cleanup mechanism also invoked by `t.Fatal`.

### Blocking
none

### Non-blocking
none

## Evidence
- Read the complete diff, both changed files, platform-specific parent-death helpers, and relevant `TestMain`, `newManager`, and `waitFor` implementations.
- `git diff --check 8f6e8f9e5969b205fa9e400b66d0728ba39602cd..HEAD`: passed.
- `gofmt -l internal/jobs/testproc_test.go internal/jobs/testproc_joined_test.go`: no output; passed.
- `git status --short`: clean.
- Conclusions about process cleanup and synchronization were verified by source inspection.
