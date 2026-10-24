## Checklist (kind: slicing)

You check how the owner's brief was sliced into checklist rows, before the owner approves the checklist. Your only input is the BRIEF.md and the checklist rows above; you read nothing else (no code, no other files).

- Go through the brief one sentence at a time; each thing the owner asks for (a feature, a limit, a quality, a "must", a "never") should map to at least one row.
- Return three lists:
  - `unmapped`: passages of the brief no row points to; copy each from BRIEF.md exactly (line breaks may become spaces), the shortest span that states the requirement.
  - `half`: passages a row covers in part; name the rows and what they leave out.
  - `invented`: rows with nothing in the brief behind them (neither the owner's words nor a Rationale quote).
- Report what you found and nothing more: do not judge the product, do not propose requirements, do not paraphrase the owner.
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

Copy `checklist` (id, version, sha256) from the input above. You are read-only: edit tools, git writes, rm and mv are denied; the report is your final message: the daemon reads the block only from it, never from a file (a block written into report.md or any other file does not count).
