[jobs:job-done] job 261106-131244-e803 BENCH M #154 glm succeeded · 7 turns · $0.51 · 38m · model openrouter:z-ai/glm-5.3 · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain

- The issue's alternative "or concatenate" for report selection was not implemented: "last result that has the report sections" covers the observed incident (a full report followed by a notification) and every existing test scenario; concatenation has no observed case to validate against.
- `num_turns`/`total_cost_usd` per-run (not cumulative) semantics were taken from the incident stream itself (2nd result carried `num_turns: 1` on an 87-turn run) and the wrapper's own doc comment; the Claude API skill reference does not document CLI stream-json fields, so no second source confirmed it.
- No UI changes in this diff, so no browser check applies.

## Progress

- 13:44 UTC изучил репозиторий и нашёл место бага (wrapper.go resultTee.end)
- 13:48 UTC локализовал фикс и существующие тесты с двумя result-событиями; создал ветку bench/154-glm
- 13:50 UTC добавил режим FAKE_BG_NOTIFY в fake claude и два регрессионных теста
- 13:55 UTC воспроизвёл баг: e2e падает с "report has no Summary section", tee-тест с 1/0.25
- 14:00 UTC применил фикс в resultTee.end() (сумма turns/cost, отчёт у последнего result с секциями)
- 14:05 UTC обновил 4 закреплённых теста на суммы по запуску; целевые тесты зелёные
- 14:10 UTC go vet и gofmt чистые; все пакеты кроме internal/jobs прошли
- 14:21 UTC полный прогон internal/jobs прошёл (972 с); коммит 09d4fa8 на bench/154-glm

## Summary

Issue #154: a background-task notification wakes the claude session after its run ended, and that extra turn's `result` event replaced the wrapper's whole `StreamResult`, so the job's report.md became the notification's short text (no Summary section → job marked failed mid-task) and turns/cost collapsed to that one turn. Fixed in the stdout tee: turns/cost now sum over the `result` events and the report comes from the last result text that follows the report contract. Committed locally as `09d4fa8` on branch `bench/154-glm`.

## Findings / Changes

- `internal/jobs/wrapper.go:244-264` (`resultTee.end`, commit `09d4fa8`): when a later `result` event arrives after an earlier one was already read, its `num_turns`/`total_cost_usd` (which cover only that turn — the incident stream 261106-040807-362a had the 2nd result carrying `num_turns: 1`) are added to the running totals instead of replacing them, and `Text` keeps the earlier result's text unless the new one is itself a report (`isReport`, same predicate the wrapper already uses everywhere). The last event still decides `subtype`/`is_error`, as before. Doc comment on the `Results` field updated (`wrapper.go:50-53`).
- `internal/jobs/jobs_test.go:267-277`: new fake-claude mode `FAKE_BG_NOTIFY` reproducing the incident shape: a full-contract report result (87 turns, $1.50), an assistant notification turn, then a second result (1 turn, $0.25) with the notification's text.
- `internal/jobs/idle_test.go:236-254`: e2e regression test `TestTrailingNotificationResultKeepsReport` — job must succeed, turns/cost 88/$1.75, report.md is the first result's report.
- `internal/jobs/hardening1_test.go:137-166`: tee-level regression test `TestResultTeeTrailingNotification` (two-result stream, JSON lines built with `json.Marshal`).
- Updated four tests that pinned the old last-wins semantics to the run total: `TestResultTee` (2/0.75 → 3/1.25), `TestIdleAfterResultClosesFinishedJob` (6/0.75 → 10/1.25), `TestIdleCloseOnFallbackRun` (6/0.75 → 10/1.25), `TestIdleAfterResultNotClosedWhileWorking` (3 → 4).

Gate jobs are unaffected: `gate.go:1102` still requires `res.Results == 1` and rejects a stream with a second result event, which is correct for their stricter trust model.

### Requirements
- R01 (the fix, with reproduce-first regression tests and a local commit): done.

## Evidence

- Before the fix, both new tests failed exactly as the issue describes:
  - `go test ./internal/jobs/ -run 'TestTrailingNotificationResultKeepsReport'` → `job: failed "report has no Summary section: likely stopped mid-task"`.
  - `TestResultTeeTrailingNotification` → `turns/cost 1/0.25, want the run total 88/1.75`.
- After the fix, targeted run: `go test ./internal/jobs/ -run 'TestResultTee|TestTrailingNotificationResultKeepsReport|TestIdleAfterResult|TestIdleCloseOn' -count=1` → `ok github.com/qwwiwi/agent-jobs/internal/jobs` (exit 0).
- Full package: `go test -count=1 -timeout 45m ./internal/jobs/` → `ok ... 972.330s` (exit 0). Note: `scripts/check.sh` documents this package takes ~14 min and sets `-timeout 45m`; a first attempt with go test's default 10-minute binary timeout hit the timeout, not a hang.
- All other packages: `go test -count=1 -timeout 15m $(go list ./... | grep -v internal/jobs)` → all `ok` (root, cmd/agent-jobs, examples/manifest-check, internal/{brain,casefold,checklist,guard,httpapi,mcp,skills,store,testutil,trash}), exit 0.
- `go vet ./...` → exit 0, no output. `gofmt -l .` → empty, exit 0.
- `git log --oneline -2` → `09d4fa8 Keep the report when a trailing result event follows the run's own (#154)` on `bench/154-glm`, working tree clean.
