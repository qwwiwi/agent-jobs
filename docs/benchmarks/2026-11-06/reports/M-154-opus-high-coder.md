[jobs:job-done] job 261106-131243-d40a BENCH M #154 opus-high succeeded · 6 turns · $1.64 · 49m · model opus · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- **Cost semantics are an assumption.** I treat `total_cost_usd` as the session's running total and take the maximum. I had no way to check this against claude's sources or the incident's stream.jsonl (outside my job dir). If it is actually per query, cost is under-counted by the trailing turn's cost (in the incident, a 1-turn turn).
- Gate jobs are unchanged. `gate.go:1102` (`res.Results != 1`) still rejects a gate result from a stream with two result events. That check is security-related and outside this issue's scope; a gate job with a background notification will still fail.
- `ParseStreamStats` (`stats.go:171`, diagnostic stats from stream.jsonl) still takes the last result's turns and cost. The job row uses the trusted wrapper values, so I did not touch it.
- I did not concatenate the trailing turn's text into the report (the issue offered that as an option). The report stays clean, and the notification text is dropped from report.md.
- The commit message is in English, as the brief asked, with one Russian line because the owner rules require Russian. This is a conflict between the two instructions.
- No subagents were used.
- The guard blocked two commands: a python heredoc and a sed with braces. I redid both with Edit, and wrote the commit message to a file used with `git commit -F`. Nothing was bypassed.

## Progress
- 13:25 UTC read wrapper.go/idle.go: the tee and ParseStream keep only the last result
- --:-- UTC added FAKE_BG_NOTIFY mode and multiresult_test.go; both new tests red with the issue's symptom
- --:-- UTC added mergeResult in wrapper.go, used in tee.end and ParseStream; new tests green
- --:-- UTC updated FAKE_IDLE_OK fixture to per-query num_turns
- 13:40 UTC created branch bench/154-opus-high; gofmt clean
- --:-- UTC full run: two old cumulative-turns expectations failed; updated idle_test and hardening1_test
- --:-- UTC full go test ./internal/jobs ok (972s), go vet ./... exit 0
- 14:13 UTC commit 524b513

## Summary
Issue #154: when a background task's notification woke a finished claude session for one more turn, a second `result` event replaced the report. That left report.md without its sections and failed the job with `report has no Summary section`. I fixed this in the wrapper. Result events are now merged: the run state comes from the last one, the report text from the last one that is a report, turns are the run total, and cost is the session's running total. The full `internal/jobs` test run and `go vet ./...` pass. Commit `524b513` on branch `bench/154-opus-high` (local only, not pushed).

## Findings / Changes
- `internal/jobs/wrapper.go`, new `mergeResult(prev, r)` (placed after `ParseStream`, ~line 120). It is applied both in the trusted tee (`resultTee.end`, the result branch, ~line 263) and in `ParseStream`:
  - subtype and is_error come from the last result. The run's real final state, including an error on the extra turn, is kept.
  - `Text` stays the last result text that starts with `## Summary`, unless the new result is itself a report, in which case it replaces the old one.
  - `Turns` is the sum of all `num_turns`, since claude counts them per query (87 and 1 in the incident).
  - `CostUSD` is the maximum of the `total_cost_usd` values (see uncertainty below).
  - The comment on `StreamResult.Results` now says that a second result can be a legitimate background-notification turn.
- Because `res.Text` now holds the report, the existing report.md writers (the regular path and `finishIdleClosed`) write the report, and the daemon no longer sets `NoteNoSummary`. Those writers were not changed.
- Tests:
  - New `internal/jobs/multiresult_test.go`.
  - New fake-claude mode `FAKE_BG_NOTIFY` in `internal/jobs/jobs_test.go`.
  - Three old fixtures and expectations assumed `num_turns` was cumulative, which the incident disproves. I changed them to per-query semantics:
    - `FAKE_IDLE_OK`: the second result's `num_turns` goes from 6 to 2. The expected run total stays 6/0.75.
    - `TestIdleAfterResultNotClosedWhileWorking` now expects 4 turns (1+3) instead of 3.
    - `TestResultTee` now expects 3 turns (1+2) instead of 2.

## Evidence
- New tests, red before the fix (`go test ./internal/jobs -run 'TestTeeTrailingResultKeepsReport|TestTrailingNotificationResultKeepsReport'` → exit=1):
  - `TestTeeTrailingResultKeepsReport`: `text taken from the trailing notification turn: "This is a notification from my own wait timer."`
  - `TestTrailingNotificationResultKeepsReport`: `job: failed "report has no Summary section: likely stopped mid-task"`. This is exactly the issue's symptom.
- After the fix, both pass, along with TestResultTee, all TestIdleAfterResult*, TestReportContractNote* and TestStreamReplacementCannotAuthorizeFallback. Run with `-v`: `ok ... 33.138s`, exit=0.
- First full run, with the default 10m timeout: it hit the go test timeout (`FAIL ... 601.246s`, a timeout panic, not a test failure).
- Second full run, `-timeout 45m`: `--- FAIL: TestResultTee` and `TestIdleAfterResultNotClosedWhileWorking` (`turns 4, want the final result's 3`). Both were the old cumulative-turns expectations, and I updated them.
- Final: `go test ./internal/jobs -count=1 -timeout 45m` → `ok github.com/qwwiwi/agent-jobs/internal/jobs 972.567s`, `test_exit=0`; `go vet ./...` → `vet_exit=0`; `gofmt -l internal/` → empty.
- `git log --oneline -1` → `524b513 Keep the report when a trailing result event follows it (#154)`, 5 files, +119/−13.
