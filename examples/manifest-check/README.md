# manifest-check.sh (reference, Wave F F6)

`scripts/manifest-check.sh` lives in the target repository, not in agent-jobs: agent-jobs never copies it there. This directory has a reference version for Go repositories (POSIX `sh` and `awk`, no `jq`) and its tests (`manifest_check_test.go`, run by `go test ./...`).

## The contract

- Tests are tagged with requirement IDs from `brief/CHECKLIST.md`: `[R01]`, `[R03i]`, `[G02]`. In Go the tag goes in a subtest name: `t.Run("[R01] login page renders", ...)`, which runs as `TestLogin/[R01]_login_page_renders`. One name can carry several tags (`[R02][R03i] ...`).
- Every **live** requirement (an `R`, `Ri` or `G` row with contract `live`; `D` rows and `deferred` / `dropped` rows never count) needs at least one tagged test that **passed in this run**. A failed or skipped test does not count, and neither does a tag that appears only in another test's output.
- Exit `0` when every live requirement has such a test; `1` when at least one has none (the line `MISSING  R03i ...` names it); `2` on bad input (no checklist, no live requirement, no test output).

A tag only says which requirement a test is for, not that it tests it. The judge reads the tagged tests and lists the ones that pass without testing their requirement in `suspect_tests`.

## How agent-jobs runs it

Before a judge job (`judge_start`) starts its model, the job's wrapper checks out the judged commit in a separate git worktree and writes the approved `brief/BRIEF.md` and `brief/CHECKLIST.md` over the commit's copies (so the script checks the requirements the owner approved), and runs `./scripts/manifest-check.sh` from the repository root, with no arguments, the way it runs `acceptance_cmd`: the job's containment, a scrubbed environment (`HOME` is a throwaway dir, no tokens), no guard hook, the `acceptance_timeout_minutes` limit. It needs config `acceptance_allowed: true`; without it, or without the script at the commit, the judge job fails before the judge starts and nothing is recorded. The exit code and the output tail are stored with the job and the commit, the output goes into the judge's prompt, and `release_check` passes only with exit `0`.

## Wiring it into a repository

1. Copy `manifest-check.sh` to `scripts/manifest-check.sh` in the repository and make it executable (`chmod +x`; without the bit the wrapper runs it with `/bin/sh`).
2. Call it from the repository's `scripts/check.sh`, so a local check fails the same way the gate will:

   ```sh
   echo "== manifest-check"
   tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
   go test -json ./... > "$tmp/go-test.json" || true   # failing tests are reported by the go test step
   scripts/manifest-check.sh "$tmp/go-test.json"
   ```

   With a file argument it reads `go test -json` output from it; without one it runs `go test -json ./...` itself (`MANIFEST_GO_TEST_ARGS` changes the arguments, `MANIFEST_CHECKLIST` the checklist path).
3. It can also be a fix or general job's `acceptance_cmd` (`scripts/manifest-check.sh`), so a job does not finish green with an untested requirement.

For another language, keep the contract and change how the passing tests are read: what matters is the list of test names that passed in this run.
