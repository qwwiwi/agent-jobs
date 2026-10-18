#!/usr/bin/env bash
# shellcheck disable=SC2317  # the step functions are called indirectly (run_step NAME FUNC)
# Release acceptance from a clean clone: clone, build, check, install as a new user would, start an
# isolated daemon and run one test job end to end. Prints a PASS/FAIL line per step.
#
#   scripts/acceptance-clean-clone.sh                    # HEAD of this checkout, cloned from origin
#   scripts/acceptance-clean-clone.sh --from . v0.4.0    # a tag, cloned from this checkout
#   scripts/acceptance-clean-clone.sh --real-model sonnet
#
# Isolation: everything lives under one mktemp dir (clone, binary, HOME, state, sockets, logs). The
# caller's checkout, ~/.agent-jobs, daemon, systemd units, tmux and config are never touched:
# AGENT_JOBS_HOME, HOME and the systemctl/tmux/claude binaries point into the temp dir, the agent name
# is a throwaway accept-<random>, the daemon is a foreground child killed on exit, and config.json
# pins cgroup_scope=off (no systemd scope, no scope probe). The Go build cache is shared.
#
# Test job: by default a stub stands in for claude (AGENT_JOBS_CLAUDE_BIN), so no model is called;
# --real-model M runs one real job instead (1 turn, budget 0.2 USD) and keeps the real HOME so claude
# finds its login (its session file goes to the real ~/.claude/projects).
#
# Inside a job the binary refuses every control command (no nested jobs). --inside-job builds the
# binary with the test hooks (-tags agentjobs_testhooks) and names the outer job's wrapper as outer,
# like the Go tests do; such a run is not a release acceptance and the summary says so.
set -euo pipefail

usage() {
  cat <<'USAGE'
usage: acceptance-clean-clone.sh [REF] [--from PATH|URL] [--no-tests] [--real-model MODEL]
                                 [--wait-minutes N] [--keep] [--inside-job]
  REF                git ref of this checkout to test (default HEAD); resolved here, checked out in the clone
  --from PATH|URL    clone from this repository (default: this checkout's origin URL)
  --no-tests         skip scripts/check.sh (gofmt, go vet, go test -count=1) in the clone
  --real-model MODEL the test job runs on the real claude with MODEL (1 turn, budget 0.2) instead of a stub
  --wait-minutes N   how long to wait for the test job (default 5, 10 with --real-model)
  --keep             keep the temp dir on success too (it is always kept on failure)
  --inside-job       run from inside an agent-jobs job: test-hooks build, outer wrapper ignored
USAGE
}

REF="HEAD"
FROM=""
NO_TESTS=0
REAL_MODEL=""
WAIT_MINUTES=""
KEEP=0
INSIDE_JOB=0
ref_set=0
while [ $# -gt 0 ]; do
  case "$1" in
    --from) [ $# -ge 2 ] || { usage >&2; exit 2; }; FROM="$2"; shift 2 ;;
    --from=*) FROM="${1#*=}"; shift ;;
    --no-tests) NO_TESTS=1; shift ;;
    --real-model) [ $# -ge 2 ] || { usage >&2; exit 2; }; REAL_MODEL="$2"; shift 2 ;;
    --real-model=*) REAL_MODEL="${1#*=}"; shift ;;
    --wait-minutes) [ $# -ge 2 ] || { usage >&2; exit 2; }; WAIT_MINUTES="$2"; shift 2 ;;
    --wait-minutes=*) WAIT_MINUTES="${1#*=}"; shift ;;
    --keep) KEEP=1; shift ;;
    --inside-job) INSIDE_JOB=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "acceptance: unknown argument: $1" >&2; usage >&2; exit 2 ;;
    *)
      if [ "$ref_set" = 1 ]; then echo "acceptance: at most one REF" >&2; usage >&2; exit 2; fi
      REF="$1"; ref_set=1; shift ;;
  esac
done
if [ -z "$WAIT_MINUTES" ]; then
  if [ -n "$REAL_MODEL" ]; then WAIT_MINUTES=10; else WAIT_MINUTES=5; fi
fi
if ! [[ "$WAIT_MINUTES" =~ ^[1-9][0-9]{0,2}$ ]]; then
  echo "acceptance: --wait-minutes wants 1..999" >&2
  exit 2
fi
if [ -n "$REAL_MODEL" ] && ! [[ "$REAL_MODEL" =~ ^[A-Za-z0-9._-]{1,64}$ ]]; then
  echo "acceptance: invalid --real-model '$REAL_MODEL'" >&2
  exit 2
fi

SRC_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if ! command -v go >/dev/null 2>&1 && [ -x "$HOME/.local/go/bin/go" ]; then
  export PATH="$HOME/.local/go/bin:$PATH"
fi
for tool in git go; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "acceptance: $tool not found" >&2
    exit 1
  fi
done

SHA="$(git -C "$SRC_REPO" rev-parse --verify --quiet "$REF^{commit}" || true)"
if [ -z "$SHA" ]; then
  echo "acceptance: ref '$REF' is not a commit in $SRC_REPO" >&2
  exit 2
fi
if [ -z "$FROM" ]; then
  FROM="$(git -C "$SRC_REPO" remote get-url origin 2>/dev/null || true)"
  if [ -z "$FROM" ]; then
    echo "acceptance: this checkout has no origin; pass --from PATH|URL" >&2
    exit 2
  fi
elif [ -d "$FROM" ]; then
  FROM="$(cd "$FROM" && pwd)"
fi

# is_wrapper_pid: the argv of an agent-jobs job wrapper (`run-wrapper <jobdir> --token <T>`, directly
# or behind an interpreter), as jobs.IsWrapperCmdline.
is_wrapper_pid() {
  local -a argv=()
  [ -r "/proc/$1/cmdline" ] || return 1
  mapfile -d '' -t argv <"/proc/$1/cmdline" 2>/dev/null || return 1
  { [ "${#argv[@]}" -gt 3 ] && [ "${argv[1]}" = run-wrapper ] && [ "${argv[3]}" = --token ]; } ||
    { [ "${#argv[@]}" -gt 4 ] && [ "${argv[2]}" = run-wrapper ] && [ "${argv[4]}" = --token ]; }
}

# stat_field PID N: field N (4 = ppid, 6 = session) of /proc/PID/stat; the comm may hold spaces.
stat_field() {
  local s
  s="$(cat "/proc/$1/stat" 2>/dev/null)" || return 1
  s="${s##*) }"
  local -a f
  read -r -a f <<<"$s"
  echo "${f[$(($2 - 3))]}"
}

# outer_wrappers prints the job wrappers this script runs under (ancestor or session leader), comma
# separated; empty outside a job or without /proc.
outer_wrappers() {
  [ -r "/proc/$$/stat" ] || return 0
  local pid=$$ found=() sid
  while [ "${pid:-0}" -gt 1 ]; do
    if is_wrapper_pid "$pid"; then found+=("$pid"); fi
    pid="$(stat_field "$pid" 4 || echo 0)"
  done
  sid="$(stat_field $$ 6 || echo 0)"
  if [ "${sid:-0}" -gt 1 ] && is_wrapper_pid "$sid" && [[ " ${found[*]-} " != *" $sid "* ]]; then found+=("$sid"); fi
  local IFS=,
  echo "${found[*]-}"
}

OUTER="$(outer_wrappers)"
if [ "$INSIDE_JOB" = 0 ] && { [ -n "${AGENT_JOBS_JOB:-}" ] || [ -n "$OUTER" ]; }; then
  echo "acceptance: running inside an agent-jobs job (marker '${AGENT_JOBS_JOB:-}', wrapper pids '$OUTER');" >&2
  echo "            the binary refuses control commands there;" >&2
  echo "            pass --inside-job for a test-hooks run, or run it from the orchestrator" >&2
  exit 2
fi
if [ "$INSIDE_JOB" = 1 ] && [ -z "${AGENT_JOBS_JOB:-}" ] && [ -z "$OUTER" ]; then
  echo "acceptance: --inside-job, but no job around this process; run without it" >&2
  exit 2
fi

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/agent-jobs-accept.XXXXXX")"
ROOT="$(cd "$ROOT" && pwd -P)"
AGENT="accept-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
CLONE="$ROOT/clone"
BIN="$ROOT/bin/agent-jobs"
SANDBOX_HOME="$ROOT/home"
STATE="$SANDBOX_HOME/.agent-jobs/$AGENT"
STUB="$ROOT/stub"
LOGS="$ROOT/logs"
SCRATCH="$ROOT/scratch"
# A unix socket path holds at most 104 bytes on macOS (108 on Linux); the daemon's sockets live in the
# state dir.
if [ "${#STATE}" -gt 88 ]; then
  rmdir "$ROOT"
  echo "acceptance: state dir $STATE is too long for its unix sockets; use a shorter TMPDIR" >&2
  exit 2
fi
mkdir -p "$ROOT/bin" "$SANDBOX_HOME" "$STUB" "$LOGS"

STEP_NAMES=()
STEP_CODES=()
DAEMON_PID=""
FAILED=0
FINISHED=0

record() { STEP_NAMES+=("$1"); STEP_CODES+=("$2"); }

stop_daemon() {
  # A job left running after a failure runs in its own session and would outlive the daemon.
  if [ "$FAILED" = 1 ] && [ -s "$ROOT/job.id" ]; then
    "$BIN" cancel --agent "$AGENT" "$(cat "$ROOT/job.id")" >>"$LOGS/cancel.log" 2>&1 || true
  fi
  if [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill -TERM "$DAEMON_PID" 2>/dev/null || true
    local i
    for i in $(seq 30); do
      kill -0 "$DAEMON_PID" 2>/dev/null || break
      sleep 0.5
    done
    if kill -0 "$DAEMON_PID" 2>/dev/null; then kill -KILL "$DAEMON_PID" 2>/dev/null || true; fi
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  DAEMON_PID=""
}

finish() {
  FINISHED=1
  stop_daemon
  echo
  echo "== acceptance summary: $REF ($SHA) from $FROM, agent $AGENT"
  local i word
  for i in "${!STEP_NAMES[@]}"; do
    case "${STEP_CODES[$i]}" in
      0) word=PASS ;;
      skip) word=SKIP ;;
      *) word=FAIL ;;
    esac
    printf '%-4s  %-10s  exit %s\n' "$word" "${STEP_NAMES[$i]}" "${STEP_CODES[$i]}"
  done
  if [ "$INSIDE_JOB" = 1 ]; then
    echo "NOTE  test-hooks build (--inside-job): not a release acceptance"
  fi
  if [ "$FAILED" = 0 ]; then
    echo "RESULT: PASS"
  else
    echo "RESULT: FAIL"
  fi
  if [ "$FAILED" = 0 ] && [ "$KEEP" = 0 ]; then
    # Go marks module files read-only; make the tree writable before removing it.
    chmod -R u+w "$ROOT" 2>/dev/null || true
    rm -rf -- "$ROOT"
    echo "temp dir removed"
  else
    echo "temp dir kept: $ROOT (logs in $LOGS)"
  fi
  if [ "$FAILED" = 0 ]; then exit 0; fi
  exit 1
}

on_exit() {
  local rc=$?
  if [ "$FINISHED" = 0 ]; then
    FAILED=1
    stop_daemon
    echo "acceptance: interrupted (exit $rc); temp dir kept: $ROOT" >&2
  fi
}
trap on_exit EXIT
trap 'exit 130' INT TERM

# run_step NAME FUNC: runs FUNC in a subshell with errexit (backgrounded and waited for, so errexit is
# not switched off by a `||` around it), output to logs/NN-NAME.log; a failure ends the run.
run_step() {
  local name="$1" fn="$2" log rc=0
  log="$LOGS/$(printf '%02d' "${#STEP_NAMES[@]}")-$name.log"
  echo "== $name"
  (set -e; "$fn") >"$log" 2>&1 </dev/null &
  if wait "$!"; then rc=0; else rc=$?; fi
  record "$name" "$rc"
  if [ "$rc" != 0 ]; then
    FAILED=1
    echo "   FAIL (exit $rc), log $log:"
    tail -n 25 "$log" | sed 's/^/   | /'
    finish
  fi
  echo "   ok"
}

skip_step() {
  echo "== $1 (skipped: $2)"
  record "$1" skip
}

# ---- steps ----

step_clone() {
  echo "clone $FROM -> $CLONE, checkout $SHA"
  git clone --quiet --no-local --no-checkout -- "$FROM" "$CLONE"
  if ! git -C "$CLONE" cat-file -e "$SHA^{commit}" 2>/dev/null; then
    git -C "$CLONE" fetch --quiet origin "$SHA" ||
      { echo "commit $SHA ($REF) is not in $FROM: push it, or pass --from with a repository that has it"; exit 1; }
  fi
  git -C "$CLONE" -c advice.detachedHead=false checkout --quiet --detach "$SHA"
  git -C "$CLONE" log -1 --format='%H %s'
  test -z "$(git -C "$CLONE" status --porcelain)"
}

step_build() {
  local version tags=()
  version="$(git -C "$CLONE" describe --tags --always 2>/dev/null || echo dev)-accept"
  if [ "$INSIDE_JOB" = 1 ]; then
    tags=(-tags agentjobs_testhooks)
    version="$version+testhooks"
  fi
  cd "$CLONE"
  # As install-linux.sh: no go env file and no GOFLAGS, so no stray build tags.
  GOENV=off GOFLAGS='' go build "${tags[@]}" -trimpath -ldflags "-X main.version=$version" -o "$BIN" ./cmd/agent-jobs
  # Captured, not piped into grep -q: with pipefail an early grep exit is a SIGPIPE failure (141).
  local out
  out="$("$BIN" version)"
  echo "$out"
  grep -qF -- "$version" <<<"$out"
}

step_tests() {
  cd "$CLONE"
  scripts/check.sh
}

write_stubs() {
  # systemctl: logs, nothing is active; the installer never reaches the real user manager.
  cat >"$STUB/systemctl" <<'EOF'
#!/bin/sh
echo "$*" >> "$(dirname "$0")/systemctl.log"
case "$*" in *is-active*) exit 3 ;; esac
exit 0
EOF
  # tmux: logs; nothing is typed into a real session.
  cat >"$STUB/tmux" <<'EOF'
#!/bin/sh
echo "$*" >> "$(dirname "$0")/tmux.log"
exit 0
EOF
  # claude: the dry job's runner, no model and no network. Commits one file in the job's worktree
  # (the cwd) and prints a stream-json result carrying the report.
  cat >"$STUB/claude" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cat > /dev/null
printf '%s\n' "$@" > ../stub-argv.txt
echo '{"type":"system","subtype":"init"}'
echo accepted > accept.txt
git add accept.txt
git -c user.name=accept -c user.email=accept@example.invalid commit -qm "Acceptance stub commit"
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0,"result":"## Summary\nACCEPT-DRY-JOB-OK\n\n## Findings / Changes\n- accept.txt\n\n## Evidence\n- stub runner\n\n## Not done / uncertain\nnothing\n\n## Progress\n- --:-- UTC stub run"}'
EOF
  chmod 0755 "$STUB/systemctl" "$STUB/tmux" "$STUB/claude"
}

step_install() {
  write_stubs
  # cgroup_scope=off before init: no systemd scope per job and no scope probe (init --pin-scope
  # leaves a set value alone). The read-only HTTP API is on, on the state dir's unix socket.
  mkdir -p "$STATE"
  chmod 0700 "$SANDBOX_HOME/.agent-jobs" "$STATE"
  printf '%s\n' '{"cgroup_scope": "off", "daily_off": true, "http_api": {"enabled": true}}' >"$STATE/config.json"
  chmod 0600 "$STATE/config.json"
  if [ "$(uname -s)" = Linux ]; then
    # README: scripts/install-linux.sh --instance NAME (a prebuilt binary: the one built above).
    HOME="$SANDBOX_HOME" AGENT_JOBS_SYSTEMCTL="$STUB/systemctl" \
      "$CLONE/scripts/install-linux.sh" --instance "$AGENT" --binary "$BIN"
    test -x "$SANDBOX_HOME/.local/bin/agent-jobs"
    test -f "$SANDBOX_HOME/.config/systemd/user/agent-jobs@.service"
    test -f "$SANDBOX_HOME/.claude/skills/agent-jobs/SKILL.md"
    grep -q "enable agent-jobs@$AGENT.service" "$STUB/systemctl.log"
  else
    # macOS: README's init (the launchd plist is not loaded here).
    "$BIN" init --agent "$AGENT"
  fi
  test -f "$STATE/owner-rules.md"
  test -f "$STATE/orchestrator.md"
  grep -q '"cgroup_scope": "off"' "$STATE/config.json"
  "$BIN" http-api token --agent "$AGENT" --rotate >/dev/null
  test -f "$STATE/http-api.token"
}

start_daemon() {
  "$BIN" daemon --agent "$AGENT" >"$LOGS/daemon.log" 2>&1 </dev/null &
  DAEMON_PID=$!
}

step_daemon() {
  local i
  for i in $(seq 40); do
    if grep -q 'daemon started' "$LOGS/daemon.log" && [ -S "$STATE/http-api.sock" ]; then break; fi
    kill -0 "$DAEMON_PID"
    sleep 0.25
  done
  kill -0 "$DAEMON_PID"
  grep 'daemon started' "$LOGS/daemon.log"
  test -S "$STATE/http-api.sock"
}

step_status() {
  local out
  out="$("$BIN" status --agent "$AGENT")"
  echo "$out"
  grep -q "^http api:  " <<<"$out"
  "$BIN" doctor procs --agent "$AGENT"
  "$BIN" list --agent "$AGENT"
  if command -v curl >/dev/null 2>&1; then
    local hdr="$ROOT/http-api.header"
    (umask 077; printf 'Authorization: Bearer %s\n' "$(cat "$STATE/http-api.token")" >"$hdr")
    curl -sS --fail --max-time 10 --unix-socket "$STATE/http-api.sock" -H "@$hdr" http://localhost/v1/health
    echo
    rm -f "$hdr"
  else
    echo "curl not found: /v1/health not checked"
  fi
}

step_job() {
  mkdir -p "$SCRATCH"
  git -C "$SCRATCH" init --quiet
  echo scratch >"$SCRATCH/README.md"
  git -C "$SCRATCH" add README.md
  git -C "$SCRATCH" -c user.name=accept -c user.email=accept@example.invalid commit -qm "Scratch repo"
  local brief="$ROOT/brief.md" id st i
  if [ -n "$REAL_MODEL" ]; then
    cat >"$brief" <<'EOF'
Acceptance test job. Do not use any tool. Reply with exactly this text and nothing else:

## Summary
ACCEPT-REAL-JOB-OK

## Findings / Changes
nothing

## Evidence
nothing

## Not done / uncertain
nothing

## Progress
- --:-- UTC replied
EOF
    id="$("$BIN" start --agent "$AGENT" --title "acceptance real job" --kind general --repo "$SCRATCH" \
      --model "$REAL_MODEL" --max-turns 1 --max-budget-usd 0.2 --timeout-minutes "$WAIT_MINUTES" --brief-file "$brief")"
  else
    echo "Acceptance dry job: a stub stands in for claude." >"$brief"
    id="$("$BIN" start --agent "$AGENT" --title "acceptance dry job" --kind general --repo "$SCRATCH" \
      --model sonnet --brief-file "$brief")"
  fi
  echo "job $id"
  echo "$id" >"$ROOT/job.id"
  # The daemon launches it on its next tick (once a minute) and collects it on the one after.
  st=""
  for i in $(seq $((WAIT_MINUTES * 12))); do
    st="$("$BIN" status --agent "$AGENT" "$id" | sed -n 's/^status: *//p')"
    case "$st" in succeeded|failed|timeout|cancelled|stopped) break ;; esac
    sleep 5
  done
  "$BIN" status --agent "$AGENT" "$id"
  echo "final status: $st"
  test "$st" = succeeded
  test -s "$STATE/jobs/$id/report.md"
  "$BIN" result --agent "$AGENT" "$id" | tee "$ROOT/result.txt"
  grep -q '^## Summary' "$ROOT/result.txt"
  if [ -n "$REAL_MODEL" ]; then
    grep -q 'ACCEPT-REAL-JOB-OK' "$ROOT/result.txt" || echo "note: the model did not echo the marker"
  else
    grep -q 'ACCEPT-DRY-JOB-OK' "$ROOT/result.txt"
    grep -qx -- '--max-turns' "$STATE/jobs/$id/stub-argv.txt"
  fi
  "$BIN" doctor procs --agent "$AGENT"
  kill -0 "$DAEMON_PID"
}

# ---- run ----

echo "acceptance: $REF ($SHA) from $FROM"
echo "temp dir: $ROOT"
run_step clone step_clone
run_step build step_build
if [ "$NO_TESTS" = 1 ]; then skip_step tests --no-tests; else run_step tests step_tests; fi

# From here on: the isolated environment (the caller's agent-jobs, git, XDG and claude settings are
# dropped; only the temp dir counts).
while IFS= read -r v; do unset "$v"; done < <(compgen -e | grep -E '^(AGENT_JOBS_|GIT_)' || true)
unset XDG_CONFIG_HOME
export AGENT_JOBS_HOME="$SANDBOX_HOME/.agent-jobs"
export AGENT_JOBS_TMUX_BIN="$STUB/tmux"
export AGENT_JOBS_CGROUP_SCOPE=off
if [ -z "$REAL_MODEL" ]; then
  export HOME="$SANDBOX_HOME"
  unset CLAUDE_CONFIG_DIR
  export AGENT_JOBS_CLAUDE_BIN="$STUB/claude"
fi
if [ "$INSIDE_JOB" = 1 ] && [ -n "$OUTER" ]; then
  export AGENT_JOBS_TEST_OUTER_WRAPPER="$OUTER"
fi

run_step install step_install
start_daemon
# The daemon is a child of this shell (the exit trap stops it); step_daemon only checks it.
run_step daemon step_daemon
run_step status step_status
if [ -n "$REAL_MODEL" ]; then run_step real-job step_job; else run_step dry-job step_job; fi
finish
