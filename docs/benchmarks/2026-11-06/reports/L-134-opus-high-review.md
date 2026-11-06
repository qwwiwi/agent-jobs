[jobs:job-done] job 261106-143007-360e BENCH-REVIEW L #134 opus-high succeeded · 13 turns · cost unknown · 3m · model gpt-6.1-sol · effort high

## Not done / uncertain
Go tests and `scripts/check.sh` were not run: their fixtures/builds write files, prohibited by this read-only sandbox. Runtime process cleanup and signal behavior remain unverified. No files changed, commits made, or subagents used.

## Progress
- 17:22 UTC Inspected commit range, changed files and risky areas.
- 17:23 UTC Reviewed runner tests and existing process, environment and guard helpers.
- 17:24 UTC Checked formatting and lock-path reproductions; confirmed clean worktree.

BENCH-VERDICT: critical=0 high=1 medium=1 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: concurrency/process cleanup, secrets in environment, guard/sandbox boundary.
Reviewed `8f6e8f9..e261687`. The runner implements most requirements, but box-wide serialization and the exit-marker guarantee remain open.

## Findings / Changes

### Delivered
- **Serialization — partly; open:** locking works for callers sharing one lock path, but defaults can diverge (`internal/jobs/gaterun.go:63`).
- **HEAD assertion — met; closed:** checked under the lock before execution and again afterward (`internal/jobs/gaterun.go:144`, `internal/jobs/gaterun.go:157`).
- **Exit marker — partly; open:** emitted on normal runner returns, but not reliably as a separate line (`internal/jobs/gaterun.go:99`).
- **Process-group timeout cleanup — met; closed by inspection:** separate group, TERM/KILL escalation, cleanup before reaping (`internal/jobs/gaterun.go:277`, `internal/jobs/gaterun.go:292`).
- **TELEGRAM_* removal — met; closed:** filtered from the command environment (`internal/jobs/gaterun.go:72`, `internal/jobs/gaterun.go:275`).

### Design
The implementation follows the added architecture description. However, that description narrows “one gate per box” to callers using the same agent-jobs home; the issue requires a box-wide boundary.

### Code
**High — Serialization depends on caller configuration.**  
`internal/jobs/gaterun.go:63`: `GateLockPath()` derives the lock from `Home()`, which honors `AGENT_JOBS_HOME` (`internal/jobs/config.go:418`). Two runners with different state roots acquire different files and can run heavy gates concurrently, preserving the OOM risk. `--lock` also permits independent locking domains.

Verified by tracing the path construction and reproducing its distinct results in memory. Minimal fix: require one invariant machine lock, independent of state-root overrides. Proposed regression: launch two runners with different `AGENT_JOBS_HOME` values and verify their commands cannot overlap. Existing serialization tests explicitly supply the same lock and miss this case (`internal/jobs/gaterun_test.go:200`). The brief explicitly requires one gate per box.

**Medium — Unterminated output breaks the marker line.**  
`internal/jobs/gaterun.go:99`: the deferred write adds no separator before `exit=N`. A command ending with `printf hello` produces `helloexit=0\n`; a line-based waiter looking for `^exit=` never sees its marker.

Verified using the exact source format string: newline-terminated output passes; unterminated output fails. Minimal fix: ensure a newline precedes the marker when needed. Proposed regression: run a command producing unterminated stdout and assert the final line equals `exit=0`. The existing test only uses `echo` (`internal/jobs/gaterun_test.go:117`). The brief explicitly requires an exit marker.

### Blocking
- `internal/jobs/gaterun.go:63` — **open:** enforce serialization across different state roots and lock overrides.
- `internal/jobs/gaterun.go:99` — **open:** guarantee a standalone exit-marker line after unterminated command output.

### Non-blocking
none

## Evidence
- Inspected all seven changed files and the existing Git, home-resolution, process-wait and CLI refusal helpers.
- `git diff --check 8f6e8f9..HEAD`: passed.
- Read-only Python checks reproduced distinct default lock paths and `helloexit=0` marker concatenation; these were source-level checks, not runner execution.
- `git status --short`: clean.
