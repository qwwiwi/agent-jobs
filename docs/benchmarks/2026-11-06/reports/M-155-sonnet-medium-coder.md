[jobs:job-done] job 261106-131244-c1b5 BENCH M #155 sonnet-medium succeeded · 2 turns · $0.55 · 1m · model sonnet · effort medium · auto-continued (attempt 2; attempt 1 ended its turn without a report after 1 turns (cost $0.51)) · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)
attempt 1: new failed · 1 turns · $0.51 · 899s · <agent-jobs-home>/jobs/261106-131244-c1b5/report.attempt-1.md
attempt 2: resume succeeded (current)

## Not done / uncertain
- Other nestings, such as `find -exec parallel setsid rsync`, were not checked.
- The inline-code check refuses any inline code or runner string that mentions the word rsync, even in a comment or a purely local call (`python3 -c 'print("rsync")'`). That follows the issue's fail-closed rule, but it can produce false positives.
- The filter exemption covers only `-f`, `--filter`, `--exclude` and `--include`. Options like `--exclude-from` still go through the old glob rule.

## Progress
- --:-- Read guard.go around rsyncRemote, remoteCopyArg, execArgv, checkGitFiles
- --:-- Wrote issue155_test.go and confirmed 12 failures
- --:-- Implemented the fixes in guard.go and updated the old `-f` test in guard_test.go
- --:-- Ran guard tests, `go vet`, full `go test ./...` (all pass)
- --:-- Committed on `bench/155-sonnet-medium` (6732627)
- --:-- Continuation: confirmed a clean worktree and wrote PROGRESS.md

## Summary
Issue #155 asked to close three remaining remote-rsync bypasses in the guard and fix two false refusals. The three High examples now block and both Medium examples pass. It also covers the shared `find -exec parallel` double nesting, for rsync and for ssh. The work is committed locally as 6732627 on `bench/155-sonnet-medium`, nothing pushed, and the worktree is clean.

## Findings / Changes
All in `internal/guard/guard.go`, plus a new `internal/guard/issue155_test.go`:
- **`execArgv`**: a `+` ends find's `-exec` argv only right after `{}`.
- **`remoteCopyArg`**: rsync mentioned inside inline interpreter code (`python3 -c`, `node -e`) or in a command string given to an exec runner (`watch '…'`) is refused. Shells are skipped because `sh -c` is already parsed as a nested script.
- **`rsyncRemoteDyn`, `localPrefix`** (new): an rsync operand with an expansion counts as remote unless its literal prefix already has a `/` before any `:`. So `"$DST"` and `host:"$N"` block, while `./dst/"$N"` stays allowed. This applies to direct rsync in `checkUserManager` and to rsync behind find/runners.
- **Double nesting**: `find -exec parallel rsync` is refused (new `stdinRunner` helper). `scopeToolArg` also refuses an out-of-scope tool after a runner, so `find -exec parallel ssh` and `find -exec setsid ssh` block.
- **`rsyncValueOpts`**: `--log-file-format` added, so its value is no longer read as an operand.
- **`rsyncFilterValues`** (new): quoted values of `-f`, `--filter`, `--exclude` and `--include` skip the `.git` glob rule in `checkGitFiles`. `gitFile` is still checked and an unquoted glob is still refused.
- **`guard_test.go`**: the `-f'- *.cache'` case now expects allow instead of block, as the issue asks.

## Evidence
- The new `TestRsyncBypassesIssue155` failed before the fix on 12 subtests:
  - find literal plus
  - python `-c`
  - node `-e`
  - setsid var operand
  - find-exec var operand
  - find-exec parallel rsync
  - find-exec parallel ssh
  - log-file-format
  - four filter-glob cases (`-f` attached, `-f` separate, `--exclude`, `--filter`)
- It passes after the fix.
- `go test ./internal/guard`: exit 0.
- `go vet ./...`: exit 0.
- `go test ./...`: the background run exited 0, and every package printed ok.
- `gofmt -l`: no output.
