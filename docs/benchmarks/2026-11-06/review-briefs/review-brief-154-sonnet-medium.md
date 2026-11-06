# Issue #154: Report lost when a background-task notification adds a trailing result event

A job whose claude session started a background task (e.g. a wait timer while tests run) can get a SECOND `result` event after it already wrote its report: the background task's notification wakes the session for one more turn, and that turn's short text becomes the job's final result.

Seen on 261106-040807-362a (GLM 5.3, #116): stream.jsonl has 2 `result` events. The first (87 turns) holds the full report (Summary / Findings / Evidence / Not done / Progress); the second (1 turn) is «Это уведомление от моего же таймера ожидания ...». The daemon kept the last one, so report.md lost every section, the job-done line said `failed · 1 turns · report has no Summary section: likely stopped mid-task`, while the work was complete (commit ef81c01, tests green).

Fix: when a stream carries several `result` events, take the report from the last one that has the report sections (or concatenate), keep turns/cost from the run total, and never mark the job failed because a trailing notification turn has no Summary. Regression test with a two-result stream.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


---
## Review task (benchmark)
Review the change on branch `bench/154-sonnet-medium` (diff `8f6e8f9e5969b205fa9e400b66d0728ba39602cd..HEAD` in this worktree) as the fix for the issue above.
Judge correctness, whether the issue is fully fixed, regressions, test quality. Do not modify files.
End the report with exactly one line in this form, counts of findings by severity:
BENCH-VERDICT: critical=<n> high=<n> medium=<n> low=<n> issue_fixed=<yes|partly|no>
