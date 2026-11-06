# Issue #119: Milestones: estimate real dependencies and fill all job slots

Owner request 2026-10-27.

Problem: on the a production train (milestone 1, 11 tasks) the milestone ran with max_parallel 2 and a chain of depends_on that was stricter than the code needed. Only 2 jobs ran while the host allows 5. Four tasks (session/login, isolated margin, history, UI defects) did not touch the order code at all and waited for nothing.

Idea:
1. At milestone_create, set a dependency only when tasks really conflict: they edit the same files/modules, or one consumes the other's output. Ask the planner to justify every depends_on edge in one line (files/modules shared); an edge without a reason is dropped.
2. Default max_parallel = the daemon's max_running (today 5), not 2.
3. Optionally estimate overlap mechanically: for each task list the paths it is expected to touch (from the brief or a quick scout), and derive edges from path overlap.
4. Idle-slot nudge: when ready tasks exist and running jobs < max_running, the daemon (or heartbeat) emits one line to the orchestrator: "N slots free, ready: T8, T9 ...".
5. Guardrail: tasks that share a money path or the same files stay serialized; parallel tasks always run in their own worktrees (already true for jobs).

Acceptance: on a milestone with independent tasks, all of them start up to max_running without manual edits; every depends_on edge carries a reason.

---
## Review task (benchmark)
Review the change on branch `bench/119-opus-high` (diff `8f6e8f9e5969b205fa9e400b66d0728ba39602cd..HEAD` in this worktree) as the fix for the issue above.
Judge correctness, whether the issue is fully fixed, regressions, test quality. Do not modify files.
End the report with exactly one line in this form, counts of findings by severity:
BENCH-VERDICT: critical=<n> high=<n> medium=<n> low=<n> issue_fixed=<yes|partly|no>
