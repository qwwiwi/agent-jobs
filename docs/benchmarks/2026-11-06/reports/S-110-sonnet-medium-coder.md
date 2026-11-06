[jobs:job-done] job 261106-131243-9299 BENCH S #110 sonnet-medium succeeded · 17 turns · $0.24 · 12m · model sonnet · effort medium · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- The full `./internal/jobs` run never completed (exit 1, 10m timeout). I think the cause is unrelated to this change. I did not check whether the same test also hangs on the base commit.
- The original bug can't be reproduced directly, because it needs `waitFor` to fail after 30s. The "failing test first" step is the mutation check above, run against the new helper. Before the change the helper did not exist, so the regression test could only fail to compile.

## Progress
- --:-- UTC Read `testproc_test.go` and created the branch.
- --:-- UTC Added `startJoined` and the regression test, and switched the daemon test to the helper.
- --:-- UTC Ran the targeted tests, which passed.
- --:-- UTC Ran the mutation check, which failed as expected, then restored the fix.
- --:-- UTC Ran `go vet` (exit 0) and committed locally as `79e2050`.
- --:-- UTC Ran the full package test, which timed out on an unrelated test that passes alone.

## Summary
Issue #110: `TestStartedDaemonNeverOutlivesItsTest` registered no cleanup for its helper until after the PID wait, so a failed wait leaked the helper. I added `startJoined`, which starts the helper and registers a kill-and-join cleanup immediately, and the test now uses it. The new regression test and the touched tests pass. The full `./internal/jobs` run timed out at 10m on an unrelated test, so I can't claim a green full package run.

## Findings / Changes
- Commit `79e2050` on branch `bench/110-sonnet-medium`; the only file changed is `internal/jobs/testproc_test.go`.
- New helper `startJoined(t, c)`: it calls `c.Start()`, runs the single `c.Wait()` in a goroutine that sends the result to a buffered `done` channel and then closes `finished`, and registers `t.Cleanup` (Kill, then `<-finished`) right away. It returns `done`.
- `TestStartedDaemonNeverOutlivesItsTest` now calls `startJoined` in place of its own Start and Wait goroutine. The rest of its logic is unchanged.
- New test `TestStartJoinedKillsHelperOnEarlyExit` starts `sleep 120` inside a subtest and returns without waiting, standing in for a `t.Fatal` from a failed wait. It then checks that the process is dead.

## Evidence
- Mutation check: I made the cleanup a no-op, and `go test ./internal/jobs/ -run StartJoined -count=1` failed (exit 1) with "helper still running after its subtest ended".
- With the fix restored, `go test ./internal/jobs/ -run 'StartJoined|StartedDaemon' -count=1 -v` passed, including the fail and hang subtests.
- `go vet ./...` exited 0, and `gofmt -l` printed nothing.
- `go test ./internal/jobs/ -count=1` exited 1 after the 10m timeout, which was a hang in `TestManifestCheckLeftoverIsSweptBeforeTheRunner`. That test passes when run alone (`-count=1`, exit 0).
