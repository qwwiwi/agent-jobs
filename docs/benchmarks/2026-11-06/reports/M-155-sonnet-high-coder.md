[jobs:job-done] job 261106-131244-89b9 BENCH M #155 sonnet-high succeeded · 3 turns · $1.06 · 1m · model sonnet · effort high · auto-continued (attempt 2; attempt 1 ended its turn without a report after 1 turns (cost $0.99)) · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)
attempt 1: new failed · 1 turns · $0.99 · 1020s · <agent-jobs-home>/jobs/261106-131244-89b9/report.attempt-1.md
attempt 2: resume succeeded (current)

## Not done / uncertain
- **Stricter than before:** `rsync -a "$SRC/" "$DST/"` with fully dynamic operands is now refused, which is what the issue asks for. The owner may want to confirm this, since it may affect existing job scripts.
- **Wrong-fit test cases dropped:** three end-to-end cases I tried (`{}` after `+`, `./out/"$D"`, a dynamic `--exclude` value) are blocked by other existing guard rules (glob words and computed write targets). I covered them with unit tests instead.
- **Rsync in inline code is refused broadly:** `sed 's/rsync -a/x/'` or `go test -run 'rsync x'` is refused, as the existing git/ssh mention checks already do.
- **Deeper nesting not checked:** I did not check ssh behind `setsid`/`ionice` runner chains nested inside `find -exec` beyond one level.
- **No documentation update:** I did not touch `docs/DECISIONS.md` or other docs.
- **Subagents:** none used.

## Progress
- 14:48 UTC Read the issue, `guard.go` rsync code and the existing tests
- --:-- Wrote failing end-to-end tests; confirmed 9 failures on base
- --:-- Rewrote `rsyncRemote` as `rsyncScan`, with dyn, pattern words and `--log-file-format`
- --:-- Replaced `execArgv` with context-aware `findExecEnd`, and added stdin-runner refusal in `remoteCopyArg`
- --:-- Added the rsync-in-inline-code check and the filter-pattern exemption in `checkGitFiles`
- --:-- Added the ssh double-nesting fix in `scopeToolArg`, plus unit tests; ran the suites and `go vet`
- --:-- Committed `edcb489` on `bench/155-sonnet-high`
- 15:05 UTC Continuation: confirmed the clean worktree and commit, wrote PROGRESS.md and this report

## Summary
Issue #155 asked to close three rsync bypasses (find `+`, inline interpreter code, dynamic operands) and remove two false refusals (`--log-file-format`, `-f'- *.cache'`). All five are done, plus the nested `parallel` case for rsync and ssh. The work is committed locally as `edcb489` on `bench/155-sonnet-high` and the worktree is clean. `go test ./...` exits 0 and `go vet ./...` exits 0. Nothing was pushed.

## Findings / Changes
All changes are in `internal/guard/guard.go` unless noted.
- **High, find `+`:** new `findExecEnd` and `findTerminator`. `;` ends the command. `+` ends it only right after `{}`, and only for `-exec`/`-execdir`. The cut now applies only inside a find action (`remoteCopyArg`, replacing `execArgv`). This also closes a related hole I found: `setsid rsync a \; host:/x` used to have its remote operand cut off.
- **High, inline code:** new `reRsyncMention` and `rsyncInCode`, called from `unverifiable`. A word that mentions rsync inside `python -c`, `node -e`, `perl -e`, `awk` or `watch '...'` is refused. Shells and `eval` are excluded because their strings are parsed as nested scripts.
- **High, dynamic operands:** `rsyncRemote(args, dyn)` now receives which words come from variables or substitutions. `checkUserManager` and `remoteCopyArg` both pass it. A dynamic operand or option name is refused. A dynamic word with a static local prefix such as `./out/$D` stays allowed (`dynLocalPrefix`), and dynamic option values are data.
- **Medium, `--log-file-format`:** its value is now consumed (`rsyncValueOpts`).
- **Medium, `-f'- *.cache'`:** new `rsyncScan` and `rsyncPatternWords` find the quoted `-f`/`--filter`/`--exclude`/`--include` patterns. `checkGitFiles` skips the `.git` glob rule for them, unless the glob is unquoted.
- **Medium, double nesting:** new `stdinRunners` (`parallel`, `entr`, `xargs`) now refuse rsync anywhere behind them (`find -exec parallel rsync`). `scopeToolArg` has the same fix for ssh, which was passing before (`find . -exec parallel ssh ::: host \;`).
- **Tests:**
  - New `internal/guard/rsync_bypass_test.go`, with an end-to-end table plus unit tests `TestRsyncRemoteDynamism` and `TestFindExecEnd`.
  - `internal/guard/guard_test.go`: new cases in `TestRemoteTransfersIndirectRefused`, and the `rsync -f'- *.cache'` case flipped from block to allow.

## Evidence
- Before the fix, `TestRsyncRemoteBypassesRefused` failed on `find_-exec_literal_plus_arg`, `python_-c_os.system_rsync`, `node_-e_rsync`, `perl_-e_rsync`, `variable_operand`, `find_-exec_dynamic_operand`, `log-file-format_value`, `filter_data_glob` and `filter_long_data_glob`.
- After the fix it passes.
- `TestRsyncRemoteDynamism` and `TestFindExecEnd` call the new `rsyncRemote(args, dyn)` signature and `findExecEnd`, so they cannot compile on the old code. They pass now.
- The new `find -exec parallel ssh` case in `guard_test.go` returned `""` (allowed) in a scratch run before the ssh fix, and passes after.
- `go test ./...` exit 0: the root package, `cmd/agent-jobs`, `examples/manifest-check`, `internal/brain`, `casefold`, `checklist`, `guard` and `httpapi` all print `ok` (the background-task notification confirmed exit code 0).
- `go test ./internal/guard` exit 0. `go vet ./...` exit 0. `gofmt -l .` is empty.
- `git status` is clean on `bench/155-sonnet-high`; HEAD is `edcb489`.
