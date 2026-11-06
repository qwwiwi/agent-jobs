[jobs:job-done] job 261106-161046-b0c9 BENCH-REVIEW S #146 sonnet-medium succeeded · 18 turns · cost unknown · 3m · model gpt-6.1-sol · effort high

## Not done / uncertain
Go tests and `scripts/check.sh` were not run: package initialization builds binaries and writes temporary fixtures, which this read-only sandbox forbids. Proposed regression cases were not executed. No external HTTP, provider calls, or browser checks were performed; the diff contains no UI changes.

## Progress
- 17:34 UTC Identified security-sensitive paths and inspected the requested diff.
- 17:35 UTC Traced source validation, snapshot reads, and repository attribution.
- 17:36 UTC Checked regression coverage, ran read-only assertions, and confirmed unchanged files.

BENCH-VERDICT: critical=0 high=2 medium=0 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: repository permissions, source confidentiality, external provider sends.
Reviewed `8f6e8f9..f6eefa6` without modifications. Direct checks were added, but alternate source paths still bypass the policy.

## Findings / Changes

### Delivered
Partly. `internal/jobs/manager.go:564` checks OpenRouter sources before snapshotting. Ordinary files in denied repositories and explicit `job:<id>` references are rejected. Two security conditions remain open below.

### Design
The implementation follows the proposed selective policy rather than disabling sources. Its attribution is incomplete, and repository discovery fails open.

### Code

**High — private reports bypass validation through absolute paths.**  
`internal/jobs/openrouter.go:822–828`: only `job:` origins consult the originating job’s `RepoPath`. Supplying the same report as `<state>/jobs/<private-id>/report.md` instead invokes Git discovery. Job artifact directories normally sit outside repositories, so validation accepts the report; `internal/jobs/source_files.go:115` then reads it as an ordinary file. Private report contents remain attachable to OpenRouter jobs.

Verified through the validation/read control flow and a read-only Git probe confirming that this job’s artifact directory has no Git top-level. Minimal fix: reject sources on OpenRouter jobs, or resolve absolute job artifact paths to their owning job and apply its repository policy. Add a regression with a valid private report submitted through both syntaxes. The brief explicitly identifies private reports, so no brief gap.

**High — repository discovery errors authorize sources.**  
`internal/jobs/openrouter.go:846–848`: every Git error becomes an empty repository list. A readable file inside a denied repository is therefore accepted when discovery fails, including metadata files for which Git cannot report a working-tree top-level. The snapshot reader independently accepts regular files and does not enforce repository policy.

Verified statically: the error branch returns `nil`, the caller interprets that as no denied repositories, and snapshotting proceeds. Minimal fix: distinguish proven non-repository paths from discovery failures; reject ambiguous results and recognize enclosing denied repositories. Add cases for failed Git discovery and a denied repository’s `.git/COMMIT_EDITMSG`, while retaining acceptance of ordinary outside files. The brief requires every file source to satisfy the policy, so no brief gap.

The added test covers direct denials, an outside file, and Claude compatibility. It misses both bypasses.

### Blocking
- `internal/jobs/openrouter.go:822`: **open** — private job reports remain attachable through absolute file paths.
- `internal/jobs/openrouter.go:847`: **open** — denied-repository files remain attachable when Git discovery fails.

### Non-blocking
none

## Evidence
- Reviewed the complete three-file diff and source snapshot, repository matching, and worktree helpers.
- `git diff --check 8f6e8f9..HEAD`: passed.
- Read-only Python assertions confirmed the relevant branch behavior and missing absolute-report test coverage.
- `git -C .. rev-parse --show-toplevel`: failed with “not a git repository,” confirming the artifact-directory attribution gap.
- Working tree remained clean.
