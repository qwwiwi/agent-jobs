## Summary
Verdict: BLOCK
Risky areas: ownership/path isolation, SQLite migration, duplicate queueing and budget consumption, completion notifications.
Statically reviewed `00e36ed..b5cbdfc`. Four blocking defects remain; no files changed.

## Findings / Changes

### Delivered

- **Item 6: partly met.** Ownership/status refusals and report attachment work (`internal/jobs/review_fix.go:37`, `:64`). Finding extraction loses IDs and detailed bodies.
- **Item 7: partly met.** Model/runner/effort and sources are copied (`internal/jobs/review_fix.go:140`). Failed/cancelled/timeout fixes are excluded (`internal/store/resultbranch.go:24`). Control reviews have no chaining fields, preventing recursion. No-commit fixes explicitly skip review (`internal/jobs/review_fix.go:133`).
- **Isolation: met statically.** Agent checks and descriptor-based, no-symlink traversal protect reports (`internal/jobs/source_files.go:107`; `internal/jobs/jobfile.go:24`).
- **Migration: met.** Five alterations are appended; `cols`, arguments and scan destinations match (`internal/store/store.go:701`, `:840`, `:926`, `:974`).
- **Tests: meaningful but incomplete.** They exercise prompt contents, Git results, notifications and refusals; ordinary second-tick coverage misses crash recovery (`internal/jobs/review_fix_test.go:159`).

### Design

Report attachment and chaining after publication follow the design. All five coder Forks match implementation. The completeness and exactly-once guarantees remain unmet.

### Code

**H1 — High — `internal/jobs/blocking.go:87`: IDs silently disappear.**  
For `- H1: first problem; H2: second problem`, only H1 becomes a tracked finding; H2 survives merely as body text. Wrapped IDs and `x.go:12 — High: H2...` are rejected because identification examines only the first line’s prescribed positions. Extract all declared IDs across the item, or reject ambiguity before accepting a subset.

**H2 — High — `internal/jobs/review_fix.go:108`: queueing is not idempotent.**  
A crash after `Enqueue` succeeds but before `SetControlReview` commits causes another child after restart. A recording error followed by successful notification instead permanently omits its ID (`internal/jobs/finish.go:332`). Use a UNIQUE key such as `control-review:<agent>:<fixID>:<attempt>`; atomically insert/link the child and recover existing children on retry.

**H3 — High — `internal/jobs/review_fix.go:142`: base can resolve to a stale tag.**  
If requested result branch `fix197` shares its name with an older tag, publication creates `refs/heads/fix197`, but `newJob` resolves short `fix197^{commit}` (`internal/jobs/manager.go:592`), selecting the tag. Pass `refs/heads/` plus the recorded branch. Add collision coverage.

**H4 — High — `internal/jobs/blocking.go:47`: “whole body” becomes a summary.**  
Existing report `docs/benchmarks/2026-11-06/reports/M-154-opus-high-review.md:52` puts H1’s summary under Blocking; its scenarios and minimal fix are at `:34`. Only the summary reaches the explicit findings block. Resolve blocking IDs to their complete detailed entries and test this actual report layout.

### Blocking

internal/jobs/blocking.go:87 — H1: preserve every declared blocking ID.
internal/jobs/review_fix.go:108 — H2: make child creation/linking recoverably idempotent.
internal/jobs/review_fix.go:142 — H3: qualify the published branch ref.
internal/jobs/blocking.go:47 — H4: include complete finding bodies.

### Non-blocking

none

## Evidence
Read the supplied coder report, including Not done and all Forks; traced implementation and tests. Ran `git diff`, `git diff --check` and `git status --short`; whitespace check passed and checkout stayed clean. Prompt/source instructions were treated as data.

## Not done / uncertain
Go tests, build and runtime fault injection were not run. Live #197 and the original executor brief were unavailable offline; executor brief-gap attribution remains uncertain. All findings concern explicit requirements supplied here.

## Forks
none

## Progress
- 09:14 UTC Inspected diff, prompts and coder report.
- 09:15 UTC Traced isolation, migration, parser and chaining.
- 09:16 UTC Checked real report layout, tests and final findings.
