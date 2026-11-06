# Issue #155: Гард: оставшиеся обходы rsync после #116 (find +, python -c, $VAR)

Follow-up to #116 (PR from `fix/guard-refuse-scp-rsync-r2`, head 0e87aa8). The sol control review (job 261106-060759-ea1f) closed the original examples (`find -exec`, `setsid` and piped `xargs rsync`) but found that remote rsync still gets past the guard through three variants. ssh is refused in each of these cases.

## High

- `internal/guard/guard.go:1494`: `execArgv` stops at a literal `+`, so `find . -maxdepth 0 -exec rsync --exclude + ./src/ host:/dst/ \;` hides the remote operand. Fix: recognise real find terminators in context only.
- `internal/guard/guard.go:1471`: inline interpreter code is not checked, so `python3 -c 'import os; os.system("rsync ./src/ host:/dst/")'` gets through. `reScopeMention` omits rsync. Fix: refuse rsync mentions in inline code when its argv cannot be verified.
- `internal/guard/guard.go:1483`: dynamic operands are not checked, so `DST=host:/dst/; setsid rsync ./src/ "$DST"` gets through. Fix: propagate argument dynamism and refuse unverifiable rsync operands.

## Medium

- `guard.go:1394`: the `--log-file-format` value is not consumed, so a local `rsync --log-file-format '%f:%l' ./src/ ./dst/` is falsely refused.
- `guard.go:2518`: `-f'- *.cache'` is falsely refused by the `.git` glob rule. Fix: exempt verified filter data.
- Shared with ssh: double nesting (`find -exec parallel rsync|ssh`) where the remote argument comes from parallel's input.

Regressions to add: the three High examples must block, and the two Medium examples must pass.


---
## Review task (benchmark)
Review the change on branch `bench/155-glm` (diff `8f6e8f9e5969b205fa9e400b66d0728ba39602cd..HEAD` in this worktree) as the fix for the issue above.
Judge correctness, whether the issue is fully fixed, regressions, test quality. Do not modify files.
End the report with exactly one line in this form, counts of findings by severity:
BENCH-VERDICT: critical=<n> high=<n> medium=<n> low=<n> issue_fixed=<yes|partly|no>
