# Issue #145: Hardening: test gaps from #124 and #125 reviews

Non-blocking Low findings from the delta reviews of PR #143 and PR #144:

1. #124 (Astra, internal/jobs/resume_skills_test.go:181): add direct launch-time rejection coverage for an already-queued legacy job (skills delivered, no manifest).
2. #125 (sol, internal/mcp/source_files_test.go:74): add `//` (double slash) regression cases for non-canonical source paths.

Behaviour is already correct (Clean(p) != p rejects `//`; legacy rows fail closed); only tests are missing.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

---
## Task (benchmark run, model tag: opus-high)
Implement the fix for the GitHub issue above in this repository (Go). Work only on what the issue asks.
- Read the code the issue points at before changing anything.
- Make exactly one tool call per step.
- Add or update tests that prove the fix; run `go test` for every package you touched and `go vet ./...`; paste the real exit codes in Evidence.
- Commit on a new branch `bench/145-opus-high` with a clear English commit message. Do not push, do not open a PR, do not message anyone.
- If part of the issue needs an owner decision or is out of reach, do the rest and list it under "Not done / uncertain".
