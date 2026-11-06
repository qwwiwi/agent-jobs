[jobs:job-done] job 261106-131244-6101 BENCH S #145 glm succeeded · 5 turns · $0.18 · 42m · model openrouter:z-ai/glm-5.3 · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- No `Requirements:` trailer on the commit: the brief lists no requirement IDs (no CHECKLIST.md requirements named for this job).
- `go test ./...` for the remaining packages was not run separately; the touched packages (`internal/jobs`, `internal/mcp`) were run in full, and `go vet ./...` covers the whole module. UI checks not applicable (no UI changes).

## Progress
- 14:29 UTC Read repo root, confirmed base commit 8f6e8f9
- 14:30 UTC Read resume_skills_test.go, resume.go, skill_manifest.go, source_files.go, mcp/source_files_test.go; wrote PROGRESS.md
- 14:31 UTC Added TestResumeLegacyQueuedJobLaunchRefused (first version failed at worktree.json check)
- 14:32 UTC Added markWorktree to fixture → test PASS; added two double-slash cases → PASS
- 14:32 UTC Mutation-verified both guards fail the new tests; reverted mutations
- 14:33 UTC go vet exit 0; gofmt clean; first full test run hit go test's default 10m package timeout (host busy with parallel benchmark jobs)
- 14:34 UTC Updated PROGRESS.md
- 14:53 UTC Re-ran full tests with -timeout 45m in background
- 15:10 UTC Full run ok: internal/jobs 1007s, internal/mcp 147s, exit=0
- 15:11 UTC Created bench/145-glm, committed 08c0059

## Summary
Issue #145 asked for two missing test coverage items (behaviour already correct): direct launch-time rejection coverage for an already-queued legacy job (skills delivered, no manifest), and `//` (double slash) regression cases for non-canonical source paths. Added both tests; they pass with the current code and fail when the corresponding guards are mutation-disabled. Committed `08c0059` on `bench/145-glm`.

## Findings / Changes
- `internal/jobs/resume_skills_test.go:237` — new `TestResumeLegacyQueuedJobLaunchRefused`: after `deliverSkills` + `markWorktree` (needed so `prepareResume` passes `verifyWorktree` and reaches the delivery check), the job is finished, resumed (`m.Resume` succeeds while the manifest is intact), claimed, then the manifest is cleared via `m.Store.SetSkillsDelivered` to model a pre-manifest legacy row; `m.prepareResume(j, "resume-test")` must refuse with "no verifiable baseline". Fails when the `checkedSkillLinks` no-manifest refusal (`internal/jobs/skill_manifest.go:101`) is removed — but with a different message, because that mutation also leaves the delivery unverified. The test guards the launch path (queueResume admission coverage already existed in `TestResumeLegacySkillsAfterCleanupRefused`).
- `internal/mcp/source_files_test.go:75` — two new table cases in `TestJobStartRejectsNonCanonicalSources`: `{"double slash before leaf", dir + "//report.md"}` and `{"double slash inside directory", filepath.Dir(dir) + "//" + filepath.Base(dir) + "/report.md"}` (constructed via Dir/Base so the parent stays canonical). Both must be rejected as "non-canonical" by `openAbsoluteSource`'s `filepath.Clean(path) != path` check (`internal/jobs/source_files.go:74`).
- Commit `08c0059` on branch `bench/145-glm` (test-only, +39 lines). No production code changed.

## Evidence
- `go test ./internal/jobs/ -run 'TestResumeLegacyQueuedJobLaunchRefused' -v` → `--- PASS (0.78s)`, package `ok`. Before the `markWorktree` fixture fix it failed at `worktree.json: no such file or directory` (prepareResume stopped at worktree verification, never reaching the delivery check) — fixed by recording the worktree as the first launch's prepare step does.
- `go test ./internal/mcp/ -run 'TestJobStartRejectsNonCanonicalSources' -v` → both new subtests `PASS`, plus the 4 pre-existing ones.
- Mutation check 1: removed the `no verifiable baseline` refusal from `checkedSkillLinks` → jobs test `--- FAIL` (delivery no longer verifiable). Reverted with `git checkout --`.
- Mutation check 2: dropped the `filepath.Clean(path) != path` condition from `openAbsoluteSource` → both new double-slash subtests `--- FAIL` (job_start accepted the paths), while `dot component` also failed as expected. Reverted with `git checkout --`.
- `gofmt -l internal/jobs/resume_skills_test.go internal/mcp/source_files_test.go` → empty (clean).
- `go vet ./...` → exit 0.
- Full run: `go test -count=1 -timeout 45m ./internal/jobs/ ./internal/mcp/` → `ok github.com/qwwiwi/agent-jobs/internal/jobs 1007.980s`, `ok github.com/qwwiwi/agent-jobs/internal/mcp 147.340s`, exit 0. An earlier run with go test's default timeout failed only on the 10-minute package timeout — `scripts/check.sh:14` itself documents `internal/jobs` takes ~14 minutes on a busy host and sets `-timeout 45m`; several parallel benchmark jobs were running full test suites on this host at the same time.
- `git status --short` after commit: clean; branch `bench/145-glm`, base `8f6e8f9`.
