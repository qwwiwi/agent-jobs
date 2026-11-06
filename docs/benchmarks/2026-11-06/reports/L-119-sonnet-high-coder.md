[jobs:job-done] job 261106-131245-863f BENCH L #119 sonnet-high succeeded · 51 turns · $1.25 · 42m · model sonnet · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- **Item 3 (edges derived from path overlap):** not implemented. The issue marks it optional. It needs a per-task paths field and a scout step, and the owner should decide how paths are supplied.
- **Dropping instead of refusing:** this follows the issue. An old client sending bare `depends_on` now gets silently independent tasks, though `depends_dropped` reports it. Edges already stored keep working without a reason.
- **Test coverage:** no test goes through the MCP tool for `depends_why` or `depends_dropped`; I added `depends_why` to the existing MCP and CLI test inputs only. The idle-slot line is not formatted for more than 20 ready tasks (it then prints a count), and that branch is untested.
- **Wording:** the line always says "N slots free" ("1 slots free"), to keep the format stable for parsing.
- **Unverified:** the six `internal/mcp` failures were not checked on the base commit.

## Progress
- 15:08 UTC Read the milestone code in `internal/jobs`, `internal/store` and `internal/mcp`, and the daemon launch path.
- 15:12 UTC Added the `depends_why` column and field in the store, and `justifyEdges` in `applyTasks`.
- 15:18 UTC Ran the existing suites: failures from tests that used bare `depends_on` edges.
- 15:22 UTC Added a `why()` helper and rewrote the existing test inputs.
- 15:30 UTC Wrote `idleslots.go` and hooked it into the daemon tick.
- 15:35 UTC Wrote `milestones_parallel_test.go` and fixed the T5 enqueue case (a blocked task cannot be enqueued).
- 15:40 UTC Updated the tool description, README (en/ru), GUIDE, SKILL and CHANGELOG.
- 15:45 UTC Ran the full suite, triaged the cgroup-refusal and load failures, and reran `internal/jobs` alone: green.
- 15:50 UTC Committed on branch `bench/119-sonnet-high` (`72ea2b9`) and ran `go vet`.

## Summary
Issue #119 asks that milestones stop serializing tasks that don't conflict, and that the daemon say when slots are idle. I did items 1, 2, 4 and 5, and left item 3 (deriving edges from touched paths), which the issue marks optional. Each `depends_on` edge now needs a one-line reason, or it is dropped and reported. The daemon sends one "N slots free, ready: …" line when slots are idle. `go vet` is clean and the `internal/jobs` and milestone-related `internal/mcp` tests pass; the full `./...` run had failures, covered below. Commit `72ea2b9` is on branch `bench/119-sonnet-high`, not pushed.

## Findings / Changes
- **Edge reasons (item 1).**
  - **Input and output:** the task input takes `depends_why`, a map of dependency key to reason (`internal/jobs/milestones.go`). The tree shows it per task, and the result lists dropped edges as `depends_dropped`, e.g. "T3 -> T1".
  - **Rules:** an edge with no reason is dropped. For a repeated edge, the reason stored for it is reused. A reason for a key not in `depends_on`, or `depends_why` without `depends_on`, is refused, as is a reason that is over 200 characters or has a line break. Cycle and unknown-key checks still apply to edges that have reasons.
  - **Storage:** reasons live in a new `depends_why` column on `milestone_tasks` (`internal/store/milestones.go`), added by an `ALTER TABLE` migration.
- **Slot filling (item 2).** `max_parallel` already defaulted to no cap, so the daemon's `max_running` is the only limit. I changed no behavior there. I rewrote the `milestone_create` description and the schema text to say "leave it unset" and to tell the planner to justify each edge.
- **Idle-slot nudge (item 4).**
  - **Behavior:** the new `internal/jobs/idleslots.go` builds the line, e.g. `3 slots free, ready: T8, T9 (milestone 1)`. It is called from the daemon tick, after `launchQueued` and `notifyPending`. It is sent once per distinct ready list, and without `notify_tmux_target` it is only logged.
  - **Not counted as ready:** a milestone at its own `max_parallel` adds no ready tasks, and blocked or running tasks aren't listed.
- **Guardrail (item 5).** Tasks that share a money path or files keep their explicit edge. Jobs already run in their own worktrees. The description, `docs/GUIDE.md` and the CHANGELOG say so.
- **Existing tests.** They relied on bare `depends_on` edges, so I gave them reasons through a small `why()` helper in several test files.
- **Docs.** `README.md`, `README.ru.md`, `docs/GUIDE.md`, `skills/agent-jobs/SKILL.md` and `CHANGELOG.md` are updated.

## Evidence
Commands run from the worktree; the test logs are in `../tmp/jobs.log` and `../tmp/full.log`.
- `go test ./internal/jobs -count=1 -timeout 30m` → `ok ... 1008s`, exit 0. The package ran slowly because the machine was loaded.
- `go test ./internal/jobs -run 'TestMilestoneEdgesNeedReasons|TestMilestoneIndependentTasksFillSlots|TestIdleSlotsLine' -count=1` → `ok`.
- `go test ./internal/mcp -run 'Milestone|JobViewShows' -count=1` → `ok`.
- `go vet ./...` → exit 0. `gofmt -l .` → no output.
- Full `go test ./... -count=1` (exit 1):
  - **Passed:** root, `cmd/agent-jobs`, `internal/httpapi`, `internal/store`, `internal/guard`, `internal/brain`, `internal/checklist`, `internal/skills` and the others.
  - **`internal/mcp`:** six failures. All print "refused: running in the cgroup of job scope … a job never starts … jobs" or an EOF on stdio. This job runs inside a job cgroup, so the binary refuses. I did not run them on the base commit, so "not caused by my change" is an inference from the message.
  - **`internal/jobs`:** a different set of failures in that run (a 10-minute timeout panic, and tests that shell out to the CLI). These were caused by load and by that same refusal. On a rerun alone the whole package passed.
  - **`TestAutoContinuedJobMergeCheckAndEstimate`:** failed once in a selective run (estimate 1.002 vs 1.202) and passed alone and in the full package run.

New tests, in `internal/jobs/milestones_parallel_test.go`:
- **`TestMilestoneEdgesNeedReasons`** covers the edge-reason rules:
  - a bare edge is dropped and reported;
  - a justified edge is kept and survives a reload;
  - `milestone_set` keeps the stored reason for a repeated edge and drops a new bare one;
  - clearing `depends_on` clears the reasons;
  - a stray, multi-line or over-long reason is refused;
  - cycle and unknown-key checks still fire.
- **`TestMilestoneIndependentTasksFillSlots`** is the acceptance case, with `max_running` at 5 and six tasks:
  - one task has a justified edge to T1 and cannot be started until T1 is done;
  - one task's edge has no reason, so it is dropped;
  - the other five run at once, and `max_parallel` stays unset.
- **`TestIdleSlotsLine`** covers the nudge line in three cases:
  - the line names the free slots and the ready tasks;
  - there is no line when no slot is free or the milestone is at its `max_parallel`;
  - the daemon types the line once, through the fake tmux.

I did not run these before the change. They use the new field, the new method and the new error path, so they did not compile on the base commit. The only behavioral before/after claim, that the bare edge on T6 used to keep it blocked, comes from reading the old `applyTasks`.
