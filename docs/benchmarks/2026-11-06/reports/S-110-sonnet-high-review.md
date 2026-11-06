[jobs:job-done] job 261106-133535-b4ee BENCH-REVIEW S #110 sonnet-high succeeded · 8 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain
Runtime and race tests were not run: this read-only sandbox prohibits their temporary-file writes, and daemon tests require unavailable sockets. Startup-failure behavior was verified statically. No files were changed.

## Progress
- 17:13 UTC Inspected the diff and prioritized subprocess cleanup and synchronization.
- 17:13 UTC Traced PID-wait failure, daemon ownership, and helper startup failure.
- 17:14 UTC Completed formatting, whitespace, and worktree checks.

BENCH-VERDICT: critical=0 high=0 medium=0 low=1 issue_fixed=yes

## Summary
Verdict: SHIP WITH FIXES
Risky areas: concurrency and subprocess cleanup.
The fix closes #110: helper cleanup precedes the PID wait and joins the single `Wait()` goroutine. One non-blocking error-path defect exists in the new regression test.

## Findings / Changes

### Delivered
- **Closed:** cleanup registered before the PID-file wait (`internal/jobs/testproc_test.go:83,142`). A failed wait now kills and joins the helper.
- **Closed:** coordinated process waiting (`internal/jobs/testproc_test.go:137–144`). Exactly one goroutine calls `Wait()`; cleanup waits for its completion.

### Design
The implementation follows the requested design. Channel closure synchronizes access to the stored wait error (`internal/jobs/testproc_test.go:85,138–139`).

### Code
**Low — failed helper startup causes a secondary panic.**  
`internal/jobs/testproc_test.go:159–161`: if `c.Start()` fails, `t.Fatal()` ends the subtest before the assignment at line 155 completes. The parent continues with nil `c` and `exited`, waits five seconds, then its cleanup dereferences `c.Process`, panicking and aborting the package.

Verified by tracing the startup-failure and cleanup paths. Minimal fix: return from the parent when `t.Run(...)` returns false.

The regression test otherwise checks cleanup after an early subtest return, which uses the same cleanup mechanism as `t.Fatal()`.

### Blocking
none

### Non-blocking
- `internal/jobs/testproc_test.go:159`: guard unsuccessful subtest completion before registering cleanup that dereferences the helper.

## Evidence
- Reviewed commit `11962db0635beb9d31ab28dfff743635eca0d2eb` against `8f6e8f9e5969b205fa9e400b66d0728ba39602cd`.
- Diff: one file, 53 insertions and seven deletions.
- Inspected helper startup, `TestMain`, `waitFor`, and platform-specific parent-death handling.
- `git diff --check`: passed.
- `gofmt -l internal/jobs/testproc_test.go`: no output.
- `git status --short`: clean.
