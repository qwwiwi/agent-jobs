[jobs:job-done] job 261106-131244-f471 BENCH L #119 opus-high succeeded · 97 turns · $4.63 · 56m · model opus · effort high · job dir 217.8 MB · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- **Dropping instead of refusing:** as the issue says, an edge without a reason is dropped, not refused, and the answer names it in `dropped_edges`. This can let two tasks run in parallel if the planner forgets a reason. The `touches` guardrail only catches that when both tasks list their paths. Whether to refuse instead is an owner decision.
- **Stored edges without a reason:** edges saved before this change keep working. They are not dropped after the fact.
- **No automatic path guessing:** `touches` comes from the planner. Nothing scans briefs to guess paths (item 3, "quick scout", is not done).
- **Money paths:** there is no separate money flag. They stay serialized only if the planner lists them in `touches`.
- **Nudge memory:** the last sent `[jobs:slots]` line is kept in memory only, so a daemon restart can send the same line once more. The daemon still never starts jobs itself; the orchestrator calls `job_start` on the nudge. Nudges cannot be turned off by config, and ready tasks in a failed state are not listed.
- **CLI output:** the `agent-jobs milestone <id>` table still shows dependency keys only, not reasons. The JSON output has them.
- **Commit message:** the brief asks for English and the owner rules ask for Russian. The subject line is Russian with an English body. No `Requirements:` trailer, since the brief covers none.
- **Guard refusals:** the job guard refused a few shell forms (a Python script fed through the shell that mentioned skills, a heredoc into git, a combined archive extraction). I used the Edit tool and split commands instead; nothing was bypassed.
- **Leftovers:** the base export is still in the job dir (`base/`, `base.tar`), outside the repo. Other jobs' `jobs.test` processes were running on the host; I did not touch them.
- No subagents were used. The UI browser check does not apply: no UI files changed.

## Progress
- 15:09 UTC Read the milestone, launch-loop, store, notify and heartbeat code
- --:-- Added the `depends_why`/`touches` columns and the task fields
- --:-- Wrote `milestone_edges.go` (reasons, path checks, derived edges) and wired it into create/set
- --:-- Wrote `milestone_slots.go` (the `[jobs:slots]` line) and called it from the daemon tick
- --:-- Gave existing tests reasons; the first full run hit the 10-minute default timeout in internal/jobs
- --:-- Added the new jobs tests and the store round-trip test; all pass
- --:-- Updated MCP descriptions, server instructions, README(.ru), GUIDE, CHANGELOG, HTTP API caps
- --:-- Self-review fixes: no writes into shared slices/maps; unknown/self keys refused even without a reason
- --:-- Reproduced the bug on the exported base (FAIL) and showed the new tests do not compile there
- --:-- Full suite with a 40-minute timeout: exit 0; vet exit 0
- 16:04 UTC Committed 0db61a9 on bench/119-opus-high (local)

## Summary
Issue #119 asks that milestones keep only real dependencies, fill every job slot, and say when slots sit idle. I implemented all of it in commit `0db61a9` on branch `bench/119-opus-high` (local only, not pushed). Every `depends_on` edge now needs a one-line reason in `depends_why`, and an edge without one is dropped and listed in `dropped_edges`. Tasks can list `touches` (paths they will edit); tasks whose paths overlap get a derived edge, so they never run in parallel. The daemon types a `[jobs:slots]` line into the session when ready tasks wait and slots are free. The docs no longer show `max_parallel: 2`; unset means every `max_running` slot. The full test suite and `go vet ./...` both exit 0.

## Findings / Changes
- **Cause:** with no cap set, a milestone already used every `max_running` slot. Two things held tasks back: edges with no stated reason, and the README/README.ru example with `"max_parallel": 2`, which planners copy. On the base, a task with an edge but no reason stays `blocked` (reproduced below).
- **Edge reasons** (`internal/jobs/milestone_edges.go:29` `edgeReasons`, `:53` `setDeps`):
  - With `depends_on` given, an edge keeps a reason given now or its stored one. An edge with neither is dropped and named in `MilestoneView.DroppedEdges` (`"T2->T1"`).
  - `depends_why` alone edits the reasons of stored edges.
  - Refused: a reason for a key not in `depends_on`, a reason starting with `auto: `, a reason on more than one line or over 200 characters.
  - A dropped edge is still checked: an unknown key or the task itself is an error, not a silent drop (`internal/jobs/milestones.go:282`).
- **Touches and the guardrail** (`milestone_edges.go:107` `normTouches`, `:173` `deriveEdges`):
  - Paths are cleaned and must be repo-relative: no absolute paths, no `..`, no commas. A glob is cut to its directory (`internal/order/*.go` → `internal/order`).
  - Two live tasks that share a path and are not already ordered by a dependency chain get an edge: the later task waits, reason `auto: shared paths …`.
  - Derived edges are recomputed on every create/set (`milestones.go:384`), so they go away when the paths stop overlapping. Tasks changed only by this are also written back.
  - Money paths are covered by listing them in `touches`.
- **Idle-slot nudge** (`internal/jobs/milestone_slots.go:33` `SlotsLine`, `:67` `readyForSlots`, `:111` `nudgeIdleSlots`; called from the daemon tick at `manager.go:681`):
  - Free slots = `max_running` − running − queued jobs. The line looks like `[jobs:slots] 3 slots free, ready: milestone 4 T8, T9; milestone 7 T2` (at most 20 keys, then `(+N more)`).
  - A milestone at its `max_parallel` offers no tasks; one below it offers at most the slots it has left.
  - The line is sent again only when it changes. A failed delivery is logged and retried next tick, and never fails the tick.
- **Storage** (`internal/store/milestones.go:117`): new columns `milestone_tasks.depends_why` (JSON) and `touches`, added by migration. Older edges keep working with no reason.
- **Views and API:** each task in the tree shows `depends_why` and `touches`. The HTTP API caps both (`internal/httpapi/milestones.go`).
- **Docs:** MCP tool and field descriptions, server instructions (`internal/mcp/server.go`), README.md, README.ru.md, docs/GUIDE.md, CHANGELOG.md.
- **Existing tests:** about 25 places that create edges without a reason now pass one (new helper `why()` in `internal/jobs/milestones_test.go`).

## Evidence
- **Reproduced on the base:** I exported commit 8f6e8f9 into the job dir (`base/`) and added a test that uses only the base API, `TestRepro119EdgeWithoutReasonBlocks`. Result: `--- FAIL … repro119_test.go:17: T2 is blocked: an edge without a reason still holds it back`, exit 1. The same check passes in the new `TestMilestoneEdgeNeedsReason`.
- **New tests on the base:** `go vet ./internal/jobs/ ./internal/store/` fails to compile them (`unknown field DependsWhy in struct literal`, `v.DroppedEdges undefined`, `tasks[1].DependsWhy undefined`), exit 1.
- **New tests after the fix** (all PASS, `go test -count=1 -v -run 'TestMilestone|TestSlotsLine|TestIdleSlotNudge' ./internal/jobs/`, exit 0):
  - `TestMilestoneEdgeNeedsReason`: dropping, keeping and editing reasons, plus 8 refusal cases.
  - `TestMilestoneTouchesDeriveEdges`: path cleaning, derived edges, recomputing them after an edit, a dropped task.
  - `TestMilestoneIndependentTasksFillAllSlots`: 6 independent tasks with `max_running` 5 → 5 running and 1 queued after one tick.
  - `TestSlotsLine`: line format and the key cap.
  - `TestIdleSlotNudge`: the line through a fake tmux, not repeated when nothing changes, `max_parallel` respected, silent when every slot is taken.
  - `TestMilestoneTaskEdgeReasonsColumns` (store): PASS. Covers the migration of an old table and the round trip through create and update.
  - Every existing milestone test also passes.
- **Full suite:** `go test -count=1 -timeout 40m ./...` → every package ok (internal/jobs took 1037s), `exit=0`. The default 10-minute timeout is too short for internal/jobs (`scripts/check.sh` says so too).
- **Vet:** `go vet ./...` → `vet_exit=0`. `gofmt -l` is clean.
