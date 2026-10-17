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
- Cgroup containment on Linux: the daemon launches each wrapper in its own transient systemd user scope (`systemd-run --user --scope --unit=agent-jobs-<agent>-<id>.scope`); control commands refuse when `/proc/self/cgroup` names a job scope; the wrapper sweeps the scope's cgroup (pidfd, sub-cgroups included) before `exit_code`; cancel, timeout and every collection stop the whole unit (`systemctl --user stop`), and a failed stop fails the job with «processes survived the job». Config `cgroup_scope` (`auto` default, `required`, `off`; env `AGENT_JOBS_CGROUP_SCOPE`); store column `scope_unit`. Without `systemd-run --user` the session containment stays, documented as weaker.
- Containment is visible: `status` (`contain:`), `list` (`CONTAIN` column), MCP `job_status` (`containment`) and the job-done line (`session only`, `stop pending`).
- A failed scope stop is recorded (`scope_pending` column) before the row turns terminal and retried every tick until the unit is gone; without the user bus the daemon sweeps the scope's cgroup itself; worktree cleanup waits meanwhile.
- `init --pin-scope` (run by `scripts/install-linux.sh`): sets `cgroup_scope: required` in `config.json` when unset and user scopes work on the host.
- Test-only build tag `agentjobs_testhooks` honours `AGENT_JOBS_TEST_OUTER_WRAPPER`, so the repository's own test suite runs inside a job; release builds have no such hook (tested).

### Changed
- In `auto`/`required`, a probe failure on a host where a scope worked before requeues the job every tick for up to 10 minutes instead of silently downgrading or refusing it.
- A timeout whose scope stop failed keeps the `timeout` status (with the survivors note) instead of turning into `failed`.
- The wrapper sweeps from its scope even when it was moved into a sub-cgroup of it; a `failed` unit counts as stopped only when its cgroup is empty.
- A job needs an explicit model or a complexity; with neither it is rejected (no default complexity, no classifier). Limits keep their defaults and ceilings.

### Security
- A job's environment no longer carries `DBUS_SESSION_BUS_ADDRESS` / `XDG_RUNTIME_DIR`; the guard and the deny rules reject `systemd-run`, `systemctl --user`, `busctl`, `dbus-send`, `gdbus`, `qdbus`, `dbus-run-session` (guard also behind wrappers, `xargs`, and as words inside other programs' arguments) and writes into the cgroup file system in any spelling (cleaned, relative, symlinked, through `/proc/<pid>/root`, control-file base names; `cd` into `/sys` or `/sys/fs`), the direct ways out of a job's scope.
- Owner decision 2026-10-17: the guard and the deny rules also reject `crontab`, `at`, `batch`, `tmux`, `screen`, `pm2`, `docker`, `docker-compose`, `podman`, `podman-compose`, `ssh`, `machinectl`, `loginctl`, `unshare`, `nsenter`, `qdbus-qt5`, `qdbus6` (daemons, schedulers and namespaces that run processes outside the job's scope).
- Guard: cgroup-file-system write targets are resolved through variables/backticks (fail closed), `~`, cd/pushd/popd chains in one call (CDPATH, `cd -`), and symlinks to `/`, `/sys`, `/sys/fs`; `cgroup.kill`, `cgroup.freeze` and the other cgroup core files count as control files.

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
