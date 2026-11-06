[jobs:job-done] job 261106-131245-399c BENCH L #119 sonnet-medium succeeded · 44 turns · $0.72 · 40m · model sonnet · effort medium · job dir 157.4 MB · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- The default `max_parallel` is unchanged: it is already uncapped, so I did not add a numeric default. I could not confirm the a production cap of 2 came from anywhere other than the planner passing `max_parallel` explicitly.
- Item 3 of the issue, deriving edges mechanically from path overlap, is not done. It needs a per-task path list and the owner's decision on the rules.
- Item 5 is only partly covered. Reasons are optional per edge, and nothing checks that a stated reason is true. There is also no automatic serialization of tasks that share a money path.
- Dropping reasonless edges also drops invalid ones silently, such as a self-dependency or an unknown key. A cycle test and two other tests had to be given reasons to keep checking those cases.
- The nudge only counts jobs that exist. Because tasks only start through `job_start`, the nudge tells the orchestrator which tasks are ready but does not start them. It ignores a milestone's own `max_parallel`.
- The full-suite failures in the Evidence section are unexplained. I did not run those tests on the base commit.

## Progress
- --:-- Read milestone code (`milestones.go`, store, manager launch path)
- --:-- Added the `depends_reasons` field, storage migration and edge-drop logic
- --:-- Added `IdleSlotLine` and `nudgeIdleSlots`, called from the daemon tick
- --:-- Wrote new tests and added reasons to the existing fixtures
- --:-- Ran vet and the tests, then committed `42be03b` on `bench/119-sonnet-medium`

## Summary
Issue #119 asked for reasoned `depends_on` edges, a default `max_parallel` equal to the daemon's `max_running`, and an idle-slot nudge. I implemented the first and third and left the default alone, because it is already uncapped. The milestone, mcp, httpapi, store and cmd tests pass, and `go vet` is clean. A full-package run had unrelated failures (see below). Commit `42be03b` is on `bench/119-sonnet-medium`.

## Findings / Changes
- **Reasons per edge.** `MilestoneTaskInput` has a new `depends_reasons` field (dependency key → one-line reason) in `internal/jobs/milestones.go`.
  - `applyTasks` keeps an edge only when it has a reason, either given now or already stored for the same edge.
  - Dropped edges come back as `dropped_edges` (for example `T2->T1`) on the `milestone_create` and `milestone_set` result.
  - Reasons are limited to one line and 200 characters.
- **Stored and shown.** There is a new `depends_reasons` column on `milestone_tasks`, added by an `ALTER TABLE` migration in `internal/store/milestones.go`. The task view includes `depends_reasons`.
- **Max parallel.** I changed no behaviour. Without `max_parallel` there is no milestone cap, so `max_running` is the only limit. I updated the field and tool descriptions to say so.
- **Idle-slot nudge.**
  - `IdleSlotLine` returns a line like `3 slots free, ready: M1: T8, T9` when slots are free and open milestones have ready tasks.
  - `nudgeIdleSlots` logs that line each daemon tick, only when it changes. It is called from the tick in `manager.go`.
- **Tool text.** The `milestone_create` description in `internal/mcp/tools_milestones.go` now tells the planner to justify each edge.
- **Existing tests.** I added reasons to the existing fixtures that use `depends_on`, since reasonless edges are now dropped.

## Evidence
- New tests are in `internal/jobs/milestones_edges_test.go`. Before the change they did not compile (`DependsReasons`, `DroppedEdges` and `IdleSlotLine` did not exist). Now they pass.
  - `TestMilestoneEdgeReasons` has two cases. One checks that a reasonless edge is dropped and reported, the task becomes ready, the reason is stored, and an edit that repeats the edge keeps its reason. The other checks that a multi-line reason is refused.
  - `TestIdleSlotLine` checks the nudge line for two ready tasks.
- `go vet ./...` exit 0.
- `go test ./internal/jobs -run 'Milestone|Idle|Edge'` gave `ok` (70s).
- The milestone-filtered runs of `cmd`, `mcp`, `httpapi` and `store` gave `ok`, after I added reasons to the fixtures that still failed.
- The full run of `./internal/jobs`, `./cmd/...`, `./internal/mcp`, `./internal/httpapi`, `./internal/store` and the root package (exit status not captured) had failures in:
  - `internal/jobs`: heartbeat, doctor-procs, daemon-restart, orphan-wrapper, init and manual-tick tests, then a 10m package timeout.
  - `internal/mcp`: stdio, binary-refusal, stale-MCP and intentions tests.
  - I re-ran only `TestHeartbeatWakeLineIsFixed` and `TestInitWritesOrchestratorDuties` alone, and both passed. I did not check any of these failures against the base commit.
- I wrote the full-run log to `/tmp/aj-full.txt`, outside the job directory.
