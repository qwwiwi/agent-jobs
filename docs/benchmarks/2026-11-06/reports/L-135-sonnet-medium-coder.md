[jobs:job-done] job 261106-131246-cb88 BENCH L #135 sonnet-medium succeeded · 20 turns · $0.34 · 13m · model sonnet · effort medium · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- `go test ./internal/jobs` failed with a 607s timeout panic in `TestMergeGateShippedOnlyValidLatest` (`mergegate_shipped_test.go:381`). I did not touch that package. I did not re-run it on the base commit, so I can't say whether it fails there too.
- The other packages in that run (`mcp`, `skills`, `store`, `testutil`, `trash`) passed. `go test ./cmd/...` showed only `cmd/agent-jobs` in the tail I read, so I ran that one explicitly.
- Jobs still get their own copy at `<job dir>/tools/ui-check.mjs`; the issue only concerns the orchestrator. The job prompts (`kind-fix.md` etc.) still point jobs at that copy. A job running it in its own worktree can still pick up Playwright from the cwd, because it is not run with `--trusted`.
- I did not run a real browser check. Playwright and Chromium were not used, and the UI check was not exercised end to end.
- I did not add a CHANGELOG entry or update the docs for the new command.

## Progress
- --:-- Read the issue, `ui-check.mjs`, the orchestrator prompt and the command dispatch in `main.go`
- --:-- Wrote the failing Go tests (build failed: `runUICheck` undefined)
- --:-- Added `--playwright`/`--trusted` to the script and the `ui-check` command; edited `orchestrator.md`
- --:-- Tests pass; the mutation without `--trusted` fails as expected
- --:-- Ran `go vet`, the node tests and the root and `cmd` packages; `internal/jobs` timed out
- --:-- Committed `b3e91ed` on `bench/135-sonnet-medium`

## Summary
Issue #135 had two problems. The orchestrator was told to run the job-dir copy of `ui-check.mjs`, and the script resolved Playwright from the cwd's `node_modules`. I added `agent-jobs ui-check`, which runs only the script embedded in the binary. The script now has `--playwright <path>` and `--trusted`, and the orchestrator prompt no longer points at the job-dir copy. The new tests pass, but `go test ./internal/jobs` fails (details under Not done). Commit `b3e91ed` on `bench/135-sonnet-medium`.

## Findings / Changes
- `cmd/agent-jobs/uicheck.go` (new):
  - `runUICheck` writes the embedded `scripts/ui-check.mjs` to a private `os.MkdirTemp` dir (0700, removed afterwards).
  - It runs `node <tmp>/ui-check.mjs --trusted <args>`, with `NODE_OPTIONS` and `NODE_PATH` removed from the environment.
  - It returns the script's exit code.
- `cmd/agent-jobs/main.go`: the `ui-check` command and its usage text. It stays refused inside a job, like the other commands.
- `scripts/ui-check.mjs`:
  - New `--playwright <path>` (a module file or package directory) is the only lookup when given.
  - New `--trusted` skips the cwd and `NODE_PATH` and uses only the global npm root.
  - `resolvePlaywright` takes a 4th parameter `{trusted, playwright}`; its old behaviour is unchanged when neither is set.
- `prompts/orchestrator.md:40`: the orchestrator runs `agent-jobs ui-check ...`, and the sentence about `<job dir>/tools/ui-check.mjs` is gone. It now says never to run a job-dir or checkout copy.
- `cmd/agent-jobs/uicheck_test.go` (new): 4 tests, listed under Evidence.

## Evidence
- Before the change, `go test ./cmd/agent-jobs -run UICheck` failed to build with `undefined: runUICheck`.
- `TestUICheckIgnoresPlantedPlaywrightInCwd`:
  - It plants `node_modules/playwright` in the cwd and sets `NODE_PATH` to it.
  - Passes with `--trusted`.
  - With `--trusted` temporarily removed it failed with "planted code was executed ... cannot launch Chromium: planted". That shows the test detects the bug.
- `TestUICheckDoesNotRunJobDirCopy`: with a planted `tools/ui-check.mjs` in the cwd, the embedded usage text is printed and the marker file is never written.
- `TestUICheckPlaywrightFlag`: `--playwright` uses the given module (exit 2, "trusted fake").
- `TestUICheckTempDirRemoved`: no temp dir is left behind.
- Exit codes:
  - `go test . ./cmd/agent-jobs` returned 0, `go vet ./...` returned 0, and `node --test scripts/ui-check.test.mjs` passed 18 of 18.
  - `go test . ./cmd/... ./internal/...` returned 1 because `internal/jobs` failed (see below).
