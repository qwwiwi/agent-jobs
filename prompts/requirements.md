Report on each covered row in a `### Requirements` block inside the report's `## Findings / Changes` section, one line per ID, fields separated by ` · `:

```
### Requirements
- R01 · done · <evidence: file:line> · ran: <command → result>
- R03i · partial · <what is there, what is not>
REDPROOF R01 · <test or command> <failure marker> before the change, passes after
```

Example of a verified `done` line and its red proof:

```
- R01 · done · internal/x/x.go:42 · ran: go test ./internal/x -run TestY → ok
REDPROOF R01 · TestY FAILS on base 813e682: want 2 got 1
```

- Status is one of `done`, `partial`, `missing`, `placeholder` (only for a row whose contract says `placeholder-ok`), `not-touched`.
- A `done` line counts only with `ran:` and a red proof of that ID: a block line `REDPROOF <ID> · <proof>` or a field `REDPROOF <ID>: <proof>` on the line. The marker is exactly `REDPROOF` and the proof names the ID as a whole word (`R01_other` does not name R01). A `done` line without both is counted as claimed, not done.
- `ran:` must name the command you ran (its result after `→`). The first word (Markdown wrappers and backticks stripped; a leading `$` and `VAR=value` assignments skipped) must be a known runner followed by an argument (`go`, `make`, `npm`, `npx`, `pnpm`, `yarn`, `node`, `python`, `pytest`, `cargo`, `bash`, `sh`, `git`, `curl`, `docker` and other common ones: `go test ./x`, `make test`) or a program path (`./scripts/check.sh`, `../bin/x`, `~/bin/x`, `/usr/bin/x`, or `dir/name.sh` with a script extension: `.sh`, `.py`, `.js`, `.ts`, …). Any other program: write its path. Free text (`not executed`, `none`, `no failures`) is not a command.
- A red proof must show failing evidence: a test or command reference (a test name like `TestY`, `BenchmarkX`, `test_y`, `x_test`, `x.test.ts`, `x.py::test_y`; a backticked command like `` `go test ./x -run TestY` ``; or the proof starting with a command) and a failure marker (`FAIL`, `fails`, `failed`, `failing`, `panic`, `panicked`, `assertion` / `AssertionError`, `error:`, or a non-zero `exit 1` / `exit code 2` / `exit status 1`; any case). A marker right after `no`, `not`, `never`, `without`, `cannot`, `nor`, `zero` or a word ending in `n't` (`did not fail`) does not count, nor does exit code 0. Free text (`no red proof`, `not cached`, `none`) is not a proof.
- A report without the block, or with no line for any covered ID, is off contract; a covered ID without a line is reported as missing.
- Optionally, commits name what they cover in a trailer: `Requirements: R01, R03i`.
- `brief/BRIEF.md` and `brief/CHECKLIST.md` are written only by the orchestrator: never create or edit them.
