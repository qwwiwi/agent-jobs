## Checklist (kind: review)

- Risky areas first. Before anything else, list the risky areas the diff touches: auth/permissions, money, data deletion or migrations, secrets, guard/sandbox, concurrency, external sends. Review them first and deepest; write "no risky areas" if there are none.
- Answer three separate questions, each under its own heading in Findings / Changes:
  1. Delivered: did the change do what the brief asked? Requirement by requirement when the brief numbers them (met / partly / not met, with `file:line`).
  2. Design: does it follow the stated design (brief, plan, architecture docs)?
  3. Code: is the code sound (bugs, error handling, tests)?
- Report only real problems, each with `file:line`, the consequence, and the minimal fix. Keep verified findings separate from suspicions; say how each one was verified.
- Brief gap: for each finding, ask whether the executor's brief gave it a way to see the problem (a rule it stated, a file it named, a check it asked for). If it did not, add `brief gap: <what the brief should have said>` to that finding.
- Fix review: when the brief says it is a review of a fix, look only at the fix diff and the conditions the fix has to close. Mark each condition `closed` or `open`. A condition that can still be defeated another way stays `open`; do not file that as a new finding. Each `open` condition is a Blocking line.
- End Findings / Changes with two subsections (still inside it: the report keeps its four sections):
  - `### Blocking`: only what must be fixed before merge, one line each, starting with `file:line`; write "none" when empty.
  - `### Non-blocking`: everything else worth fixing, one line each with `file:line`; write "none" when empty.
- The first line of Summary is the verdict: `Verdict: SHIP`, `Verdict: SHIP WITH FIXES` or `Verdict: BLOCK`. BLOCK if and only if Blocking is not empty; SHIP WITH FIXES when only Non-blocking has items; SHIP when both are empty.
- You are read-only: edit tools, git writes, rm and mv are denied. Do not change the code under review; the report is your final message, and progress notes go into the job directory with a shell heredoc.
- Sources: when the brief has a Sources section, read the local copy under sources/ in the job directory; do not study code through the GitHub API or website. Never run it on the host (see that section).
