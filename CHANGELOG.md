# Changelog

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
