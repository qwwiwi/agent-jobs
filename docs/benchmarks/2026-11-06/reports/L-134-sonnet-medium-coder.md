[jobs:job-done] job 261106-131244-8905 BENCH L #134 sonnet-medium succeeded · 13 turns · $0.25 · 4m · model sonnet · effort medium · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- `scripts/check.sh` and the pg-suite and vite-build gates are not wired through `gate.sh`. The issue names no entry points, so the call sites are for the owner to decide.
- `check.sh`'s own timeout and the gate's `--timeout` are separate; I did not add a default timeout.
- Linux only, since the script needs `flock` and `setsid`. macOS lacks both by default, and the Mac mini was not tested.
- The CHANGELOG and README were not updated.
- I did not run `scripts/check.sh` as a whole, only the pieces listed in Evidence.

## Progress
- --:-- Read `scripts/check.sh` and the existing script tests.
- --:-- Wrote `scripts/gate.sh`.
- --:-- Wrote `gate_script_test.go`; the first attempt was blocked by the guard, so I redid it with Edit/Write.
- --:-- Fixed the pipe hold-up in the timeout watcher.
- --:-- Ran `go test` and `go vet`, then committed `8418c96` on `bench/134-sonnet-medium`.

## Summary
Issue #134 asks for a runner that serializes heavy local gates. I added `scripts/gate.sh` plus `gate_script_test.go`, and committed them locally on `bench/134-sonnet-medium`. The runner takes a per-box lock, asserts HEAD, always prints `exit=N`, kills the whole process group on timeout, and drops `TELEGRAM_*` from the environment. The new tests, the root-package tests and `go vet` all pass.

## Findings / Changes
Commit `8418c96` adds two files:
- `scripts/gate.sh`. Usage: `gate.sh [--head sha] [--timeout s] [--lock f] [--wait s] -- cmd...`.
  - **Lock:** it takes an exclusive `flock` on `GATE_LOCK` (default `$TMPDIR/agent-jobs-gate.lock`). A waiter gives up after `--wait` seconds (default 3600) with exit 125.
  - **HEAD:** `--head` is a prefix match against `git rev-parse HEAD`. A mismatch prints a message and exits 126 before the lock is taken.
  - **Process group:** the command runs under `setsid`. On timeout the script sends TERM to the group, then KILL after 5 seconds, and exits 124. It also kills any leftover group members after the command exits.
  - **Environment:** every `TELEGRAM_*` variable is removed with `env -u`. The lock file descriptor is closed for the child, so a surviving child cannot hold the lock.
  - **Exit marker:** the last line is always `exit=<code>`. Codes: 124 timeout, 125 usage error or busy lock, 126 wrong HEAD, otherwise the command's own code.
- `gate_script_test.go` has six tests:
  - `TestGatePrintsExitMarker`
  - `TestGateRefusesWrongHead`
  - `TestGateDropsTelegramEnv`
  - `TestGateTimeoutKillsProcessGroup`
  - `TestGateSerializes`
  - `TestGateUsageErrors`

## Evidence
- `go test -count=1 -run TestGate -v .` gives exit 0 and all six tests pass. The timeout test takes 1.31s and `TestGateSerializes` takes 3.01s.
- `go test -count=1 .` gives exit 0 (`ok github.com/qwwiwi/agent-jobs 64.6s`).
- `go vet ./...` gives exit 0, and `gofmt -l .` prints nothing.
- **Before the change:** `scripts/gate.sh` did not exist, so every new test would fail with exit 127 and no `exit=` line. I did not run the tests against the base commit to confirm that.
- **Grandchild kill:** `TestGateTimeoutKillsProcessGroup` starts a `sleep 300` grandchild and checks that it is dead after the timeout.
- **First draft:** the timeout test took 6.3s because the watcher's `sleep` held the output pipe open. I fixed that in the final script, and the test now takes 1.3s.
