# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- MCP server `agent-jobs mcp --role orchestrator|job --agent A` (stdio, `github.com/modelcontextprotocol/go-sdk` v1.8.0) with a tool registry of two audiences; the job role is an allowlist, and the server itself refuses hidden tools and filters the tool list.
- Orchestrator tools `job_start`, `job_status`, `job_result`, `job_list`, `job_cancel` over the existing Manager; `job_result` reads `report.md` only as a regular file (no FIFO, symlink or directory), capped at 256 KB.
- Each job's `mcp.json` now names the job-role server instead of an empty stub.
- Store columns `complexity`, `runner`, `owner_waits`, `resumed_from`, `schedule_id`, `billing` (tolerant migrations).
- `tmux_socket` config: notify on a named tmux socket (`tmux -L`).
- Nested-job prevention: job marker `AGENT_JOBS_JOB`; the binary refuses control commands under the marker, under a wrapper ancestor, in a wrapper's session or in a running job's session; guard rules against `agent-jobs` as a command and against changing `AGENT_JOBS_*` (incl. `mapfile`/`readarray`, `printf -v`).
- Session sweep: after claude exits the wrapper ends every other process of the job's session before writing `exit_code`. Signals go through a pidfd opened before the membership check, so a reused pid is never signalled; without pidfd support nothing is signalled. If processes survive or the sweep fails, the wrapper writes a `sweep_incomplete` marker and the daemon fails the job with «processes survived the job», which the job-done line repeats.

- `agent-jobs prune [--dry-run] [--json]`: folders of jobs finished more than 48 hours ago keep the records (`report.md`, patches, `base_commit`, `diffstat.txt`) and small top-level text files (≤ 1 MiB); subdirectories, binaries, large files and symlinks (removed as links, never followed) go. Repo jobs whose cleanup has not run are skipped; a worktree kept for a human is left alone. Runs under the lifecycle lock.
- `agent-jobs doctor procs [--kill] [--json]`: finds processes carrying a finished job's marker, markerless leftovers in a finished job's session (leader gone, the session's first member too), both only when started while the job ran in the current boot; leaders (a marked process that daemonized into its own session, or a marked process under such a leader or in a live session that is not the job's; a running row whose wrapper died is matched by marker only, never by session); running rows with a dead wrapper pid; and suspects (preview servers, browsers, wait loops whose session leader is gone). Report only by default; `--kill` ends the job ghosts (never leaders or suspects) through a pidfd after re-checking start time and session.
- Orchestrator MCP tools `system_doctor` (disk, queue, processes, hooks), `system_logs` (daemon log or a job's `stderr.log`), `activity_report` (jobs over a window), `trash_move` / `trash_restore` (reversible deletion by rename into `<state>/trash/<id>/`, `renameat2(RENAME_NOREPLACE)` on a re-checked parent descriptor). None of them is open to jobs.
- Config `trash_roots` (no default: trash moves nothing until it is set). Always protected: the agent-jobs home (every agent), owner rules, the agent-jobs binary, `~/.claude`, an absolute `$CLAUDE_CONFIG_DIR`, `~/.claude.json`, any `.git`. On macOS trash uses `renameatx_np(RENAME_EXCL)`, an fstat device/inode re-check of the parent descriptor and case-folded `.git`/protected-path checks; without pidfds (macOS) the sweep and `doctor procs --kill` signal nothing.
- `GOOS=darwin` builds again (`pidfd_linux.go`/`pidfd_other.go`, `trash_linux.go`/`trash_darwin.go`).

### Changed
- A job needs an explicit model or a complexity; with neither it is rejected (no default complexity, no classifier). Limits keep their defaults and ceilings.

## [0.1.0] – 2026-10-17 – Stage 1 core, Linux port, guard hardening, public release

### Added
- SQLite job queue (`jobs` table) with statuses queued/running/succeeded/failed/timeout/cancelled; only queued/running are mutable.
- Limits: defaults 80 turns / $10 / 45 min, ceilings 200 / $50 / 180 – over-ceiling requests rejected, never clamped. At most 2 running jobs per agent.
- `agent-jobs daemon` (tick every minute) and `agent-jobs tick`; launchd template with KeepAlive.
- Launch exactly per the reference via a detached `agent-jobs run-wrapper` in its own process group; exit_code file; a daemon restart neither kills nor loses jobs.
- Per-job git worktree (detached at base) or empty work dir; `base_commit` file.
- settings.json: «nothing outside» deny rules, web tools only for research, PreToolUse guard hook (`agent-jobs guard`).
- prompt.md: preamble + owner rules (≤ 8 KB, required) + task + kind checklist + report contract.
- report.md extracted from stream.jsonl; exit 0 without a report = failed.
- One-line tmux notify `[jobs:job-done] …` (disabled unless configured); undelivered lines stay pending and are retried.
- Cleanup 30 min after notify: diffstat.txt, commits.patch, staged.patch, uncommitted.patch, untracked.tar, `refs/agent-jobs/jobs/<id>`, then removal of only that job's worktree; restore tested.
- `prompts/orchestrator.md` – main-session duties quoted verbatim from the reference (read the report, «Not done / uncertain» first, check the diff, publish only after; cleanup/patches); `agent-jobs init` writes it to `<state>/orchestrator.md`.
- CLI: init, start, status, list, result, cancel, `version` (also `--version`; the version is set at build time with `-ldflags "-X main.version=…"`, `dev` otherwise).
- Linux support: systemd user unit template `deploy/systemd/agent-jobs@.service` (instance = agent, `KillMode=process`, `Restart=on-failure`, env file `$XDG_CONFIG_HOME/agent-jobs/<agent>.env`) and `scripts/install-linux.sh` (idempotent build/install/enable/restart, `--uninstall`, no sudo, current user only; stamps the binary with `git describe --tags --always --dirty`).
- Tests for the unit template and the install script (fake systemctl, sandboxed HOME), including `systemd-analyze --user verify` of the template when available.
- Local checks: `scripts/check.sh`, `.githooks/pre-push`.
- Docs: English README with a Russian version (`README.ru.md`), English translation of the reference design (`docs/REFERENCE.md`, original in `docs/REFERENCE.ru.md`), public roadmap (`docs/PLAN.md`, `docs/PLAN.ru.md`), `docs/DECISIONS.md`, DEVIATIONS #11 (systemd user service on Linux), diagrams in `docs/images/`, MIT `LICENSE`.

### Changed
- Model table: review/fix/general light → sonnet, normal → opus, hard → `fable`; research light/normal → sonnet, hard → opus.
- Reference timings fixed: daemon `--interval` removed (tick every minute), `cleanup_delay_minutes` removed (cleanup 30 min after notify); a config that still sets it is rejected.
- Preamble wording 1:1 with the reference – no «unless the brief names a path» exception.

### Fixed
- Per-agent `flock` around tick and cancel; atomic claim with a launch token; a tick only finishes jobs it owns.
- Claim before spawn; the wrapper registers in `wrapper.json` before running claude and holds `wrapper.lock`; the next tick adopts an orphan wrapper; a claim whose wrapper never registered fails loudly after 1 minute.
- stream.jsonl is read after exit_code is seen.
- Cleanup no longer touches the index, and verifies worktree ownership (worktree.json + porcelain listing + owner mark in the admin dir) before touching anything.
- `--uninstall` keeps the binary while job wrappers started from it run (it is their guard hook; exit 1 with «N jobs still running»); «other instances» also counts active-but-not-enabled `agent-jobs@` units.
- Installer: env file parsed (`KEY=VALUE` only), never sourced; `umask 077` scoped to the env-file write; installer lock (`flock -n`); unit written via tmp + rename; warning on a non-default `XDG_CONFIG_HOME`.

### Security
- Job-done line is printable-only (control/format characters dropped, whitespace → space, title ≤ 120 runes), so a title cannot type keystrokes into the session.
- Guard parses Bash commands and blocks git push/tag and gh release/api/pr/issue writes in any spelling (global options, quotes, paths, env prefixes, wrappers, substitutions, `sh -c`, `eval`, `-R`, aliases).
- Guard fails closed: a program not positively classified as data-only is rejected when a git/gh or shell word is among its arguments (closes awk `system()`, `vim -c '!…'`, `docker exec`, `python3 -W ignore -c`, `rg --pre`, `man -P` and similar). Shell detection is case-insensitive and covers csh/tcsh/ash/mksh/fish; `busybox` is a wrapper, `nix-shell --run` is parsed like `sh -c`.
- gh is checked against a read-only allowlist; gh flags before the subcommand other than `-R`/`--repo`/`--hostname` are rejected; `gh auth status` is not allowed.
- `git commit -C/-c`, `--reuse-message`, `--reedit-message`, `--fixup`, `--squash` and abbreviated long options are rejected; `-t/--template` files are checked like `-F`; pipefail state is tracked; commit message and identity checks parse real arguments; `pgrep -f` is blocked only when its pattern matches its own loop.
- File tools (Write/Edit/MultiEdit/NotebookEdit) cannot write inside `.git` or the job's own git dirs (symlinks resolved, `..` rejected).
- Wrapper starts claude with a scrubbed git environment (job-only global config with the job identity, no system config, hooks/fsmonitor/ssh/credential helper/pager/editor overridden); the daemon's own git calls use the same overrides, and cleanup makes no patch when the repository config names a filter, diff command or textconv.

[Unreleased]: https://github.com/qwwiwi/agent-jobs/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/qwwiwi/agent-jobs/releases/tag/v0.1.0
