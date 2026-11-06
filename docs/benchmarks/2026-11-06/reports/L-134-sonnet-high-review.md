[jobs:job-done] job 261106-140555-7bbf BENCH-REVIEW L #134 sonnet-high succeeded · 14 turns · cost unknown · 3m · model gpt-6.1-sol · effort high

## Not done / uncertain

Integration tests were not run: they create repositories/files and terminate processes, outside this job’s constraints. Cancellation and cross-platform findings were verified statically; runtime behavior remains unmeasured. No subagents used.

## Progress

- 17:17 UTC Inspected revision, diff, and risky areas.
- 17:18 UTC Traced locking, signals, environment handling, and tests.
- 17:20 UTC Completed read-only probes and finalized findings.

BENCH-VERDICT: critical=0 high=3 medium=1 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Reviewed `8f6e8f9..61b4148` against issue #134; no files changed.
The runner implements the main mechanisms, but serialization, HEAD validation, and queued cancellation retain gaps.

## Findings / Changes

Risky areas reviewed first: concurrency, process-group termination, credential environment cleanup. No auth, money, migrations, external sends, or UI changes.

### Delivered

- **Serialization: partly / open** — `scripts/gate.sh:63` scopes the default lock to `TMPDIR`.
- **HEAD assertion: partly / open** — `scripts/gate.sh:98` checks twice, but inherited Git variables can select another repository.
- **Exit marker: partly / open** — `scripts/gate.sh:153` installs signal handlers after the potentially long lock wait.
- **Process-group timeout: met / closed by inspection** — `scripts/gate.sh:107` implements group TERM, grace, then KILL.
- **Telegram environment cleanup: met / closed by inspection** — `scripts/gate.sh:149` removes exported `TELEGRAM_*` variables before launch.

### Design

The wrapper follows the requested design, except its default lock is not box-wide and signal handling does not cover queuing.

### Code

**High — Job-local locks permit concurrent heavy gates.**  
`scripts/gate.sh:63`: different `TMPDIR` values produce different lock files. This repository explicitly assigns each job its own temporary directory (`internal/jobs/wrapper.go:1050,1091`). Gates from separate jobs therefore overlap, preserving the OOM condition. Verified through source inspection and a read-only expansion probe. Fix: use a stable shared default independent of `TMPDIR`; test two callers with different temporary directories.

**High — Queued cancellation bypasses signal handling.**  
`scripts/gate.sh:142,153`: the blocking `flock` runs before INT/TERM/HUP handlers are installed. Cancellation cannot use `on_signal` to produce the promised signal status; the surviving foreground waiter can retain output descriptors until its wait expires. Verified by control-flow inspection. Fix: install handlers before queuing and make lock acquisition interruptible through a tracked asynchronous waiter. Test TERM while queued, asserting prompt completion and `exit=143`.

**High — Inherited Git environment defeats repository selection.**  
`scripts/gate.sh:98`: an absolute inherited `GIT_DIR` overrides discovery through `git -C "$dir"`. The assertion can validate repository A while executing the gate in repository B. Verified by inspection and a probe confirming inherited `GIT_DIR` overrides normal discovery. Fix: clear Git repository-selection variables before assertion and execution; test two repositories with conflicting inherited settings.

**Medium — Linux-only tests regress macOS checks.**  
`gate_script_test.go:47,210,254`: tests invoke the runner without platform/tool guards. Successful-path tests fail where util-linux `flock` or `setsid` is unavailable, including standard macOS installations supported by `docs/GUIDE.md:669`. Verified by inspection. Fix: centralize prerequisite checks and skip unsupported environments. **Brief gap:** explicitly require preserving macOS local checks when introducing Linux-only runner tests.

### Blocking

- `scripts/gate.sh:63`: open — use a shared default lock.
- `scripts/gate.sh:98`: open — isolate HEAD checks from inherited Git selection.
- `scripts/gate.sh:142`: open — handle queued cancellation and its exit marker.
- `gate_script_test.go:47`: guard Linux-only integration tests.

### Non-blocking

none

## Evidence

- Inspected the complete four-file diff and related wrapper/check scripts.
- `bash -n scripts/gate.sh` and `git diff --check`: passed.
- Read-only rejection paths produced `exit=2` for invalid HEAD syntax and `exit=3` for HEAD mismatch.
- Lock expansion probe produced distinct paths for different `TMPDIR` values.
- Final `git status --short`: clean.
