[jobs:job-done] job 261106-170259-0e17 BENCH-REVIEW M #114 sonnet-high succeeded · 11 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain
- No tests, builds, vet, or `scripts/check.sh` run: they require generated files or temporary repositories unavailable under the read-only constraints.
- macOS kqueue/sysctl runtime behavior remains unverified; its implementation is unchanged.
- Proposed changes and tests are report-only. No files modified, commits created, network accessed, or subagents used.

## Progress
- 17:39 UTC Identified concurrency and process-lifecycle risks; inspected revision range and diff.
- 17:39 UTC Traced stdout ownership, PID reservation, deadline cleanup, and regression assertions.
- 17:40 UTC Verified missing writer closure and the default ten-minute delay through static analysis.
- 17:40 UTC Completed formatting and diff-whitespace checks; prepared findings and proposed tests.

BENCH-VERDICT: critical=0 high=0 medium=1 low=0 issue_fixed=yes

## Summary
Verdict: BLOCK
Risky areas: concurrency, process-group termination, PID reservation, and timeout cleanup.
Reviewed `8f6e8f9..60f4863`. The descendant leak is addressed, but the replacement pipe introduces an EOF-handling regression.

## Findings / Changes

### Delivered
- **Closed:** manually consumed stdout participates in runner completion (`internal/jobs/gate.go:514–516`), retaining the PID reservation while descendants hold stdout.
- **Closed:** deadline cleanup kills the group before closing the consumer (`internal/jobs/gate.go:527–530`).
- Regression test added at `internal/jobs/git_timeout_test.go:337`: fake cat-file exits with an inheriting descendant; assertions cover timeout identity and child termination. Execution remains unverified.

### Design
The fix reuses the existing pre-reap copy protocol as requested. Its new pipe lacks completion propagation to the consumer.

### Code
**Medium — `internal/jobs/gate.go:514`: stdout EOF never reaches the consumer.**  
`outW` is never closed. When cat-file exits before completing a header or blob, the runner’s `io.Copy` finishes (`internal/jobs/gitrun.go:114`), but `outR` remains open. `ReadString` or `CopyN` therefore blocks until the deadline closes it, converting an immediate EOF failure into a timeout after the default **10 minutes** (`internal/jobs/manager.go:305`).

Verified by tracing pipe ownership, copy completion, and every close path; no runtime reproduction performed.

Minimal fix: close the pipe writer when the stdout copy finishes, propagating its error, while preserving kill-before-consumer-close ordering.

Proposed tests: fake cat-file reads one request, then exits with either no response or a truncated blob. Both calls must fail promptly before the configured deadline and must not report `errGitTimeout`.

Brief coverage: the named files and requested correctness/regression review exposed this problem; no brief gap.

### Blocking
- `internal/jobs/gate.go:514`: propagate stdout copy completion to the pipe reader and add premature-EOF coverage.

### Non-blocking
none

## Evidence
- Confirmed HEAD `60f4863526273cc3744a4358be173592916500a4`; working tree clean.
- Reviewed the requested diff: two files, 65 insertions, 13 deletions.
- Inspected runner reaping/signalling, materialization cleanup, existing timeout/materialization tests, and platform process-state helpers.
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -l internal/jobs/gate.go internal/jobs/git_timeout_test.go`: no output.
