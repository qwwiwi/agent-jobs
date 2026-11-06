[jobs:job-done] job 261106-171500-b695 BENCH-REVIEW L #135 sonnet-medium succeeded · 13 turns · cost unknown · 2m · model gpt-6.1-sol · effort high

## Not done / uncertain

- New Go integration tests and `scripts/check.sh` were not run: they require writable build/test directories; the planted-Playwright test can also launch a browser and access loopback.
- No browser or screenshots: prohibited by this sandbox. No product UI changed, so the screenshot matrix is inapplicable.
- No files changed, commits created, or results published.

## Progress

- 17:41 UTC Identified risky areas and reviewed the requested diff.
- 17:42 UTC Traced trusted resolution and verified the retained unsafe skill instruction.
- 17:43 UTC Confirmed skill installation, completed static checks, and finalized the finding.

BENCH-VERDICT: critical=0 high=1 medium=0 low=0 issue_fixed=partly

## Summary
Verdict: BLOCK
Risky areas: guard/sandbox boundaries and execution with orchestrator privileges.
Reviewed `8f6e8f9..b3e91ed`. The trusted command is implemented, but shipped orchestrator guidance still exposes the original execution path.

## Findings / Changes

### Delivered

1. **Embedded script only — partly met; open.** `cmd/agent-jobs/uicheck.go:23` loads the embedded script into a private temporary directory. `prompts/orchestrator.md:40` switches to that command, but the installed skill retains the unsafe instruction.
2. **Trusted Playwright resolution — met; closed for the new command.** `scripts/ui-check.mjs:589` bypasses cwd and `NODE_PATH` lookup in trusted mode; `cmd/agent-jobs/uicheck.go:39` forces that mode. Explicit `--playwright` resolution is implemented at `scripts/ui-check.mjs:569`.
3. **Planted-file regression tests — implemented.** `cmd/agent-jobs/uicheck_test.go:36` and `:52` cover planted Playwright and script copies. They do not cover consistency of orchestrator instructions.

### Design

The embedded-command design is followed. The requirement that the orchestrator use **only** that command remains unmet because its shipped instructions conflict.

### Code

**High — original job-controlled execution condition remains open.**  
`prompts/orchestrator.md:40` changes only one instruction source. `skills/agent-jobs/SKILL.md:130` still tells the orchestrator to execute `node <job dir>/tools/ui-check.mjs` itself. Following that instruction executes a job-replaced script with orchestrator privileges and also bypasses trusted Playwright resolution.

**Verification:** inspected the retained instruction and confirmed that `scripts/install-linux.sh:79` selects this skill and `:286` installs its contents. `assets.go:9` also embeds it. This is a verified incomplete closure of the original HIGH condition, not a separate vulnerability.

**Minimal fix:** update the skill to require `agent-jobs ui-check`; extend the instruction regression test in `ui_check_test.go:13` to require the trusted command and reject direct job-directory execution in both orchestrator instruction sources. The brief explicitly required embedded-only orchestrator execution.

### Blocking

- `prompts/orchestrator.md:40` — close the remaining unsafe orchestrator route in `skills/agent-jobs/SKILL.md:130` and add an instruction-consistency regression check.

### Non-blocking

none

## Evidence

- Reviewed the complete five-file diff for commit `b3e91ed922d993e9a80f281198d0c54386b52958`.
- `node --test scripts/ui-check.test.mjs` — passed.
- `node --check scripts/ui-check.mjs` — passed.
- `gofmt -l` on changed Go files — no output.
- `git diff --check 8f6e8f9..HEAD` — passed.
- Final `git status --short` — clean.
- Repository instruction text was inspected as data, not followed.
