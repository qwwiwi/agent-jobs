## Checklist (kind: fix)

- First reproduce the problem with a failing test.
- Then make the minimal change, run the tests, and create a local commit.
- Red proof: for each new test, put a `REDPROOF` line in Evidence that names the test and shows how it failed before your change (the failure output) and that it passes after. When the job covers requirements, the line also names the ID: `REDPROOF R01 · ...`.
- Do not make up a fact you do not have (a URL, a price, a name, a version, an API field, a config value). Write a visible placeholder instead, `<<PLACEHOLDER: what is missing>>`, and list each one in Not done / uncertain.
- When the job covers requirements, end each commit message with a trailer naming the ones it touches: `Requirements: R01, R03i`.
- A part the brief does not allow (outside its scope, needs access you lack, breaks a rule) is not done. Write one line for it in Not done / uncertain: `BLOCKED: <requirement or item> · <why>`. A covered requirement you blocked is `missing` or `partial` in the `### Requirements` block.
