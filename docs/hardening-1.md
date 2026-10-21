# Trusted result and resume state

Scope: batch 1 of the 2026-10-21 hardening triage, based on main `bcd5a14`.

- The wrapper parses runner stdout while teeing it to `stream.jsonl`. Claude result
  events and Codex terminal events/usage are recorded in the protected database,
  bound to the current launch token. Collection, stop, cancel and auto-continue
  use that record. The writable stream remains diagnostic; killed attempts keep
  the existing assistant-turn estimate and unknown cost. Older wrappers without
  a trusted record do not regain result trust through the stream file.
- `exec.Cmd.WaitDelay` bounds inherited stdout pipes after runner exit.
  `exec.ErrWaitDelay` preserves exit 0; other runner exit codes remain intact.
  The existing wrapper sweep still handles surviving processes.
- Session project directories (including physical paths, session subdirectories
  and fallback sessions) receive the same file-tool deny rules and guard
  protection as skills. Long project names protect the entire projects root.
  Review worktrees retain their read-only settings. This is guard protection,
  with the same limits as skill protection, rather than kernel isolation.
- A resume fallback marker is accepted only in resume mode and only for the
  daemon-generated fallback UUID stored before spawn. Claim clears previous
  trusted launch state; changing the session clears its fallback UUID.
- Unsupported escaped-process verification refuses resume, including Linux
  without `/proc`. Darwin retains an explicit exception: the kernel must return
  `ESRCH` for both recorded groups; live groups, permission errors, unknown errors
  and unverifiable groups refuse. This exception checks group absence and does
  not provide a system-wide scan for processes that escaped those groups.

Selective reuse from unreviewed WIP `d0e4cdb` (not merged), as identified in
`hardening-triage-20261021.md`: result tee/DB record in `wrapper.go:85,101,155`,
collection in `manager.go:942`, transcript protection in `resume.go:156`, fallback
validation/storage in `resume.go:797,970` and `store.go:733,750`, and batch-1
regressions in `hardening1_test.go:20,41,55,77,87,118`. The port preserves current
TMPDIR, read-only review, auto-continue, stop/cancel and process-group handling;
it also protects the Codex result path that WIP left file-based. WIP walk-budget
and heartbeat changes are outside this batch.

Regression coverage lives in `internal/jobs/hardening1_test.go` and
`internal/store/trusted_launch_test.go`, alongside the existing resume,
auto-continue, idle-close, acceptance and process-group suites. Darwin policy is
exercised through mocked kernel responses on Linux; native macOS execution is
not part of this change's validation.
