[jobs:job-done] job 261106-141600-329e BENCH-REVIEW M #154 opus-high succeeded · 16 turns · cost unknown · 3m · model gpt-6.1-sol · effort high

## Not done / uncertain

Go tests and `scripts/check.sh` were not run: the read-only sandbox prohibits their temporary files and builds; manager tests also use sockets. The Python trace models the inspected logic and is not Go execution. No live Claude stream was available to independently verify metadata semantics. No files changed.

## Progress

- 17:19 UTC Inspected revision, worktree status and complete diff.
- 17:20 UTC Reviewed trusted-result boundaries, accounting and report validation.
- 17:21 UTC Reproduced baseline and classifier failures in memory; confirmed clean worktree.

BENCH-VERDICT: critical=0 high=1 medium=0 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: trusted stdout/result validation and turns/cost accounting.
Reviewed `8f6e8f9..524b513`. The reported two-result case is fixed, but report detection can still discard a valid report.

## Findings / Changes

### Delivered

- **Closed:** The supplied report → notification sequence preserves the report and aggregates 87 + 1 turns with cost $3.25 (`internal/jobs/wrapper.go:127`).
- **Closed:** Regression coverage exercises the tee, file parser, job status, saved report and completion line (`internal/jobs/multiresult_test.go:24`, `:60`).
- **Open — H1:** Selecting the last result with report sections remains incomplete (`internal/jobs/wrapper.go:133`).

### Design

The shared merge function follows the requested approach. However, its report predicate differs from the existing section validator, so report selection and final validation disagree.

### Code

**H1 — High: Report detection can still lose the completed report.**  
`internal/jobs/wrapper.go:133` uses `isReport`, which only checks a case-sensitive `## Summary` prefix (`internal/jobs/idle.go:234`).

Two verified consequences of this same open condition:

- A valid report with introductory text or lowercase headings is discarded when a plain notification follows. That report format is explicitly accepted by `internal/jobs/autocontinue_test.go:289`.
- A notification beginning `## Summary of background task` replaces a complete report despite lacking an actual Summary section. Final validation then fails the job through `internal/jobs/acceptance.go:214`.

A Summary-only notification also replaces the report and loses its remaining sections; the new test explicitly expects this replacement (`internal/jobs/multiresult_test.go:49`).

**Minimal fix:** Select report candidates using the existing section parser, requiring exact Summary and Not done / uncertain sections. Add cases for accepted introductory/lowercase reports and incomplete notification summaries, alongside replacement by a complete newer report.

**Verification:** Traced the Go selection, write and validation paths; reproduced these outcomes with a source-derived, in-memory Python model. Go tests were not executed.

**Brief coverage:** The brief explicitly requires selecting a result with report sections and preventing notification-driven report loss; no brief gap.

### Blocking

internal/jobs/wrapper.go:133 — H1: Report-preservation condition remains open because prefix detection disagrees with section validation.

### Non-blocking

none

## Evidence

- Inspected the complete five-file diff for commit `524b513`.
- Reviewed trusted-result handling, gate event-count checks, report writing and final contract validation.
- `git diff --check 8f6e8f9..HEAD`: passed.
- Source-derived Python trace: baseline preserved report, turns=88, cost=3.25; all three classifier edge cases discarded the valid report.
- `git status --short`: clean.
