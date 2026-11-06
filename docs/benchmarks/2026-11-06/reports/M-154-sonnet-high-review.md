[jobs:job-done] job 261106-135549-b264 BENCH-REVIEW M #154 sonnet-high succeeded · 16 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain
Go tests and `scripts/check.sh` were not run: compilation and tests require filesystem writes unavailable in this read-only job. End-to-end daemon behavior was traced, not executed. No subagents used.

## Progress
- 17:15 UTC Inspected diff, commit scope, and risky areas.
- 17:16 UTC Traced report selection, contract validation, accounting, and regression coverage.
- 17:17 UTC Reproduced edge cases with a read-only model and checked formatting and worktree cleanliness.

BENCH-VERDICT: critical=0 high=1 medium=1 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Reviewed `8f6e8f9..47d0ca0` for issue #154.
The plain trailing-notification case is fixed, but report selection and total-turn accounting remain incomplete. Two verified findings; no files changed.

## Findings / Changes

Risky areas: background-notification result sequencing and usage accounting. No changes to auth, secrets, deletion, sandbox enforcement, or external sends.

### Delivered
- **Closed:** preserve a complete report followed by an ordinary notification, in both stream readers (`internal/jobs/wrapper.go:132`, `:263`).
- **Open:** select the last result containing report sections; the prefix check can select an incomplete notification (`internal/jobs/wrapper.go:110`).
- **Open:** retain run totals regardless of which report wins (`internal/jobs/wrapper.go:111`).
- **Met:** two-result regression tests added (`internal/jobs/trailing_result_test.go:32`, `:49`).

### Design
Sharing one merge function between live stdout and stream-file parsing is appropriate. Report selection and accounting need independent handling.

### Code
**High — Incomplete notification can still overwrite the report.**  
`internal/jobs/wrapper.go:110` uses `isReport`, which only checks the prefix `## Summary` (`internal/jobs/idle.go:234`). After a complete report, a notification reading `## Summary of timer notification\nDone.` replaces it despite lacking an actual Summary section. The contract consequently produces `NoteNoSummary`, recreating the reported failure. A Summary-only notification also discards every other section.

Verified by source tracing and a read-only Python model of the merge branch. Minimal fix: select reports using parsed required sections, consistent with `reportContractNote`; preserve the previous complete report otherwise. Add both notification variants as regression cases. The brief explicitly required report sections; no brief gap.

**Medium — Choosing a newer report discards earlier turns.**  
`internal/jobs/wrapper.go:110` returns `next` before aggregation whenever the later result is a report. Two valid reports with 87 and 1 turns therefore produce 1 turn instead of 88. A report → notification → newer-report sequence similarly loses accumulated turns.

Verified against the branch logic and the existing newer-report fixture: `internal/jobs/trailing_result_test.go:61` checks text selection but omits totals. Minimal fix: aggregate turns independently of text selection and assert totals for these sequences. The brief explicitly required run totals; no brief gap.

### Blocking
- `internal/jobs/wrapper.go:110`: Open — require report sections before replacing an earlier complete report.
- `internal/jobs/wrapper.go:111`: Open — preserve accumulated turns when selecting a newer report.

### Non-blocking
none

## Evidence
- Inspected the complete fix diff, report parser, wrapper output handling, and downstream verdict paths.
- Read-only Python branch model confirmed ordinary-notification preservation and reproduced both findings; this was not execution of Go tests.
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -l internal/jobs/wrapper.go internal/jobs/trailing_result_test.go`: clean.
- `git status --short`: clean.
