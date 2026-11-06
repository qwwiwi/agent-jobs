[jobs:job-done] job 261106-140354-7189 BENCH-REVIEW S #110 haiku-high succeeded · 21 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain
Runtime tests, race checks, and `scripts/check.sh` were not run: the sandbox is read-only, and these tests create files and start socket-bound daemons. Findings were verified statically.

Proposed checks: force helper initialization beyond the PID timeout and verify kill/join; run the regression on Darwin and verify no daemon survives.

## Progress
- 17:17 UTC Identified concurrency risks and inspected the complete review diff.
- 17:18 UTC Traced cleanup ordering, channel synchronization, and platform containment.
- 17:18 UTC Completed formatting and whitespace checks; prepared findings.

BENCH-VERDICT: critical=0 high=0 medium=2 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: concurrency and subprocess cleanup.
Reviewed `8f6e8f9..8f591bc`. The original helper cleanup is fixed, but the new regression test introduces two cleanup gaps.

## Findings / Changes

### Delivered
- **Closed:** `internal/jobs/testproc_test.go:113` registers helper cleanup before the PID wait. A failed wait now kills and joins the helper.
- **Closed:** `internal/jobs/testproc_test.go:109` retains one `c.Wait()` caller. Closing `exited` synchronizes reads of `waitErr`.
- **Open:** the new regression test does not guarantee cleanup on every failure path.

### Design
The helper fix follows the requested design. The regression test needs unconditional ownership of its recorded cleanups and must respect platform differences in daemon containment.

### Code
**Medium — regression test repeats the early-failure cleanup gap.**  
`internal/jobs/testproc_test.go:166`: recorded `fake.cleanups` run only after another PID wait succeeds, at line 176. If helper initialization stalls beyond that wait, `t.Fatalf` exits before those cleanups run; the helper remains running, with no registered safety net yet.

Verified by tracing `waitFor` → `Fatalf` and cleanup registration order. Minimal fix: register a real `t.Cleanup` immediately after creating `fake` that executes its recorded cleanups; invoke the same idempotent runner before asserting helper termination. The brief explicitly identified PID-wait failure before cleanup registration.

**Medium — new test leaks its daemon outside Linux.**  
`internal/jobs/testproc_test.go:159`: the unconditional `silent` case starts a daemon, then kills the helper through its recorded cleanup. Outside Linux, `setParentDeathSignal` is a no-op, so killing the helper bypasses its daemon cleanup. The silent PID file contains only the helper PID, leaving the parent unable to kill the daemon. The test can pass while the daemon survives.

Verified against `testproc_other_test.go:8-10`, the daemon start at `testproc_test.go:59-65`, and the existing platform gate at lines 81-82. Minimal fix: restrict this daemon-owning scenario to platforms with parent-death signals, or use a helper-only fixture for this cleanup regression.
Brief gap: the brief did not explicitly require non-Linux regression coverage.

### Blocking
- `internal/jobs/testproc_test.go:166`: **open** — guarantee recorded cleanups run even when the helper PID wait fails.
- `internal/jobs/testproc_test.go:159`: **open** — prevent the unconditional silent scenario from orphaning a daemon outside Linux.

### Non-blocking
none

## Evidence
- Inspected both changed files and supporting process/platform implementations.
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -l internal/jobs/testproc_test.go internal/jobs/jobs_test.go`: no output.
- `git status --short`: clean.
- No files changed.
