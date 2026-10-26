# Milestones research: how established tools organise milestones

Summary of research job `261025-191825-00b4` (2026-10-25), run at the owner's request before the milestones build. Recorded in [DECISIONS.md](../DECISIONS.md).

**Question.** How do milestones relate to tasks, dependencies and acceptance criteria in GitHub, GitLab, Linear, Jira and Google practice, and in open-source agent orchestrators?

**Answer in short.** In every tool a milestone is a stage or a target that groups tasks; acceptance criteria are a separate artefact. Recommendation: do not rename the checklist; add milestones as a local plan layer, milestone -> tasks (`depends_on`, `covers`) -> jobs. The checklist stays the measure of acceptance; a milestone sits under the brief and is measured against its rows.

Sections 1 and 2 were checked against the sources listed at the end; section 3 is an inference. What could not be verified is listed in *Not verified*.

## Mainstream trackers (verified against sources)

| Tool | What a milestone is | Contents | Progress | Dependencies | When done |
|---|---|---|---|---|---|
| GitHub | a group of issues and pull requests in one repository, due date optional | issues and pull requests; sub-issues (up to 100 per parent, up to 8 levels) | a "completion percentage", formula not documented; the API gives open / closed counts | issue blocked-by / blocking: shown but not enforced (does not block) | state open / closed; automatic closing not documented |
| GitLab | a timebox with dates; can be used as a release | issues, merge requests, epics; one milestone per item | closed / total issues; weight is not counted | blocks / is blocked by (Premium), a warning only | closes after its due date or by hand; open issues stay open |
| Linear | a project milestone: a "stage in a project's lifecycle", a deliverable inside a project | issues of one project | a started issue counts partially, a completed one fully | blocked-by between issues; projects depend end -> start, violations are shown in red | not documented; the project status is set by hand |
| Jira | the release target is a Fix version; the deliverable is an Epic | fix versions; epic -> stories; acceptance criteria sit on the story, not on the version or the epic | counts by status category To Do / In Progress / Done | blocks / is blocked by; enforcement not documented | a version is released by hand, with a decision on its unresolved issues |
| Google | a design doc written before a large project | goals, non-goals, implementation strategy | - | - | reviewed against its goals; no primary source on milestones found |

**Key conclusions.**

- Acceptance criteria are not a milestone. Atlassian defines acceptance criteria as "conditions that a ... user story ... must satisfy to be complete", and the Definition of Done "is not the same thing as acceptance criteria". So acceptance criteria belong to the unit of work, not to the stage.
- Dependencies are edges between tasks (blocked-by), not a property of a milestone. Parallelism follows from them: tasks with no open blocker can run at the same time.
- Progress is almost always the share of closed tasks; it does not show acceptance.

## OSS agent orchestrators (verified against README and code)

| System | Plan model | Dependencies and parallelism | How a later task sees an earlier one | Acceptance |
|---|---|---|---|---|
| claude-task-master | `tasks.json`: id, dependencies, subtasks, details, testStrategy; tags instead of milestones (by convention) | explicit dependencies with a cycle check; `next` returns one ready task, no parallelism | by hand: notes appended to `details` | `set-status done` following testStrategy, not enforced |
| CrewAI | Task (context, async_execution, guardrail), Flows | `context=[tasks]` is the dependency; async tasks fan out, context waits for them | the full raw output of the context tasks, joined with a separator | a guardrail (code or LLM) with retries; human_input |
| LangGraph plan-and-execute | plan: `List[str]`, past_steps | strictly sequential | the executor sees only the plan; results go to the replanner | the LLM replanner decides |
| LLMCompiler | `{idx, tool, args, dependencies}` | a DAG run on a thread pool | the full result substituted for `$N` | a joiner: FinalResponse or Replan |
| OpenHands SDK | `TASKS.json` `{title, notes, status}`; DelegateTool | no dependencies; a delegate runs up to 5 threads | a shared workspace plus the joined answers | the agent itself; the rule "not done with failing tests" |
| GitHub spec-kit | `tasks.md`: `T001 [P] [US1] ...`, grouped in phases | phase order; `[P]` marks tasks on different files and no dependencies; tasks on the same file run sequentially | files on disk (`plan.md`, contracts, `[X]` marks) | a checkpoint at every phase ("testable independently"); failing tests first |
| Magentic-One | a task ledger and a progress ledger | one executor per turn; replan on a stall | a shared thread | the LLM's `is_request_satisfied` |

**Conclusion.** An explicit "stage + checkpoint" layer exists only in spec-kit, the closest analogue to the owner's idea. Results are passed on either as full text (CrewAI, LLMCompiler) or as curated files and notes (task-master, spec-kit). For agent-jobs a hybrid fits: a short summary in the brief, the full report as a file.

## Outcome and recommendation [inference]

The research recommended, and the milestones build follows it:

- The project does not rename the checklist. GitLab, Linear and Jira use "milestone" for a stage or target that contains tasks, Atlassian keeps acceptance criteria on the task level, and `PLAN.md` already used "milestone" for a stage with its Acceptance. The checklist (`brief/CHECKLIST.md`) stays the acceptance list of the repository: what is to be built, approved by the owner, the measure of acceptance.
- A milestone is a stage of work under the brief: the agent's technical plan, M1..Mn.
- A milestone is made of tasks T1..Tk with `depends_on` (the dependency edges) and `covers` (the checklist rows the task works on); jobs are the attempts at a task.
- A task's status is derived, not set by hand (blocked, ready, running, review, done, failed; as built also dropped); done comes from the accepted outcome of its job, not from closing a ticket.
- Progress is two numbers: tasks done / total (the GitLab formula, dropped tasks not counted) and acceptance met / N over the covered checklist rows.
- Prior results: a job on a task gets a short summary (redacted) of each dependency's report in its brief, plus the full report as a file in its job dir (`prior/<key>/report.md`): CrewAI-style context, compressed, plus spec-kit-style files. See [GUIDE.md, Milestones](../GUIDE.md#milestones).

## Not verified

- The GitHub progress formula (the "completion percentage").
- "One milestone per issue" for GitHub and Linear (only implied by the docs).
- The Linear issue-relations page (it returned 404).
- Whether Jira allows several fix versions on one issue.
- A primary Google or Meta source on milestones (none found).
- Not checked at all: Claude Code TodoWrite, BMAD-METHOD, the legacy OpenHands AgentDelegate.

## Sources

Opened by the research job:

- GitHub: docs.github.com pages about-milestones, REST `issues/milestones`, creating-issue-dependencies, adding-sub-issues
- Linear: linear.app/docs/project-milestones, linear.app/docs/projects, linear.app/docs/project-dependencies
- GitLab: docs.gitlab.com/user/project/milestones/, related_issues, epics
- Jira: support.atlassian.com (configure-versions-in-a-scrum-project, manage-versions, link-issues, release-a-version); atlassian.com (epics, acceptance-criteria, definition-of-done)
- Google: industrialempathy.com/posts/design-docs-at-google/; abseil.io/resources/swe-book/html/ch10.html
- CrewAI: docs.crewai.com/en/concepts/tasks
- spec-kit: raw.githubusercontent.com/github/spec-kit/main/templates/tasks-template.md
- claude-task-master: docs/task-structure.md and docs/command-reference.md
- LangGraph: plan-and-execute.ipynb and LLMCompiler.ipynb
- OpenHands software-agent-sdk: task_tracker and delegate
- AutoGen: `_magentic_one`
