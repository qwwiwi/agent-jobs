[jobs:job-done] job 261106-131243-4f15 BENCH S #110 sonnet-high succeeded · 22 turns · $0.34 · 21m · model sonnet · effort high · job dir 116.5 MB · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

The base commit times out the same way on the full package (no code of mine involved), so the full-package run is slow or hangs in this sandbox. I'll finish with the targeted evidence.

## Not done / uncertain
- Full `go test ./internal/jobs` did not complete in this sandbox, on my branch or on base. I only confirmed the targeted tests, `go vet ./...` and `gofmt`.
- I did not find why the full package is slow or hangs. It looks environmental, but I did not prove it.
- The commit message is in English and ends with the Co-Authored-By trailer, as the task requires. The owner rules ask for Russian commit messages, so there is a conflict.
- Leftover files: `base-full.log` is in the job directory. I briefly wrote and then deleted `/tmp/ownedfull.log`, which is outside the job directory.

## Progress
- --:-- Read `testproc_test.go` and `waitFor`.
- --:-- Extracted `startOwnedHelper` without cleanup and added the `idle` mode and the regression test.
- --:-- Reproduced the failure (exit=1).
- --:-- Added the cleanup right after `Start()`; the targeted tests pass, `vet` and `gofmt` are clean.
- --:-- Committed `11962db` on `bench/110-sonnet-high`.
- --:-- Checked the full-package timeout against the base commit; it fails the same way there.

## Summary
Issue #110: `TestStartedDaemonNeverOutlivesItsTest` registered the helper's cleanup only after waiting for the PID file. If that wait failed, the helper and its daemon stayed alive. I moved the helper start into `startOwnedHelper`, which registers the kill-and-join cleanup right after a successful `Start()`. I added a regression test that failed before the fix and passes after it. The full `internal/jobs` package test run does not finish in this sandbox, and the base commit behaves the same way.

## Findings / Changes
Commit `11962db` on branch `bench/110-sonnet-high`. The only file changed is `internal/jobs/testproc_test.go`.
- New `startOwnedHelper(t, mode, pidFile)` starts the helper. A single goroutine runs `c.Wait()` and closes an `exited` channel. `t.Cleanup` does `Process.Kill()` and then `<-exited`, so there is no double `Wait`.
- `TestStartedDaemonNeverOutlivesItsTest` uses it. Its `done` channel is now fed from `exited`.
- New helper mode `idle` in `TestOwnedHelperDaemon`: it sleeps for two minutes and never writes the PID file. It simulates a helper whose PID wait would fail.
- New test `TestOwnedHelperEndsWithItsTestWhenTheWaitFails`: a subtest starts an idle helper and ends. The parent then checks that the helper has exited within 5s. The test has its own safety-net kill so it never leaks the idle helper even when it fails.

## Evidence
- Before the fix (helper extracted but no cleanup): `go test ./internal/jobs -run TestOwnedHelperEndsWithItsTestWhenTheWaitFails -count=1` gave `FAIL … the helper is still running after the test that started it ended`, exit=1.
- After the fix: `go test ./internal/jobs -run 'TestOwnedHelper|TestStartedDaemon' -count=1 -v` gave exit=0.
  - `TestStartedDaemonNeverOutlivesItsTest/fail` and `/hang` both PASS.
  - `TestOwnedHelperEndsWithItsTestWhenTheWaitFails` PASS.
  - `TestOwnedHelperDaemon` SKIP, as designed.
- `gofmt -l internal/` printed nothing.
- `go vet ./...` gave exit=0.
- After the runs, `pgrep` showed no leftover `TestOwnedHelper` processes.
- The full `go test ./internal/jobs` run failed with exit=1 after the 600s timeout. A second run with `-timeout 150s` also panicked on timeout, in a different test. The base commit `8f6e8f9` with `-timeout 150s` also exits 1 with the same kind of timeout panic. `TestManifestCheckLeftoverIsSweptBeforeTheRunner`, which was running when the 600s run was killed, passes alone on both base and my branch (exit=0, about 7s).
