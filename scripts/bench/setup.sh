#!/usr/bin/env bash
# Prepare an isolated benchmark root: agent-jobs built from this checkout, a separate state dir
# (AGENT_JOBS_HOME) with the agent "bench", fixture git repos, the parent's padding and the MCP config,
# then start that agent's daemon in the background. Touches no other agent, unit or launchd job.
# Usage: scripts/bench/setup.sh BENCH_DIR   (then: BENCH=... AGENT_JOBS_HOME=BENCH_DIR/home scripts/bench/bench.sh)
set -euo pipefail
BENCH="${1:?usage: setup.sh BENCH_DIR}"
SRC="$(cd "$(dirname "$0")/../.." && pwd)"
HERE="$SRC/scripts/bench"
mkdir -p "$BENCH/bin" "$BENCH/home" "$BENCH/fixtures" "$BENCH/scripts"
echo "build agent-jobs"
(cd "$SRC" && go build -o "$BENCH/bin/agent-jobs" ./cmd/agent-jobs)
export AGENT_JOBS_HOME="$BENCH/home"
[ -d "$AGENT_JOBS_HOME/bench" ] || "$BENCH/bin/agent-jobs" init --agent bench
cat > "$AGENT_JOBS_HOME/bench/config.json" <<JSON
{ "git_name": "Bench Job", "git_email": "bench@example.com", "cgroup_scope": "off", "daily_off": true }
JSON
cp "$HERE"/*.py "$HERE"/*.sh "$BENCH/scripts/"
rm -rf "$BENCH/hidden" && cp -R "$HERE/hidden" "$BENCH/hidden"
for t in t1-fix t2-leads t3-report; do
  echo "fixture $t"
  rm -rf "${BENCH:?}/fixtures/$t"
  cp -R "$HERE/fixtures/$t" "$BENCH/fixtures/$t"
done
mkdir -p "$BENCH/fixtures/t3-report/sources"
cp "$SRC/README.md" "$BENCH/fixtures/t3-report/sources/README.md"
cp "$SRC/docs/ARCHITECTURE.md" "$BENCH/fixtures/t3-report/sources/ARCHITECTURE.md"
for t in t1-fix t2-leads t3-report; do
  git -C "$BENCH/fixtures/$t" init -q
  git -C "$BENCH/fixtures/$t" add -A
  git -C "$BENCH/fixtures/$t" -c user.name=Bench -c user.email=bench@example.com commit -qm "fixture"
done
# ~28k tokens of unrelated earlier-session context for every parent
head -c 100000 "$SRC/internal/guard/guard.go" > "$BENCH/padding.txt"
cat > "$BENCH/mcp.json" <<JSON
{"mcpServers":{"agent-jobs":{"command":"$BENCH/bin/agent-jobs","args":["mcp","--role","orchestrator","--agent","bench"],"env":{"AGENT_JOBS_HOME":"$AGENT_JOBS_HOME"}}}}
JSON
echo "start daemon (log: $BENCH/daemon.out)"
nohup "$BENCH/bin/agent-jobs" daemon --agent bench > "$BENCH/daemon.out" 2>&1 &
echo "ready: BENCH=$BENCH AGENT_JOBS_HOME=$AGENT_JOBS_HOME $HERE/bench.sh"
