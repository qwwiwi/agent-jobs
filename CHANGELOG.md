# Changelog

## [Unreleased] – fix-loop 2 (Codex + Fable review, conformance walk)

### Fixed
- Guard regression from N6: back to fail closed. A program not positively classified as data-only (echo/printf/grep/rg/cat/ls/head/tail/wc/file/stat/which/type/man/test/[, mkdir/touch/cp/mv/rm, find without -exec; brew install|info|search|list, apt-get install, go get|install, pip/npm install; a python script file with no -c/-m and option values skipped) is rejected when a git/gh or shell word is among its arguments. Closes awk system(), vim/ex -c '!…', docker exec, Rscript -e, deno eval, bun -e, expect -c, pwsh -c, gdb -ex shell, sed …/e, `python3 -W ignore -c`, rg --pre, man -P.
- Shell detection is case-insensitive (`BASH -c` on a case-insensitive FS) and covers csh/tcsh/ash/mksh/fish (`--command`) and others; `busybox` is a wrapper, `nix-shell --run|--command` is parsed like `sh -c`; `eval --` and `time -p` no longer hide the command.
- B1: gh is checked against a read-only allowlist (view/list/diff/checks/status …); `gh pr|issue close|reopen --comment`, `merge --body`, gist, repo edit, auth token and any future subcommand are rejected; deny list extended with `gh pr|issue close|reopen`, `gh gist create|new|edit`.
- B2: `git commit -C/-c`, `--reuse-message`, `--reedit-message`, `--fixup` (incl. amend:/reword:), `--squash` and abbreviated long options (`--mess=`) are rejected; `-t/--template` files are checked like `-F`.
- B3: preamble wording 1:1 with the reference – no «unless the brief names a path» exception.
- B4: reference timings fixed: daemon `--interval` removed (tick every minute), `cleanup_delay_minutes` removed (cleanup 30 min after notify); a config that still sets it is rejected.

### Added
- B6: `prompts/orchestrator.md` – main-session duties quoted verbatim from the reference (read the report, «Not done / uncertain» first, check the diff, publish only after; cleanup/patches); `agent-jobs init` writes it to `<state>/orchestrator.md`, README links it.

### Docs
- B5: the separate code-graph MCP server is listed in ARCHITECTURE «Not in stage 1» as stage 2+.

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
