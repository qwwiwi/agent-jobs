Report on each covered row in a `### Requirements` block inside the report's `## Findings / Changes` section, one line per ID, fields separated by ` · `:

```
### Requirements
- R01 · done · <evidence: file:line> · ran: <command → result>
- R03i · partial · <what is there, what is not>
REDPROOF R01 · <test name> failed before the change, passes after
```

- Status is one of `done`, `partial`, `missing`, `placeholder` (only for a row whose contract says `placeholder-ok`), `not-touched`.
- A `done` line counts only with `ran:` (the command you ran and its result) and a red proof of that ID: a block line `REDPROOF <ID> · <proof>` or a field `REDPROOF <ID>: <proof>` on the line. The marker is exactly `REDPROOF`, the proof names the ID as a whole word (`R01_other` does not name R01), and `none`, `n/a`, `na`, `n.a.`, `no`, `not …`, `nothing …`, `tbd`, `skipped`, `pending`, `missing`, `?` or a dash (also as `**none**`, `_n/a_` or in backticks) are not a proof; the same words as a `ran:` value are not a run. A `done` line without both is counted as claimed, not done.
- A report without the block, or with no line for any covered ID, is off contract; a covered ID without a line is reported as missing.
- Optionally, commits name what they cover in a trailer: `Requirements: R01, R03i`.
- `brief/BRIEF.md` and `brief/CHECKLIST.md` are written only by the orchestrator: never create or edit them.
