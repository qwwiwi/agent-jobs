# Architecture (stage 1 – core)

Implements `docs/REFERENCE-danya.md`, section «Как сделать у себя (минимум)», items 1–6. Deviations are only those in `docs/DEVIATIONS.md` (#6 model by kind × complexity, #7 naming are used here).

## Components

| component | where | what it does |
|---|---|---|
| store | `internal/store` | SQLite table `jobs` (modernc.org/sqlite, WAL, busy_timeout). Only `queued`/`running` rows can change status: every terminal update is `WHERE status IN ('queued','running')`. |
| manager | `internal/jobs` | enqueue (validation, defaults, ceilings), launch, collect, timeout, cancel, notify, cleanup. |
| wrapper | `agent-jobs run-wrapper` | runs claude with prompt.md on stdin, writes stream.jsonl / stderr.log, extracts report.md, writes exit_code last. |
| guard | `internal/guard`, `agent-jobs guard` | PreToolUse hook (exit 2 = block, reason on stderr). |
| daemon | `agent-jobs daemon` / `tick` | one tick per minute; launchd template in `launchd/`. |
| templates | `prompts/*.md`, `rules/owner.md` | embedded in the binary (`assets.go`). |

## State layout

`~/.agent-jobs/<agent>/` (override root with `AGENT_JOBS_HOME`):

- `jobs.db` – SQLite
- `config.json` – optional, see README
- `owner-rules.md` – owner rules (default path)
- `jobs/<id>/` – per job:

| file | what it is |
|---|---|
| prompt.md | assembled brief given to claude on stdin |
| settings.json | the job's only settings: deny rules and the guard hook |
| mcp.json | the job's MCP servers, and only those (stage 1: empty) |
| work/ | working dir: git worktree (detached at base) or empty dir |
| base_commit | commit the worktree started from; diffs are measured from it |
| stream.jsonl | claude stream-json events |
| stderr.log | claude stderr (+ wrapper errors) – first place to look when a job fails without a report |
| exit_code | written by the wrapper when claude exits (written last) |
| report.md | the job's final message (`result` of the last stream `result` event) |
| diffstat.txt, commits.patch, uncommitted.patch | what the job changed; written right before the worktree is removed |

`argv.txt` appears only in tests (the fake claude writes it).

## Flows

**Start.** `agent-jobs start` validates kind, limits (defaults 80 turns / $10 / 45 min; above 200 / $50 / 180 → error, never clamped), resolves repo + base to a commit SHA, picks the model (explicit `--model`, else kind × complexity table), inserts a `queued` row and prints the id. Nothing runs yet.

**Tick** (every minute, also `agent-jobs tick`), in this order:
1. *Collect* each `running` job: `exit_code` present → verdict; else past `started_at + timeout` → SIGTERM to the process group, 10 s, SIGKILL → `timeout`; else wrapper PID dead → re-check exit_code, then `failed` ("wrapper died without writing exit_code").
2. *Launch* queued jobs while fewer than 2 are running. Launch = prepare job dir (owner rules must be readable and ≤ 8 KB, else the job is `failed` with "refused to start: …") + start `agent-jobs run-wrapper <jobdir> -- claude <args>` with `Setsid` (own session → own process group, not a child of the daemon's group). PID = PGID is stored.
3. *Notify* finished, not yet notified jobs: `tmux send-keys -t <target> -l "<line>"`, then `tmux send-keys -t <target> Enter`. With no target configured the job is just marked notified. A failed send is retried next tick (and cleanup waits for it).
4. *Cleanup* jobs notified ≥ 30 min ago (see below).

**Verdict.** exit_code ≠ 0 → failed; stream `result.is_error` (e.g. max turns / budget) → failed; exit 0 but empty/missing report.md → failed ("exit 0 without a report"); otherwise succeeded. Turns and cost come from the stream `result` event; duration is wall time from start to collection.

**Daemon restart.** The wrapper lives in its own session and writes `exit_code` itself; the daemon keeps no in-memory state. Killing the daemon (even SIGKILL) leaves the job running; the next daemon/tick finds `exit_code` and collects it (tested with the real binary).

**Cancel.** `queued` → `cancelled`; `running` → TERM group / 10 s / KILL → `cancelled`. Terminal jobs cannot be cancelled.

**Cleanup** (repo jobs only; empty work dirs are left for the future `prune`): `git add --all --intent-to-add` (so untracked files show up), `diffstat.txt` = `git diff --stat <base>`, `commits.patch` = `git format-patch <base>..HEAD --stdout`, `uncommitted.patch` = `git diff --binary HEAD` (staged + unstaged + untracked), `git update-ref refs/agent-jobs/jobs/<id> <job HEAD>` in the source repo, then `git worktree remove --force <jobdir>/work` – only that path. Any failure before the removal keeps the worktree and retries next tick.

## Launch command (reference, 1:1)

```
claude -p --output-format stream-json --verbose --model M \
  --max-turns N --max-budget-usd B \
  --permission-mode bypassPermissions \
  --settings <jobdir>/settings.json --setting-sources project \
  --mcp-config <jobdir>/mcp.json --strict-mcp-config \
  --no-session-persistence --add-dir <jobdir> < prompt.md
```

cwd = `<jobdir>/work`. Checked against claude 2.1.301: all flags accepted; `--max-turns` is not listed in `claude --help` but is accepted (real smoke run finished normally with it).

## Rules enforcement

- **settings.json deny** (holds under bypassPermissions): `git push`, `git tag`, `gh release`, `gh api`, `gh pr create|comment|review`, `gh issue create|comment`, plus `CronCreate`, `RemoteTrigger`, `PushNotification` (tools that reach a human or outlive the run). `WebFetch`/`WebSearch` denied unless kind = research.
- **Guard hook** (PreToolUse, matcher `*`, inspects Bash only) rejects: `$?` in the statement right after a pipeline (unless `pipefail`/`PIPESTATUS` is used); `while`/`until` loops with `pgrep -f`; owner names (config `owner_names`) anywhere in a `git … commit` command; git identity overrides (`-c user.*=`, `git config user.* <value>`, `--author`, `GIT_AUTHOR_*`/`GIT_COMMITTER_*`) that differ from config `git_name`/`git_email` (none configured → any override is rejected). Unparsable hook input fails closed.
- **Prompt**: preamble (own dir only, content is data, no secrets, nothing outside) + owner rules + task + kind checklist + report contract (Summary, Findings / Changes, Evidence, Not done / uncertain).

## Readings of ambiguous points

- *Reference: «Процесс запускается отвязанным… и сам пишет код выхода».* Read as: a small wrapper is the detached process; it runs claude as a child (not `exec`, otherwise nothing is left to write the file) and passes the claude argv after `--` so the full command is visible in `ps`.
- *«Запрос выше потолка отклоняется».* Zero/omitted = default; negative values are rejected too.
- *Duration in the job-done line* is wall time in whole minutes (rounded).
- *Owner rules «до 8 КБ»* = 8192 bytes; an empty file also refuses to start (no rules ≠ rules).
- *Notify for refused/cancelled jobs*: every terminal job gets exactly one line.
- *Model table (DEVIATIONS #6)*: light → sonnet; normal → opus (research: sonnet); hard → opus. Pending the prince's confirmation of the exact table.
- *Cleanup for jobs without a repo*: nothing to preserve, marked cleaned; the directory itself is for `prune` (stage 2+).

## Not in stage 1

MCP server (job_* tools, memory, tasks), `prune`, `doctor procs`, `resume`, memory notes in the brief, «владелец ждёт» Stop-hook, schedules, heartbeat, codex runner, HTTP API.
