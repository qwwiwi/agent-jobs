[jobs:job-done] job 261106-163252-ae63 BENCH-REVIEW S #146 opus-high succeeded · 29 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain
Go tests and `scripts/check.sh` were not run: builds and fixtures require writes unavailable in this read-only sandbox. Runtime behavior remains unverified.

Recommended regression cases for the orchestrator: inherited milestone file sources rejected; explicit empty override accepted; complexity-selected OpenRouter model rejected with sources. No UI changed, so browser evidence is inapplicable.

## Progress
- 17:37 UTC Identified risk areas and inspected the requested fix diff.
- 17:37 UTC Traced source validation, copying, budget reservation, and launch checks.
- 17:38 UTC Reviewed tests and milestone/model resolution; formatting and whitespace checks passed.

BENCH-VERDICT: critical=0 high=0 medium=0 low=0 issue_fixed=yes

## Summary
Verdict: SHIP
Risky areas: permissions/sandbox, private-data disclosure to external providers, budget reservation.
Reviewed `8f6e8f9..6f2413a`; the blanket sources prohibition fixes #146. No verified findings.

## Findings / Changes

### Delivered
- **Closed:** OpenRouter jobs reject file and `job:` sources before snapshots or budget reservation: `internal/jobs/manager.go:508`, preceding copying at `manager.go:370` and insertion at `manager.go:381`.
- **Closed:** Milestone sources are inherited before validation (`internal/jobs/milestones.go:1042`) and rejected by the same check. Explicit `sources: []` suppresses inheritance.
- **Closed:** Previously queued jobs carrying either source representation fail before provider setup and runner execution: `internal/jobs/wrapper.go:874`, called at `wrapper.go:713`.

### Design
Commit `6f2413a` implements the brief’s explicitly permitted simpler policy: refuse all sources until a source policy exists. Documentation and MCP schema describe that restriction.

### Code
No correctness or regression defect found by static inspection. Validation uses the resolved model, covering complexity-selected OpenRouter models too.

Tests cover denied-repository files, outside files, `job:` reports, zero budget reservation, no snapshot directory, queued-job rejection, source-free OpenRouter acceptance, and retained subscription sources (`internal/jobs/openrouter_test.go:1036`; `internal/jobs/openrouter_sources_test.go:18,42`). The collision test retains budget-reservation coverage after removing now-forbidden sources (`internal/jobs/jobid_test.go:179`).

### Blocking
none

### Non-blocking
none

## Evidence
- `git rev-parse HEAD`: `6f2413a8b78d9618269617e1851218f360a4a811`.
- Reviewed the requested diff: nine files, 93 insertions, 32 deletions.
- Traced enqueue, milestone inheritance, source copying, persisted source fields, and wrapper launch.
- `git diff --check 8f6e8f9e5969b205fa9e400b66d0728ba39602cd..HEAD`: passed.
- `gofmt -d` on all seven changed Go files: no output.
- No files changed; no subagents used.
