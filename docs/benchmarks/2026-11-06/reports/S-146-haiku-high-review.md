[jobs:job-done] job 261106-164455-0401 BENCH-REVIEW S #146 haiku-high succeeded · 17 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain
Go tests and `scripts/check.sh` were not run: they create temporary files, repositories, databases, and build artifacts prohibited by the read-only sandbox. Network/provider execution was unavailable. The queued-job finding is statically verified, without runtime reproduction. No files changed; no subagents used.

## Progress
- 17:37 UTC Inspected the requested diff and identified policy and disclosure risks.
- 17:38 UTC Traced source validation, inheritance, persistence, launch, and wrapper behavior.
- 17:38 UTC Checked formatting and whitespace; completed the report.

BENCH-VERDICT: critical=0 high=1 medium=0 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Reviewed commit `55663de` against `8f6e8f9`.
New OpenRouter jobs reject sources, but previously queued jobs can still expose attached files after upgrade.

## Findings / Changes

Risky areas: permissions/policy enforcement, confidential source data, external provider sends, and budget reservation tests. No deletion, migration, or concurrency logic changed.

### Delivered
- **closed:** New OpenRouter requests reject every nonempty sources list before copying files or reserving budget (`internal/jobs/manager.go:508`, `internal/jobs/openrouter.go:425`).
- **closed:** Milestone sources are inherited before this check (`internal/jobs/manager.go:477`, `internal/jobs/milestones.go:1042`).
- **closed:** Source-free OpenRouter requests and subscription jobs retain their intended behavior; regression assertions cover both (`internal/jobs/openrouter_sources_test.go:64`).
- **open — High:** `internal/jobs/manager.go:508` applies the restriction only during job creation. An OpenRouter job queued before upgrade retains its copied private files and can launch afterward. `launchQueued` checks only legacy `j.Sources`, not `j.SourceFiles` (`internal/jobs/manager.go:735`). Prompt generation still advertises those attachments (`internal/jobs/prepare.go:523`), and the wrapper starts OpenRouter without checking them (`internal/jobs/wrapper.go:880`). Consequence: the original disclosure condition remains possible for persisted queued jobs.
  
  Verified by static tracing: the parent commit’s `TestOpenRouterJobWithSourceFiles` establishes that these queued rows and snapshots were previously accepted; store deserialization preserves `SourceFiles`. No exploit was executed.
  
  Minimal fix: reject OpenRouter rows with nonempty `SourceFiles` before launch, preferably also at the wrapper boundary. Add a regression that seeds a previously accepted queued row and confirms cancellation before runner startup.
  
  brief gap: explicitly require protection for jobs queued before deployment of the fix.

### Design
The blanket refusal follows the issue’s explicitly permitted simpler design. Its enforcement must also cover persisted jobs to make that policy complete.

### Code
The new validation is straightforward and correctly precedes source reads. Tests cover denied paths, outside paths, report references, folder cleanup, and accepted requests. Budget collision coverage remains. The missing case is launch of a previously queued source-bearing OpenRouter job.

### Blocking
internal/jobs/manager.go:508 — High, open condition: enforce source refusal when launching persisted OpenRouter jobs.

### Non-blocking
none

## Evidence
- `git diff 8f6e8f9..HEAD`: five changed files; no UI changes.
- Inspected creation, milestone inheritance, source copying/storage, launch, prompt rendering, wrapper setup, and resume restrictions.
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -l` on changed Go files: no output.
- `GOTOOLCHAIN=local go version`: `go1.27.2 linux/amd64`.
- `git status --short`: clean.
