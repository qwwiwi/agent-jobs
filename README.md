# agent-jobs

Фоновые джобы `claude -p` / `codex exec` для агентов Orgrimmar. Реализация 1:1 по `docs/REFERENCE-danya.md`, допустимые отличия – только `docs/DEVIATIONS.md`.

Статус: этап 1 (ядро) готов; `codex exec` runner (DEVIATIONS #5) – следующие этапы.

Background `claude -p` jobs for an agent's main session: the session hands work to one-shot processes, a daemon starts and cleans them up, the session gets one line when a job is done and reads the report on demand. Design: [`docs/REFERENCE-danya.md`](docs/REFERENCE-danya.md) (implemented 1:1), allowed deviations: [`docs/DEVIATIONS.md`](docs/DEVIATIONS.md), internals: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md), owner decisions: [`docs/DECISIONS.md`](docs/DECISIONS.md).

Stage 1 (this release): queue + daemon + launch + settings/guard + report contract + tmux notify + cleanup.

## Build

```bash
go build -o ~/.local/bin/agent-jobs ./cmd/agent-jobs
git config core.hooksPath .githooks   # once per clone: pre-push runs scripts/check.sh
scripts/check.sh                       # gofmt, go vet, go test
```

Requires Go 1.27, git, `claude` (tested with 2.1.301 on macOS, 2.1.302 on Linux), tmux for notify. Runs on macOS and Linux (same code: POSIX sessions, `flock`, process groups; no build tags).

**Dependency:** `modernc.org/sqlite` – pure-Go SQLite driver. Chosen over `mattn/go-sqlite3` because it needs no cgo/C toolchain, so the binary cross-builds and runs anywhere (Mac mini, Linux VPS) as a single static file. It is the only direct dependency.

## Setup

```bash
agent-jobs init --agent silvana        # creates ~/.agent-jobs/silvana/, owner-rules.md (edit it, ≤ 8 KB) and orchestrator.md
```

**Main session duties:** [`prompts/orchestrator.md`](prompts/orchestrator.md) (also written to `~/.agent-jobs/<agent>/orchestrator.md` by `init`) – add it to the agent's instructions. The main session reads every report («Not done / uncertain» first), checks the diff itself and only then publishes; jobs never publish.

Optional `~/.agent-jobs/<agent>/config.json`:

```json
{
  "owner_rules_path": "/Users/me/.agent-jobs/silvana/owner-rules.md",
  "notify_tmux_target": "channel-silvana",
  "tmux_socket": "",
  "owner_names": ["Dashi"],
  "git_name": "",
  "git_email": "",
  "claude_bin": "/Users/me/.local/bin/claude",
  "tmux_bin": ""
}
```

- `notify_tmux_target` empty (default) = notify off: the job-done line goes to the daemon log and the job counts as notified, so its worktree is still cleaned up 30 minutes later. With a target set, a failed tmux delivery keeps the line pending (retried each tick) and cleanup waits.
- `tmux_socket` (optional) is the tmux socket name of the session in `notify_tmux_target` (`tmux -L <name>`); empty = tmux's default socket. A name only, not a path.
- `owner_names` must never appear in a job's commit.
- `git_name`/`git_email` is the job's git identity: the wrapper writes it into `<jobdir>/gitconfig` and points `GIT_CONFIG_GLOBAL` at it (repo-local `user.*` still wins, as in git). A job cannot override it (`-c user.*`, `GIT_AUTHOR_*`/`GIT_COMMITTER_*`, `--author` are rejected; `git config user.*` only with the same value). Empty = no identity from agent-jobs.
- Timings are fixed by the reference and not configurable: the daemon ticks every minute, a worktree is removed 30 minutes after the job-done line was delivered.
- Env overrides: `AGENT_JOBS_HOME` (state root), `AGENT_JOBS_AGENT` (default `--agent`), `AGENT_JOBS_CLAUDE_BIN`, `AGENT_JOBS_TMUX_BIN`.

## CLI

```bash
agent-jobs start --agent silvana --title "review auth" --kind review \
  --repo ~/projects/x [--base main] (--complexity light|normal|hard | --model opus) \
  [--max-turns 80] [--max-budget-usd 10] [--timeout-minutes 45] \
  --brief-file brief.md            # or --brief "..." or stdin; prints the job id immediately
agent-jobs list   --agent silvana
agent-jobs status --agent silvana <id>
agent-jobs result --agent silvana <id>   # job-done line + report.md
agent-jobs cancel --agent silvana <id>
agent-jobs tick   --agent silvana        # one daemon pass
agent-jobs daemon --agent silvana        # tick every minute
agent-jobs mcp --agent silvana --role orchestrator   # MCP server (stdio): job_start/status/result/list/cancel
```

Give `--model` or `--complexity` (kind × complexity picks the model); with neither the job is rejected – no default, no classifier (owner 2026-10-17). `job_start` over MCP takes the same fields.

Kinds: `review`, `fix`, `research`, `general`. Limits: defaults 80 turns / $10 / 45 min, ceilings 200 / $50 / 180 – a request above a ceiling is rejected, never clamped. At most 2 running jobs per agent.

When a job finishes the session gets one line:

```
[jobs:job-done] job 261015-124136-70d3 smoke OK succeeded · 1 turns · $0.03 · 0m
```

## Daemon on macOS (launchd)

Not installed automatically. Fill `launchd/com.agent-jobs.daemon.plist.template`:

```bash
AGENT=silvana BIN="$HOME/.local/bin/agent-jobs"
sed -e "s|{{AGENT}}|$AGENT|g" -e "s|{{BIN}}|$BIN|g" -e "s|{{HOME}}|$HOME|g" \
  launchd/com.agent-jobs.daemon.plist.template > ~/Library/LaunchAgents/com.agent-jobs.$AGENT.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agent-jobs.$AGENT.plist
```

`KeepAlive` restarts the daemon; jobs run in their own sessions, so a daemon restart neither kills nor loses them.

## Daemon on Linux (systemd user unit)

Same binary, same state layout (`~/.agent-jobs/<agent>/`); the daemon runs as a systemd **user** service, one instance per agent: `agent-jobs@<agent>` from [`deploy/systemd/agent-jobs@.service`](deploy/systemd/agent-jobs@.service). Installed for the current user only, no sudo:

```bash
scripts/install-linux.sh --instance thrall              # build → ~/.local/bin/agent-jobs, unit → ~/.config/systemd/user/,
                                                        # daemon-reload, agent-jobs init, enable + (re)start agent-jobs@thrall
scripts/install-linux.sh --instance thrall --uninstall  # stop + disable; unit and binary removed when no other instance is enabled
systemctl --user status agent-jobs@thrall
journalctl --user -u agent-jobs@thrall -f               # daemon log (macOS: ~/.agent-jobs/<agent>/daemon.log)
```

- Idempotent: re-running with an unchanged binary and unit leaves the running daemon alone; a changed one is swapped atomically (rename, never written in place – a running job's guard hook points at this path) and the daemon restarted.
- `KillMode=process`: stop/restart signals only the daemon. Jobs live in their own sessions but in the unit's cgroup, so the default `control-group` would kill every running job on restart; with `process` they keep running and the next daemon adopts and collects them (the launchd counterpart is `AbandonProcessGroup`). `Restart=on-failure`. On such a restart the journal shows «Found left-over process … in control group while starting unit. Ignoring.» for the job processes – expected, that is the adoption (verified live on Thrall: a job started under one daemon was collected by the next).
- **Stop/uninstall caveat:** after `stop`, `disable --now` or `--uninstall`, running jobs keep running but **without timeout enforcement or collection** until a daemon for that agent starts again. `--uninstall` therefore keeps the binary while any job wrapper started from it is alive (it is every running job's guard hook; a missing hook would let the job run unguarded) and exits 1 with «N jobs still running — cancel or wait, then rerun --uninstall». The shared template and binary are also kept while another `agent-jobs@` instance is enabled or active.
- Env file `~/.config/agent-jobs/<agent>.env` (`$XDG_CONFIG_HOME`; created once with everything commented out, never overwritten, kept on uninstall; the installer parses only `KEY=VALUE` lines, never sources it; a non-default `XDG_CONFIG_HOME` draws a warning, since the user manager must see the same one): `AGENT_JOBS_HOME`, `AGENT_JOBS_CLAUDE_BIN`, `AGENT_JOBS_TMUX_BIN`. `PATH` in the unit is `~/.local/bin:/usr/local/bin:/usr/bin:/bin`.
- The daemon survives logout only with lingering on (`loginctl enable-linger <user>`); the script warns when it is off.
- Without a login session `systemctl --user` needs `XDG_RUNTIME_DIR=/run/user/$(id -u)`; the script sets it when missing.

## Where to look when a job fails

`~/.agent-jobs/<agent>/jobs/<id>/`: `stderr.log` first, then `stream.jsonl`, `exit_code`, `report.md`. After cleanup the work survives as `commits.patch`, `staged.patch`, `uncommitted.patch`, `untracked.tar`, `diffstat.txt` and the ref `refs/agent-jobs/jobs/<id>` in the source repo.
