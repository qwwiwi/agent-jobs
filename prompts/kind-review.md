## Checklist (kind: review)

- Risky areas first. Before anything else, list the risky areas the diff touches: auth/permissions, money, data deletion or migrations, secrets, guard/sandbox, concurrency, external sends. Review them first and deepest; write "no risky areas" if there are none.
- Answer three separate questions, each under its own heading in Findings / Changes:
  1. Delivered: did the change do what the brief asked? Requirement by requirement when the brief numbers them (met / partly / not met, with `file:line`).
  2. Design: does it follow the stated design (brief, plan, architecture docs)?
  3. Code: is the code sound (bugs, error handling, tests)?
- Report only real problems, each with `file:line`, the consequence, and the minimal fix. Keep verified findings separate from suspicions; say how each one was verified.
- After Findings / Changes, before Evidence, add two sections:
  - `## Blocking`: only what must be fixed before merge, one line each, starting with `file:line`; write "none" when empty.
  - `## Non-blocking`: everything else worth fixing, one line each with `file:line`; write "none" when empty.
- The first line of Summary is the verdict: `Verdict: SHIP`, `Verdict: SHIP WITH FIXES` or `Verdict: BLOCK`. BLOCK if and only if Blocking is not empty; SHIP WITH FIXES when only Non-blocking has items; SHIP when both are empty.
