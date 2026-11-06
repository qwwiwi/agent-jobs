[jobs:job-done] job 261106-131244-4485 BENCH M #155 glm succeeded · 1 turns · $0.86 · 50m · model openrouter:z-ai/glm-5.3 · effort high · job dir 136.3 MB · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

Эти уведомления относятся к моим собственным ожидающим циклам для уже завершенного прогона `internal/jobs` — ничего нового не произошло. Все задачи выполнены: коммит `ad50f5f` находится в ветке `bench/155-glm`, все проверки пройдены. В соответствии с контрактом отчета, финальное сообщение:

## Not done / uncertain

- `find -exec parallel -j2 rsync ./src/ host:/b` and `find -exec watch -n5 ssh host ls` (options between the runner and the command word) still pass: the new predecessor-word detection looks only at the immediately preceding word, and the runners' own option positions are not parsed here. This is a pre-existing shape beyond the issue's named examples (the simple forms are covered and tested); noting it as a follow-up, not fixed.
- The H2 fix is deliberately fail-closed like ssh today: inline code naming rsync is refused even when the embedded rsync is purely local (`python3 -c 'os.system("rsync -a ./src/ ./dst/")'` blocks), same as `python3 -c 'print("use ssh host")'` already blocked before this change. The issue's ask ("refuse rsync mentions in inline code when its argv cannot be verified") is satisfied; verifying the embedded argv would need parsing arbitrary interpreter code, out of scope.
- UI check not applicable (no UI files touched).
- No `Requirements:` trailer on the commit: the brief carries no requirement IDs (no `brief/CHECKLIST.md` in this worktree).

## Progress

- 15:06 UTC Read repo layout, issue pointers and guard code (unverifiable, rsyncRemote, remoteCopyArg, execArgv, scopeToolArg, checkGitFiles).
- 15:10 UTC Reproduced all six issue examples with a scratch test; confirmed the exact failure modes and ssh parallels.
- 15:15 UTC Probed ssh-mention consistency (shells/eval nested parsing, tar "$D/ssh", python "use ssh") to match the fix to existing semantics.
- 15:20 UTC Implemented H1 (execArgvEnd, `+` only after `{}`) and M1 (--log-file-format value option).
- 15:25 UTC Implemented rsyncDynOperand, rsyncMentionArg, rsyncFilterArgs and the unverifiable/checkGitFiles call sites.
- 15:30 UTC Rewrote remoteCopyArg (dyn propagation, runner capture) and added the scopeToolArg execRunner case.
- 15:35 UTC Added TestRemoteTransfersBypasses155 (~40 cases), flipped the old `-f` expectation; full guard package green.
- 15:40 UTC Updated CHANGELOG and docs (ARCHITECTURE, GUIDE, GUIDE.ru, SKILL.md) per repo convention.
- 15:45 UTC gofmt, go vet, commit-message check clean; committed ad50f5f on bench/155-glm.
- 15:51 UTC Full `go test ./...`: all ok except internal/jobs at the default 10m timeout (documented ~14m package, #97).
- 15:55 UTC `go test -timeout 45m ./internal/jobs/` rerun: ok (1005s); job complete.

## Summary

Issue #155 asked to close the remaining rsync guard bypasses after #116: three High (a `+` hiding the remote operand after `find -exec`, rsync named in inline interpreter code, dynamic rsync operands) and two Medium false refusals (`--log-file-format` read as an operand, `-f'- *.cache'` refused by the `.git` glob rule), plus double nesting (`find -exec parallel rsync|ssh`). All six items are implemented in `internal/guard/guard.go`, with a ~40-case regression test, and committed as `ad50f5f` on branch `bench/155-glm` (worktree clean, not pushed). `go vet`, `gofmt`, `scripts/check-commit-messages.sh` and the full `go test ./...` pass.

## Findings / Changes

Commit `ad50f5f` (branch `bench/155-glm`, base `8f6e8f9`):

- **H1, `+` terminator** — `internal/guard/guard.go` `execArgv`/new `execArgvEnd`: a `+` terminates a `find -exec` argv only directly after `{}` (find's real batch-form rule); a lone `+` is a command argument, so `--exclude +` no longer hides `host:/dst/`.
- **H2, inline code** — new `rsyncMentionArg` + `reRsyncMention` (called from `unverifiable`): refuses an rsync word in inline code (an argument with spaces/quotes/parens) or a dynamic word, mirroring the ssh treatment via `reScopeMention`; rsync itself, `shells` and `eval` are exempt (their code is parsed as a nested script where rsyncRemote already checks the rsync command).
- **H3, dynamic operands** — new `rsyncDynOperand` (option walk mirroring `rsyncRemote`, value options consumed): a dynamic word in an operand position is refused for rsync directly in `unverifiable`, and in `remoteCopyArg` behind wrappers/runners/`find -exec`, with the `dyn` slice cut in step with the argv.
- **M1** — `--log-file-format` added to `rsyncValueOpts`, so its format value is consumed, not read as a `host:path` operand.
- **M2** — new `rsyncFilterArgs`: verified filter values (`--exclude`/`--include`/`--filter`/`-f`, attached or separate) are exempted from the `.git` glob rule in `checkGitFiles`; other value options (`--backup-dir`, `--temp-dir`) stay subject to it.
- **M3, double nesting** — `remoteCopyArg` now records which runner established the command position and refuses `parallel`/`entr` there (`find -exec parallel rsync`); `scopeToolArg` gains a case for a scope tool preceded by an `execRunners` word, closing `find -exec parallel ssh` and `find -exec setsid ssh` (shared ssh gap from the issue).
- **Tests** — `internal/guard/guard_test.go`: new `TestRemoteTransfersBypasses155` (~40 cases: the three High examples must block, the two Medium examples must pass, plus allowed-local and still-refused regressions); the old `rsync -f glob value still refused` expectation flipped to allowed (it asserted exactly the Medium #2 false refusal).
- **Docs** — `CHANGELOG.md` (Fixed, #155 entry), `docs/ARCHITECTURE.md`, `docs/GUIDE.md`, `docs/GUIDE.ru.md`, `skills/agent-jobs/SKILL.md`, matching what #157 did for #116.

## Evidence

Reproduction first (scratch test, deleted afterwards): before the change all six issue examples behaved wrong — `find . -maxdepth 0 -exec rsync --exclude + ./src/ host:/dst/ \;`, `python3 -c 'import os; os.system("rsync ./src/ host:/dst/")'`, `DST=host:/dst/; setsid rsync ./src/ "$DST"` and `find . -maxdepth 0 -exec parallel rsync ./src/ \;` returned `""` (allowed), while `rsync --log-file-format '%f:%l' ./src/ ./dst/` and `rsync -f'- *.cache' ./src/ ./dst/` were blocked (by the remote-host rule and the `.git` glob rule respectively).

After the change:

- `go test -count=1 ./internal/guard/` → `ok github.com/qwwiwi/agent-jobs/internal/guard` (exit 0), including `TestRemoteTransfersBypasses155` (all subtests pass) and the pre-existing `TestRemoteTransfersRefused` / `TestRemoteTransfersIndirectRefused`.
- Full suite `go test ./...` → every package `ok` except `internal/jobs`, which hit go's default 10-minute per-package timeout at 608s. `scripts/check.sh` itself documents this (comment: "internal/jobs alone takes ~14 minutes on a busy host (#97)") and sets `GO_TEST_TIMEOUT=45m`; rerun with `go test -count=1 -timeout 45m ./internal/jobs/` → `ok github.com/qwwiwi/agent-jobs/internal/jobs 1005.667s` (exit 0). The first failure was the default timeout, not a test failure.
- `go vet ./...` → exit 0, no output. `gofmt -l .` → empty. `scripts/check-commit-messages.sh` → exit 0.
- Behavior spot-check of the new rules (scratch probe, deleted): tightened — `watch 'rsync -a src/ dst/'`, `find -exec sh -c 'rsync -a src dst' \;`, `python3 -c 'print("use rsync -a src/ dst/")'`, `tar czf x.tgz "$D/rsync"`, `rsync -a src/ "$DST"` now blocked (consistent with how ssh mentions in the same positions were already blocked — verified on the base); preserved allowed — `bash -c "rsync -a src/ dst/"`, `echo run rsync -a src/ dst/`, `grep -q rsync Makefile`, `rsync -a src/ dst/`, `rsync -f'-x.cache'`, `rsync --exclude "$PAT" ./src/ ./dst/`, `find . -exec rsync a b \; -printf '%p:%y\n'`; preserved blocked — `rsync -avze ssh a host:/b`, `xargs rsync`, `parallel rsync`, `find -exec rsync a host:/b +`.

Named tests: `TestRemoteTransfersBypasses155` (new; fails before the change on all issue examples, passes after), and the pre-existing case `TestRemoteTransfersIndirectRefused/rsync_-f_glob_value_still_refused` whose expectation changed from block to pass (it pinned the Medium #2 false refusal; updated to `rsync_-f_glob_value_allowed`).
