[jobs:job-done] job 261106-163252-1bcd BENCH-REVIEW S #146 sonnet-high succeeded · 14 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain
Tests and `scripts/check.sh` were not run: package initialization builds helper binaries and creates temporary files, incompatible with the read-only sandbox. No runtime disclosure reproduction or network requests were performed. The queued-job finding is verified through static control-flow inspection.

## Progress
- 17:37 UTC Inspected diff, commit identity, and risky areas.
- 17:38 UTC Traced enqueue, inherited sources, persisted-job launch, and provider setup.
- 17:38 UTC Checked formatting, diff whitespace, and worktree cleanliness; completed review.

BENCH-VERDICT: critical=0 high=1 medium=0 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: provider permissions, private-data disclosure through sources, external sends, budget reservations. No deletion, migration, concurrency, or UI changes.
Reviewed `8f6e8f9..e45252e`. New requests are protected; previously queued OpenRouter jobs retain a disclosure path.

## Findings / Changes

### Delivered
- **closed:** Explicit file and `job:` sources are rejected for new OpenRouter jobs at `internal/jobs/manager.go:510`.
- **closed:** Inherited milestone sources are checked because inheritance occurs before validation (`internal/jobs/manager.go:477`).
- **closed:** Rejection precedes source copying and budget reservation (`internal/jobs/manager.go:352`, `:370`, `:381`).
- **open:** Existing queued jobs carrying copied sources can still start after upgrading.

### Design
The unconditional source ban matches the issue’s permitted simpler solution. Its placement in shared request validation covers CLI, MCP, and model selection through complexity. Enforcement remains incomplete across persisted job lifecycle boundaries.

### Code
**High — Existing queued jobs bypass the new ban.**  
`internal/jobs/manager.go:510` checks only `newJob`. A job queued by the previous version already has `SourceFiles` and copied content. After upgrade, `launchQueued` checks only legacy `j.Sources` (`internal/jobs/manager.go:738`), so this row passes. Preparation still renders its file sources (`internal/jobs/prepare.go:523`), and `wrapperOpenRouter` starts provider setup without checking `SourceFiles` (`internal/jobs/wrapper.go:880`). A copied private file or private-job report remains available to the third-party model.

Verified by tracing the persisted-row launch path and inspecting the parent revision’s `TestOpenRouterJobWithSourceFiles`, which explicitly accepted and persisted this state. No database tampering is required.

Minimal fix: reject queued OpenRouter jobs with nonempty `SourceFiles` before preparation; enforce the same invariant in the wrapper before provider setup. Add a regression fixture representing a previously queued job and assert refusal before runner launch/provider setup.

Brief gap: explicitly require enforcement for jobs queued before installation of the fix.

The added tests cover new-request rejection, budget preservation, and continued Claude/source-free OpenRouter enqueueing. They do not exercise persisted jobs bypassing `newJob`.

### Blocking
- `internal/jobs/manager.go:510` — **High, open:** enforce the source ban when launching existing queued OpenRouter jobs.

### Non-blocking
none

## Evidence
- Inspected the complete four-file diff and lifecycle, milestone inheritance, source copying, resume, wrapper, and provider setup code.
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -l` on all four changed Go files: no output.
- `git status --short`: clean.
- Reviewed commit: `e45252e6a70213d99f17635e83f7e11e47887c92`.
