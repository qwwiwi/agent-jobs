---
remote: github.com/qwwiwi/agent-jobs
---
# Project: agent-jobs

## Stack
- Go 1.27, a single static binary (`cmd/agent-jobs`). Packages: `internal/store` (SQLite via modernc.org/sqlite, no cgo), `internal/jobs` (lifecycle, wrapper, cleanup), `internal/guard` (PreToolUse hook), `internal/mcp` (MCP server, go-sdk).
- Prompts and rules templates are embedded from `prompts/`, `rules/`, `cards/`, `skills/` (`assets.go`).

## Check runner
- `bash scripts/check.sh`: gofmt, go vet, go test (a few minutes; the jobs tests build the binary and run fake claude jobs).
- No CI: the same script runs from `.githooks/pre-push`.

## Do not touch
- `docs/REFERENCE.md` and `docs/REFERENCE.ru.md`: the reference design is the source of truth, not edited by jobs.
- `docs/DEVIATIONS.md` and `docs/DECISIONS.md` rows need the owner's OK; a job may propose a row in its report, not add one.

## Known traps
- Tests run git in temp repos: under a git hook `GIT_DIR` and friends leak in; `scripts/check.sh` and `TestMain` drop every `GIT_*` variable first. Keep it that way.
- Everything in the repo is public and in English (`*.ru.md` files carry the Russian); never write personal names, emails or home paths.
- A guard rule needs a test case that blocks and one that passes; check the hook as written into settings.json, not only the Go function.
