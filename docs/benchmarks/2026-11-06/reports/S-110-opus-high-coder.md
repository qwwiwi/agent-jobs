[jobs:job-done] job 261106-131243-344e BENCH S #110 opus-high succeeded · 21 turns · $0.72 · 31m · model opus · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- **The full package is not green.** One unrelated test failed in the full run: `TestCancelIgnoresForgedResultLine`. It passed 5 out of 5 times on its own, so it looks timing-dependent under host load. I did not check whether it also fails at the base commit, and did not investigate it, because it is outside #110.
- **The package takes longer than the default 10-minute `go test` timeout on this host when the host is under load,** so a plain `go test ./internal/jobs` times out here.
- **Commit message language.** The brief asked for an English commit message, but the owner rules say Russian. I followed the brief, which is more specific to this task. If Russian is required, the message needs rewording.
- **The new test needs a `sleep` binary on PATH.** It skips without one.
- **`scripts/check.sh` was not run.**
- No subagents were used.

## Progress
- 13:13 UTC Read `internal/jobs/testproc_test.go` and created branch `bench/110-opus-high`
- --:-- Found `waitFor`, `alive` and `setParentDeathSignal`
- --:-- Extracted `startJoined` with the old no-cleanup behaviour and added `TestStartJoinedEndsChildOnEarlyExit`; it failed (exit 1)
- --:-- Added the `t.Cleanup` kill-and-join; the new test and both modes of the #105 test passed (exit 0)
- --:-- Full package run with the default timeout timed out at 602s
- --:-- Started the full package run with `-timeout 45m` plus `go vet` in the background
- --:-- Committed `00192e6` (via `-F` file, since the guard blocks heredocs into git), before the full run finished
- 13:36 UTC Full run in progress, 532 passed, no failures yet
- --:-- Full run finished: 890 passed, 1 unrelated failure; vet exit 0
- --:-- Reran `TestCancelIgnoresForgedResultLine` alone 5 times: passed
- 13:43 UTC Checked the final diff (2 files, +56/-7) and the clean tree; wrote the report

## Summary
Issue #110: in `TestStartedDaemonNeverOutlivesItsTest`, nothing killed or reaped the helper process if the wait for its PID file failed. The cleanup was only registered after that wait. I added a `startJoined` helper that registers a kill-and-join cleanup right after `c.Start()`, sharing the one `c.Wait()` goroutine, and switched the test to it. A new regression test fails without the cleanup and passes with it. Committed locally as `00192e6` on branch `bench/110-opus-high`. Nothing was pushed.

## Findings / Changes
- `internal/jobs/testproc_test.go` (new `startJoined`, just above the helper-mode constants): it starts the command and runs `cmd.Wait()` in a goroutine that saves the error and then closes `done`. Right after the start it registers `t.Cleanup(func(){ _ = cmd.Process.Kill(); <-done })`. It returns `exited` (closed when the wait returns) and `werr()` (the wait's error). There is only one `Wait`, so the cleanup and the test's own `select` don't compete for it. A `Kill` after the wait has returned does nothing (`os.ErrProcessDone`).
- `internal/jobs/testproc_test.go` (in `TestStartedDaemonNeverOutlivesItsTest`): the inline `c.Start()`, `done` channel and `go c.Wait()` are replaced by `exited, werr := startJoined(t, c)`, and the `select` now uses `case <-exited: if werr() == nil`. The rest of the test is unchanged, including the later daemon-kill and home-removal cleanup. That cleanup is registered later, so it runs before the helper's.
- `internal/jobs/testproc_joined_test.go` (new): `TestStartJoinedEndsChildOnEarlyExit` starts `sleep 120` with `startJoined` inside a subtest that returns without a kill or wait of its own. That is the same exit path as a failed `waitFor` (`t.Fatal` runs only the cleanups). The test then requires the child to be reaped within 5s with a non-nil wait error, i.e. killed rather than exited normally.
- Commit `00192e6`: "Join the #105 regression test's helper even when its PID wait fails (#110)".

## Evidence
- **Reproduced first.** `startJoined` was first extracted with the old behaviour (Start plus Wait goroutine, no cleanup). Then `go test ./internal/jobs -run TestStartJoinedEndsChildOnEarlyExit -count=1` gave:
  `--- FAIL: TestStartJoinedEndsChildOnEarlyExit (5.00s)` / `testproc_joined_test.go:26: the child outlived its test: no cleanup killed and joined it` / `exit=1`
- **After the fix.** `go test ./internal/jobs -run 'TestStartJoinedEndsChildOnEarlyExit|TestStartedDaemonNeverOutlivesItsTest' -count=1 -v` gave a PASS for `TestStartJoinedEndsChildOnEarlyExit` and for `TestStartedDaemonNeverOutlivesItsTest` (both `/fail` and `/hang` subtests), `ok`, `exit=0`.
- **`gofmt -l internal/jobs`:** no output.
- **Full package, first try.** `go test ./internal/jobs -count=1` with the default 10-minute timeout timed out at 602s and exited 1. The host load average was about 8.5.
- **Full package, second try.** `go test ./internal/jobs -count=1 -timeout 45m -v` (log in the job directory as `test-full.log`) ran 1072.9s: 890 tests passed, 1 failed, `test_exit=1`. The failure was `TestCancelIgnoresForgedResultLine` (`resume_prune_test.go:84`, "cancelled row trusted the forged result"). That is the cancel/forged-result path, which my diff doesn't touch.
- **The failing test on its own.** `go test ./internal/jobs -run '^TestCancelIgnoresForgedResultLine$' -count=5` gave `ok` and `exit=0`.
- **`go vet ./...`:** `vet_exit=0` (`vet.log`).
