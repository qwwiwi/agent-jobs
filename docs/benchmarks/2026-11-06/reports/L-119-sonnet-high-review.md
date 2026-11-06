[jobs:job-done] job 261106-155241-a7ce BENCH-REVIEW L #119 sonnet-high succeeded · 19 turns · cost unknown · 3m · model gpt-6.1-sol · effort high

## Not done / uncertain

Go tests, vet, and `scripts/check.sh` were not run: the sandbox is read-only, and the test harness creates temporary files, builds binaries, and starts processes. Socket-bound checks are prohibited. Test conclusions above come from inspection, not passing executions. No UI files changed. Real planner behavior was not exercised.

## Progress

- 17:28 UTC Established the review range and identified risky areas.
- 17:29 UTC Reviewed dependency validation, migration, concurrency, and notification delivery.
- 17:30 UTC Checked acceptance coverage, verified the HTTP cap regression, and completed read-only formatting and diff checks.

BENCH-VERDICT: critical=0 high=0 medium=0 low=1 issue_fixed=yes

## Summary
Verdict: SHIP WITH FIXES
Reviewed `8f6e8f9..72ea2b9` against issue #119, read-only.
The requested scheduling behavior is implemented. One low-severity HTTP response regression remains; no blocking findings.

## Findings / Changes

Risky areas: concurrency, SQLite schema migration and dependency persistence, money-path serialization, and tmux notification sends. Reviewed these first. No changes to auth, secrets, deletion, or sandbox enforcement.

### Delivered

1. **Met / closed:** Planner guidance limits dependencies to conflicts or consumed outputs (`internal/mcp/tools_milestones.go:21`). Reasons are validated, persisted, and returned; unjustified edges are dropped and reported (`internal/jobs/milestones.go:380`, `internal/store/milestones.go:244`).
2. **Met / closed:** Unset `max_parallel` leaves the daemon limit as the effective cap (`internal/jobs/milestones.go:667`). The new acceptance test enqueues five independent jobs with `max_running=5` (`internal/jobs/milestones_parallel_test.go:108`).
3. **Optional, omitted:** Mechanical path-overlap estimation is not implemented.
4. **Met / closed:** Idle-slot notifications name ready tasks, respect milestone caps, and suppress repeated ready lists (`internal/jobs/idleslots.go:18`, `internal/jobs/idleslots.go:78`).
5. **Met / closed:** Justified dependencies retain existing launch checks (`internal/jobs/milestones.go:1196`); repository jobs retain separate worktrees (`internal/jobs/prepare.go:626`, `internal/jobs/prepare.go:657`).

### Design

The implementation follows the brief: planner-provided explanations, explicit reporting of dropped edges, daemon-controlled capacity, and an orchestrator nudge. Runtime scheduling remains with the existing queue.

### Code

**Low — Newly exposed reasons bypass the HTTP string cap.**  
`internal/jobs/milestones.go:902` adds `DependsWhy` to the shared response, but `internal/httpapi/milestones.go:40` does not sanitize its values. A valid 200-character Cyrillic reason contains 400 bytes and reaches both milestone HTTP endpoints unchanged, violating their documented 200-byte field limit (`docs/GUIDE.md:783`).

Verified by tracing reason validation, tree construction, and the HTTP adapter; no runtime reproduction was performed. Minimal fix: copy the map in `apiMilestone` and apply `capField` to each value. Extend `TestMilestoneFieldsCapped` with a multibyte reason and check both endpoints.

**Brief gap:** Require new response fields to preserve existing HTTP sanitization and size limits.

Tests cover reason persistence, dropped edges, invalid reasons, dependency validation, five-slot scheduling, and notification deduplication. Notification retry/reset and the large-ready-list branch lack dedicated coverage; no defect was established there.

### Blocking

none

### Non-blocking

internal/jobs/milestones.go:902 — Low: cap `depends_why` values in the HTTP adapter and add multibyte endpoint coverage.

## Evidence

- Inspected the complete diff and relevant scheduler, store, notification, schema, HTTP, and worktree paths.
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -l` on all changed Go files: no output.
- `git status --porcelain`: clean.
- No subagents used.
