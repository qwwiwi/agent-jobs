[jobs:job-done] job 261106-131243-706c BENCH M #154 sonnet-high succeeded · 27 turns · $0.40 · 20m · model sonnet · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- Cost uses the larger of the two events on the assumption that `total_cost_usd` is cumulative for the session. I didn't confirm this against the claude docs or a real stream. If it is per-event, the cost is underreported in this rare case.
- A trailing result with `is_error` or a non-success subtype still replaces the status. A complete report followed by an error notification turn would still fail the job; the issue only mentions the missing-Summary case, so I left this.
- `ParseStreamStats` in `stats.go` still takes the last result's turns and cost for the stats row. The job row and job-done line come from the wrapper's record, which is fixed.
- `Results` still counts every result event, so a gate job with a trailing result still fails its "exactly one result" check. I left that unchanged.
- The first commit attempt was blocked by the guard (heredoc), so the commit used `-m`. The commit message is English with the Co-Authored-By trailer. The brief names no requirement IDs, so there is no `Requirements:` trailer.

## Progress
- --:-- Read wrapper.go, report.go, idle.go and stats.go to find where the result is chosen
- --:-- Wrote the failing tests and confirmed they fail
- --:-- Created branch `bench/154-sonnet-high`
- --:-- Added `mergeResult` and wired it into the tee and `ParseStream`
- --:-- Ran targeted tests, `go vet` and `gofmt`, all clean
- --:-- Ran the full `go test ./...`, exit 0
- --:-- Committed 47d0ca0

## Summary
Issue #154: a second `result` event (from a background-task notification) replaced the first one's full report, so report.md lost its sections and the job was marked failed. I added `mergeResult`, which keeps the earlier result's report when the later result isn't one. It is used by the live stdout tee and by `ParseStream`. Commit 47d0ca0 on `bench/154-sonnet-high`. The full `go test ./...` and `go vet ./...` both exit 0.

## Findings / Changes
- `internal/jobs/wrapper.go`: new `mergeResult(prev, next)`, applied in `resultTee.end` and in `ParseStream`. Only the wrapper's tee decides report.md, the job record and the job-done line; `ParseStream` isn't called anywhere outside tests.
  - If the earlier result starts with `## Summary` (`isReport`) and the later one doesn't, the result keeps the earlier text. Turns are the sum of both (87+1=88) and cost is the larger figure.
  - Otherwise the later result replaces the earlier one, as before. That covers two reports (the later wins) and two non-reports (the last wins, so `TestResultTee` is unchanged).
  - With the report kept, `reportContractNote` finds the Summary, so the "report has no Summary section" failure no longer fires.
- `internal/jobs/trailing_result_test.go` (new): three regression tests built on a two-result stream shaped like 261106-040807-362a (87 turns with the report, then 1 turn with the notification text).

## Evidence
- Before the fix, `go test ./internal/jobs -run 'TrailingResult|TeeKeeps|ParseStreamKeeps'` exited 1. `TestTeeKeepsReportOverTrailingNotificationResult` failed with `report text lost: "Это уведомление от моего же таймера ожидания; ..."`. `TestParseStreamKeepsReportOverTrailingNotificationResult` returned `Turns:1` and the notification text.
- After the fix, the same command plus `ResultTee` exited 0. `TestTrailingResultMergeRules` passed throughout because it covers behaviour that didn't change.
- `go vet ./...` exited 0; `gofmt -l .` printed nothing.
- `go test -count=1 -timeout 45m ./...` exited 0, with `internal/jobs` taking 1026s and every other package ok.
