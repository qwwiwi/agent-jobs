[jobs:job-done] job 261106-131243-c033 BENCH M #154 sonnet-medium succeeded · 27 turns · $0.35 · 23m · model sonnet · effort medium · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- The full `go test ./internal/jobs/` did not finish here, so it is unverified. This sandbox has slow fsync, and the cause is not tied to this change. I only ran the relevant subsets listed above.
- Other packages: I ran only `go vet ./...` on them, not `go test`. The change touches only `internal/jobs`.
- Several `-run` patterns containing `|` were rejected by the command guard, so I ran each pattern separately. `Codex`, `Report`, `Midtask` and `Stats` were not run on their own.
- Judgement call: any later result event whose text is not a report is merged into an earlier report, including an error result. It keeps the later status, so a real failure still shows as a failure, but the report text is preserved.
- I did not touch the gate's one-result-event rule. The issue concerns claude jobs that start background tasks, not gate jobs.

## Progress
- --:-- Read `ParseStream`, `resultTee` and the report-writing code in `wrapper.go`
- --:-- Wrote the two-result regression tests and confirmed they failed
- --:-- Added `mergeResult` and wired it into `ParseStream` and `resultTee`
- --:-- Ran `go vet` and the test subsets, which passed; the full suite timed out
- --:-- Committed on `bench/154-sonnet-medium` (f453e64)

## Summary
Issue #154: a trailing `result` event from a background-task notification replaced the report, so the job was marked failed. I added `mergeResult` in `internal/jobs/wrapper.go`. When a later result event has no report but an earlier one did, the earlier report text is kept. Turns are summed and cost is the larger of the two. The status fields (success or error) come from the later event. It is applied in both `ParseStream` and the wrapper's `resultTee`. I committed it as f453e64 on `bench/154-sonnet-medium`. The two new tests and the subsets I ran pass, but the full `internal/jobs` suite never finished in this sandbox, so I haven't seen it pass.

## Findings / Changes
- `internal/jobs/wrapper.go`: new `mergeResult(prev, next)`. `ParseStream` and `resultTee.end` both call it, so the daemon and the wrapper agree.
- `internal/jobs/hardening1_test.go`: added `TestParseStreamKeepsReportOverTrailingResult` and `TestResultTeeKeepsReportOverTrailingResult`, both using a two-result stream (87 turns with a full report, then 1 turn with a timer notification).
- `report.md` is written from `res.Text`, so it now keeps all report sections. The "report has no Summary section" note no longer fires for such a stream.
- The tee's `Results` counter still counts 2, so the gate's "exactly one result event" check is unchanged.

## Evidence
- Before the change, both new tests failed. `ParseStream` returned `Turns:1`, `CostUSD:2.6` and `Text` equal to the notification text. The tee gave the same, with `Results:2`.
- After the change:
  - `go test ./internal/jobs/ -run KeepsReportOverTrailing` passed; I didn't capture its exit code.
  - `-run Tee`, `-run Stream`, `-run Verdict`, `-run Trusted` and `-run Idle` all returned `ok`.
  - `go vet ./...` produced no output and exited 0.
  - `gofmt -l internal` listed nothing.
- I separately ran the whole `internal/jobs` package twice, once plain and once with `-skip Manifest`. Both runs hit the Go test timeout (10 minutes and 9m10s). The stack traces show a manifest test and a sqlite fsync in cleanup, not an assertion. The first run printed no `--- FAIL` lines; I didn't grep the second.
