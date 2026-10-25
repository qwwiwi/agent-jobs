# agent-jobs <<PLACEHOLDER: next version>> (draft)

Draft notes for the next release. The full list of changes is in [CHANGELOG.md](../../CHANGELOG.md).

## What changed: Wave F gates cut back (owner decision 2026-10-25)

The heavy Wave F gates were barely exercised (0 judge verdicts, 0 release passes, 3 approvals and 3 slicing runs ever) and `release_check` could not pass for Claude coders at all. They are cut back; each change has a dated row in [DECISIONS.md](../DECISIONS.md).

- **`release_check` can pass for Claude coders.** A Codex judge launched under the read-only Codex sandbox counts as blind by enforcement (that sandbox blocks writes and network, not reads), and the `family` condition counts only succeeded feature jobs, so a failed, cancelled or small coder no longer blocks a judge of its family. The `trace` condition is gone; `covered_by` stays as information.
- **Waivers survive fix commits.** An owner waiver binds to the checklist hash and the requirement, not to a commit. `checklist_set` still accepts a waiver's `sha` and ignores it with a warning.
- **The merge gate warns instead of refusing an unreadable command.** A command whose `gh pr merge` form or target the reader cannot tell runs with one warning line; a parsed `gh pr merge` in a gated repository without a release pass is still blocked.
- **Requirement lines count as written.** A `done` line no longer needs `ran:` or a `REDPROOF` line; the job prompt's rules are five lines.
- **Size is optional.** `covers` imply `large`, otherwise `small`; `size_required` stays as a knob, default off.
- **Quieter brief reminder.** It speaks only before an owner prompt and only while a checklist decision is pending.
- **Slicing is advisory.** `checklist_set approve` no longer needs a slicing result; one that ran still needs the owner's decision on each unmapped quote.

## Upgrade notes

Rebuild and install the binary (README «Build and check»), then start a new session so that the MCP server is the new one. No config, store schema or hook registration changes. Waivers stored before keep their sha, which is no longer read.
