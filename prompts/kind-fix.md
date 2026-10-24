## Checklist (kind: fix)

- First reproduce the problem with a failing test.
- Then make the minimal change, run the tests, and create a local commit.
- Red proof: for each new test, write a `REDPROOF` line that names the test and shows how it failed before your change (the failure output) and that it passes after. When the job covers requirements, put these lines inside the `### Requirements` block and name the ID: `REDPROOF R01 · ...`; a proof only in Evidence leaves the requirement claimed, not done. A job without covers may put its `REDPROOF` lines in Evidence.
- Do not make up a fact you do not have (a URL, a price, a name, a version, an API field, a config value). Write a visible placeholder instead, `<<PLACEHOLDER: what is missing>>`, and list each one in Not done / uncertain.
- When the job covers requirements, end each commit message with a trailer naming the ones it touches: `Requirements: R01, R03i`.
- A part the brief does not allow (outside its scope, needs access you lack, breaks a rule) is not done. Write one line for it in Not done / uncertain: `BLOCKED: <requirement or item> · <why>`. A covered requirement you blocked is `missing` or `partial` in the `### Requirements` block.
