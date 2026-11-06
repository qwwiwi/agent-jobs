# Issue #134: Serialized gate runner: head assert, exit marker, process-group kill, clean env

Heavy local gates (check.sh, pg suites, vite builds) caused OOM, orphan suites, a gate on the wrong HEAD and hung waiters (about 3-4 h in two weeks). One gate at a time per box, assert HEAD, always print exit=, kill the process group on timeout, drop TELEGRAM_* from env.

Source: speed and autonomy audit 2026-11-04.

---
## Review task (benchmark)
Review the change on branch `bench/134-opus-high` (diff `8f6e8f9e5969b205fa9e400b66d0728ba39602cd..HEAD` in this worktree) as the fix for the issue above.
Judge correctness, whether the issue is fully fixed, regressions, test quality. Do not modify files.
End the report with exactly one line in this form, counts of findings by severity:
BENCH-VERDICT: critical=<n> high=<n> medium=<n> low=<n> issue_fixed=<yes|partly|no>
