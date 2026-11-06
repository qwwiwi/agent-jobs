[jobs:job-done] job 261106-140153-58b9 BENCH-REVIEW M #154 sonnet-medium succeeded · 11 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain
Go tests and `scripts/check.sh` were not run: they require writable temporary/build state unavailable in this read-only job. The Python reproduction validates merge logic, not compiled Go execution. No end-to-end daemon verification was performed.

## Progress
- 17:17 UTC Inspected changed files and identified risky areas.
- 17:18 UTC Traced report validation, trusted stdout, and usage accounting.
- 17:18 UTC Reproduced uncovered merge cases and checked diff whitespace.

BENCH-VERDICT: critical=0 high=1 medium=1 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: cost accounting, multi-event sequencing, trusted stdout handling.
Reviewed `8f6e8f9..f453e64`. The reported two-event example is fixed, but report selection and total-turn accounting remain incomplete.

## Findings / Changes

### Delivered
- **Closed:** canonical full report followed by an unstructured notification retains the report and records 88 turns / $2.60 (`internal/jobs/wrapper.go:111`).
- **Closed:** regression tests cover both stream parsing and trusted stdout (`internal/jobs/hardening1_test.go:320`, `:330`).
- **Open:** preserve the last result containing report sections across contract-valid formatting variants.
- **Open:** retain total turns independently of which result supplies the report.

### Design
Sharing `mergeResult` between both parsers is appropriate. However, report selection and usage aggregation are coupled, and report recognition differs from the existing section-based contract.

### Code
1. **High — `internal/jobs/wrapper.go:108`: report selection can still discard completed work.**  
   `isReport` only checks whether text starts with `## Summary`. A complete report with an introductory paragraph is accepted by the existing contract parser (`internal/jobs/report.go:117`) but fails this predicate; the trailing notification replaces it, reproducing the missing-Summary failure. Conversely, a trailing `## Summary` notification without the remaining sections replaces a complete report.  
   **Verified:** source inspection and an in-memory reproduction of the exact merge branches; existing contract tests explicitly accept introductory text and lowercase headings (`internal/jobs/autocontinue_test.go:288`).  
   **Minimal fix:** use section-based report recognition consistent with the contract, preserving the previous complete report when the next result lacks its sections. Add both regression cases.  
   **Brief coverage:** explicitly requires selecting the last result containing report sections.

2. **Medium — `internal/jobs/wrapper.go:108`: later reports reset total turns.**  
   The early return bypasses aggregation whenever the next event contains a report. Two complete reports with 87 and 1 turns produce **1**, rather than **88**. A subsequent notification then continues from that truncated count.  
   **Verified:** exact merge-branch reproduction returned 1; both newly added tests exercise only the branch that sums turns.  
   **Minimal fix:** aggregate usage independently of text selection. Test two complete reports and a three-result sequence.  
   **Brief coverage:** explicitly requires turns/cost from the run total.

### Blocking
- `internal/jobs/wrapper.go:108`: open report-preservation condition; valid reports can still be overwritten.
- `internal/jobs/wrapper.go:108`: open run-total condition; report-bearing events discard earlier turns.

### Non-blocking
none

## Evidence
- Inspected the complete two-file diff, wrapper report-writing paths, report contract parser, and gate validation.
- `git diff --check 8f6e8f9..HEAD`: passed.
- Source-checked Python model: canonical case retained the report; two reports returned 1 turn; introductory report and Summary-only notification cases lost the full report.
- Worktree remained unchanged.
