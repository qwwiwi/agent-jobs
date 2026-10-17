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
- `agent-jobs` skill (`skills/agent-jobs/SKILL.md`): the MCP job tools, the mandatory model-or-complexity, the routing rule (do it yourself, a subagent, or a job), the subagent cap and how to read a result. `scripts/install-linux.sh` installs it per agent (`--skills-dir DIR`, default `${CLAUDE_CONFIG_DIR:-~/.claude}/skills`; `--no-skill`). It records the installed hash (`.installed-sha256`) and updates the skill only while the destination still matches it; a locally edited skill is left with a notice, replaced only with `--force-skill` (backup `SKILL.md.bak.<time>`). Ownership is recorded per instance (`~/.config/agent-jobs/<instance>.skill`, canonical path): uninstall goes by that record, keeps a skill another instance recorded, and keeps an edited one. A skills dir that is the repository's own `skills/` is refused.
- Jobs may use subagents: at most three per job, inside the job's own limits. The brief carries a «Subagents» section; the guard hook holds the cap with a monotonic counter in the job's store row (new column `subagents`) and logs each call in `<state>/subagents/<id>.log`; a shortened log refuses further subagents (DEVIATIONS #12).
- Job profile: `agent-jobs profile --agent A --from FILE... [--force] [--dry-run]` generates the owner-rules file from the sections of the agent's own rules marked `<!-- agent-jobs:profile -->` … `<!-- /agent-jobs:profile -->`; over 8 KB is refused and nothing is written.
- Project cards: `<state>/cards/<name>.md` (config `cards_dir`), front matter `repo:` / `remote:`, ≤ 4 KB each; cards matching a job's repository are attached to its brief (≤ 8 KB for the rendered section, headings included). Remotes match as identities: network `host/path` (host case ignored, path case kept, scheme/user/port/`.git` ignored) or a local absolute path (relative remotes resolved against the repository), so a relative local remote never matches a network one. `agent-jobs card new --repo PATH [--name N]` writes one from the template, `agent-jobs card list [--repo PATH]` shows them. Example card: `cards/example-agent-jobs.md`.
- The orchestrator MCP instructions and `prompts/orchestrator.md` state the routing rule and the subagent cap.
- Shared-brain tools (DEVIATIONS #2, #3): `memory_search`, `memory_write`, `memory_already_exists`, `task_create`, `task_list`, `task_update`, `task_get`, `send_message`, `inbox`, with the reference's names, backed by the team's shared brain (gbrain) over MCP streamable HTTP. The job role gets `memory_search` and `memory_write` only; a job's notes are tagged `agent-jobs-job:<id>`. New package `internal/brain`: `Brain` interface, HTTP `Client` (one short session per call, bounded by `timeout_seconds`), `Disabled`, and the test `Fake`.
- Config block `brain`: `recall_url`, `memory_url`, `tasks_url`, `swarm_url` (https, plain http only on loopback), `token_file` (mode 600) or `token_env` (one is required, never the token itself), `agent`, `timeout_seconds`, `already_exists_min_score`. Without the block the tools answer «brain not configured» and nothing else changes; a malformed block is a config error.
- Memory notes in the brief: when a job is prepared the daemon asks shared memory for up to six notes closest to its title and brief and adds a «Memory notes» section after the project cards (snippets quoted line by line, marked as data). An error or no answer within 5 s never blocks the job: the section then says memory was unreachable.
- Docs: `docs/WORKFLOW.md` (Russian: `docs/WORKFLOW.ru.md`), how the main session works as an orchestrator: routing between itself, subagents and jobs, the pipeline, review policy, reading results, known limits; optional reminder hook `examples/hooks/prefer-jobs-reminder.sh`.

### Changed
- A job needs an explicit model or a complexity; with neither it is rejected (no default complexity, no classifier). Limits keep their defaults and ceilings.
- A job with a repository refuses to start («refused to start: project card …») when a card in the cards directory is unreadable, unparsable or over 4 KB, or when its matching cards exceed 8 KB, like unreadable owner rules.

### Fixed
- `agent-jobs card new` works for an origin whose host has no dot (`git@myserver:org/repo.git`, `ssh://localhost/…`) or is an IPv6 address: such remotes are written as `ssh://host/path`.
- Subagent log and counter: the log line is appended (under the flock) before the counter is reserved, and taken back when the counter refuses; a failed write is reported as a write error and no longer leaves the counter ahead of the log (which refused every later subagent as «shortened»).

### Security
- Guard: a job's subagent must stay inside the job. Its input may carry only the string keys `description`, `prompt`, `subagent_type` and `model`, matched exactly and case-sensitively by a token walk (encoding/json would match `Isolation` to `isolation` and let a later duplicate win); duplicate keys and every other key (`isolation`, `run_in_background`, `team_name`, …) are refused. Without a subagent log every subagent is refused. The hook input must be a single JSON object.
- Guard: protected paths (`--protect`: the subagent logs and `jobs.db*`) cannot be written by file tools or literal Bash targets (redirections; arguments of any program that is not a plain reader), symlinks resolved.
- Guard: protected-path resolution (`internal/guard/pathres.go`) expands `~/` and `$HOME`, follows literal `cd`/`pushd` through the call, and refuses protected paths inside option values and inline code (`dd of=`, `python3 -c`, `perl -e`, `node -e`), ancestors as targets of `rm`/`mv`/`cp`/`ln`/`rsync`/`chmod`/`chown`/`tar`/`find -delete`, the directory holding one as a destination, `xargs <non-reader>` fed a protected path, and relative words that could reach one after an unresolvable `cd` (`$X`, `-`, `CDPATH`) or under `~user/`.
- Project cards are opened with `openat(O_NOFOLLOW)` relative to the cards directory and read bounded from the same descriptor (no symlink escape, no growth after the size check); front matter selectors and a new card's origin with control characters (CR/LF, …) are refused.

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
