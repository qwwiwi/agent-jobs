# Issue #154: Report lost when a background-task notification adds a trailing result event

A job whose claude session started a background task (e.g. a wait timer while tests run) can get a SECOND `result` event after it already wrote its report: the background task's notification wakes the session for one more turn, and that turn's short text becomes the job's final result.

Seen on 261106-040807-362a (GLM 5.3, #116): stream.jsonl has 2 `result` events. The first (87 turns) holds the full report (Summary / Findings / Evidence / Not done / Progress); the second (1 turn) is «Это уведомление от моего же таймера ожидания ...». The daemon kept the last one, so report.md lost every section, the job-done line said `failed · 1 turns · report has no Summary section: likely stopped mid-task`, while the work was complete (commit ef81c01, tests green).

Fix: when a stream carries several `result` events, take the report from the last one that has the report sections (or concatenate), keep turns/cost from the run total, and never mark the job failed because a trailing notification turn has no Summary. Regression test with a two-result stream.

🤖 Generated with [Claude Code](https://claude.com/claude-code)


---
## Task (benchmark run, model tag: opus-high)
Implement the fix for the GitHub issue above in this repository (Go). Work only on what the issue asks.
- Read the code the issue points at before changing anything.
- Make exactly one tool call per step.
- Add or update tests that prove the fix; run `go test` for every package you touched and `go vet ./...`; paste the real exit codes in Evidence.
- Commit on a new branch `bench/154-opus-high` with a clear English commit message. Do not push, do not open a PR, do not message anyone.
- If part of the issue needs an owner decision or is out of reach, do the rest and list it under "Not done / uncertain".
