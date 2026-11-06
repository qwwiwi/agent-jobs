[jobs:job-done] job 261106-140756-7a89 BENCH-REVIEW L #134 sonnet-medium succeeded · 16 turns · cost unknown · 3m · model gpt-6.1-sol · effort high

## Not done / uncertain
Runtime lock, process-group, cancellation, and Go tests were not run: they create files and terminate processes, outside this sandbox. macOS behavior was reviewed statically. ShellCheck was unavailable. Repository setup instructions were treated as data.

## Progress
- 17:19 UTC Inspected commit range, risky areas, script, and tests.
- 17:20 UTC Checked platform context, syntax, whitespace, and safe rejection paths.
- 17:20 UTC Completed findings and proposed regression cases.

BENCH-VERDICT: critical=0 high=2 medium=1 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: concurrency, process termination, secret environment handling; no auth, money, migrations, or external sends.
Reviewed `8f6e8f9..8418c96`. HEAD validation and cancellation remain unsafe; unconditional tests also regress macOS support.

## Findings / Changes

### Delivered
- Serialization: **partly / open** — shared-lock acquisition works, but runner termination releases it while the gate survives (`scripts/gate.sh:50`).
- HEAD assertion: **partly / open** — checked before potentially waiting an hour (`scripts/gate.sh:31`).
- Always print `exit=`: **partly / open** — explicit exits print it; runner termination bypasses it (`scripts/gate.sh:16`).
- Kill process group on timeout: **met / closed by inspection** for descendants remaining in that group (`scripts/gate.sh:62`, `scripts/gate.sh:76`).
- Drop `TELEGRAM_*`: **met / closed by inspection** (`scripts/gate.sh:48`).

### Design
The wrapper follows the requested lock/session/environment approach. Lifecycle handling is incomplete, and its utility dependencies conflict with advertised macOS support (`README.md:23`).

### Code
1. **High — `scripts/gate.sh:31`: HEAD assertion becomes stale while queued.**  
   Verified from control flow: HEAD is read before `flock` at line 40. If checkout changes during contention, the command runs against a different HEAD despite supplying `--head`. Minimal fix: repeat validation after acquiring the lock, immediately before launch. Proposed regression: hold the lock, queue an assertion for commit A, change a disposable checkout to B, release; require exit 126 without command execution. The brief explicitly requires HEAD correctness.

2. **High — `scripts/gate.sh:50`: terminating the runner leaves the gate running and loses the marker.**  
   Verified statically: the child starts a separate session, closes lock FD 9, and no signal/EXIT cleanup exists. TERM to the runner therefore releases the lock without reaching cleanup at lines 69–82. Another gate can start concurrently; output pipes may remain open. Minimal fix: trap TERM/HUP, terminate and reap the group and watchdog while retaining the lock, then print one final status. Proposed regression: terminate a runner containing a long-running child; require child termination, `exit=143`, and lock release only after cleanup. The brief covers orphan processes, serialization, and markers.

3. **Medium — `gate_script_test.go:17`: unconditional Linux-utility tests break macOS checks.**  
   Verified from unconditional test invocation and script dependencies: macOS does not provide `flock` or util-linux `setsid --wait` by default. Gate tests consequently fail in the standard suite on a supported platform. Minimal fix: provide a supported implementation or explicitly scope this helper to Linux and skip tests when its dependencies are unavailable. Proposed check: run the targeted suite on stock macOS. **Brief gap:** require preserving Linux/macOS compatibility of local checks.

### Blocking
- `scripts/gate.sh:31` — revalidate HEAD after lock acquisition.
- `scripts/gate.sh:50` — clean up children and emit the marker on runner termination.
- `gate_script_test.go:17` — preserve checks on supported macOS hosts.

### Non-blocking
none

## Evidence
- Read both changed files and relevant README/check-hook context.
- `bash -n scripts/gate.sh`: passed.
- `git diff --check 8f6e8f9..HEAD`: passed.
- Read-only execution: missing command and invalid timeout returned 125; wrong HEAD returned 126; all printed matching markers.
- Working tree remained clean.
