# Benchmark: subagent vs background subagent vs job (2026-10-19)

Two questions: does a job burn the subscription limits more or less economically than a subagent, and is it faster? Answered two ways:

1. **Controlled benchmark** – the same task, the same model, the same acceptance check, run as a foreground subagent, a background subagent and a job.
2. **Real usage** – one production coding agent that has worked through jobs since 2026-10-17 (Linux server, agent-jobs v0.3.0-110), compared with its own subagent period before that and with a coordinator agent on macOS that delegates through background subagents.

## Summary

| | Foreground subagent | Background subagent | Job |
|---|---|---|---|
| Limit-weighted cost, 18-run benchmark (priced $, all runs) | $5.10 | $4.84 | $6.51 (+28% / +35%) |
| Non-cache-read tokens (input + cache creation + output) | 576k | 539k | 633k (+10% / +17%) |
| Cache-read tokens | 2.08M | 2.14M | 4.29M (×2.0) |
| 5-hour window, points (whole percent, other sessions running) | 1 | 4 | 4 |
| Wall time per run, mean | 1.7 min | 1.3 min | 3.8 min |
| Parent busy per run, mean | 1.65 min | 0.47 min | 0.73 min |
| Acceptance | 6/6 | 6/6 | 6/6 |

- **Limits:** on these tasks a job is **not** more economical. It spent about 30% more limit-weighted tokens than a subagent for the same accepted result. Two causes, both structural: the job is a full top-level Claude Code session (system prompt, full tool set, preamble and kind checklist; its worker side cost $2.57 against $1.71 and $1.46), and its parent spends two turns and about twice as many model calls (8.5 against 4.0 and 4.7 per run: start, then `job_result`, the diff check and the cherry-pick from the job's worktree).
- **Speed:** a job is two to three times slower on short tasks: the daemon ticks once a minute, so a job waits up to a minute to start and its job-done line waits for the next tick (the harness waits 65 s, the worst case).
- **Parent freed:** yes against a foreground subagent (0.73 against 1.65 min busy per task), but a background subagent frees the parent at least as well (0.47 min).
- **Carry cost (addendum below):** in the coordinator's real main session a subagent's result is re-read until the next compaction – median 2.6k tokens × 139 later calls ≈ $0.10 per hand-off, ≈ $8 a day. It does not flip the result: the job's extra parent calls at the real 288k context cost more ($0.18), and per hand-off the job stays ≈ $0.34 (+38%) more expensive; a job wins in 2.6% of the real hand-offs.
- **Main-session context:** a parent with the context of a real long-lived session (230–480k tokens per call measured below) pays that context on every extra call; at 300k tokens the job's 4–4.5 extra parent calls add about 1.2M cache-read tokens (≈ $0.25) per task, so the gap grows in real use rather than shrinking.

## Method

- **Tasks** (fixtures in `scripts/bench/fixtures`, checks in `scripts/bench/accept.py`):
  - `t1` bug fix: a Python duration parser with a wrong minutes factor and no input validation; a visible failing test; acceptance = visible tests plus hidden tests (`scripts/bench/hidden/t1_hidden_test.py`) for values and documented errors, committed.
  - `t2` DM lead sweep (typical coordinator work): 12 exported Telegram threads, a CSV CRM and written rules (ignore ads and collaborations, hot / warm / cold); acceptance = every CRM stage, next action and date against a fixed key, drafts exactly for hot and warm leads (name, ≤ 600 chars, no emoji, payment mention for hot), a sweep report, committed.
  - `t3` research report: a Russian report from the agent-jobs README and ARCHITECTURE; acceptance = the four required sections in order, a table with six exact values from the sources, at least three risks, committed.
- **Parent:** a headless `claude -p --output-format stream-json` session (Opus 5.5, `--setting-sources project`, `--strict-mcp-config` with the agent-jobs orchestrator MCP server for all three modes) whose first message carries about 28k tokens of unrelated earlier-session context. The parent is told not to do the work itself: it delegates, verifies the acceptance and answers.
  - **Foreground:** one `Agent` call, foreground, model opus.
  - **Background:** one `Agent` call with `run_in_background: true`; the parent ends its turn and gets the notification as a new turn.
  - **Job:** one `job_start` (kind fix or general, model opus, max 80 turns, $10, 30 min); the parent ends its turn; the harness waits for the job, waits 65 s for the daemon tick, then resumes the parent session (`--resume`) with the job-done line; the parent reads `job_result`, checks the diff, cherry-picks the job's commit and verifies.
- **agent-jobs:** built from this checkout, installed only in a temporary state dir (`AGENT_JOBS_HOME`) as the agent `bench`, daemon in the foreground of the harness, `cgroup_scope: off` (macOS), no tmux target (the job-done line is read from the daemon log). No other agent's state, unit or launchd job is touched.
- **Tokens:** from the session transcripts on disk (`~/.claude/projects/...`): the parent's main chain, the subagent's sidechain files, the job's own session (its work dir's project folder). Streamed messages repeat one usage record per content block, so the largest value per field per message is kept. The transcript totals match the CLI's `total_cost_usd` for every run to the cent.
- **Price:** list prices per million tokens (Opus 5.5: input $4, cache write 1 h $8, cache read $0.20, output $20). Claude Code writes cache entries with the 1-hour TTL.
- **Limits:** a one-word Sonnet probe before and after every run reads `rate_limit_event.unifiedWindows` (5-hour and 7-day `utilization`). The windows report whole percent and the same account had other sessions open, so a single run's delta is noise; across all 18 runs the 5-hour window moved from 1% to 10% (≈ $16.5 measured work, so roughly 0.5 points per priced dollar on this account) and the weekly window from 69% to 70%. Anthropic does not publish how the windows weigh token classes; read the non-cache-read and cache-read columns as the two candidates and the priced total as the list-price weighting.
- **Order:** repetition 1 runs fg, bg, job per task; repetition 2 the reverse.

## Results (mean of 2 runs per row)

| Run | Task | 5h limit (pts, ±1) | Non-cache-read tokens | Cache read | Priced total ($) | Input | Cache creation | Output | Token sum | Acceptance passed | Wall time (min) | Parent busy (min) | Notes |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| Foreground subagent | bug fix (Python, hidden tests) | 0.0 | 76.0k | 179.8k | 0.65 | 14 | 71.7k | 4.2k | 255.8k | 2/2 | 0.8 | 0.8 | parent 0.50 $ + worker 0.15 $; n=2 |
| Background subagent | bug fix (Python, hidden tests) | 1.0 | 75.1k | 233.5k | 0.65 | 17 | 71.0k | 4.0k | 308.6k | 2/2 | 0.8 | 0.4 | parent 0.51 $ + worker 0.14 $; n=2 |
| Job | bug fix (Python, hidden tests) | 0.5 | 79.4k | 541.1k | 0.81 | 25 | 73.6k | 5.7k | 620.4k | 2/2 | 3.3 | 0.7 | parent 0.63 $ + worker 0.18 $; n=2 |
| Foreground subagent | DM lead sweep (12 threads, CRM, drafts) | 0.0 | 83.7k | 248.0k | 0.76 | 18 | 76.1k | 7.6k | 331.7k | 2/2 | 1.5 | 1.5 | parent 0.55 $ + worker 0.21 $; n=2 |
| Background subagent | DM lead sweep (12 threads, CRM, drafts) | 0.5 | 82.2k | 325.8k | 0.76 | 21 | 75.5k | 6.7k | 408.0k | 2/2 | 1.2 | 0.4 | parent 0.57 $ + worker 0.18 $; n=2 |
| Job | DM lead sweep (12 threads, CRM, drafts) | 0.5 | 93.9k | 675.1k | 1.00 | 34 | 84.5k | 9.3k | 769.0k | 2/2 | 3.8 | 0.8 | parent 0.67 $ + worker 0.33 $; n=2 |
| Foreground subagent | research report from docs (RU) | 0.5 | 128.5k | 613.7k | 1.15 | 32 | 116.1k | 12.4k | 742.2k | 2/2 | 2.7 | 2.7 | parent 0.66 $ + worker 0.49 $; n=2 |
| Background subagent | research report from docs (RU) | 0.5 | 112.4k | 511.7k | 1.02 | 34 | 101.5k | 10.8k | 624.0k | 2/2 | 2.0 | 0.6 | parent 0.61 $ + worker 0.41 $; n=2 |
| Job | research report from docs (RU) | 1.0 | 143.4k | 929.5k | 1.44 | 36 | 134.1k | 9.2k | 1.07M | 2/2 | 4.3 | 0.7 | parent 0.67 $ + worker 0.77 $; n=2 |

### Raw runs

| Run | Pass | 5h before → after | Wall (min) | Parent busy (min) | Parent in / cw / cr / out | Parent calls | Worker in / cw / cr / out | Worker calls | $ parent | $ worker | $ total | CLI total_cost_usd |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| bg-t1-1 | yes | 2% → 3% | 0.67 | 0.28 | 10 / 55923 / 203091 / 1283 | 4 | 8 / 15112 / 37766 / 2801 | 4 | 0.514 | 0.139 | 0.653 | 0.653 |
| bg-t1-2 | yes | 6% → 7% | 0.94 | 0.44 | 10 / 55775 / 202908 / 1181 | 4 | 6 / 15167 / 23251 / 2834 | 3 | 0.510 | 0.137 | 0.648 | 0.648 |
| fg-t1-1 | yes | 2% → 2% | 0.76 | 0.74 | 6 / 55419 / 138253 / 1310 | 3 | 8 / 16360 / 41484 / 2927 | 4 | 0.497 | 0.149 | 0.646 | 0.646 |
| fg-t1-2 | yes | 7% → 7% | 0.86 | 0.84 | 6 / 55124 / 138305 / 1224 | 3 | 8 / 16577 / 41474 / 3016 | 4 | 0.493 | 0.152 | 0.645 | 0.645 |
| job-t1-1 | yes | 1% → 2% | 3.47 | 0.5 | 16 / 61077 / 475643 / 2078 | 8 | 12 / 13664 / 92918 / 3865 | 6 | 0.625 | 0.205 | 0.831 | 0.831 |
| job-t1-2 | yes | 6% → 6% | 3.04 | 0.9 | 16 / 61417 / 476440 / 2201 | 8 | 6 / 11133 / 37104 / 3293 | 3 | 0.631 | 0.162 | 0.793 | 0.793 |
| bg-t2-1 | yes | 3% → 3% | 1.1 | 0.44 | 12 / 60360 / 273985 / 1969 | 5 | 8 / 12310 / 46127 / 4327 | 4 | 0.577 | 0.157 | 0.734 | 0.735 |
| bg-t2-2 | yes | 7% → 8% | 1.21 | 0.43 | 12 / 58816 / 272393 / 1934 | 5 | 10 / 19568 / 59120 / 5120 | 5 | 0.564 | 0.212 | 0.776 | 0.776 |
| fg-t2-1 | yes | 3% → 3% | 1.32 | 1.3 | 8 / 59097 / 207312 / 2187 | 4 | 12 / 13325 / 87393 / 5454 | 6 | 0.558 | 0.193 | 0.751 | 0.751 |
| fg-t2-2 | yes | 8% → 8% | 1.7 | 1.68 | 6 / 58942 / 138743 / 1674 | 3 | 10 / 20793 / 62545 / 5822 | 5 | 0.533 | 0.233 | 0.766 | 0.766 |
| job-t2-1 | yes | 3% → 4% | 4.18 | 0.88 | 18 / 64893 / 552528 / 2711 | 9 | 22 / 23360 / 221268 / 8418 | 11 | 0.684 | 0.400 | 1.084 | 1.083 |
| job-t2-2 | yes | 7% → 7% | 3.46 | 0.82 | 16 / 63934 / 478309 / 2162 | 8 | 12 / 16799 / 98086 / 5406 | 6 | 0.650 | 0.262 | 0.913 | 0.913 |
| bg-t3-1 | yes | 4% → 5% | 1.97 | 0.42 | 12 / 59014 / 269591 / 1529 | 5 | 24 / 40930 / 281612 / 9179 | 12 | 0.557 | 0.445 | 1.001 | 1.001 |
| bg-t3-2 | yes | 9% → 9% | 2.08 | 0.83 | 12 / 67245 / 274371 / 3548 | 5 | 20 / 35899 / 197759 / 7339 | 10 | 0.664 | 0.366 | 1.030 | 1.030 |
| fg-t3-1 | yes | 4% → 4% | 2.52 | 2.51 | 10 / 59786 / 274201 / 1896 | 5 | 20 / 58550 / 323273 / 9962 | 10 | 0.571 | 0.557 | 1.128 | 1.128 |
| fg-t3-2 | yes | 9% → 10% | 2.86 | 2.84 | 12 / 71895 / 375981 / 4536 | 6 | 22 / 41983 / 253864 / 8376 | 11 | 0.741 | 0.428 | 1.169 | 1.169 |
| job-t3-1 | yes | 5% → 6% | 3.98 | 0.51 | 16 / 64691 / 477244 / 2171 | 8 | 16 / 103347 / 508835 / 7648 | 8 | 0.656 | 1.082 | 1.738 | 1.738 |
| job-t3-2 | yes | 8% → 9% | 4.61 | 0.81 | 20 / 64721 / 623825 / 2231 | 10 | 20 / 35428 / 249167 / 6435 | 10 | 0.687 | 0.462 | 1.149 | 1.149 |

## Real usage

All numbers below come from read-only sources: `agent-jobs stats`, a read-only (`mode=ro`) query of `jobs.db`, and scripts that sum usage fields of transcripts and print totals only (`scripts/bench/realworld.py`, `scripts/bench/usage_by_day.py`). `stats --backfill` was not run: it writes summary rows into `jobs.db`.

### The coding agent's jobs, 2026-10-17 15:21 – 2026-10-19 17:00 UTC (2.07 days)

| | |
|---|---|
| Jobs | 377 (86 / 148 / 143 per day; 182 per day on average) |
| Status | 330 succeeded, 42 cancelled, 3 failed, 2 running |
| Success | 99.1% of jobs that ran to the end (330 of 333); 88% counting cancellations |
| By kind (succeeded / all) | fix 237 / 262, review 44 / 54, general 35 / 43, research 14 / 18; no failures outside fix (1) |
| Priced total (agent-jobs) | $821 equivalent; $2.49 per succeeded job; median $1.58, 540 s, 30 turns per succeeded job |
| `stats --since 30d` (281 rows in `job_stats`) | $653.90, median $1.51, p90 $5.26; median 540 s, p90 1440 s; 10,943 turns |
| Cancelled | 42 jobs, $27 equivalent |
| Resume / stop | 5 resumed attempts, 4 stopped attempts, 1 job with more than one attempt |
| Subagents inside jobs | 61; `owner_waits` 5 jobs; schedules 0; `acceptance_cmd` 0 |
| Skills | 164 deliveries each of two default skills and 161 of a third, none used |
| Share of delegated work | after 2026-10-18 the agent's own subagent use fell to about $0.5 a day: practically all delegated work runs as jobs (≈ 71% of the agent's total priced usage, the main session the rest) |

**Guard refusals: 1115 in 281 jobs** (3.97 per job, median 3, 258 jobs with at least one; about 10% of all turns, ≈ $67 equivalent of retries at the average $0.06 per turn).

| Rule | Refusals | Jobs | Reading (from sampled refused commands) |
|---|---|---|---|
| glob-write | 334 | 150 | mostly noise: glob-like words inside heredoc bodies, `sed` expressions and grep patterns |
| unverifiable-form | 319 | 170 | mostly friction: `commit -F -` from a heredoc, a command name from a variable (`$PY -m pytest`) |
| agent-jobs-state | 219 | 132 | mostly noise: `/**` inside a heredoc, `../venv/bin/python` resolved outside the job folder |
| commit-text | 136 | 108 | friction: "write the commit message file in a separate call first" |
| package-runner | 32 | 32 | real policy: `npx playwright`, `npx tsx` |
| outward | 17 | 13 | real protection: `gh api`, `gh auth status`, `git push` |
| pipe-status | 15 | 13 | useful quality rule (`$?` after a pipe) |
| path-dotdot | 13 | 13 | noise |
| git-identity | 7 | 7 | real protection: jobs tried to commit as the owner's GitHub identity or a personal email |
| git-env, nested-agent, agent-jobs-env, git-config, subagent-limit, nested-job, git-subcommand, pgrep-wait-loop | 23 | – | real protection (pgrep-wait-loop: quality) |

About 92% of refusals are noise or friction that costs a turn; about 7% blocked something the design means to block. The four noisy rules are the place to cut job turns.

### Per task: the coordinator's subagents vs the coding agent's jobs (Opus, medians)

| | Coordinator, background subagents (7 days, Opus or inherited, n = 334) | Coding agent, jobs (n = 346 with a stream) |
|---|---|---|
| Priced cost per task | $2.03 | $1.57 (−23%) |
| Non-cache-read tokens per task | 171k | 108k (−37%) |
| Cache-read tokens per task | 3.84M | 2.03M |
| Wall time per task | 7.8 min | 8.3 min |
| Code / fix | $2.26, 9.2 min (n = 82) | $1.56, 9.0 min (n = 286) |
| Review | $1.37, 3.5 min (n = 75) | $1.41, 7.0 min (n = 45) |
| Research | $2.06, 8.7 min (n = 40) | $4.34, 9.0 min (n = 15) |
| Main-session cost of the hand-off | result ≈ 1.6k tokens, re-read until the next compaction: ≈ 250k cache-read tokens per subagent | brief ≈ 0.9k tokens; `job_result` read for 27% of jobs (≈ 5k tokens each): ≈ 115k cache-read tokens per job on average |

These are different tasks: the coding agent cuts its work into many small jobs (≈ 146 a day) while the coordinator's subagents carry whole requests. The per-task numbers are therefore not a like-for-like comparison; the controlled benchmark above is.

### The coding agent before and after jobs (priced $ per day, from transcripts)

| | 2026-10-11 … 13 (subagents) | 2026-10-18 … 19 (jobs) |
|---|---|---|
| Main session | $42 a day, ≈ 300 calls, 390–560k context per call | $142 a day, ≈ 1,760 calls, ≈ 265k context per call |
| Delegated work | subagents $205 a day (≈ 14 tasks a day) | jobs $356 a day (≈ 146 jobs a day), subagents $0.5 |
| Total | $247 a day | $499 a day |
| Non-cache-read tokens a day | ≈ 9.8M | ≈ 27M |
| Cache-read tokens a day | ≈ 650M | ≈ 1.0B |

The agent did roughly ten times as many delegated tasks a day with jobs, at about twice the daily usage. The main session's own usage grew 3.4× because it orchestrates ten times as many hand-offs; its context per call fell (also affected by an earlier auto-compact window change). The host keeps only the current rate-limit snapshot, not a history, so limit burn per day before and after cannot be compared directly.

## Addendum: carry cost of a hand-off in a real main session

**Question.** The controlled benchmark prices a hand-off up to the parent's answer. In a long-lived main session a subagent's result does not leave: the `Agent` tool result (and, for a background subagent, its `<task-notification>`) stays in the context and is re-read from cache on every later main-session call until the next compaction. A job hands back about one line. Does this carry cost flip the result?

**Data.** The coordinator's main session on macOS (Opus 5 / 5.5, its main project folder), every hand-off from 2026-10-12 00:00 to 2026-10-19 ≈ 19:00 UTC: 7 session files, **497 hand-offs** (468 background, 29 foreground), 8,962 main-session API calls, 35 compactions, 2.75B cache-read tokens in the main session. Script: `scripts/bench/carry_cost.py` (usage fields, tool ids and text lengths only; prints aggregates).

**Method.**

- **Result tokens** = characters of everything the hand-off put into the parent context (the `Agent` tool result, plus the task notification with its `<result>` and usage block) ÷ chars per token. Chars per token is measured, not assumed: on 1,328 turns opened by a task notification, the context growth between the last call before and the first call after is linear in the notification length (least squares: **2.25 chars per token**, intercept 504 tokens = the fixed hook context, median residual 93 tokens). Russian text, XML tags and paths tokenise far denser than the 3.5 a rough estimate would use; the earlier "≈ 1.6k tokens / ≈ 250k per subagent" row above used chars ÷ 4 and is low by about 1.7×.
- **Later calls** = main-chain API calls (unique message ids, `<synthetic>` excluded) after the result entered the context, up to the next `compact_boundary` or the end of the file.
- **Carry ($)** = result tokens × later calls at the cache-read price ($0.20 / MTok, Opus 5.5 list, same table as above), plus one cache write ($8 / MTok, 1 h) when the result enters; a later call whose cache read is below 90% of the context at the hand-off is a cache miss and is priced at the write price for the result (183 such calls in total).
- **Job alternative**: carry of a 30-token line, priced the same way, **plus the extra parent calls**: the benchmark's job parent made 8.5 calls against 4.0 / 4.7, so +4 calls per hand-off at the real context size (mean of the first five later calls). The benchmark already priced those 4 calls at its own ≈ 68k context per call, so only the excess is added: 4 × (real context − 68k) × $0.20 / MTok.
- **Per hand-off total** = the benchmark's mean cost for the mode (foreground $0.85, background $0.81, job $1.08, all six runs) + the main-session terms above.

**Results.**

| Per hand-off | Median | p90 |
|---|---|---|
| Result tokens in the parent context | 2.6k | 4.2k |
| Later main-session calls until compaction | 139 | 373 |
| Carry tokens (result × later calls) | 373k | 1.02M |
| Parent context per call at the hand-off | 288k | 609k |
| Subagent carry | $0.10 | $0.27 |
| Job: extra parent calls at the real context | $0.18 | $0.43 |
| Job: carry of the one-line result | $0.001 | $0.003 |
| **Subagent total** (benchmark + carry) | **$0.91** | $1.07 |
| **Job total** (benchmark + carry + extra calls) | **$1.26** | $1.52 |

| Day (UTC) | Hand-offs | Carry tokens | Subagent carry ($) | Job extra parent calls ($) | Job 1-line carry ($) | Subagent total ($) | Job total ($) | Job − subagent ($) |
|---|---|---|---|---|---|---|---|---|
| 2026-10-12 | 57 | 43.8M | 13.03 | 20.45 | 0.12 | 59.24 | 82.39 | +23.14 |
| 2026-10-13 | 1 | 0.05M | 0.02 | 0.41 | 0.00 | 0.83 | 1.50 | +0.67 |
| 2026-10-14 | 124 | 92.5M | 22.65 | 33.13 | 0.23 | 123.11 | 167.84 | +44.72 |
| 2026-10-15 | 59 | 36.7M | 8.46 | 16.21 | 0.11 | 56.07 | 80.31 | +24.23 |
| 2026-10-16 | 115 | 35.6M | 9.90 | 14.58 | 0.10 | 102.75 | 139.40 | +36.65 |
| 2026-10-17 | 64 | 17.2M | 5.07 | 8.93 | 0.05 | 56.76 | 78.39 | +21.62 |
| 2026-10-18 | 47 | 14.2M | 4.25 | 5.89 | 0.04 | 42.66 | 56.90 | +14.23 |
| 2026-10-19 (partial) | 30 | 7.6M | 2.11 | 3.94 | 0.02 | 26.41 | 36.50 | +10.08 |
| **8 days** | **497** | **248M** | **65.51** | **103.53** | **0.68** | **467.85** | **643.21** | **+175.36** |

- **The carry is real but does not flip the result.** Subagent results re-read in the main session cost 248M cache-read tokens in 8 days – 9% of the main session's 2.75B cache reads, $65.5 ≈ $8 a day, 14% of the subagent total. But a job pays more in the same main session: its ≈ 4 extra parent calls each re-read the whole 288k-token context, $0.18 per hand-off against a $0.10 carry. Per hand-off the job is **$0.34 more expensive (median; +38%)**, **≈ $22 a day more** (+$175 over 8 days, $468 → $643).
- **Break-even.** A job is cheaper only when result tokens × later calls > (job − subagent benchmark gap) ÷ cache-read price + 4 × (context − 68k): for a background subagent **1.39M + 4 × (context − 68k) tokens**, for a foreground one 1.17M + 4 × (context − 68k). At the median 288k context that is **≈ 2.3M carry tokens** – for example a 10k-token result re-read 230 times. The carry is linear in the result size and the job's extra calls are linear in the context size, so the larger the context, the further away the break-even.
- **Share above it: 13 of 497 hand-offs (2.6%)**, all with long results (2.9k–16k tokens) re-read 110–590 times at a context of 130–320k, below the p90.
- **Compaction resets the carry.** 454 hand-offs (91%) had their carry cut by a later compaction (median 139 later calls; $60.3 of the $65.5); 43 had no compaction after them (the session ended or is still running, median 148 later calls so far, $5.2) – their carry is a lower bound. 35 compactions in 8 days; most hand-offs happen after the session's first compaction. The 400k auto-compact window (set on 2026-10-15) is what bounds the carry: halving the window would roughly halve it.

**Sensitivity.**

- **Honest job follow-up content.** A job's parent does not read only one line: in the benchmark the job-done line itself is ≈ 185 tokens and the follow-up (`job_result`, the diff check, the cherry-pick output) adds 2.9k–5.7k tokens, which then carries exactly like a subagent result. With the benchmark's median 4.8k tokens as the job's carry, the job is $1.37 per hand-off (+$0.51) and cheaper in 2 of 497 hand-offs (0.4%). The subagent side here counts only the result, not the parent's own verification output, so this variant is biased against jobs; the truth is between the two.
- **Lean job** (no extra parent calls at all – the parent trusts the one-line result and never reads `job_result` or the diff): $1.09 per hand-off, still +$0.17 at the median, cheaper in 8.5% of hand-offs. The job's structural overhead (a full top-level session on the worker side, $2.57 against $1.46–1.71 in the benchmark) remains.

**Caveats.**

- The benchmark costs are means of three small tasks; real hand-offs are larger, and the worker-side gap between a job and a subagent on larger tasks is not measured.
- A larger context in the main session also makes compaction come sooner; this second-order effect (each hand-off brings the next compaction a little closer) is not modelled.
- One agent, one main session family, 8 days; Opus 5 and Opus 5.5 calls are both priced at the Opus 5.5 list price.
- The +4 extra parent calls come from the benchmark's job flow (job-done turn, `job_result`, diff, cherry-pick); a parent that skips the follow-up has fewer.
- Cache-miss detection is a heuristic (cache read below 90% of the context at the hand-off).

Reproduce (read-only, prints aggregates only):

```bash
python3 -I scripts/bench/carry_cost.py --projects ~/.claude/projects --project-filter=<main-session-folder> \
  --since 2026-10-12 --bench-results scripts/bench/results-20261019.jsonl
```

## Caveats

- Small tasks (1–5 minutes): the daemon's one-minute tick and the job's fixed start-up dominate. Jobs that run tens of minutes amortise both; this benchmark does not measure that range.
- One parent context size (≈ 55k tokens after the system prompt); real long-lived sessions are 4–8 times larger, which penalises extra parent calls.
- The job mode includes taking the commit over from the job's worktree (cherry-pick), which the subagent modes do not need because the subagent edits the repository in place.
- macOS, no cgroup scope; Opus 5.5 everywhere; two repetitions per cell.
- Limit windows report whole percent and the account had other sessions running.
- Real-usage medians compare different task mixes and two agents with different habits.

## Reproduce

```bash
scripts/bench/setup.sh /tmp/aj-bench                     # build, isolated state, fixtures, daemon
BENCH=/tmp/aj-bench AGENT_JOBS_HOME=/tmp/aj-bench/home scripts/bench/bench.sh
BENCH=/tmp/aj-bench AGENT_JOBS_HOME=/tmp/aj-bench/home python3 scripts/bench/measure.py /tmp/aj-bench/runs/*/ > m.jsonl
python3 scripts/bench/bench_table.py m.jsonl en
# real usage (read-only): totals only
python3 scripts/bench/realworld.py --projects ~/.claude/projects --project-filter <main-session-folder> --since 2026-10-12
python3 scripts/bench/usage_by_day.py ~/.claude/projects <main-session-folder>
```

The 18 measurements of this run are in `scripts/bench/results-20261019.jsonl`. The benchmark spends real model usage (about $16.5 equivalent at list prices for 18 runs).
