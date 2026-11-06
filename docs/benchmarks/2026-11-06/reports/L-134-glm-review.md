[jobs:job-done] job 261106-145418-a6d3 BENCH-REVIEW L #134 glm succeeded · 19 turns · cost unknown · 3m · model gpt-6.1-sol · effort high

## Not done / uncertain

Go tests, builds, vet, and `scripts/check.sh` were not run: the sandbox forbids writes, and `internal/jobs/jobs_test.go:361` creates temporary build artifacts. Runtime behavior remains unverified. No files changed; no subagents used.

## Progress

- 17:24 UTC Identified risky areas and inspected the diff.
- 17:25 UTC Traced locking, HEAD validation, cleanup, environment filtering, and tests.
- 17:26 UTC Ran read-only probes and whitespace checks; finalized findings.

BENCH-VERDICT: critical=0 high=2 medium=1 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: concurrency, process-group termination, HEAD validation, and secret filtering.
Reviewed `8f6e8f9..bca5338` read-only. Three fix conditions remain open; timeout group termination and Telegram environment filtering are implemented.

## Findings / Changes

### Delivered

- **Serialization: partly / open** — `internal/jobs/gaterunner.go:61` permits separate agent locks; `internal/jobs/acceptance.go:438` also permits unlocked execution.
- **HEAD assertion: partly / open** — `internal/jobs/gaterunner.go:129` checks ancestry, not the intended commit.
- **Exit marker: partly / open** — `internal/jobs/acceptance.go:293` and `:313` return without `exit=`.
- **Timeout process-group kill: met / closed by inspection** — `internal/jobs/acceptance.go:458` sends group TERM; `:475` sends group KILL after waiting.
- **Clean environment: met / closed by inspection** — `internal/jobs/repoprepare.go:257` removes `TELEGRAM_*`; acceptance and manifest checks use the allowlist at `internal/jobs/acceptance.go:525`.

### Design

Routing all three gate types through one runner follows the requested design. Fail-open locking, ancestry-only validation, and incomplete marker coverage weaken its explicit guarantees.

### Code

**High — Serialization remains conditional.** `internal/jobs/gaterunner.go:61`, `internal/jobs/acceptance.go:438`: if the shared lock cannot open, agents select different fallback files. If neither file opens, execution proceeds unlocked. Heavy gates can therefore overlap and recreate OOM pressure. Verified by tracing both branches and evaluating paths for two agents. **Fix:** require one shared lock and fail closed on acquisition errors. **Test:** make the common lock unavailable for two agents; neither may start using independent locks.

**High — Another task’s HEAD passes validation.** `internal/jobs/gaterunner.go:129`: any descendant of the base passes, including another task’s commit. Additionally, `internal/jobs/acceptance.go:307` checks HEAD before lock waiting; nothing revalidates it after acquisition. A gate can run on different code from the asserted commit. Verified through ancestry predicate probes and call tracing. **Fix:** record the intended SHA and compare actual HEAD immediately after acquiring the lock. **Tests:** reject a different descendant and a HEAD changed while queued.

**Medium — Refused gates can still end without a marker.** `internal/jobs/acceptance.go:293`, `:313`: authorization/configuration errors and start-record failures return without `gateExitMarker`. Marker-dependent waiters can still miss completion. Verified by tracing these returns; the existing opt-in revocation fixture exercises one such scenario (`internal/jobs/acceptance_loop1_test.go:269`). **Fix:** emit exactly one terminal marker for every attempted gate, including refusals. **Test:** extend that fixture and inject start-record failure.

All findings follow explicit brief requirements; no brief gap.

The added tests cover normal contention, bounded waiting, unrelated ancestry, marker formatting, environment filtering, and TERM-resistant children. They omit the failure cases above.

### Blocking

- `internal/jobs/gaterunner.go:61` — open: require a shared lock without independent or unlocked fallback.
- `internal/jobs/gaterunner.go:129` — open: validate the intended SHA after acquiring the lock.
- `internal/jobs/acceptance.go:293` — open: emit terminal markers on refusal paths.

### Non-blocking

none

## Evidence

- Inspected the seven-file diff and relevant callers, configuration, lifecycle code, and tests.
- `git diff --check 8f6e8f9..HEAD`: passed.
- In-memory Python probes confirmed divergent fallback paths and marker-free returns; local Git ancestry probes returned exit 0.
- `git status --short`: clean.
