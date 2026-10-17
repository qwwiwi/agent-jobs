#!/usr/bin/env bash
# PreToolUse hook for the main session (matcher "Agent|Task").
# It never blocks: it only adds a reminder to prefer a background job for long work.
# See docs/WORKFLOW.md, "Optional hook".
set -euo pipefail

# The hook input (JSON on stdin) is not needed; read and discard it.
cat >/dev/null

cat <<'EOF'
{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"Reminder (agent-jobs): a subagent is for short work whose result is needed inside this turn. Coding, fix loops, reviews, research and anything long or overnight belong in a job (job_start with an explicit model or complexity). When unsure, use a job."}}
EOF
exit 0
