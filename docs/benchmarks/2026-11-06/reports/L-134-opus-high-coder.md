[jobs:job-done] job 261106-131244-8ea0 BENCH L #134 opus-high succeeded · 61 turns · $2.60 · 35m · model opus · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- **Commit message language:** the owner rules say Russian, but the brief says English and the repo's `scripts/check-commit-messages.sh` rejects Cyrillic. My first local commit had a Russian subject; I amended it to English before finishing (never pushed).
- **Wiring into callers:** nothing calls `gate run` yet. Making `check.sh`, the orchestrator's workflow or other repos' gates go through it is outside this repo's code.
- **Inside a job**, `agent-jobs gate run` is refused like every CLI command (`refuseInsideJob`). So a job's own `go test` or `check.sh` is not serialized by this lock. Changing that would touch the guard and the nested-job policy and needs an owner decision.
- **Escapes:** a process that leaves the group with `setsid` isn't killed. The runner then waits at most 5 s more for output pipes it still holds (`WaitDelay`).
- **"Per box" means per OS user:** the lock lives under `<agent-jobs home>`. Agents running as different OS users, or with different `AGENT_JOBS_HOME` values, don't share it unless they pass the same `--lock`.
- **Defaults are my choice:** 30m timeout and 60m lock wait; the issue gives no values.
- Only the full test runs of the touched packages were done, not `scripts/check.sh` as a whole.
- No subagents were used.
- No instructions were found embedded in the data I read.

## Progress
- 13:54 UTC Read the job directory, repo layout, `gate.go` and `acceptance.go` (the existing group kill and env filtering)
- 13:55 UTC Read the checklist lock pattern, `Home()`, the git and no-reap helpers; wrote PROGRESS.md
- --:-- Wrote `internal/jobs/gaterun_test.go`; it failed to build (undefined GateRun)
- --:-- Wrote `internal/jobs/gaterun.go`; fixed a test helper name clash; 7 tests pass
- --:-- Added the `gate run` CLI, usage text and TestGateRunCLI; build and CLI tests ok
- --:-- Mutation check: 6 tests failed under 6 regressions; file restored
- --:-- Added the ARCHITECTURE and CHANGELOG entries
- 14:01 UTC gofmt clean, `go vet ./...` exit 0
- --:-- Package tests: cmd and root ok; internal/jobs hit the 10m default; reran with 30m
- --:-- Committed on `bench/134-opus-high`, amended the subject to English; message check exit 0
- 14:28 UTC `internal/jobs` full run ok (1009s); final PROGRESS.md update

## Summary
Issue #134 asks for a gate runner for heavy local gates: one gate at a time per box, a HEAD check, an `exit=` line printed every time, the whole process group killed on timeout, and `TELEGRAM_*` removed from the environment. I added `agent-jobs gate run` (code in `jobs.RunGate`), which does all five. The new tests pass, and go vet plus the full test runs of the touched packages exit 0. Everything is in one local commit, `e261687`, on branch `bench/134-opus-high`. Nothing was pushed.

## Findings / Changes
- **Starting point:** the existing acceptance runner (`internal/jobs/acceptance.go:408` `runShellGroup`) already kills its process group and runs with a filtered environment. But nothing ran the orchestrator's own heavy gates (check.sh, pg suites, vite builds) in a serialized way, nothing checked HEAD, and nothing guaranteed an exit marker. That is the gap the issue describes, so I built a new runner rather than changing acceptance.
- **`internal/jobs/gaterun.go` (new):**
  - **One at a time per box:** `lockGate` takes an exclusive `flock` on `<agent-jobs home>/gate.lock`, shared by all agents of the user. Waiting is capped by `--lock-wait` (default 60m), so a waiter can't hang forever. The holder writes one line into the lock file (pid, start time, commit, dir, command), and waiters print it.
  - **HEAD check:** `assertGateHead` runs after the lock is taken, so a tree that moved during the wait is caught. HEAD must match `--head` (any revision git resolves, compared as full ids). It is checked again after the command: if HEAD moved, a passing gate becomes exit 125. `--head` is required.
  - **Process-group kill:** `runGateCmd` makes the command the leader of its own process group, with stdin at `/dev/null`.
    - On timeout, or on SIGTERM/SIGINT/SIGHUP, the group gets SIGTERM, then SIGKILL after 5 s.
    - After the leader exits, the whole group gets SIGKILL. This uses the existing `waitExitNoReap`, so the leader is not yet reaped and its group id can't have been reused. Background leftovers of a suite therefore don't survive.
  - **Clean env:** `GateEnv` drops every `TELEGRAM_*` variable.
  - **Exit marker:** a deferred `exit=N` is always the last stdout line. Codes: the command's own; 128+signal; 124 on timeout; 143 when stopped; 125 when the runner refuses or fails (usage, lock, wrong or moved HEAD); 127 when the command can't start.
- **`cmd/agent-jobs/gaterun.go` (new):** `gate run --head REV [--repo DIR] [--timeout 30m] [--lock-wait 60m] [--lock PATH] -- CMD [ARGS...]`. Flags stop at the command, so the command's own flags pass through untouched. Usage errors also print `exit=125`.
- **`cmd/agent-jobs/main.go`:** routes `gate run`, and the usage text plus the `gate` usage error now mention it.
- **Docs:** a "Gate runner" paragraph in `docs/ARCHITECTURE.md` (before the acceptance command section) and an entry under Unreleased/Added in `CHANGELOG.md`.
- **Tests:**
  - `internal/jobs/gaterun_test.go`: TestRunGateRefusesWrongHead, TestRunGatePrintsExitMarker, TestRunGateTimeoutKillsProcessGroup, TestRunGateKillsLeftoversAfterExit, TestRunGateDropsTelegramEnv, TestRunGateSerialized, TestRunGateHeadMovedDuringRun.
  - `cmd/agent-jobs/gaterun_test.go`: TestGateRunCLI.

## Evidence
- **Before the fix:** `go test ./internal/jobs/ -run TestRunGate` gave `undefined: GateRun`, `undefined: GateExitRunner`, `FAIL [build failed]`, exit=1. The runner didn't exist, so all new tests failed.
- **After the fix:** `go test ./internal/jobs/ -run TestRunGate -count=1 -v`: all 7 PASS, exit=0.
- **Mutation check** (my substitute for a behavioural "before" run). I put six regressions into a temporary copy of `gaterun.go` and ran the tests. Each mutated guarantee had its test fail, then I restored the file and confirmed it was byte-identical with `cmp`:

  | Regression | Test that failed |
  |---|---|
  | No group kill after the leader exits | KillsLeftoversAfterExit |
  | SIGTERM sent only to the leader | TimeoutKillsProcessGroup |
  | TELEGRAM_* kept | DropsTelegramEnv |
  | Lock skipped | Serialized |
  | HEAD checks before/after removed | RefusesWrongHead, HeadMovedDuringRun |

  Result: 6 `--- FAIL`, exit=1. TestRunGatePrintsExitMarker had no mutation of its own.
- `go vet ./...`: exit 0. `gofmt -l internal cmd`: no output.
- `go test ./internal/jobs/ ./cmd/agent-jobs/ . -count=1`:
  - `cmd/agent-jobs`: ok (27.6s); root package: ok (53.8s).
  - `internal/jobs` hit go's default 10-minute timeout (`panic: test timed out after 10m0s` while TestMilestoneLaunchRechecks, an existing test, was running). `scripts/check.sh` already notes this package takes about 14 minutes and passes a longer timeout.
- Rerun `go test ./internal/jobs/ -count=1 -timeout 30m`: `ok ... 1009.087s`, exit 0.
- `bash scripts/check-commit-messages.sh`: exit 0.
