[jobs:job-done] job 261106-131245-077c BENCH M #114 opus-high succeeded · 24 turns · $1.13 · 31m · model opus · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- `-race` was not run: there is no gcc on the host (`cgo: C compiler "gcc" not found`).
- The full `internal/jobs` run was not clean: `TestPrepareRecordsGroupAndProgress` failed once and passed 10/10 alone. It ran before `TestGitHeldReapedOnlyAfterRelease` was added, so that test has only been run on its own (it passed).
- I did not run the full `scripts/check.sh` (about 14+ minutes), only its parts: commit messages, gofmt, vet and the jobs tests.
- Scope limit: if `cat-file` exits normally and a descendant still holds stdout, that descendant still survives. After `releaseStreams` git is reaped, and only the copied streams have a `gitWaitDelay` kill. The issue only covers the timeout case, so I left this alone.
- The darwin wait path (kqueue/sysctl) mentioned in the issue was not run on macOS. The change itself has no platform-specific code.
- Owner rules say commit messages in Russian; the brief and the repo's message check want English. I followed the brief and wrote the message in English.
- No subagents used, no new dependencies.

## Progress
- 16:31 UTC Read `gitrun.go`, the runner tests and the linux wait code
- --:-- Read `materializeTree` (`gate.go`), the `startGit` callers and the test helpers
- --:-- Added `TestMaterializeTreeTimeoutKillsStdoutHolder`; it failed (child survived)
- --:-- Added `startGitHeld`/`releaseStreams` and the reaper wait; switched `materializeTree` to it; focused tests pass
- --:-- Full package run (stopped by my 600s tool timeout); `go vet` exit 0
- --:-- Second full run (verbose, in background), with my focused test run going at the same time
- --:-- Added `TestGitHeldReapedOnlyAfterRelease`; both new tests pass
- --:-- Full run finished: one flaky prepare test, which passes 10/10 alone
- --:-- `-race` not possible (no gcc)
- 17:01 UTC Committed `41138c2` on `bench/114-opus-high`; commit-message check exit 0; `PROGRESS.md` updated

## Summary
Issue #114: when `materializeTree` timed out, a descendant of `git cat-file` that held the stdout pipe kept running, because the runner had already reaped git and the group kill at the deadline did nothing. I added a "held" mode to the git runner: git is not reaped until the caller says it is done with its own streams, or until the group is killed. `materializeTree` now uses this mode. The new regression test failed before the change and passes after it. Commit `41138c2` on branch `bench/114-opus-high`, local only, not pushed.

## Findings / Changes
- `internal/jobs/gitrun.go`:
  - `gitProc` has two new fields, `held` and `heldOnce`. The comment above them explains the protocol.
  - `startGit` now wraps a new `startGitProc(..., held bool, ...)`. A new `startGitHeld` creates `p.held`.
  - In `reap()`, after the existing output-copy phase and before `reaped = true`, a held git waits for `<-p.held` or `<-p.killed`. While it waits, git stays a zombie, so its PID still reserves the process group's ID. A `kill()` at the deadline then signals the group, and the descendant dies too.
  - New `releaseStreams()` closes `held` once. It does nothing for a plain `startGit`.
- `internal/jobs/gate.go:517`: `materializeTree` calls `startGitHeld`. In its deferred cleanup it calls `p.releaseStreams()` after `in.Close()` and before `p.wait(ctx)`. This order is required: releasing after `wait` would block the normal path. When the call fails or times out, the existing `p.kill()` calls end the wait instead.
- Tests:
  - `internal/jobs/git_timeout_test.go`: new `TestMaterializeTreeTimeoutKillsStdoutHolder`. A fake `cat-file` starts `sleep 60` sharing its stdin and stdout, writes the child's PID and exits at once. The test sets `gitLongTimeout=1s` and `gitWaitDelay=30s`, so only the deadline can kill the child. It checks that the call returns `errGitTimeout` and that the child has ended.
    - The child gets stdin through fd 3 because a background command in sh gets `/dev/null` as stdin. Without it, writing to stdin would fail at once with EPIPE and there would be no timeout to test.
  - `internal/jobs/gitrun_test.go`: new `TestGitHeldReapedOnlyAfterRelease`. A held git that has exited is not reaped and can still be signalled as a group. `releaseStreams` lets the reaper finish, and after that `kill` signals nothing.

## Evidence
- **Before the fix:** `go test ./internal/jobs/ -run TestMaterializeTreeTimeoutKillsStdoutHolder -count=1` exited 1 with `git_timeout_test.go:381: cat-file's child 3281606 still runs after the deadline: git's group was not killed`. So the call did return a timeout, but the child survived.
- **After the fix:**
  - The two new tests passed (`TestMaterializeTreeTimeoutKillsStdoutHolder` 1.05s, `TestGitHeldReapedOnlyAfterRelease` 0.21s). Exit 0.
  - The focused run with `-run 'TestMaterializeTree|TestGit|TestDaemonGitCalls|Materialize|JudgeTree'` exited 0 (21.7s).
  - `TestGitHeldReapedOnlyAfterRelease` cannot run before the fix, because `startGitHeld` did not exist yet. It covers the new API, not the original bug.
- **Full package run** (`go test ./internal/jobs/ -count=1 -v -timeout 20m`): exit 1 after 1010s. 890 PASS lines and one FAIL: `TestPrepareRecordsGroupAndProgress`, with `repoprepare_test.go:347: prepare process group not recorded: 0`.
  - That test checks the prepare step's shell command, not the git runner. Rerun alone with `-count=10` it exited 0 (`ok … 31.201s`).
  - I take it as a timing flake under load, since my focused test run was going at the same time.
  - An earlier full run without `-v` was stopped at about 491s ("signal: terminated") by my tool's own 600s timeout, not by a test.
- `go vet ./...`: exit 0. `gofmt -l .`: no output. `scripts/check-commit-messages.sh`: exit 0.
