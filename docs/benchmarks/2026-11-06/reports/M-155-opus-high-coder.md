[jobs:job-done] job 261106-131244-35ff BENCH M #155 opus-high succeeded · 92 turns · $6.00 · 56m · model opus · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- **New false refusals (fail-closed by design):**
  - `rsync -a ./src/ "$HOME/x/"` or `"$PWD/out/"` is refused, because nothing literal comes before the expansion. Allowing these would mean resolving variables from the environment when the call does not set them — your call.
  - The rsync-mention rule, like the existing ssh rule, also refuses data such as `sed -i 's/rsync -a/…/' f` or `awk '/rsync /{…}'`, and local rsync inside `python -c` or `perl -e`.
- **Same root cause, not fixed:** `internal/guard/readonly.go:440` still stops at any `+` in find's argv (read-only mode, e.g. `find . -exec rm -f + README.md \;`). It is outside rsync, so it should be a separate ticket.
- **Filter exemption is partial:** it only applies to a top-level rsync. `setsid rsync --exclude '*.o' a b` is still refused by the glob rule (a false refusal, low severity).
- **Commit message:** the owner rules ask for Russian, the benchmark brief for English. The subject line is Russian and the body English. There is no `Requirements:` trailer because the brief names no requirements.

## Progress
- 14:42 UTC Read guard.go (rsyncRemote, remoteCopyArg, scopeToolArg, execArgv, checkGitFiles, unverifiable)
- --:-- Wrote rsync_bypass_155_test.go and reproduced 15 failures on the base code
- --:-- Checked how the parser marks variable words (dyn/bare/glob)
- --:-- Implemented scanRsync, localPrefix, --log-file-format, execContext/findEnd, the rsync code-mention rule and the filter exemption
- --:-- guard tests green; flipped the guard_test.go case; added CHANGELOG entry
- --:-- First full go test: internal/jobs hit the default 600s timeout; reran the other packages with -timeout 40m
- --:-- Review subagent; applied tilde, -ok, code without spaces, rsync self-exempt and --exclude .git, with tests
- --:-- Committed a7563c9 on bench/155-opus-high
- --:-- Confirmed the new tests fail on the base guard.go (stubs) and pass after
- 15:37 UTC Clean rerun of internal/jobs, root and cmd: ok; go vet exit 0

## Summary
Issue #155 listed remote-rsync bypasses of the guard that #116 left open. All three High examples now block and both Medium examples now pass; the double nesting (`find -exec parallel rsync|ssh`) blocks too. I also fixed more gaps found while doing this and by a review subagent: `find -exec setsid ssh`, tilde operands, perl code with no spaces, `rsync --exclude .git`. Everything is in one local commit, `a7563c9`, on branch `bench/155-opus-high`, not pushed. `go vet` and every test package pass.

## Findings / Changes
All in `internal/guard/guard.go` unless noted.
- **High, the literal `+`** (`execContext` :1589, `findEnd` :1633; they replace `execArgv`): a find action's command now ends only at a real terminator. That is a `;` word, or a `+` right after `{}` for `-exec`/`-execdir` (never for `-ok`/`-okdir`). So `find … -exec rsync --exclude + ./src/ host:/dst/ \;` blocks.
  - `execContext` also follows runners nested inside other runners or find actions, and cuts the argv only at find's own terminators. Before, a quoted `';'` after `setsid setsid rsync` hid the operand.
- **High, inline code** (`remoteCopyArg` :1550, `reRsyncMention` :1172, `shellCodeParsed` :1580): the word rsync inside code is refused (`python3 -c`, `node -e`, `perl -e`, also exec-option code with no spaces such as `perl -e system+q+rsync+…`).
  - Not refused when the code is shell code the guard parses itself (`sh -c`, `trap`, `watch`), or when the program is rsync itself.
- **High, `$VAR` operands** (`scanRsync` :1373, `localPrefix` :1440, `tildeVariable` :1451): rsync now sees whether each argument comes from a variable, at all three call sites. These are refused:
  - an operand from a variable or substitution with no literal `/` before the expansion (`"$DST"` blocks, `./"$X"` passes);
  - an expansion in an option word (`"$OPT"`);
  - any unquoted expansion;
  - `~+`, `~-` and directory-stack tildes, and `~` when the call sets HOME.
  - A quoted value of a known option (`--exclude "$PAT"`) stays allowed.
- **Medium, `--log-file-format`** (:1474): its value is now consumed, so `'%f:%l'` is no longer read as a remote operand.
- **Medium, `-f'- *.cache'`** (`checkGitFiles` :2660): quoted rsync filter patterns (`-f`, `--exclude`, `--include`, `--filter`) are exempt from the `.git` glob and `.git` path rules. An unquoted glob is still refused.
- **Medium, double nesting**: `stdinRunners` (`parallel`, `entr`) now applies to the runner directly in front of the command, not only to the outer program. `scopeToolArg` uses `execContext` too, so ssh nested in a find action or another runner is refused.
- Tests: new file `internal/guard/rsync_bypass_155_test.go` with `TestRsyncBypasses155` (48 cases), `TestFindEnd155` and `TestExecContext155`.
  - In `guard_test.go` I flipped one case: `rsync -f'- *.cache'` was expected to block, the issue says it must pass.
  - Added an entry to `CHANGELOG.md`.

## Evidence
- **Before the fix**, the new table on the base code: 15 cases failed (`Check(...) = "", want block=true` for the High examples, and false refusals for `--log-file-format` and `-f'- *.cache'`).
- **On the base code with the final tests** (base `guard.go` plus stubs in a temporary test file, deleted afterwards), these failed:
  - 22 cases of `TestRsyncBypasses155`, including all three High and both Medium examples, `find_-exec_parallel_rsync|ssh`, `find_-exec_setsid_ssh`, the tilde cases, `perl_-e_code_without_spaces` and `rsync_--exclude_.git`;
  - `TestRemoteTransfersIndirectRefused/rsync_-f_quoted_glob_filter`;
  - `TestFindEnd155` and `TestExecContext155` (they only fail there because of the stubs).
- **After the fix**:
  - The four tests together give 70 `--- PASS` lines.
  - `go test -count=1 ./internal/guard/` → `ok … 12.293s`, exit 0.
  - `go vet ./...` → exit 0.
  - `go test -count=1 -timeout 40m ./internal/jobs ./ ./cmd/agent-jobs` → all `ok` (internal/jobs 1008.771s), exit 0.
  - Earlier runs: examples/manifest-check, brain, casefold, checklist, httpapi, mcp, skills, store, testutil and trash all `ok`.
- **Two earlier failures were not caused by the fix**:
  - internal/jobs ran past go's default 10-minute timeout (607s).
  - `TestPrepareTestHooksIgnoredWithoutTag` broke because I edited `guard.go` while it was building the binary (`too many arguments in call to scanRsync`). Both passed on the clean rerun above.
- **Subagent**: one general-purpose subagent, read-only, reviewed the diff and found no index risks. I applied five of its points (tilde, `-ok {} +`, code with no spaces, rsync exempt from its own mention rule, `--exclude .git`) and added a test for each.
