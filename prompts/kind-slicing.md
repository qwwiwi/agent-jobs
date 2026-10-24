## Checklist (kind: slicing)

Ideas adapted from [nick-vels/skills](https://github.com/nick-vels/skills) «autopilot» (MIT, © 2026 Nick Vels).

You check how the owner's brief was sliced into checklist rows, before the owner approves the checklist. Your only input is the BRIEF.md and the checklist rows above; you read nothing else (no code, no other files).

- Read the brief sentence by sentence. Every statement of what the owner wants (a feature, a constraint, a quality, a "must", a "never") must be in some row.
- Return three lists:
  - `unmapped`: brief quotes that no row covers at all. A quote is copied from BRIEF.md verbatim, character for character (line breaks may become spaces); the shortest span that carries the requirement.
  - `half`: brief quotes that rows cover only in part (say which rows and what part is missing).
  - `invented`: rows the brief does not support (neither the owner's words nor a Basis quote back them).
- Facts only: no opinions on the product, no new requirements of your own, no rewording of the owner's words.
- An empty list is `[]`. Nothing else goes into the lists.

Put exactly one fenced block like this inside Findings / Changes (the daemon validates it; a report without it, with unknown fields, with a quote that is not in BRIEF.md verbatim, or with a row that is not in the checklist fails the job):

```json
{
  "schema": "agent-jobs/slicing/v1",
  "checklist": {"id": "<checklist id>", "version": 0, "sha256": "<checklist sha256>"},
  "unmapped": [{"quote": "<verbatim brief quote>", "why": "<one line>"}],
  "half": [{"quote": "<verbatim brief quote>", "rows": ["R01"], "missing": "<what the rows leave out>"}],
  "invented": [{"row": "R07", "why": "<one line>"}]
}
```

Copy `checklist` (id, version, sha256) from the input above. You are read-only: edit tools, git writes, rm and mv are denied; the report is your final message.
