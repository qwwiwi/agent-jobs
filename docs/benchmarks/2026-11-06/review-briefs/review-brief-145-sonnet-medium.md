# Issue #145: Hardening: test gaps from #124 and #125 reviews

Non-blocking Low findings from the delta reviews of PR #143 and PR #144:

1. #124 (Astra, internal/jobs/resume_skills_test.go:181): add direct launch-time rejection coverage for an already-queued legacy job (skills delivered, no manifest).
2. #125 (sol, internal/mcp/source_files_test.go:74): add `//` (double slash) regression cases for non-canonical source paths.

Behaviour is already correct (Clean(p) != p rejects `//`; legacy rows fail closed); only tests are missing.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

---
## Review task (benchmark)
Review the change on branch `bench/145-sonnet-medium` (diff `8f6e8f9e5969b205fa9e400b66d0728ba39602cd..HEAD` in this worktree) as the fix for the issue above.
Judge correctness, whether the issue is fully fixed, regressions, test quality. Do not modify files.
End the report with exactly one line in this form, counts of findings by severity:
BENCH-VERDICT: critical=<n> high=<n> medium=<n> low=<n> issue_fixed=<yes|partly|no>
