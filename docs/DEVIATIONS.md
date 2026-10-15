# Approved deviations from REFERENCE-danya.md

Only these. Anything else must match the reference 1:1. New deviation = prince's explicit OK, recorded here.

| # | Reference | Ours | Why / approved |
|---|---|---|---|
| 1 | Heartbeat: Go check → Haiku decides | Go check → Sonnet decides | Haiku is not used in the fleet (prince 2026-07-14); Sonnet approved 2026-10-15 |
| 2 | Memory tools backed by own store | Same tool names (memory_search / memory_write / memory_already_exists) backed by gbrain (recall / create_*_note) | One shared brain for all agents, no split-brain; approved 2026-10-15 |
| 3 | Tasks / team tools backed by own store | Same tool names backed by gbrain tasks / swarm | same reason |
| 4 | Owner notify via own Telegram | notify_owner goes through the agent's existing Telegram channel (bot isolation: one bot per agent) | fleet hard rule |
| 5 | Only `claude -p` runner | + `codex exec` runner (review on gpt-6.1-sol high, architecture on gpt-6-astra) | prince asked 2026-10-15 |
| 6 | Model chosen by brief | Model chosen by kind × complexity (light/normal/hard) | prince asked 2026-10-15 |
| 7 | Ref namespace `refs/praxis/jobs/<id>`, tag `[praxis:...]` | `refs/agent-jobs/jobs/<id>`, tag `[jobs:...]` | naming only |
| 8 | — | Read-only HTTP API for a future «Джобы» tab in Dashi Stats | prince asked 2026-10-15 (Agent Native) |

## Conformance check (prince 2026-10-15: «все сверки делай»)
After every stage, before merge: Codex `gpt-6.1-sol` high walks REFERENCE-danya.md line by line → for each claim: implemented? where (file:line)? 1:1 / deviation. Any deviation not in the table above = blocker, no merge. Result table goes to the prince.
