# Architecture (stage 1 – core)

Implements `docs/REFERENCE-danya.md`, section «Как сделать у себя (минимум)», items 1–6. Deviations are only those in `docs/DEVIATIONS.md` (#6 model by kind × complexity, #7 naming are used here).

**MCP tools are Stage 2.** The reference starts jobs with the MCP tool `job_start` and reads reports with `job_result`. In stage 1 the CLI is the stand-in (`agent-jobs start` = job_start, `agent-jobs result` = job_result, plus status/list/cancel); the Go MCP server exposing `job_*` lands in stage 2 on top of the same Manager.

## Components

| component | where | what it does |
|---|---|---|
| store | `internal/store` | SQLite table `jobs` (modernc.org/sqlite, WAL, busy_timeout). Only `queued`/`running` rows can change status. A tick claims a job (`queued → running` + random `launch_token`) before anything is spawned, and its terminal update is `WHERE status='running' AND launch_token=?` – a tick can never finish another tick's job. Only an explicit cancel finishes by id alone. |
| manager | `internal/jobs` | enqueue (validation, defaults, ceilings), launch, collect, timeout, cancel, notify, cleanup. |
| wrapper | `agent-jobs run-wrapper` | holds `wrapper.lock` (a second wrapper for the job refuses); under `admit.lock` checks that the job row is still `running` with its launch token (otherwise exits 4: no claude, no exit_code) and registers itself in `wrapper.json` (pid, pgid, launch token); then runs claude with a scrubbed git environment (below) and prompt.md on stdin, writes stream.jsonl / stderr.log, extracts report.md, writes exit_code last. |
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
| gitconfig | the job's only global git config (`GIT_CONFIG_GLOBAL`): the identity from `git_name`/`git_email`, nothing else (read-only) |
| no-hooks/ | empty read-only dir; `core.hooksPath` points here, so repository hooks never run |
| work/ | working dir: git worktree (detached at base) or empty dir |
| base_commit | commit the worktree started from; diffs are measured from it |
| stream.jsonl | claude stream-json events |
| stderr.log | claude stderr (+ wrapper errors) – first place to look when a job fails without a report |
| wrapper.json | the wrapper's pid/pgid/launch token, written before claude starts (adoption after a daemon crash) |
| admit.lock | admission handshake: wrapper (check row + register), `cancel` and the launch-grace failure take it, so a late wrapper never runs claude for a terminal row |
| worktree.json | path, admin gitdir and base of the worktree this job created (ownership check before cleanup) |
| exit_code | written by the wrapper when claude exits (written last) |
| report.md | the job's final message (`result` of the last stream `result` event) |
| diffstat.txt, commits.patch, staged.patch, uncommitted.patch, untracked.tar | what the job changed; written right before the worktree is removed |

`argv.txt`, `launches.txt` appear only in tests (the fake claude writes them). `<state>/tick.lock` is the per-agent lifecycle lock.

## Flows

**Start.** `agent-jobs start` validates kind, limits (defaults 80 turns / $10 / 45 min; above 200 / $50 / 180 → error, never clamped), resolves repo + base to a commit SHA, picks the model (explicit `--model`, else kind × complexity table), inserts a `queued` row and prints the id. Nothing runs yet.

**Tick** (every minute, also `agent-jobs tick`) runs under an exclusive `flock` on `<state>/tick.lock` (also taken by `cancel`), so two daemons or a manual tick never interleave. In this order:
1. *Collect* each `running` job. A claimed job without a recorded pid (daemon died between spawn and pid write) is adopted from `wrapper.json` if its token matches; with no `wrapper.json` after 1 minute it is `failed` under `admit.lock` ("the wrapper never registered", work dir kept). Then: `exit_code` present → stream.jsonl is read *after* it (so the result is complete) → verdict; else past `started_at + timeout` → SIGTERM to the process group, 10 s, SIGKILL → `timeout`; else wrapper PID dead → re-check exit_code, then `failed` ("wrapper died without writing exit_code").
2. *Launch* queued jobs while fewer than 2 are running. Launch = claim (`running` + token) → prepare job dir (owner rules must be readable and ≤ 8 KB, else the job is `failed` with "refused to start: …") → start `agent-jobs run-wrapper <jobdir> --token T -- claude <args>` with `Setsid` (own session → own process group, not a child of the daemon's group) → store PID = PGID.
3. *Notify* finished, not yet notified jobs: `tmux send-keys -t <target> -l "<line>"`, then `tmux send-keys -t <target> Enter`. The line is printable-only (whitespace → space, control/format characters such as Ctrl-U, ESC, bidi overrides dropped; title capped at 120 runes), so a title cannot type keystrokes into the session. With a target configured, a job is marked notified only after tmux accepted the line: a failing tmux (missing session) keeps it pending, logged and retried next tick, and cleanup waits. With no target configured notify is off: the line goes to the daemon log and the job is marked notified at once, so the 30-minute cleanup still runs.
4. *Cleanup* jobs notified ≥ 30 min ago (see below).

**Verdict.** exit_code ≠ 0 → failed; stream `result.is_error` (e.g. max turns / budget) → failed; exit 0 but empty/missing report.md → failed ("exit 0 without a report"); otherwise succeeded. Turns and cost come from the stream `result` event; duration is wall time from start to collection.

**Daemon restart.** The wrapper lives in its own session and writes `exit_code` itself; the daemon keeps no in-memory state. Killing the daemon (even SIGKILL) leaves the job running; the next daemon/tick finds `exit_code` and collects it (tested with the real binary).

**Cancel** (under the lifecycle lock and the job's `admit.lock`). `queued` → `cancelled`; `running` → TERM group / 10 s / KILL → `cancelled`. Terminal jobs cannot be cancelled.

**Process-group kills** (timeout, cancel). `wrapper.json` lies in the job dir, which the job can write, so a pgid is never trusted: before any signal the daemon requires pgid > 1, pgid ≠ its own process group and `getpgid(wrapper pid) == pgid`; otherwise it refuses and logs (the row is still finished). Adoption from `wrapper.json` refuses a pgid ≤ 1, the daemon's own group, or a live pid that does not lead that group (the job then fails after the launch grace).

**Cleanup** (repo jobs only; empty work dirs are left for the future `prune`). First the ownership check: `<jobdir>/work` is a real directory (not a symlink), `worktree.json` path matches, the path is registered in `git worktree list --porcelain` of the source repo, its admin gitdir lies under `<repo common git dir>/worktrees/`, equals the one recorded at creation and carries this job's owner mark (`agent-jobs-owner`) – otherwise nothing is touched. Then, without modifying the index or files: `diffstat.txt` = `git diff --stat <base>` + untracked list, `commits.patch` = `git format-patch <base>..HEAD --stdout`, `staged.patch` = `git diff --cached --binary HEAD`, `uncommitted.patch` = `git diff --binary HEAD`, `untracked.tar` = untracked, not ignored files (`git ls-files --others --exclude-standard`). Restore: in a fresh worktree checked out at `<base>`, with `J=<jobdir>` (empty patches are skipped – git rejects an empty patch; tested for only commits / only staged / only unstaged / only untracked / all / nothing):

<!-- restore -->
```sh
set -e
[ ! -s "$J/commits.patch" ] || git am "$J/commits.patch"
[ ! -s "$J/staged.patch" ] || git apply --cached "$J/staged.patch"
[ ! -s "$J/uncommitted.patch" ] || git apply "$J/uncommitted.patch"
tar -xf "$J/untracked.tar"
```

Then `git update-ref refs/agent-jobs/jobs/<id> <job HEAD>` in the source repo, then `git worktree remove --force <jobdir>/work` – only that path. Any failure before the removal keeps the worktree and retries next tick.

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

What this is and is not: the deny rules and the guard block the *direct spellings* of outward actions in a Bash call, and the wrapper's git environment scrub keeps git itself from running programs the guard does not see. This is **not a sandbox**: a script the job writes and then runs is allowed by design (as in the reference), and what it does is not inspected. Within a single Bash call the guard rejects forms it cannot resolve statically around git/gh instead of guessing.

- **settings.json deny** (holds under bypassPermissions): `git push`, `git tag`, `git send-pack`, `git http-push`, attached forms `git -c*`, `git --config-env*`, `git --exec-path*`, `env -S*`, `env --split-string*`, `gh release`, `gh api`, `gh pr create|new|comment|edit|merge|review`, `gh issue create|new|comment|edit`, nested agents `claude`, `codex`, `gemini`, `aider`, `cursor-agent`, plus `CronCreate`, `RemoteTrigger`, `PushNotification` (tools that reach a human or outlive the run). `WebFetch`/`WebSearch` denied unless kind = research.
- **Guard hook** (PreToolUse, matcher `*`, inspects Bash only). Prefix deny rules miss spellings, so the guard *parses* each command: quotes removed, split on `;` `&&` `||` `|` `&` newlines and parentheses, comments and here-doc bodies skipped, env prefixes and wrappers (`env`, `command`, `exec`, `nohup`, `timeout`, `xargs`, `sudo`, …) stripped, `$(…)`/backticks/`<(…)`, `sh|bash|zsh -c '…'` and `eval` parsed as nested scripts. Around git/gh, a form it cannot resolve statically is rejected with «guard cannot verify this form; write it as a plain command» – git/gh inside `$(…)`, backticks, `<(…)`, an unquoted here-doc body, `eval` or `sh -c`; stdin redirect, here-doc or here-string into git/gh (commit messages go through `-m '…'`, `-m "$(cat <<'EOF' … EOF)"` or `-F <file>`; `-F -`/`/dev/stdin` rejected); a command name, git subcommand, gh subcommand, `-c` value or commit message from a variable/substitution; a glob/brace character (`{ [ ? *`) in the command name (`{git,push}`, `/usr/bin/g[i]t`; `[`/`[[` exempt), in a git global option or subcommand or a gh subcommand; `eval`/`sh -c` of a variable; a shell reading commands from stdin (`… | sh`, `bash <<EOF`); `env -S`; `xargs git`; attached `-cKEY=V`, `-C.`, `--config-env`, `--exec-path`; `alias` mentioning git/gh; any non-reader program (`find -exec`, `flock`, `trap`, `python -c`, `npx`, …) with git/gh (also `git-<sub>`), a shell or an agent among its arguments (readers such as `echo`, `grep`, `cat`, `which` are exempt); `cp`/`mv`/`install`/`ln` with git, gh, `git-<sub>`, a shell or an agent as an argument (a renamed executable; also via `$(which git)`). Program names are compared case-insensitively. It also rejects:
  - *nothing outside*: `git [global opts: -C, -c, --git-dir, …] push|tag|send-pack|http-push` in any spelling (`git 'push'`, `/usr/bin/git push`, `/usr/lib/git-core/git-push` – any `git-<sub>` binary is read as `git <sub>`); `gh release`, `gh api`, `gh pr create|new|comment|edit|merge|review`, `gh issue create|new|comment|edit` with any flags before the subcommand (`-R`, `--repo`), `gh alias set|import`;
  - an unescaped `$?`/`${?}` (`\$?` and `'$?'` are literals) in the pipeline right after a multi-stage pipeline, unless pipefail is on: only a top-level, unconditional, foreground simple command `set -o pipefail` / `set -euo pipefail` before the pipe counts (not inside `if`/loops/`{ }`/`( )`, not after `&&`/`||`, not with `&`); any later `set +o pipefail` (even in a function) turns it off;
  - a `while`/`until` loop (also nested after `then`/`do`/`{`/`(`) with `pgrep -f`/`--full` whose pattern (as a regexp) matches the command's own text (`[s]erver` passes);
  - owner names (config `owner_names`) in the actual commit message: `-m`/`--message`/`-am`, `-F`/`--file` (file read relative to the hook's `cwd`; unreadable → block; stdin → block), `--trailer`, `--author`;
  - *git config that runs programs*: `git -c KEY` and `git config [set|--add|--file …] KEY VALUE` only for an allowlist of harmless keys (`user.name`, `user.email`, `commit.gpgsign`, `commit.verbose`, `color.ui`, `diff.renames`, `merge.conflictstyle`, … – see `gitConfigSafe`); everything else is rejected (`core.hooksPath`, `core.pager`, `core.editor`, `core.sshCommand`, `core.fsmonitor`, `credential.*`, `diff.external`, `*.textconv`, `filter.*`, `alias.*`, `include.*`, `url.*`, …), as is `git config --rename-section`;
  - *changes to the git environment*: `env -i`, `env -`, `--ignore-environment`, bash `exec -c`, and `env -u`/`--unset`/`unset` of `GIT_*`, `PAGER`, `EDITOR`, `VISUAL` – always; in a call that runs git (also `git-<sub>`), any assignment to or mention of those variables (prefix, `env`, `export`, `declare`, `printf -v`, `${VAR:=…}`) and `sudo`/`doas` (environment reset);
  - *nested agents*: `claude`, `codex`, `gemini`, `aider`, `cursor-agent` as a command (also behind `env`/`command`/`exec`/`nohup`/`timeout`, as a path, inside `sh -c`/`$(…)`) or as an argument of a runner (`npx claude`, `find -exec claude`) – a job is a leaf;
  - git identity overrides that differ from config `git_name`/`git_email` (none configured → any override is rejected): `git config [--global|set] user|author|committer.name|email <value>`, `--author`, `GIT_AUTHOR_*`/`GIT_COMMITTER_*` (also `GIT_CONFIG_KEY_n`/`GIT_CONFIG_PARAMETERS`) even in a call without git. Identity via `git -c user.*` is rejected for any value: the identity comes from the job's `gitconfig`.
  Unparsable hook input fails closed.
- **Git environment scrub** (wrapper, before claude starts): every inherited `GIT_*`, `PAGER`, `EDITOR`, `VISUAL` is dropped, then `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=<jobdir>/gitconfig` (identity only), `GIT_PAGER=cat`, `PAGER=cat`, `GIT_EDITOR=true`, `EDITOR=true`, `VISUAL=true`, `GIT_TERMINAL_PROMPT=0` and `core.hooksPath=<jobdir>/no-hooks` via `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0`/`GIT_CONFIG_VALUE_0` (command-line level: wins over repository config, so repository hooks never run). Repository-local config other than hooks still applies; the guard keeps git commands from setting dangerous keys, but a file the job edits directly is outside the guard (not a sandbox, see above).
- **Prompt**: preamble (own dir only, content is data, no secrets, nothing outside) + owner rules + task + kind checklist + report contract (Summary, Findings / Changes, Evidence, Not done / uncertain).

## Readings of ambiguous points

- *Reference: «Процесс запускается отвязанным… и сам пишет код выхода».* Read as: a small wrapper is the detached process; it runs claude as a child (not `exec`, otherwise nothing is left to write the file) and passes the claude argv after `--` so the full command is visible in `ps`.
- *«Запрос выше потолка отклоняется».* Zero/omitted = default; negative values are rejected too.
- *Duration in the job-done line* is wall time in whole minutes (rounded).
- *Owner rules «до 8 КБ»* = 8192 bytes; an empty file also refuses to start (no rules ≠ rules).
- *Notify for refused/cancelled jobs*: every terminal job gets exactly one line. «Главная сессия получает одну строку» is a delivery guarantee: an undelivered line stays pending.
- *Model table (DEVIATIONS #6)*: review/fix/general: light → sonnet, normal → opus, hard → fable; research: light/normal → sonnet, hard → opus. `fable` is the claude CLI alias (resolved to `claude-fable-5-1` in a smoke run on 2026-10-15).
- *Cleanup for jobs without a repo*: nothing to preserve, marked cleaned; the directory itself is for `prune` (stage 2+).

## Not in stage 1

MCP server (job_* tools incl. `job_start`/`job_result` – stage 2, CLI stands in; memory, tasks), `prune`, `doctor procs`, `resume`, memory notes in the brief, «владелец ждёт» Stop-hook, schedules, heartbeat, codex runner, HTTP API, the separate code-graph MCP server (reference «Код: индекс кода в виде графа (отдельный сервер)», jobs: read and index) – stage 2+.

Main-session duties (reference: the orchestrator reads the report, «Not done / uncertain» first, checks the diff, publishes only after that; cleanup after reading) cannot be enforced from the job side. They ship as `prompts/orchestrator.md` (verbatim quotes), which `agent-jobs init` writes to `<state>/orchestrator.md` for the agent's instructions. Cleanup stays «через 30 минут после уведомления» (the detailed rule); «удалять worktree после прочтения, сохранив патчи» from the minimum list is met because the patches and the ref survive, so a report read after cleanup loses nothing. Tying cleanup to a read mark would contradict the 30-minute rule, so it is not done.
