[jobs:job-done] job 261106-131244-882e BENCH S #145 sonnet-medium succeeded · 16 turns · $0.24 · 16m · model sonnet · effort medium · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- The full `go test ./internal/jobs` run hit the 600s timeout. The panic was in `TestManifestCheckMustNotRewriteTheApprovedBrief` (`manifest_test.go:443`), which is unrelated to this change. I did not check whether it also times out on the base commit, so whole-package green is unverified.
- Nothing else is open.

## Progress
- --:-- UTC Read the existing tests and the `resume.go` launch path.
- --:-- UTC Added the `//` cases to `source_files_test.go`.
- --:-- UTC Wrote the launch-time legacy test and fixed two fixture problems.
- --:-- UTC Ran the targeted tests and `go vet`.
- --:-- UTC Committed `a412050` on `bench/145-sonnet-medium`.

## Summary
Issue #145 asked for two test gaps to be closed: launch-time rejection of a queued legacy job, and `//` regression cases for non-canonical source paths. I added both as tests only, with no production changes. The new tests pass. The full `internal/jobs` run timed out, so the whole package is not confirmed green.

## Findings / Changes
Commit `a412050` on branch `bench/145-sonnet-medium`:
- `internal/jobs/resume_skills_test.go`: new `TestResumeQueuedLegacySkillsRefusedAtLaunch`.
  - It builds a job with delivered skills and a manifest, then fails it and resumes it, so admission passes.
  - It claims the job and calls `SetSkillsDelivered` to clear the manifest, which makes it a legacy row.
  - It asserts that `prepareResume` refuses with "no verifiable baseline".
  - It also checks that the delivered link is untouched.
- `internal/mcp/source_files_test.go`: two new cases in `TestJobStartRejectsNonCanonicalSources`.
  - `dir + "//report.md"`.
  - `"/" + dir + "/report.md"`, which gives a leading `//`.

## Evidence
- `go test -run 'TestResumeQueuedLegacy' ./internal/jobs`: PASS.
- `go test -run 'NonCanonical' -v ./internal/mcp`: all 6 subtests PASS, including both new `//` cases.
- `go test -run 'Resume|Skills' ./internal/jobs`: exit 0 (`ok`, 86s).
- `go test ./internal/mcp`: ok.
- `go vet ./...`: exit 0. `gofmt -l internal` printed nothing.
- No "fails before" run: the issue says behaviour is already correct, so these are coverage tests that pass on the existing code. I did not temporarily break the code to watch them fail.
- The first drafts of the launch test failed on fixture problems, which I fixed:
  - `SetSkillsDelivered` needs a running job, so I now claim the job first.
  - The repo-backed fixture had no `worktree.json`, so I switched to the no-repo fixture used by the symlink test.
