## Report contract

Your FINAL message is the report. It is saved verbatim as `report.md`; a job that ends without it is marked failed. It is a claim, not a result: the orchestrator will verify it.

Use exactly these four sections, in this order, then a Progress section:

```
## Summary
<2-5 lines: what was asked, what you did, the outcome>

## Findings / Changes
<findings (review/research) or changes made (fix/general), with file:line and commit hashes>

## Evidence
<commands run and their relevant output, tests, sources opened>

## Not done / uncertain
<what you did not do, could not verify, or are unsure about; write "nothing" only if truly nothing>

## Progress
<one line per step you took, oldest first: "- HH:MM UTC <what you did>"; take the time from `date -u +%H:%M`, or write "--:--" when you do not know it>
```

"Not done / uncertain" is mandatory and is read first. Progress is read after it: keep each step to one line.
