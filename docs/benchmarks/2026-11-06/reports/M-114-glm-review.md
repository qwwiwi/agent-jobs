[jobs:job-done] job 261106-174247-64ca BENCH-REVIEW M #114 glm (job failed: report lost) succeeded · 15 turns · cost unknown · 3m · model gpt-6.1-sol · effort high

## Not done / uncertain
Go tests and `scripts/check.sh` were not run: package initialization builds a helper binary, and tests create temporary repositories and executables, incompatible with this read-only job. The reported test race was verified independently, not through the repository test binary.

Native macOS execution remains unverified, as already identified in the brief. No files were changed.

## Progress
- 17:43 UTC Inspected the review range and identified concurrency as the risky area.
- 17:43 UTC Traced stream registration, deadline cleanup, and kill/reap ordering.
- 17:44 UTC Reproduced the regression test’s stdin race without filesystem writes.
- 17:44 UTC Checked formatting, diff whitespace, and repository cleanliness.

BENCH-VERDICT: critical=0 high=0 medium=0 low=1 issue_fixed=yes

## Summary
Verdict: SHIP WITH FIXES
Risky areas: concurrency—PID reservation, pipe lifetime, and kill/reap ordering.
Reviewed `8f6e8f9..d3e27ed`. The production fix closes #114; one non-blocking test race remains.

## Findings / Changes

### Delivered
- **closed:** Manually consumed stdout participates in pre-reap completion (`internal/jobs/gate.go:521`, `internal/jobs/gitrun.go:193`).
- **closed:** The deadline signals the process group before releasing the tracked stream (`internal/jobs/gate.go:534`, `internal/jobs/gate.go:553`).
- **closed:** The requested child-survival regression test and timeout assertion were added (`internal/jobs/git_timeout_test.go:295`).

### Design
The implementation follows the requested design: register streams before starting Git, reserve the leader PID while consumption continues, and release it after completion or group signalling. Error cleanup signals the group before closing the tracking channel.

### Code
**Low — `internal/jobs/git_timeout_test.go:306`: fake cat-file can exit before the first request is written.** With noninteractive `/bin/sh`, background `sleep` gets `/dev/null` as stdin. If the shell exits before `materializeTree` reaches `fmt.Fprintln` (`internal/jobs/gate.go:562`), that write returns EPIPE while the child still holds stdout. Cleanup then returns the write error, failing the timeout assertion at line 319 despite a correct implementation.

Verified the shell/pipe behavior with an in-memory reproducer, then traced the error path through `gate.go:545` and `gate.go:555`. Minimal fix: have the fake read its first request before spawning the child and exiting. The brief explicitly requested this regression scenario; no brief gap.

### Blocking
none

### Non-blocking
- `internal/jobs/git_timeout_test.go:306`: synchronize fake exit with receipt of the first request to prevent scheduler-dependent EPIPE failures.

## Evidence
- `git diff 8f6e8f9..HEAD`: four changed files; reviewed all changes and relevant callers, wait implementations, and tests.
- `git diff --check`: passed.
- `gofmt -l` on all four changed files: no output.
- Read-only Python pipe reproduction: `leader_exited: True`, `stdout_still_held: True`, `stdin_write: EPIPE`.
- `git status --short`: clean.
