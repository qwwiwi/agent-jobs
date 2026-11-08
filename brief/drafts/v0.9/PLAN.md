# Plan: agent-jobs v0.9.0, crew v2 and metrics

> DRAFT for the orchestrator; installed as `brief/PLAN.md` next to `BRIEF.md` and `CHECKLIST.md` (see
> [README.md](README.md)). Brief: [BRIEF.md](BRIEF.md). Checklist rows R01 to R11: [CHECKLIST.md](CHECKLIST.md).
> The v1.0.0 plugin plan follows v0.9 and sits in `brief/queued/v1.0/`.

## Start condition and order

The owner starts this after CRM 792 and iNative are finished (msgs 75619, 75677). Nothing is built before
that. Order, as the owner gave it: build v0.9, then PR, review, merge, release v0.9.0, then install on the
owner's host (Thrall). The owner's decisions are in `docs/DECISIONS.md` (rows of 2026-11-08).

## What v0.8 already gives (reuse, do not rebuild)

| Existing (v0.8 or earlier) | Used by |
|---|---|
| Crew advisor (#169): `advisor_model`, `advisor_ask`, caps, the `crew` column in `internal/store/crew.go`, advisor proxy in `internal/jobs/advisor*.go` | R01 (advisor role, call recording), R02 (`job_crew_calls` rows start from advisor calls) |
| SLO record and show (#170): `slo record`, `slo show`, norms in `slo.targets`, one breach note on the next delivery, optional `slo.investigate` | R05 (extend names, per-hour cap, `slo_show`), R04 (`check-time`), R10 (land samples) |
| Measured orchestrator load (#170): per-category delivery and hook-injection byte records | R05 (hook-injected text and context names) |
| `job_stats` with token columns (`input_tokens`, `cache_read_tokens` ...), `effort`, `provider`, outcome, guard refusal counts and rules | R02, R09, R11 (spend by model and reason) |
| Box-wide gate lock, `agent-jobs gate run` (#208) | R03 (daemon verify in the heavy slot) |
| `max_running` with ceiling 8, `idleslots`, `doctor procs` (exact job ghosts only) | R06 |
| `scripts/ui-check.mjs`, its test, the job UI-check rule | R07 |
| Benchmark of 2026-11-06: nine tasks, `scripts/bench`, Opus rows already measured, `docs/benchmarks/2026-11-06-models.md` | R08 |
| Codex runner (`gpt-*`), `openrouter:` models, effort per runner | R11 (Astra, Sol, GLM routes) |
| Changelog fragments, PR title check, `docs/RELEASING.md` | release path |

Not reusable as is: `job_plugins` (internal/jobs/plugins.go) are Claude Code skill packs loaded into jobs;
the per-project land flow of R10 is a different thing and gets another name in code and docs («land flow»).

## Waves

Each wave is one or two PRs, each PR small enough for one review. A wave starts when the previous one is
merged, except where the table says the waves can overlap. The numbers in brackets are checklist rows.

### W1: routing policy, tables, context metrics

- **R11 routing policy (#216).** Default model: `job_start` with neither model nor complexity starts on
  sonnet 5.5 effort high. A routing table in config (`routing`) maps kind or `task_class` to model and
  effort: frontend/UI to `gpt-6-astra` low, review to `gpt-6.1-sol`, bulk mechanics to GLM via OpenRouter.
  `model_reason` enum, escalation counter over a task chain, `job_stats` by model and by reason, escalation
  rate; skill and orchestrator prompt state the default. First because it is the root of the cost complaint
  (msgs 75633, 75640) and every later wave runs on it.
- **R02 tables and context metrics.** `job_crew_calls` (fed by advisor calls now, by all four roles in W2);
  per-job context at start, peak, total cache read, top heavy tool outputs from the stream; `job_result` and
  `job_stats` show them; backfill. Needed before W2 so the crew's cost can be seen from its first job.
- Docs: GUIDE and REFERENCE rows, README default-model line, changelog fragments.

### W2: crew roles, verify, test quality

- **R01 crew kind.** `crew` kind with the four roles passed as subagent definitions, built-in subagents
  refused by the guard in crew, advisor answer shape (plan, correction, stop) enforced in the proxy, reviewer
  one-pass rule and the «clean only if the reviewer said so» check on the report. `crew_default` stays off.
- **R03 daemon verify.** After the report the daemon runs `verify_command` under the gate lock and writes
  `verify` into the result and the job-done line.
- **R04 test quality.** Reviewer rules, `check-time` sample from each verify run, per-repository median in
  `stats`, wording in the fix and general checklists.
- Depends on W1 (`job_crew_calls`, routing for the Opus roles). R03 and R04 can run in parallel with R01.

### W3: SLO dashboard, land, admission

- **R10 land kind and land flows.** Stage runner (wait, integration, gate, push, deploy, health), stage timing
  as SLO samples, the land flow file format fixed in a short design note at the start of the wave, zero-token
  run, orchestrator-only start.
- **R05 SLO dashboard.** Record the remaining names from real events (land stages come from R10, verify gate
  from R03, context names from R02), `slo_show` tool, `--tails`, default norms, one breach note per hour.
- **R06 admission and reaper.** Resource probes, `admission.mode`, held-back reasons, reaper in the tick.
- Depends on W2 for R03 timing and on W1 for context metrics; R10, R06 are independent of each other and of
  R05's table work and can run as parallel jobs (at most 2 running at once).

### W4: design kind, guard noise, benchmark, release

- **R07 design kind** (needs R11 for the Astra route and the `ui-check` tool).
- **R09 guard-noise pass.** Measure first, loosen only where a refusal is pure noise, dual review for every
  guard change. Runs after W1 to W3 so it measures the new job mix.
- **R08 benchmark** (needs R01 and R11): crew on sonnet 5.5 high against Opus solo on the nine tasks of
  2026-11-06, pass rule fixed before the run; its verdict decides the R01 (g) switch.
- **Release v0.9.0.** Flip `crew_default` only on a passed R08 verdict, assemble the changelog
  (`docs/RELEASING.md`), conformance row for v0.9 in `docs/CONFORMANCE.md`, release PR «Release v0.9.0 (#N)».

## Dependencies at a glance

| Row | Needs | Gates |
|---|---|---|
| R11 | nothing | R07, R08, everything that starts a job |
| R02 | R11 not required, but shipped in the same wave | R05 (context names), R08 (cost data) |
| R01 | R11, R02 | R08, `crew_default` |
| R03 | gate lock (exists) | R04, R05 (verify-gate name) |
| R04 | R03 | none |
| R10 | none (stage runner is new) | R05 (land names) |
| R05 | R02, R03, R10 | none |
| R06 | none | none |
| R07 | R11 | none |
| R09 | W1 to W3 merged (to measure the new mix) | none |
| R08 | R01, R11 | R01 (g), release flip |

## Release path

1. **Build.** One feature job per row or pair of rows, in a worktree on a `feature/` branch (jobs never push;
   result branches are create-only). Each job covers its rows with `covers`, and the job's tests fail
   before and pass after. The checklist has to be approved (`checklist_set approve`) before the first job,
   as for any large work.
2. **PR.** One PR per wave (title in English, `scripts/check-pr-title.sh`), a changelog fragment per change,
   `scripts/check.sh` through the gate lock.
3. **Review.** One review job per PR on a different model than the coder (`gpt-6.1-sol` by R11's routing);
   dual review for the guard rows (R01 f, R09, R10 e) as the guard rules require. At most 2 fix loops
   (the owner's cap); a non-critical finding outside the acceptance criteria becomes a hardening ticket.
4. **Merge.** Squash merge after `release_check` passes on the exact sha (the merge gate).
5. **Release.** The R08 verdict is in the repository; assemble the changelog, release PR, tag and GitHub
   release v0.9.0 by the orchestrator.
6. **Install on the owner's host.** Update the `agent-jobs@thrall` service on Thrall with a backup first
   (DECISIONS 2026-10-17: install on Thrall only until the owner says roll out), restart the daemon (jobs
   survive it: `KillMode=process`), run `agent-jobs doctor`, start one small crew job and read `slo show` and
   `job_stats` as the smoke test. The new default model is visible in the first `job_result`.

## Risks

- **A reversal of two earlier decisions.** R11 drops «model or complexity required» for callers that name
  neither, and keeps «no classifier» by routing on kind and an explicit `task_class`. Both are written in
  DECISIONS so the daemon, the skill and the owner agree (the «мы забыли» of msg 75646).
- **Crew costs more per job than a plain Sonnet fix.** The reference numbers: 1.69 USD per crew job against
  0.57 USD per fix job, almost all of the difference is Opus review. R08 decides on accepted share and cost
  together; a failed benchmark keeps crew opt-in.
- **Land pushes and deploys.** It is the only kind that touches a remote. Orchestrator-only start, deploy
  stage only when the land flow declares it, hard gates (money, prod DB, mass sends) stay behind the owner's
  explicit «да»; the existing «jobs never push» rule applies to every other kind.
- **Resource admission can starve or overload.** Probes are advisory, `max_running` stays the ceiling, the
  `slots` mode stays as the fallback and the default until the owner flips it; macOS probes may differ from
  Linux and fall back to `slots`.
- **Guard loosening in R09 can open a hole.** Measure first, change only noise, test the unsafe form still
  fails, dual review.
- **Reaper kills a live process.** It keeps the exact-ghost rule; pattern-only suspects are only reported.
- **SLO breach spam.** One breach note per hour in total; `slo.investigate` stays off.
- **Reference numbers are another operator's.** They show shape, not our targets; our norms are the owner's
  (BRIEF, item 5) and are measured on our own jobs before any tuning.
- **Schedule.** The start depends on CRM 792 and iNative; if they slip, v0.9 slips and v1.0 with it.

## Open items for the orchestrator

- GitHub issues per row (only #216 exists): `<<PLACEHOLDER: issue numbers for R01 to R10>>`.
- `escalate_after_rounds` default 3 is a proposal inside the owner's «два три круга»; confirm or set 2.
- Land flow file format and location: decided in the W3 design note, `<<PLACEHOLDER: land flow format>>`.
