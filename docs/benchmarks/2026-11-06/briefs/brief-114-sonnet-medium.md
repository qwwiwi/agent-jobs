# Issue #114: Git runner: cat-file descendants survive the timeout when stdout is an *os.File

Found by the Codex sol control round on PR #112 (head ba62ebb), the fix-loop cap for #100 is reached, so this is a follow-up.

**Problem.** `materializeTree` (internal/jobs/gate.go:516) passes stdout as an `*os.File`, which bypasses the runner's output tracking (internal/jobs/gitrun.go:92). `p.copied` closes at once, the reaper reaps git (gitrun.go:171) while a descendant still holds the manually read stdout pipe, and at the deadline `p.kill()` is a no-op because `reaped` is already true. The call returns errGitTimeout, but the descendant keeps running.

**Fix.** Make manually consumed streams take part in the pre-reap completion protocol: keep the PID reservation until those streams finish or the group is killed.

**Regression test.** A fake `cat-file` starts `sleep 60` inheriting stdout, records its PID and exits at once. Assert that `materializeTree` returns errGitTimeout and that the child has ended.

Severity: Medium. Also open: the darwin wait path (kqueue/sysctl) is cross-built and vetted but not yet run on macOS.

---
## Task (benchmark run, model tag: sonnet-medium)
Implement the fix for the GitHub issue above in this repository (Go). Work only on what the issue asks.
- Read the code the issue points at before changing anything.
- Make exactly one tool call per step.
- Add or update tests that prove the fix; run `go test` for every package you touched and `go vet ./...`; paste the real exit codes in Evidence.
- Commit on a new branch `bench/114-sonnet-medium` with a clear English commit message. Do not push, do not open a PR, do not message anyone.
- If part of the issue needs an owner decision or is out of reach, do the rest and list it under "Not done / uncertain".
