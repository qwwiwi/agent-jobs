# Issue #146: OpenRouter: file sources bypass providers.openrouter.deny_repos

Found while merging main (#144 sources) into the OpenRouter runner (#136).

`providers.openrouter.deny_repos` checks only the job's repository. With `job_start sources`, a file from a denied repository (or another job's report about a private repo) can be attached to an OpenRouter job and so reach a third-party provider.

Proposal: refuse `sources` for jobs whose runner is a third-party provider unless every source path is outside the denied repos and `job:` sources belong to jobs on allowed repos; or simpler, refuse sources on OpenRouter jobs until a policy exists.

Pilot workaround (owner chose the $20-capped pilot): OpenRouter jobs are started without sources, public repos only.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

---
## Task (benchmark run, model tag: haiku-high)
Implement the fix for the GitHub issue above in this repository (Go). Work only on what the issue asks.
- Read the code the issue points at before changing anything.
- Make exactly one tool call per step.
- Add or update tests that prove the fix; run `go test` for every package you touched and `go vet ./...`; paste the real exit codes in Evidence.
- Commit on a new branch `bench/146-haiku-high` with a clear English commit message. Do not push, do not open a PR, do not message anyone.
- If part of the issue needs an owner decision or is out of reach, do the rest and list it under "Not done / uncertain".
