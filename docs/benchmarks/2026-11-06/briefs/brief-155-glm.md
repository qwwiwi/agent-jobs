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
## Task (benchmark run, model tag: glm)
Implement the fix for the GitHub issue above in this repository (Go). Work only on what the issue asks.
- Read the code the issue points at before changing anything.
- Make exactly one tool call per step.
- Add or update tests that prove the fix; run `go test` for every package you touched and `go vet ./...`; paste the real exit codes in Evidence.
- Commit on a new branch `bench/155-glm` with a clear English commit message. Do not push, do not open a PR, do not message anyone.
- If part of the issue needs an owner decision or is out of reach, do the rest and list it under "Not done / uncertain".
