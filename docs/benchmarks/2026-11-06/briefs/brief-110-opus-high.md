# Issue #110: Test TestStartedDaemonNeverOutlivesItsTest can leak its helper if the PID wait fails

Follow-up from the final Codex review of #105 (commit 994dde2), out of the fix-loop budget.

internal/jobs/testproc_test.go:82-99: the regression test starts helper `c`, then waits for its PID file before registering cleanup. If `waitFor` fails, nothing kills and joins the helper; in `hang` mode the helper and its daemon survive the subtest until the two-minute sleep ends.

Fix: register the helper cleanup right after a successful `c.Start()`, before any wait, coordinated with the existing `c.Wait()` goroutine.

---
## Task (benchmark run, model tag: opus-high)
Implement the fix for the GitHub issue above in this repository (Go). Work only on what the issue asks.
- Read the code the issue points at before changing anything.
- Make exactly one tool call per step.
- Add or update tests that prove the fix; run `go test` for every package you touched and `go vet ./...`; paste the real exit codes in Evidence.
- Commit on a new branch `bench/110-opus-high` with a clear English commit message. Do not push, do not open a PR, do not message anyone.
- If part of the issue needs an owner decision or is out of reach, do the rest and list it under "Not done / uncertain".
