# Issue #114: Git runner: cat-file descendants survive the timeout when stdout is an *os.File

Found by the Codex sol control round on PR #112 (head ba62ebb), the fix-loop cap for #100 is reached, so this is a follow-up.

**Problem.** `materializeTree` (internal/jobs/gate.go:516) passes stdout as an `*os.File`, which bypasses the runner's output tracking (internal/jobs/gitrun.go:92). `p.copied` closes at once, the reaper reaps git (gitrun.go:171) while a descendant still holds the manually read stdout pipe, and at the deadline `p.kill()` is a no-op because `reaped` is already true. The call returns errGitTimeout, but the descendant keeps running.

**Fix.** Make manually consumed streams take part in the pre-reap completion protocol: keep the PID reservation until those streams finish or the group is killed.

**Regression test.** A fake `cat-file` starts `sleep 60` inheriting stdout, records its PID and exits at once. Assert that `materializeTree` returns errGitTimeout and that the child has ended.

Severity: Medium. Also open: the darwin wait path (kqueue/sysctl) is cross-built and vetted but not yet run on macOS.

---
## Review task (benchmark)
Review the change on branch `bench/114-opus-high` (diff `8f6e8f9e5969b205fa9e400b66d0728ba39602cd..HEAD` in this worktree) as the fix for the issue above.
Judge correctness, whether the issue is fully fixed, regressions, test quality. Do not modify files.
End the report with exactly one line in this form, counts of findings by severity:
BENCH-VERDICT: critical=<n> high=<n> medium=<n> low=<n> issue_fixed=<yes|partly|no>
