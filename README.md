# agent-jobs

Фоновые джобы `claude -p` / `codex exec` для агентов Orgrimmar. Реализация 1:1 по `docs/REFERENCE-danya.md`, допустимые отличия – только `docs/DEVIATIONS.md`.

Статус: этап 1 (ядро) готов; `codex exec` runner (DEVIATIONS #5) – следующие этапы.

Background `claude -p` jobs for an agent's main session: the session hands work to one-shot processes, a daemon starts and cleans them up, the session gets one line when a job is done and reads the report on demand. Design: [`docs/REFERENCE-danya.md`](docs/REFERENCE-danya.md) (implemented 1:1), allowed deviations: [`docs/DEVIATIONS.md`](docs/DEVIATIONS.md), internals: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

Stage 1 (this release): queue + daemon + launch + settings/guard + report contract + tmux notify + cleanup.

## Build

```bash
go build -o ~/.local/bin/agent-jobs ./cmd/agent-jobs
git config core.hooksPath .githooks   # once per clone: pre-push runs scripts/check.sh
scripts/check.sh                       # gofmt, go vet, go test
```

Requires Go 1.27, git, `claude` (tested with 2.1.301), tmux for notify.

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
  "owner_names": ["Dashi"],
  "git_name": "",
  "git_email": "",
  "claude_bin": "/Users/me/.local/bin/claude",
  "tmux_bin": ""
}
```

- `notify_tmux_target` empty (default) = no delivery: job-done lines stay pending (logged each tick) and worktrees are not cleaned up until a target is set.
- `owner_names` must never appear in a job's commit; `git_name`/`git_email` is the only identity a job may set explicitly (empty = no override allowed).
- Timings are fixed by the reference and not configurable: the daemon ticks every minute, a worktree is removed 30 minutes after the job-done line was delivered.
- Env overrides: `AGENT_JOBS_HOME` (state root), `AGENT_JOBS_AGENT` (default `--agent`), `AGENT_JOBS_CLAUDE_BIN`, `AGENT_JOBS_TMUX_BIN`.

## CLI

```bash
agent-jobs start --agent silvana --title "review auth" --kind review \
  --repo ~/projects/x [--base main] [--complexity light|normal|hard | --model opus] \
  [--max-turns 80] [--max-budget-usd 10] [--timeout-minutes 45] \
  --brief-file brief.md            # or --brief "..." or stdin; prints the job id immediately
agent-jobs list   --agent silvana
agent-jobs status --agent silvana <id>
agent-jobs result --agent silvana <id>   # job-done line + report.md
agent-jobs cancel --agent silvana <id>
agent-jobs tick   --agent silvana        # one daemon pass
agent-jobs daemon --agent silvana        # tick every minute
```

Kinds: `review`, `fix`, `research`, `general`. Limits: defaults 80 turns / $10 / 45 min, ceilings 200 / $50 / 180 – a request above a ceiling is rejected, never clamped. At most 2 running jobs per agent.

When a job finishes the session gets one line:

```
[jobs:job-done] job 261015-124136-70d3 smoke OK succeeded · 1 turns · $0.03 · 0m
```

## launchd (daemon)

Not installed automatically. Fill `launchd/com.agent-jobs.daemon.plist.template`:

```bash
AGENT=silvana BIN="$HOME/.local/bin/agent-jobs"
sed -e "s|{{AGENT}}|$AGENT|g" -e "s|{{BIN}}|$BIN|g" -e "s|{{HOME}}|$HOME|g" \
  launchd/com.agent-jobs.daemon.plist.template > ~/Library/LaunchAgents/com.agent-jobs.$AGENT.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agent-jobs.$AGENT.plist
```

`KeepAlive` restarts the daemon; jobs run in their own sessions, so a daemon restart neither kills nor loses them.

## Where to look when a job fails

`~/.agent-jobs/<agent>/jobs/<id>/`: `stderr.log` first, then `stream.jsonl`, `exit_code`, `report.md`. After cleanup the work survives as `commits.patch`, `uncommitted.patch`, `diffstat.txt` and the ref `refs/agent-jobs/jobs/<id>` in the source repo.
