[jobs:job-done] job 261106-171300-0e16 BENCH-REVIEW M #114 sonnet-medium succeeded · 15 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain

- Go tests, race checks, and `scripts/check.sh` were not run: they require filesystem writes unavailable in this read-only job.
- macOS runtime behavior remains unverified; the Darwin wait implementation is unchanged.
- The regression’s failing interleaving was verified separately, not reproduced through the compiled Go test.
- No subagents used.

## Progress

- 17:40 UTC Inspected revision range and identified concurrency and process-cleanup risks.
- 17:40 UTC Traced tracked-copy completion, PID reservation, and deferred cleanup.
- 17:41 UTC Examined regression helpers and verified the stdin-exit race with an isolated pipe reproduction.
- 17:41 UTC Completed formatting, whitespace, and clean-worktree checks.

BENCH-VERDICT: critical=0 high=0 medium=1 low=0 issue_fixed=yes

## Summary
Verdict: BLOCK
Reviewed `8f6e8f9..fbcdb87` for issue #114.
The production change closes the descendant-survival path. One Medium finding: the regression test can fail before reaching its timeout because its fake git races the first stdin write.

## Findings / Changes

Risky areas: concurrency and process-group cleanup. Reviewed stream completion, PID reservation, reaping, and deadline handling first. No auth, money, deletion/migrations, secrets, or external-send changes.

### Delivered

- **closed — production fix:** `internal/jobs/gate.go:514` routes stdout through the runner’s tracked copy. A descendant holding stdout keeps `p.copied` pending, preserving the PID reservation until the group is signalled.
- **closed — error cleanup:** `internal/jobs/gate.go:548` closes the consumer pipe before waiting, releasing a copy blocked on writing abandoned output.
- **open — reliable regression coverage:** `internal/jobs/git_timeout_test.go:344` does not synchronize fake-git exit with the first request.

### Design

The change in `fbcdb87` follows the requested pre-reap completion design by using the existing runner protocol. No runner or platform-wait changes were needed.

### Code

**Medium — synchronize the fake git before exiting**  
`internal/jobs/git_timeout_test.go:344`

The fake starts its child and exits without reading stdin. If it exits before `materializeTree` writes its first object request (`internal/jobs/gate.go:560`), that write returns `EPIPE`. Error cleanup immediately kills the group, and the call returns the pipe error before the deadline. The assertion at line 369 then fails despite correct production cleanup.

**Verification:** traced the ordering through the fake and `materializeTree`; an isolated shell/pipe reproduction confirmed `EPIPE` when the fake exits before the request write. The Go test itself was not run.

**Minimal fix:** make the fake read one request with `IFS= read -r obj` before spawning the stdout-holding child and exiting. Then retain both timeout and child-termination assertions.

The brief explicitly required this regression scenario; no brief gap.

### Blocking

- `internal/jobs/git_timeout_test.go:344` — Open: synchronize the first request so the regression reliably reaches the timeout path.

### Non-blocking

none

## Evidence

- Inspected the complete two-file diff and surrounding runner, materialization, timeout-test, and platform-wait code.
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -l internal/jobs/gate.go internal/jobs/git_timeout_test.go`: no output.
- Filesystem-free Python/shell pipe reproduction: `stdin write after fake git exits: EPIPE`.
- `git status --porcelain`: clean. No files changed.
