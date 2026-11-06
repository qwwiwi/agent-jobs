[jobs:job-done] job 261106-155241-eccf BENCH-REVIEW L #119 sonnet-medium succeeded · 18 turns · cost unknown · 3m · model gpt-6.1-sol · effort high

## Not done / uncertain
Go tests, vet, and `scripts/check.sh` were not run: they require writable build/test directories unavailable in this read-only sandbox. No browser checks apply.

Proposed regression cases: nudge delivery and retry/deduplication; blocked backlog with independent ready tasks; five concurrent independent tasks under `max_running=5`; multibyte reasons capped on both HTTP endpoints. No files, commits, external actions, or subagents were used.

## Progress
- 17:30 UTC Inspected diff and prioritized concurrency and migration risks.
- 17:31 UTC Traced scheduling, notification delivery, API conversion, and tests.
- 17:32 UTC Verified counterexamples, formatting, and clean worktree.

BENCH-VERDICT: critical=0 high=1 medium=1 low=1 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: concurrency and dependency serialization, SQLite migration, HTTP response boundaries.
Reviewed `8f6e8f9..42be03b` without changes. Dependency reasons are implemented; idle-slot notification remains incomplete.

## Findings / Changes

### Delivered
1. **Met / closed:** planner instructions require justified edges; unexplained edges are dropped and reported (`internal/mcp/tools_milestones.go:25`, `internal/jobs/milestones.go:312`).
2. **Met / closed:** omitted `max_parallel` leaves the daemon limit authoritative. This behavior already existed (`internal/jobs/milestones.go:642`, `internal/jobs/manager.go:735`).
3. **Not implemented, optional:** no mechanical path-overlap estimation.
4. **Partly / open:** the nudge is calculated and logged, but never delivered to the orchestrator; blocked queued jobs can suppress it.
5. **Met for declared conflicts / closed:** retained dependencies still gate launch; worktree isolation is unchanged (`internal/jobs/milestones.go:1161`).

### Design
The reason map and persisted edge explanations follow the brief. Logging the nudge does not satisfy the requested orchestrator notification. No dependencies or CI changes were introduced.

### Code

**High — H1: nudge never reaches the orchestrator.**  
`internal/jobs/milestones.go:1259` only calls `m.Log.Printf`. Verified by tracing its callers and comparing the actual notification channel at `internal/jobs/finish.go:315`; heartbeat does not consume this line either. Ready tasks can remain unstarted indefinitely despite free capacity. **Fix:** deliver through the existing notification channel, retry failures, and update deduplication state only after successful delivery.

**Medium — M1: blocked queue entries incorrectly consume slots.**  
`internal/jobs/milestones.go:1218` subtracts every queued job. With `max_running=5`, one running job, four dependency-blocked queued jobs, and another independent ready task, it computes zero free slots despite four available. Verified against the formula and launch behavior, which skips blocked jobs (`internal/jobs/manager.go:758`). **Fix:** count occupied running slots; queued blocked jobs must not suppress the nudge.

**Low — L1: new HTTP field bypasses response limits.**  
`internal/jobs/milestones.go:116` adds `depends_reasons`, but `internal/httpapi/milestones.go:40` never caps its values. A valid 200-character Cyrillic reason produces 400 bytes, exceeding the API’s 200-byte string limit. Verified by tracing validation and response conversion. **Fix:** clone the map and apply `capField` to its values. **Brief gap:** preserve HTTP response limits when exposing new fields.

### Blocking
- `internal/jobs/milestones.go:1259` — H1: deliver the idle-slot nudge to the orchestrator.
- `internal/jobs/milestones.go:1218` — M1: exclude blocked queued jobs from occupied-slot accounting.

### Non-blocking
- `internal/jobs/milestones.go:116` — L1: cap dependency reasons in HTTP responses.

## Evidence
- Inspected the complete requested diff, scheduling, notification, persistence, API conversion, and changed tests.
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -l` on all changed Go files: no output.
- Read-only Python assertions confirmed the source-based queue and HTTP counterexamples; these were not Go integration tests.
- `git status --porcelain`: clean.
