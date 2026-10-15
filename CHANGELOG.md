# Changelog

## [Unreleased] – fix-loop 1 (Codex review of stage 1)

### Fixed
- B1: job-done line is printable-only (control/format chars dropped, whitespace → space, title ≤ 120 runes); `sendTmux` sanitizes too.
- B2: guard parses Bash commands and blocks git push/tag and gh release/api/pr/issue writes in any spelling (global options, quotes, paths, env prefixes, wrappers, substitutions, `sh -c`, `eval`, `-R`, aliases); deny list extended with `gh pr new|edit|merge`, `gh issue new|edit`.
- B3: per-agent `flock` around tick and cancel; atomic claim with a launch token; a tick only finishes jobs it owns.
- B4: claim before spawn; the wrapper registers in `wrapper.json` before running claude and holds `wrapper.lock`; the next tick adopts an orphan wrapper; a claim whose wrapper never registered fails loudly after 1 minute.
- B5: stream.jsonl is read after exit_code is seen.
- B6: cleanup no longer touches the index; saves staged.patch and untracked.tar besides commits/uncommitted patches; restore tested.
- Cleanup verifies worktree ownership (worktree.json + porcelain listing + owner mark in the admin dir) before touching anything.
- B7–B9: pipefail state tracked (`set ±o pipefail`); commit message and identity checks parse real arguments (case-insensitive config keys, env identity); `pgrep -f` blocked only when its pattern matches its own loop.
- #26: undelivered job-done lines stay pending (no target or failing tmux) and are retried.

### Changed
- Model table: hard → `fable` (review/fix/general); research hard → opus.

## [0.1.0] – 2026-10-15 – Stage 1: core

### Added
- SQLite job queue (`jobs` table) with statuses queued/running/succeeded/failed/timeout/cancelled; only queued/running are mutable.
- Limits: defaults 80 turns / $10 / 45 min, ceilings 200 / $50 / 180 – over-ceiling requests rejected, never clamped.
- `agent-jobs daemon` (tick every minute) and `agent-jobs tick`; launchd template with KeepAlive.
- Launch exactly per reference via detached `agent-jobs run-wrapper` in its own process group; exit_code file; daemon restart does not kill or lose jobs.
- Per-job git worktree (detached at base) or empty work dir; `base_commit` file.
- settings.json: «nothing outside» deny rules, web tools only for research, PreToolUse guard hook (`agent-jobs guard`).
- prompt.md: preamble + owner rules (≤ 8 KB, required) + task + kind checklist + report contract.
- report.md extracted from stream.jsonl; exit 0 without a report = failed.
- One-line tmux notify `[jobs:job-done] …` (disabled unless configured).
- Cleanup 30 min after notify: diffstat.txt, commits.patch, uncommitted.patch, `refs/agent-jobs/jobs/<id>`, then removal of only that job's worktree.
- CLI: init, start, status, list, result, cancel.
- Local checks: `scripts/check.sh`, `.githooks/pre-push`.
