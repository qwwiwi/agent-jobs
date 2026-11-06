[jobs:job-done] job 261106-131244-a1c5 BENCH S #145 sonnet-high succeeded · 2 turns · $0.40 · 1m · model sonnet · effort high · auto-continued (attempt 2; attempt 1 ended its turn without a report after 1 turns (cost $0.36)) · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)
attempt 1: new failed · 1 turns · $0.36 · 1980s · <agent-jobs-home>/jobs/261106-131244-a1c5/report.attempt-1.md
attempt 2: resume succeeded (current)

## Not done / uncertain
- The full `go test ./internal/jobs` run ended with `FAIL` after 601s, which is the Go test timeout. I did not find the cause and did not run it on the base commit. Two test runs overlapped during that attempt, which may have slowed it. The `Skills|Resume` subset passes.
- The new legacy-launch test uses the no-repo fixture, because the repo-backed fixture has no worktree metadata and `prepareResume` fails earlier. It has no "work dir missing" variant.
- The three new `//` cases are not tied to individual failures under the mutation (see Evidence).
- The commit message is in English as the benchmark brief required, although the owner rules ask for Russian commit messages.

## Progress
- 13:15 UTC read the issue, the two test files and the guards in `resume.go`, `skill_manifest.go`, `source_files.go`
- 13:30 UTC created the branch and added the `//` cases and a first version of the legacy-launch test
- 13:45 UTC reworked the legacy-launch test onto the no-repo fixture and confirmed it passes
- 13:55 UTC checked both tests fail when the guards are disabled, then reverted
- 14:20 UTC ran `go vet` and the full `internal/mcp` suite, which passed; the full `internal/jobs` run timed out
- 14:35 UTC ran the `Skills|Resume` subset of `internal/jobs`, which passed
- 14:38 UTC committed locally as `98e702d`
- 14:47 UTC continuation: confirmed the tree is clean and the commit is in place, wrote `PROGRESS.md`

## Summary
Issue #145 asked for two missing tests, and I added both in commit `98e702d` on branch `bench/145-sonnet-high`. One test checks that an already-queued legacy job (skills delivered, no manifest) is refused at launch. Three new `//` cases check that non-canonical source paths are rejected. No production code changed, and the working tree is clean. The full `internal/jobs` suite did not finish in this environment, so that package is verified only through a `Skills|Resume` subset.

## Findings / Changes
- `internal/jobs/resume_skills_test.go`: new `TestResumeQueuedLegacySkillsRefusedAtLaunch`.
  - A no-repo job gets skills delivered with a manifest, then fails. `Resume` admits it, and the test claims it as a launch would.
  - The test then clears the manifest in the DB, which makes the row a legacy one.
  - `prepareResume` must return an error containing "no verifiable baseline".
- `internal/mcp/source_files_test.go`: three new cases in `TestJobStartRejectsNonCanonicalSources`:
  - `dir + "//report.md"`
  - `"//" + origin` (a double slash right after the root)
  - `dir + "/linked//report.md"`
- Commit: `98e702d`.

## Evidence
- Reproduce, since behaviour is already correct: I broke each guard temporarily and reverted it afterwards.
  - The legacy-launch test passes on unmodified code. With `checkedSkillLinks` in `internal/jobs/skill_manifest.go` made to accept legacy rows (`return nil, nil`), it fails: the error is no longer "no verifiable baseline".
  - With the `filepath.Clean(path) != path` check in `internal/jobs/source_files.go` disabled, 8 `--- FAIL` lines appeared in `TestJobStartRejectsNonCanonicalSources`. I did not identify which subtests those were, so I did not confirm that all three new `//` cases are among them.
- `go test ./internal/mcp -run NonCanonical -v`: all 7 subtests PASS, including the 3 new ones.
- `go test ./internal/jobs -run QueuedLegacy -v`: PASS.
- `go test ./internal/jobs -run 'Skills|Resume' -timeout 400s`: `ok`, exit 0.
- `go test ./internal/mcp` (full package): `ok`.
- `go vet ./...`: exit 0.
- `gofmt -l internal`: no output.
- `git status --short` after the commit: clean.
