# v0.9 brief drafts

The v0.9.0 «crew v2 + metrics» release as a brief, a checklist and a plan. These are drafts: the brief
files `brief/BRIEF.md` and `brief/CHECKLIST.md` are written only by the orchestrator's checklist tools, so
a job cannot put them in place, and the v1.0.0 plugin files were not in the repository at the base commit
(they are untracked in the orchestrator's checkout, so a job's worktree does not have them).

Install, done by the orchestrator when the owner starts v0.9:

1. Move the existing v1.0.0 plugin brief, checklist and plan to `brief/queued/v1.0/`, unchanged except one
   line at the top of each file saying it follows v0.9 (`git mv`; untracked files: plain move, then add).
2. Put the three files of this folder in `brief/`: `BRIEF.md` and `CHECKLIST.md` through the checklist
   tools (the rows already parse in the checklist format and every quote is a substring of the brief),
   `PLAN.md` with `git mv`; then delete this folder.
3. Approve the checklist with `checklist_set approve` after the owner's «да» (the owner approved the
   eleven items in msg 75619; the added acceptance criteria change the hash).

Links inside `PLAN.md` to `BRIEF.md` and `CHECKLIST.md` stay valid after step 2.
