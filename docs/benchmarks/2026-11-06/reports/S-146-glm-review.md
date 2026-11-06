[jobs:job-done] job 261106-170147-9231 BENCH-REVIEW S #146 glm (job failed: report lost) succeeded · 13 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain

Go tests and `scripts/check.sh` were not run: they create build artifacts, temporary repositories, and other files incompatible with the read-only sandbox. No runtime reproduction or external requests were performed. No UI changed. No subagents used.

## Progress

- 17:38 UTC Inspected commit range, changed files, and security checks.
- 17:39 UTC Traced report reads and linked-worktree ownership; identified two open conditions.
- 17:39 UTC Completed formatting, whitespace, and clean-worktree checks.

BENCH-VERDICT: critical=0 high=2 medium=0 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: permissions/guard, confidential-data exposure, external sends, and budget handoff.
Reviewed `8f6e8f9..9db5f5c` for issue #146. Basic checks work, but two source-provenance conditions remain open.

## Findings / Changes

### Delivered

- **Closed:** ordinary files beneath a denied path/name/glob are rejected (`internal/jobs/openrouter.go:464`).
- **Closed:** explicit `job:<id>` references to denied repositories, and unreadable job rows, are rejected (`internal/jobs/openrouter.go:452`).
- **Closed:** launch repeats source validation before provider setup (`internal/jobs/orproxy.go:960`).
- **Open:** denied-repository files remain attachable through linked worktrees.
- **Open:** denied-job reports remain attachable through absolute file paths.

### Design

The change follows the proposed selective policy rather than disabling sources. However, filesystem ancestry alone does not establish repository provenance. Both open conditions defeat the original confidentiality requirement.

### Code

**H1 — High: linked worktrees bypass file-source denial.**  
`internal/jobs/openrouter.go:477` checks only lexical parent paths. With `/repos/private` denied, a source `/state/jobs/A/work/notes.md` from its linked worktree passes when those parent names are allowed. The repository itself creates such worktrees (`internal/jobs/prepare.go:657`). Unlike repository validation (`internal/jobs/manager.go:557`), source validation never resolves the main worktree.

Consequence: private repository files can still reach OpenRouter. Minimal fix: resolve and check the owning repository/main worktree, preserving provenance for launch validation; alternatively refuse OpenRouter sources. Add enqueue and launch regression cases using a differently named linked worktree. **Verified by static control-flow inspection; not executed.** The brief explicitly covers files from denied repositories.

**H2 — High: absolute report paths bypass job-source denial.**  
`internal/jobs/openrouter.go:451` resolves job ownership only for the `job:` syntax. Replacing rejected `job:A` with `/state/jobs/A/report.md` enters the ancestry-only branch at line 464. With state outside the denied repository, that path passes, and `internal/jobs/source_files.go:125` opens the same report without consulting its job row.

Consequence: reports about denied repositories remain attachable. Minimal fix: map job-artifact paths to their owning job and apply the same denial, rejecting unresolved provenance; alternatively refuse OpenRouter sources. Add paired tests asserting both spellings are rejected at enqueue and launch. **Verified by tracing validation and snapshot reads; not executed.** The brief explicitly identifies private-repository reports.

### Blocking

- `internal/jobs/openrouter.go:477` — H1: **open**, close linked-worktree file provenance.
- `internal/jobs/openrouter.go:451` — H2: **open**, enforce equivalent policy for absolute report paths.

### Non-blocking

none

## Evidence

- `git rev-parse HEAD`: `9db5f5cff45e6cc4e240ae9d4b56609652dc6a23`.
- Reviewed the seven-file diff and relevant source-copy, worktree, wrapper, and test code.
- `git diff --check 8f6e8f9..HEAD`: passed.
- `gofmt -d` on all changed Go files: no output.
- Added tests cover direct paths and explicit job references, but neither bypass above.
- Final `git status --short`: clean.
