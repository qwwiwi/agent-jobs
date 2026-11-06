[jobs:job-done] job 261106-142404-b148 BENCH-REVIEW M #154 glm succeeded · 16 turns · cost unknown · 3m · model gpt-6.1-sol · effort high

## Not done / uncertain

Go tests and `scripts/check.sh` were not run: compilation and package `TestMain` require filesystem writes unavailable in this read-only sandbox.

Actual Claude cost-counter semantics remain unverified: the new fixtures assume each result’s cost is incremental. No original incident stream or runner implementation was available locally, and network access was prohibited.

## Progress

- 17:20 UTC Inspected review range and identified accounting and finalization risks.
- 17:21 UTC Traced report selection, validation, persistence, and regression tests.
- 17:22 UTC Evaluated counterexamples, checked formatting and whitespace, and confirmed a clean worktree.

BENCH-VERDICT: critical=0 high=1 medium=0 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Reviewed `8f6e8f9..09d4fa8` for issue #154.
The canonical two-result case is fixed, but reports accepted by the existing contract validator can still be overwritten.

## Findings / Changes

Risky areas reviewed: cost accounting, concurrent stream finalization, and trusted result provenance. No auth, deletion, migration, secrets, sandbox-policy, or external-send changes.

### Delivered

- **Closed:** A canonical report survives a plain trailing notification; turns and costs are accumulated (`internal/jobs/wrapper.go:255`).
- **Closed:** Unit and lifecycle regression tests cover two results, report contents, successful status, and additive totals (`internal/jobs/hardening1_test.go:140`, `internal/jobs/idle_test.go:240`).
- **Open:** Selecting the last result containing report sections is incomplete; eligibility uses a heading-prefix check.

### Design

Terminal status remains taken from the last event while report text is preserved separately. Result counting and existing gate checks remain intact. Report eligibility disagrees with the existing contract parser.

### Code

**High — Report detection can still lose a valid report and falsely fail the job.**  
`internal/jobs/wrapper.go:257` uses `isReport`, which only checks the case-sensitive prefix `## Summary` (`internal/jobs/idle.go:234`). The existing contract test explicitly accepts `intro\n\n## summary\nok\n\n## not done / uncertain\nnone` (`internal/jobs/autocontinue_test.go:283`). Such a first result is overwritten by an ordinary notification, producing `NoteNoSummary`. Conversely, a notification headed `## Summary of timer notification` passes the prefix check and overwrites a complete report despite containing no Summary section.

Verified through source tracing and an in-memory evaluation of the exact selection predicate. Both counterexamples select the notification and produce `NoteNoSummary`; the canonical case preserves the report.

Minimal fix: use the existing section parser to recognize required report headings. Add both counterexamples to the regression test. The brief explicitly requires report-section recognition; no brief gap.

### Blocking

- `internal/jobs/wrapper.go:257` — **Open:** Align report eligibility with section parsing so valid reports survive trailing notifications and heading-prefix lookalikes cannot replace them.

### Non-blocking

none

## Evidence

- Inspected the complete five-file diff and downstream report validation, persistence, idle completion, and gate checks.
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -l` on all five changed files: no output.
- In-memory predicate probe: canonical case preserved; existing accepted report format and heading-prefix notification cases lost the report.
- `git status --short`: clean.
