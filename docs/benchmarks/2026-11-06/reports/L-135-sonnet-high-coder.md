[jobs:job-done] job 261106-131245-a878 BENCH L #135 sonnet-high succeeded · 37 turns · $0.68 · 6m · model sonnet · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain
- I did not run the full `go test ./...` or `scripts/check.sh`. I ran only the root, `cmd/agent-jobs` and the UI-check tests in `internal/jobs`.
- No real browser run. The Go tests use fake Playwright modules, so the Chromium path through `agent-jobs ui-check` is unexercised. This diff has no UI files, so the browser check does not apply to it.
- The job-dir copy `tools/ui-check.mjs` still exists for jobs that run it themselves. That is fine for them, but a reviewer job that runs it (`kind-review.md`) is still a job, not the orchestrator.
- `NODE_PATH` absolute entries and the global npm root are still trusted in trusted mode, because they come from the orchestrator's own environment. If that environment is considered attacker-influenced, only `--playwright` should be used.
- I did not check that `agent-jobs ui-check` is refused inside a job. It goes through the existing `refuseInsideJob` path, and I left that as is.
- The guard blocked inline Python, perl and here-doc forms. I made the edits with the Edit tool and passed the commit message with `-m`.

## Progress
- --:-- Read assets.go, prepare.go, the prompts, the skill and `scripts/ui-check.mjs`
- --:-- Created branch `bench/135-sonnet-high`
- --:-- Wrote failing Node tests (trusted resolution, new flags)
- --:-- Wrote failing Go tests and a stub (`uicheck_test.go`)
- --:-- Implemented `--trusted` and `--playwright` in `ui-check.mjs`; Node tests pass
- --:-- Implemented `runUICheck` and wired the `ui-check` command into `main.go`
- --:-- Updated `orchestrator.md`, the skill, GUIDE and CHANGELOG; added the prompt test and fixed `TestOrchestratorUIRule`
- --:-- Ran vet, gofmt and tests (all green); committed `8258c6f`

## Summary
Issue #135 had two parts. The orchestrator was told to run `<job dir>/tools/ui-check.mjs`, which the job can rewrite. `ui-check.mjs` also resolved Playwright from `node_modules` under the working directory. I added `agent-jobs ui-check`, which runs only the copy embedded in the binary, and a trusted resolution mode that never reads the working directory. The orchestrator prompt and skill no longer point at the job-dir copy. The new tests, the full root and `cmd/agent-jobs` packages, and `go vet` pass. Local commit `8258c6f` is on branch `bench/135-sonnet-high`; nothing was pushed.

## Findings / Changes
- **`cmd/agent-jobs/uicheck.go` (new):** `runUICheck` writes the embedded `scripts/ui-check.mjs` to a private `os.MkdirTemp` directory (0700, file 0600). It runs `node <tmp>/ui-check.mjs --trusted <args>` and passes the exit code through (0 pass, 1 fail, 2 not run). The temp directory is removed afterwards. A missing `node` gives `BROWSER CHECK NOT RUN: ...` and exit 2.
- **`cmd/agent-jobs/main.go`:** new `ui-check` command in the dispatch and the usage text. It stays refused inside a job, like the other commands.
- **`scripts/ui-check.mjs`:**
  - New flags `--trusted` and `--playwright <path>`.
  - `resolvePlaywright` now takes an options object. In trusted mode it skips the working-directory `node_modules` and ignores relative `NODE_PATH` entries. It runs `npm root -g` from `os.tmpdir()`, because npm reads `.npmrc` from its working directory.
  - `--playwright` accepts a Playwright package directory or a `node_modules` root and is then the only place looked at.
- **`prompts/orchestrator.md`, `skills/agent-jobs/SKILL.md`:** the instruction to run `node scripts/ui-check.mjs` or `<job dir>/tools/ui-check.mjs` is replaced by `agent-jobs ui-check`. Jobs and `kind-*.md` still use their own copy, which is intended.
- **`docs/GUIDE.md`, `CHANGELOG.md`:** documented the new command and the trust model.

## Evidence
- **Node tests (`scripts/ui-check.test.mjs`):**
  - New `resolvePlaywright: trusted mode ignores the working directory`: a planted `node_modules/playwright` is found untrusted, not found trusted (including with a relative `NODE_PATH`), and `explicit` wins.
  - New `parseArgs: --playwright and --trusted`.
  - Before the change both failed: `unknown option --playwright`, and the trusted case could not be expressed. After: `# pass 20, # fail 0`.
- **Go tests (`cmd/agent-jobs/uicheck_test.go`):**
  - `TestUICheckIgnoresPlantedToolAndPlaywright` plants `tools/ui-check.mjs` and `node_modules/playwright` in the worktree. Each writes a sentinel file if executed. The test expects exit 2 with `Playwright not found`, no sentinel from either, and a fake `npm` that did not run in the worktree.
  - `TestUICheckExplicitPlaywright` checks that `--playwright` loads the trusted module and not the planted one.
  - `TestUICheckTempDirPrivateAndRemoved` checks that the temp directory is gone after the run.
  - Against a stub `runUICheck` returning 99, all three failed (`exit 99, want 2` and similar). After: all pass.
- **Root tests:** `TestOrchestratorRunsEmbeddedUICheckOnly` (new) fails if the orchestrator prompt or skill mention `tools/ui-check.mjs` or `node scripts/ui-check.mjs`. The existing `TestOrchestratorUIRule` now asks for `agent-jobs ui-check` instead of `ui-check.mjs`; it failed once on the old string and passes now.
- **Commands and exit codes:**
  - `go vet ./...` exit 0, `gofmt -l .` empty.
  - `go test -count=1 . ./cmd/agent-jobs` exit 0 (`ok` for both).
  - `go test -count=1 -run UICheck ./internal/jobs` exit 0.
  - `node --test scripts/ui-check.test.mjs`: 20 pass, 0 fail.
