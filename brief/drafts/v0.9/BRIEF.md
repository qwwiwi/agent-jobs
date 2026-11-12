# Brief: agent-jobs v0.9, crew v2 and metrics

> DRAFT for the orchestrator. Installed as `brief/BRIEF.md` through the checklist tools (see
> [README.md](README.md)). The v1.0.0 plugin brief follows v0.9 and moves to `brief/queued/v1.0/`.

## Owner's words (verbatim, Telegram, 2026-11-08)

Everything between here and «Summary» is the owner's own text, voice messages transcribed, unedited.
Message ids are the Telegram ids of that chat.

- **75613** (voice): «слушай virgins jobs добавь версию версию следующий соответственно нужно проработать jobs crew и crew job внимательно изучи добавить идеи внимательно изучи все сообщения которые вот это наша переписка с данником нужно тебе изучить»
- **75619** (voice): «Да, утверждаю этот чек-лист, сделаю его после CRM. Сначала доделаю CRM и iNative, а потом делаю этот чек-лист.»
- **75628** (voice): «вот еще эту задачу тоже возьми, тоже учти ее, тоже Даник скинул» (about forwarded msg 75624, SLO + land)
- **75633**: «ну вот да, у вас как-то дороже всё выходит»
- **75634**: «странно»
- **75635**: «нужен анализ того что уходит джобам, возможно у них сразу раздувается контекст» (75633 to 75635 are forwarded remarks)
- **75640** (voice): «Мы же согласовывали, что у нас job is on Sonnet. Посмотри в GitHub. Мы же через Sonnet High перевели. Почему у нас это не работает?»
- **75646** (voice): «то есть мы забыли ... ok супер тоже добавь это в репозитория же jobs как чтобы доработал»
- **75651** (voice): «Также у нас есть и Клоу, также у нас Астра есть. Вся работа с фронтендом Астра в лоу режиме.»
- **75656**: «по дефолту sonnet 5.5 high, если нужно что-то другое то сессия сама решает»
- **75661** (voice): «Смотри, у Даника по дефолту стоит Sonnet 5.5 High. Если что-то сложное и не получается, то только потом Opus выступает. То есть Opus как запасной выступает.»
- **75666** (voice): «Я не говорю, что запасной. То есть сначала Sonnet стартует, а потом уже решаешь сам.»
- **75671** (voice): «можем еще ... есть смысл опус хай запускать если же не получается решить за два три круга»
- **75677** (voice): «Супер того, все добавь в репозиторию, распиши план, еще чек-лист еще раз распиши. И, соответственно, после CRM и инейтифа это будем делать. Потом сразу выкат, потом PR, мерж и выкат тебе. Версия 0.9.»

## Checklist the owner approved (msg 75617, with additions 75632, 75644, 75650, 75660, 75675)

The approved items, as the owner approved them (English, as they were put to the owner). The owner's
approval is the voice message 75619 above; the order and release path are 75677.

1. Crew = default kind for code: Sonnet 5.5 high main loop + explorer (Haiku, Read/Grep/Glob, paths with line numbers) + researcher (Haiku, read + web, every claim with a URL actually read) + advisor (Opus, read-only, answers exactly one of: plan / correction / stop; called before the plan, after 2 failures of one approach, at each design fork) + reviewer (Opus, read + git diff/log/show + repo tests; one pass over the whole diff before the report; every named defect must be fixed; «clean» only if the reviewer said so). Claude Code built-in subagents forbidden inside crew.
2. Table job_crew_calls: role, why called, turns, model, cost, outcome; shown in job_result and job_stats. Plus per job: context at start, peak context, total cache read, top heavy tool outputs (measured 11-08: fix job $6.70, start 28k, peak 229k, 115 turns, 34M cache-read tokens; cat/sed of whole files 175k of 265k result chars).
3. After a job the daemon itself runs the repo checks in the heavy slot; «green» only if they passed.
4. Test quality: reviewer rejects tests written only to be green and slow tests; metric = check.sh duration.
5. SLO dashboard + norms: land total and per stage, queue wait, verify gate, hooks, context per turn, owner reply time, deploy per project. Norms: owner reply 1 min; land/gate/deploy 10 min (goal 5); hooks 250 ms; context 150k, first call after compaction 80k; hook-injected text 1000 chars. A breach reaches the orchestrator context at most once per hour. Fix p95 tails first.
6. Admission by free cores and memory instead of fixed 2 slots; reaper of dead processes.
7. Kind design: fresh screenshots mandatory, pixel comparison with the reference.
8. Benchmark before making crew the default: crew-Sonnet vs Opus solo on the same 9 tasks, by accepted share and cost.
9. Guard noise: 2 437 refusals in 7 days (glob-write 779, unverifiable-form 621); find where it wastes turns.
10. Land as its own staged kind: wait, integration, gate, push, deploy, health check; each stage timed; per-project land flow as a plugin defined once, zero tokens.
11. Routing policy in the daemon (issue #216): default sonnet high (job_start without model starts on it); frontend/UI = gpt-6-astra effort low; review = gpt-6.1-sol; bulk mechanics = GLM via OpenRouter; any other model is the session's own choice with model_reason (escalation|money|auth|prod-db|trading|infra|large); escalation rule: Sonnet failed 2–3 rounds on a task -> next round opus high; job_stats spend by model and by reason, escalation rate.

Order (75677): after CRM 792 and iNative are finished, build v0.9; then PR, merge, release, and install
on the owner's host (Thrall). The v1.0 plugin follows v0.9.

## Summary (agent text, not the owner's words)

**Goal.** Release v0.9.0 «crew v2 + metrics»: crew becomes the default kind for code work (a Sonnet 5.5
high main loop with four fixed subagent roles), the daemon measures what a job costs and where its
context goes, and the daemon enforces the norms of an SLO dashboard. The routing policy of issue
[#216](https://github.com/qwwiwi/agent-jobs/issues/216) fixes which model a job starts on: Sonnet high by
default, a named model for frontend, review and bulk work, anything else only with a stated reason.
Crew only becomes the default after a benchmark against Opus solo (item 8).

**Why now.** On 2026-11-08 a fix job cost $6.70: 115 turns, context from 28k at the start to 229k at the
peak, 34M cache-read tokens, and whole-file `cat`/`sed` output was 175k of 265k result characters. The
owner had already agreed that jobs start on Sonnet high (msg 75640), yet the daemon did not do it: a
decision lived in chat and not in the product. A second operator's setup (below) shows both halves
working: cheap Sonnet loops with Opus only at the review and design forks, and metrics that catch tails.

**Reference setup («Danik»).** Another operator's setup, shared in a chat on 2026-11-08 (msgs 75580 to
75608 and the forwarded 75624). It is a reference, not a spec: we take the ideas, keep his numbers as
his, and build on our own v0.8 code.

- *Crew.* The default job kind; one `claude -p` session in its own worktree, main loop on Sonnet effort
  high, the only one that edits files, runs tests and commits. Four subagents passed with `--agents`:
  explorer (Haiku, Read/Grep/Glob, answers paths with line numbers), researcher (Haiku, read and web,
  every claim with a URL it read), advisor (Opus, read-only, answers plan, correction or stop; called
  before the plan, after two failures of one approach, at each design fork), reviewer (Opus, read plus
  git diff/log/show and go test/vet, one pass over the whole diff before the report, every named defect
  fixed, «clean» only if the reviewer says so). Claude Code's built-in subagents are forbidden in crew;
  only his PreToolUse guard applies, Stop and SessionStart hooks are not set; push is forbidden.
- *After the job.* The daemon runs the repository checks in the heavy slot, stores the report and writes
  every subagent call into a table `job_crew_calls` (role, why called, turns, model, cost, outcome).
- *His 7-day numbers.* 119 crew jobs, 1.69 USD and 65 main-loop turns on average. Reviewer: 109 calls,
  24.87 USD in total, 7 calls failed. Advisor: 59 calls, 7.65 USD. Explorer: 23 calls, 0.45 USD.
  Researcher: 2 calls. Almost all of the crew surcharge is the Opus review; Haiku search is nearly free.
  Other kinds: fix (Sonnet, no subagents, 0.57 USD on average), design (mandatory fresh screenshots,
  pixel comparison with the reference), review, research, sweep.
- *Test quality.* His crew jobs stopped writing tests that only turn green and slow the system down
  while coding; the metrics became sane once the reviewer rejected such tests.
- *Metrics and SLO.* The daemon writes timings to its database; `praxis slo --since 168h` shows p50, p95,
  last and the norm. Norms after his owner's rule of 27.10: tests, builds and lands up to 10 minutes
  (goal 5). 7-day figures (p50 / p95): owner reply, norm 1 min: 45 s / 5 min 22 s; verify gate: 4 min 51 s
  / 17 min; land total: 8 min 03 s / 27 min 35 s (land gate 4 min 50 s / 14 min, land deploy 4 min 02 s /
  16 min 20 s; slot wait, integration, push, health: seconds); queue wait 52 s / 32 min and job runtime
  13 min 19 s / 1 h 15 min (observe only); hooks, norm 250 ms: 22 ms / 605 ms (guard p95 950 ms; guard,
  stop and session-start slowest); context, norm 150k: 115k / 146k, first call after compaction, norm 80k:
  104k (always over); hook-injected text, norm 1000 characters: 172 / 1147. A breach goes into the
  orchestrator's context at most once an hour. His takeaway: medians are in the norm, the tails of
  land, gate and deploy are 2 to 3 times over (the main fix), context after compaction is second.
- *Land.* A separate job kind that splits the stages and times each (build, tests, deploy, health) to
  locate degradation. Per-project land flows are plugins: each project's flow is built once and spends
  zero tokens.
- *Scheduling.* The daemon computes how many resources are free before it starts a task (core load,
  memory) and finds dead processes now and then; his machine ran at 144% load without junk piling up.

**Not in this brief.** The v1.0.0 plugin (queued after v0.9), any CRM or iNative work (the owner's other
projects that come first), and the API-credit fallback (deferred by the owner on 2026-10-18).
