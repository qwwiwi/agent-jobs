# Issue #134: Serialized gate runner: head assert, exit marker, process-group kill, clean env

Heavy local gates (check.sh, pg suites, vite builds) caused OOM, orphan suites, a gate on the wrong HEAD and hung waiters (about 3-4 h in two weeks). One gate at a time per box, assert HEAD, always print exit=, kill the process group on timeout, drop TELEGRAM_* from env.

Source: speed and autonomy audit 2026-11-04.

---
## Task (benchmark run, model tag: sonnet-high)
Implement the fix for the GitHub issue above in this repository (Go). Work only on what the issue asks.
- Read the code the issue points at before changing anything.
- Make exactly one tool call per step.
- Add or update tests that prove the fix; run `go test` for every package you touched and `go vet ./...`; paste the real exit codes in Evidence.
- Commit on a new branch `bench/134-sonnet-high` with a clear English commit message. Do not push, do not open a PR, do not message anyone.
- If part of the issue needs an owner decision or is out of reach, do the rest and list it under "Not done / uncertain".
