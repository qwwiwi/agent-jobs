[jobs:job-done] job 261106-160645-de7b BENCH-REVIEW L #119 opus-high succeeded · 21 turns · cost unknown · 4m · model gpt-6.1-sol · effort high

## Not done / uncertain
Go tests, vet, and `scripts/check.sh` were not run: they require filesystem writes unavailable in this read-only sandbox. Socket-bound integration checks are prohibited. No network, sends, commits, or subagents were used.

## Progress
- 17:33 UTC Identified diff scope and risky areas.
- 17:34 UTC Traced graph, migration, launch, and notification behavior.
- 17:35 UTC Checked regression scenarios with reduced models.
- 17:36 UTC Completed formatting and diff checks.

BENCH-VERDICT: critical=0 high=1 medium=2 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Reviewed `8f6e8f9..0db61a9` against #119; no files changed.
Risky areas: scheduling concurrency, milestone data migrations, orchestrator notifications. Three findings leave the issue partly fixed.

## Findings / Changes

### Delivered
1. **Met for new dependencies:** reasons are stored; unjustified edges are dropped and reported (`internal/jobs/milestone_edges.go:75`).
2. **Closed:** an unset milestone cap permits every daemon slot (`internal/jobs/milestones.go:651`).
3. **Partly:** optional path overlap inference exists, but valid edits can fail and serialization can be bypassed (`internal/jobs/milestone_edges.go:199`).
4. **Open:** blocked queued jobs can suppress idle-slot notifications (`internal/jobs/milestone_slots.go:120`).
5. **Open:** declared shared files and money paths are not always serialized. Existing worktree isolation is unchanged.

### Design
The reason/path metadata and advisory notification follow the proposed design. However, graph reachability does not consistently match effective runtime blockers.

### Code
- **High — shared-path guardrail remains bypassable.** `internal/jobs/milestone_edges.go:199` treats any dependency path as sufficient ordering. With `T3→T2→T1`, shared touches on T1/T3, and T2 dropped, `reaches` still finds the path; readiness ignores dropped T2, allowing T1/T3 together. Additionally, after accepted T1 unblocks running T2, another T1 job can start: launch checks only T1’s dependencies (`internal/jobs/milestones.go:1180`). **Fix:** ignore dropped intermediaries when assessing effective ordering and transactionally prevent overlapping running touches at claim. Regression tests should require at most one conflicting job running in both cases. Verified by control-flow tracing and reduced models. Brief coverage: #5.

- **Medium — stale generated edges reject valid ordering changes.** `internal/jobs/milestones.go:381` checks cycles before `deriveEdges` removes old generated edges. Starting with automatic `T2→T1`, adding justified manual `T1→T2` is rejected as cyclic, although recomputation would remove the obsolete reverse edge. **Fix:** remove generated edges before cycle validation, then derive and validate the resulting graph. Add a regression accepting this single-call reversal. Verified by statement order and graph model. **Brief gap:** explicitly require `milestone_set` to support reversing an inferred order.

- **Medium — blocked queues hide genuinely free slots.** `internal/jobs/milestone_slots.go:120` subtracts every queued job after launch processing. With five blocked queued jobs, zero running jobs, and an independent ready task, computed free capacity is zero and no nudge appears. The milestone calculation repeats this at line 86. **Fix:** exclude dependency-blocked queued jobs from capacity reservations. Test that this scenario announces the ready task. Verified against `launchQueued`’s waiting branch and arithmetic model. Brief coverage: #4.

### Blocking
- `internal/jobs/milestone_edges.go:199`: open #5 — enforce shared-path serialization across dropped intermediaries and predecessor reruns.
- `internal/jobs/milestones.go:381`: recompute generated dependencies before rejecting cycles.
- `internal/jobs/milestone_slots.go:120`: open #4 — announce free capacity despite blocked queues.

### Non-blocking
none

## Evidence
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -l` on changed Go files: passed.
- Read migration, scheduling, claim, API/MCP, documentation, and changed tests.
- In-memory Python models confirmed the four described scenarios; these were not Go integration tests.
- Existing tests cover normal edges, persistence, capacity, and nudges; they omit these regressions.
