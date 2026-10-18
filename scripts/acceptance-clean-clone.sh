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
# caller's checkout, ~/.agent-jobs, daemon, systemd units, tmux, skills and config are never touched:
# - before the first git command every GIT_* variable but the transport ones (ssh, askpass, TLS
#   CA, proxy) is dropped, so GIT_DIR, GIT_WORK_TREE or GIT_INDEX_FILE never redirect a git command;
# - after the clone and before any of its code is built or run, HOME is the temp dir's, the
#   caller's AGENT_JOBS_*, GIT_*, XDG_{CONFIG,DATA,CACHE,STATE}_HOME and CLAUDE_CONFIG_DIR are gone,
#   AGENT_JOBS_HOME and the systemctl/tmux/claude binaries point into the temp dir, and the installer
#   gets an explicit --skills-dir in the temp dir; the Go caches stay the caller's (pinned first);
# - the agent name is a throwaway accept-<random>, config.json pins cgroup_scope=off (no systemd
#   scope, no scope probe);
# - every step, the daemon and each teardown command run under a supervisor, the leader of a process
#   group of its own: the script only writes `stop` to the supervisor's FIFO and waits for it, and the
#   supervisor signals only its own group (kill 0, as a member), so no PID or group id is signalled
#   after it could have been reused; on exit, failure or INT/TERM the active step is stopped first,
#   then the sandbox's queued/running jobs are cancelled (each list/cancel with a 10 s deadline) and
#   the daemon is stopped. The teardown ignores INT/TERM (also a terminal's Ctrl-C to its commands),
#   so repeated signals do not cut it short, and every wait in it is bounded.
#
# Test job: by default a stub stands in for claude (AGENT_JOBS_CLAUDE_BIN), so no model is called;
# --real-model M runs one real job instead (1 turn, budget 0.2 USD): the daemon alone runs with the
# caller's HOME and CLAUDE_CONFIG_DIR so claude finds its login, and that claude writes its session
# data there (~/.claude/projects, or $CLAUDE_CONFIG_DIR) as any claude run does; this is expected.
#
# Required checks: curl must exist (GET /v1/health is part of the acceptance), the installed binary
# must be byte-identical to the one built and is the one the later steps run, and a --real-model
# job's report must carry its marker.
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
  --real-model MODEL the test job runs on the real claude with MODEL (1 turn, budget 0.2) instead of a stub;
                     that claude runs with your HOME/CLAUDE_CONFIG_DIR and writes its session data there
  --wait-minutes N   how long to wait for the test job (default 5, 10 with --real-model)
  --keep             keep the temp dir on success too (it is always kept on failure)
  --inside-job       run from inside an agent-jobs job: test-hooks build, outer wrapper ignored
Needs git, go and curl. The clone never prompts (git and ssh in batch mode): use an ssh agent or a
credential helper. Nothing outside the temp dir is written, except that real-model session data.
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

# Before the first git command: no inherited repository, work tree, index, object store or config
# override may redirect git (git -C does not override GIT_DIR, GIT_WORK_TREE or GIT_INDEX_FILE). Only
# what reaching the remote may need stays.
while IFS= read -r v; do
  case "$v" in
    GIT_SSH|GIT_SSH_COMMAND|GIT_SSH_VARIANT|GIT_ASKPASS|GIT_TERMINAL_PROMPT|GIT_SSL_CAINFO|GIT_SSL_CAPATH|GIT_PROXY_COMMAND) ;;
    *) unset "$v" ;;
  esac
done < <(compgen -e | grep -E '^GIT_' || true)

SRC_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if ! command -v go >/dev/null 2>&1 && [ -x "$HOME/.local/go/bin/go" ]; then
  export PATH="$HOME/.local/go/bin:$PATH"
fi
for tool in git go curl; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "acceptance: $tool not found (required)" >&2
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

INSTALLED="$SANDBOX_HOME/.local/bin/agent-jobs"
# AJ is the binary the steps run: the one built until the install step put it in place, then the
# installed one.
AJ="$BIN"
REAL_HOME="$HOME"
REAL_CLAUDE_CONFIG_DIR_SET=0
REAL_CLAUDE_CONFIG_DIR=""
if [ -n "${CLAUDE_CONFIG_DIR+x}" ]; then
  REAL_CLAUDE_CONFIG_DIR_SET=1
  REAL_CLAUDE_CONFIG_DIR="$CLAUDE_CONFIG_DIR"
fi

STEP_NAMES=()
STEP_CODES=()
FAILED=0
CLEANED=0
SUMMARY_DONE=0
# While CRITICAL=1 (a supervisor forked whose PID is not recorded yet) INT/TERM are held in
# PENDING_SIGNAL and acted on right after, so the teardown always knows which supervisor to wait for.
CRITICAL=0
PENDING_SIGNAL=""
# Set once a cleanup list/cancel timed out: the binary is stuck, later job passes are skipped.
CANCEL_STUCK=0
SUP_DIR="$ROOT/sup"
SUP_SEQ=0
# The control fds of the live supervisors (closed in every child but their own supervisor).
SUP_FDS=()
# The active step's, the daemon's and a teardown command's supervisor (spawn PREFIX): PREFIX_PID,
# PREFIX_FD (control fd) and PREFIX_RC (the file the exit code lands in).
STEP_PID=""
DAEMON_PID=""
DAEMON_RC=""
BOUNDED_PID=""
REAP_RC=""

record() { STEP_NAMES+=("$1"); STEP_CODES+=("$2"); }

# release_signal acts on an INT/TERM held while CRITICAL was set (once: the teardown that follows
# spawns supervisors too).
release_signal() {
  local s="$PENDING_SIGNAL"
  PENDING_SIGNAL=""
  if [ -n "$s" ]; then exit "$s"; fi
}

# supervisor FD RCFILE TIMEOUT GRACE CMD...: the body of a supervisor, the leader of a process group
# of its own (spawn). It runs CMD (INT/TERM back to default, explicit errexit) in its group and reads
# its control fd, a FIFO the script holds open: `done RC` (CMD ended), `stop` (the script's teardown)
# or `timeout` (TIMEOUT seconds passed, 0 = none) send TERM to the group (the supervisor ignores it),
# GRACE seconds later `kill` ends the wait. Then it writes CMD's exit code (124 after a timeout, 143
# when stopped before the start) to RCFILE and KILLs its own group: whatever CMD left behind, and the
# supervisor itself. Only a member of the group signals the group (kill 0), so no group id is ever
# signalled after it could have been reused; the script itself signals no process.
supervisor() {
  local fd="$1" rcf="$2" timeout="$3" grace="$4" line rc="" timed_out=0 f
  shift 4
  set +e
  trap '' INT TERM
  for f in "${SUP_FDS[@]}"; do [ "$f" = "$fd" ] || exec {f}>&-; done
  # @acceptance-test-hook: supervisor start@
  # A stop that arrived before the start: nothing runs.
  if read -r -t 0 -u "$fd"; then
    echo 143 >"$rcf"
    exit 0
  fi
  {
    (
      for f in "${SUP_FDS[@]}"; do exec {f}>&-; done
      trap - INT TERM
      set -e
      "$@"
    )
    echo "done $?" >&"$fd"
  } &
  if [ "$timeout" -gt 0 ]; then
    { sleep "$timeout"; echo timeout >&"$fd"; } </dev/null >/dev/null 2>&1 &
  fi
  while read -r line <&"$fd"; do
    case "$line" in
      "done "*) rc="${line#done }"; break ;;
      stop|timeout)
        if [ "$line" = timeout ]; then
          timed_out=1
          echo "acceptance: timed out after ${timeout}s: $*" >&2
        fi
        kill -TERM 0
        { sleep "$grace"; echo kill >&"$fd"; } </dev/null >/dev/null 2>&1 &
        ;;
      kill) break ;;
    esac
  done
  if [ "$timed_out" = 1 ]; then rc=124; fi
  echo "${rc:-137}" >"$rcf"
  kill -KILL 0
}

# spawn PREFIX TIMEOUT GRACE CMD...: runs CMD under a supervisor in a process group of its own (job
# control is on for the fork only) and sets PREFIX_PID (the supervisor, a child of this shell, only
# ever waited for), PREFIX_FD and PREFIX_RC. CMD's redirections are the caller's
# (`spawn P 0 5 cmd >log`); the caller calls release_signal once they are undone.
spawn() {
  local p="$1" timeout="$2" grace="$3" fifo fd
  shift 3
  SUP_SEQ=$((SUP_SEQ + 1))
  mkdir -p "$SUP_DIR"
  fifo="$SUP_DIR/$SUP_SEQ.ctl"
  mkfifo -m 0600 "$fifo"
  # Read-write: the open never blocks, writes never block or raise SIGPIPE, and a message sent
  # before the supervisor reads waits in the FIFO. The name is not needed once open.
  exec {fd}<>"$fifo"
  rm -f "$fifo"
  SUP_FDS+=("$fd")
  printf -v "${p}_FD" '%s' "$fd"
  printf -v "${p}_RC" '%s' "$SUP_DIR/$SUP_SEQ.rc"
  CRITICAL=1
  set -m
  supervisor "$fd" "$SUP_DIR/$SUP_SEQ.rc" "$timeout" "$grace" "$@" &
  printf -v "${p}_PID" '%s' "$!"
  set +m
  CRITICAL=0
}

# reap PREFIX [stop]: (with stop: asks the supervisor to stop CMD,) waits for the supervisor, closes
# its control fd and sets REAP_RC to CMD's exit code. Bounded by the supervisor's own timers.
reap() {
  local p="$1" pid fd rcf f keep=()
  REAP_RC=""
  pid="${p}_PID"
  pid="${!pid:-}"
  [ -n "$pid" ] || return 0
  fd="${p}_FD" rcf="${p}_RC"
  fd="${!fd}" rcf="${!rcf}"
  if [ "${2:-}" = stop ]; then echo stop 2>/dev/null >&"$fd" || true; fi
  wait "$pid" 2>/dev/null || true
  printf -v "${p}_PID" '%s' ""
  exec {fd}>&-
  for f in "${SUP_FDS[@]}"; do [ "$f" = "$fd" ] || keep+=("$f"); done
  SUP_FDS=(${keep[@]+"${keep[@]}"})
  REAP_RC="$(cat "$rcf" 2>/dev/null || true)"
  REAP_RC="${REAP_RC:-137}"
}

# daemon_alive: the daemon has not exited (its supervisor has not written its exit code).
daemon_alive() { [ -n "$DAEMON_RC" ] && [ ! -e "$DAEMON_RC" ]; }

# bounded SECONDS OUT CMD...: CMD under a supervisor with a deadline (then TERM, 2 s, KILL), stdout to
# OUT, stderr to the cancel log; returns CMD's exit code, 124 on the deadline.
bounded() {
  local secs="$1" out="$2"
  shift 2
  spawn BOUNDED "$secs" 2 "$@" >"$out" 2>>"$LOGS/cancel.log" </dev/null
  reap BOUNDED
  return "$REAP_RC"
}

# cancel_jobs cancels every queued or running job of the sandbox agent (one submitted by an
# interrupted step included); a cancelled job's processes are ended by the binary. Each list and
# cancel has a deadline: a stuck binary never blocks the daemon's teardown (it fails the run).
cancel_jobs() {
  [ -x "$AJ" ] && [ -d "$STATE" ] && [ "$CANCEL_STUCK" = 0 ] || return 0
  local st id rc out="$LOGS/cancel.list"
  for st in queued running; do
    rc=0
    bounded 10 "$out" "$AJ" list --agent "$AGENT" --status "$st" --limit 1000 || rc=$?
    if [ "$rc" = 124 ]; then
      cancel_stuck "list --status $st"
      return 0
    fi
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      echo "cancel $id ($st)" >>"$LOGS/cancel.log"
      rc=0
      bounded 10 /dev/null "$AJ" cancel --agent "$AGENT" "$id" || rc=$?
      if [ "$rc" = 124 ]; then
        cancel_stuck "cancel $id"
        return 0
      fi
    done < <(awk 'NR > 1 { print $1 }' "$out")
  done
}

cancel_stuck() {
  CANCEL_STUCK=1
  FAILED=1
  echo "acceptance: teardown: \`agent-jobs $1\` timed out; job cancel skipped, see $LOGS/cancel.log" >&2
}

# cleanup: the active step first (it may be submitting a job), then the sandbox's jobs, the daemon,
# and once more the jobs (one the daemon launched meanwhile). Idempotent; CLEANED is set only once the
# teardown ran to its end. INT, TERM and PIPE are ignored from here on, by this shell and by the
# teardown's own commands (a terminal's Ctrl-C reaches the whole foreground group, and a closed
# `| tee` must not end the shell mid-teardown); every wait is bounded by a supervisor's timers (step
# 5 s, each list/cancel 10 s + 2 s, daemon 15 s after TERM) and KILL still works.
cleanup() {
  [ "$CLEANED" = 0 ] || return 0
  trap '' INT TERM PIPE
  reap STEP stop
  reap BOUNDED stop
  cancel_jobs
  reap DAEMON stop
  cancel_jobs
  CLEANED=1
}

finish() {
  cleanup
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
  SUMMARY_DONE=1
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
  if [ "$CLEANED" = 0 ]; then
    FAILED=1
    cleanup
  fi
  if [ "$SUMMARY_DONE" = 0 ]; then
    echo "acceptance: interrupted (exit $rc); temp dir kept: $ROOT" >&2
  fi
}

# on_signal CODE: exit CODE (the EXIT trap tears down), or after the critical section when inside one.
# The teardown itself ignores INT/TERM (cleanup), so a repeated signal never cuts it short.
on_signal() {
  FAILED=1
  if [ "$CRITICAL" = 1 ]; then
    PENDING_SIGNAL="$1"
    return 0
  fi
  exit "$1"
}
trap on_exit EXIT
trap 'on_signal 130' INT
trap 'on_signal 143' TERM

# run_step NAME FUNC: runs FUNC with errexit under a supervisor in a process group of its own (spawn),
# output to logs/NN-NAME.log; a failure ends the run. Whatever the step left in its group is killed
# when it ends.
run_step() {
  local name="$1" fn="$2" log rc
  log="$LOGS/$(printf '%02d' "${#STEP_NAMES[@]}")-$name.log"
  echo "== $name"
  spawn STEP 0 5 "$fn" >"$log" 2>&1 </dev/null
  release_signal
  reap STEP
  rc="$REAP_RC"
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
  # The step runs in a background process group: a prompt on the terminal would stop it (SIGTTIN)
  # for good, so git and ssh must fail instead of asking.
  export GIT_TERMINAL_PROMPT=0
  if [ -z "${GIT_SSH:-}" ] && [ -z "$(git config --get core.sshCommand || true)" ]; then
    export GIT_SSH_COMMAND="${GIT_SSH_COMMAND:-ssh} -o BatchMode=yes"
  fi
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
  local aj="$BIN"
  write_stubs
  # cgroup_scope=off before init: no systemd scope per job and no scope probe (init --pin-scope
  # leaves a set value alone). The read-only HTTP API is on, on the state dir's unix socket.
  mkdir -p "$STATE"
  chmod 0700 "$SANDBOX_HOME/.agent-jobs" "$STATE"
  printf '%s\n' '{"cgroup_scope": "off", "daily_off": true, "http_api": {"enabled": true}}' >"$STATE/config.json"
  chmod 0600 "$STATE/config.json"
  if [ "$(uname -s)" = Linux ]; then
    # README: scripts/install-linux.sh --instance NAME (a prebuilt binary: the one built above).
    # The skills dir is explicit: the installer's default follows CLAUDE_CONFIG_DIR, which the
    # sandbox has dropped, but no default may ever reach the caller's skills.
    AGENT_JOBS_SYSTEMCTL="$STUB/systemctl" \
      "$CLONE/scripts/install-linux.sh" --instance "$AGENT" --binary "$BIN" --skills-dir "$SANDBOX_HOME/.claude/skills"
    test -x "$INSTALLED"
    # The installed bytes are the ones built and tested, and they run; the later steps use them.
    if ! cmp -s "$BIN" "$INSTALLED"; then
      echo "installed binary differs: $INSTALLED is not the build $BIN"
      exit 1
    fi
    "$INSTALLED" version
    aj="$INSTALLED"
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
  "$aj" http-api token --agent "$AGENT" --rotate >/dev/null
  test -f "$STATE/http-api.token"
}

# start_daemon: the daemon under a supervisor in a process group of its own (spawn; on stop TERM,
# 15 s, KILL). With --real-model it alone gets the caller's HOME and CLAUDE_CONFIG_DIR: the claude it
# runs needs the login there.
start_daemon() {
  local env_cmd=()
  if [ -n "$REAL_MODEL" ]; then
    env_cmd=(env "HOME=$REAL_HOME")
    if [ "$REAL_CLAUDE_CONFIG_DIR_SET" = 1 ]; then env_cmd+=("CLAUDE_CONFIG_DIR=$REAL_CLAUDE_CONFIG_DIR"); fi
  fi
  spawn DAEMON 0 15 exec ${env_cmd[@]+"${env_cmd[@]}"} "$AJ" daemon --agent "$AGENT" >"$LOGS/daemon.log" 2>&1 </dev/null
  release_signal
}

step_daemon() {
  local i
  for i in $(seq 40); do
    if grep -q 'daemon started' "$LOGS/daemon.log" && [ -S "$STATE/http-api.sock" ]; then break; fi
    daemon_alive
    sleep 0.25
  done
  daemon_alive
  grep 'daemon started' "$LOGS/daemon.log"
  test -S "$STATE/http-api.sock"
}

step_status() {
  local out
  out="$("$AJ" status --agent "$AGENT")"
  echo "$out"
  grep -q "^http api:  " <<<"$out"
  "$AJ" doctor procs --agent "$AGENT"
  "$AJ" list --agent "$AGENT"
  # Required (curl is checked at the start): the read-only HTTP API answers with the token.
  local hdr="$ROOT/http-api.header"
  (umask 077; printf 'Authorization: Bearer %s\n' "$(cat "$STATE/http-api.token")" >"$hdr")
  curl -sS --fail --max-time 10 --unix-socket "$STATE/http-api.sock" -H "@$hdr" http://localhost/v1/health
  echo
  rm -f "$hdr"
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
    id="$("$AJ" start --agent "$AGENT" --title "acceptance real job" --kind general --repo "$SCRATCH" \
      --model "$REAL_MODEL" --max-turns 1 --max-budget-usd 0.2 --timeout-minutes "$WAIT_MINUTES" --brief-file "$brief")"
  else
    echo "Acceptance dry job: a stub stands in for claude." >"$brief"
    id="$("$AJ" start --agent "$AGENT" --title "acceptance dry job" --kind general --repo "$SCRATCH" \
      --model sonnet --brief-file "$brief")"
  fi
  echo "job $id"
  echo "$id" >"$ROOT/job.id"
  # The daemon launches it on its next tick (once a minute) and collects it on the one after.
  st=""
  for i in $(seq $((WAIT_MINUTES * 12))); do
    st="$("$AJ" status --agent "$AGENT" "$id" | sed -n 's/^status: *//p')"
    case "$st" in succeeded|failed|timeout|cancelled|stopped) break ;; esac
    sleep 5
  done
  "$AJ" status --agent "$AGENT" "$id"
  echo "final status: $st"
  test "$st" = succeeded
  test -s "$STATE/jobs/$id/report.md"
  "$AJ" result --agent "$AGENT" "$id" | tee "$ROOT/result.txt"
  grep -q '^## Summary' "$ROOT/result.txt"
  if [ -n "$REAL_MODEL" ]; then
    if ! grep -q 'ACCEPT-REAL-JOB-OK' "$ROOT/result.txt"; then
      echo "the report lacks the marker ACCEPT-REAL-JOB-OK: the real job is not verified"
      exit 1
    fi
  else
    grep -q 'ACCEPT-DRY-JOB-OK' "$ROOT/result.txt"
    grep -qx -- '--max-turns' "$STATE/jobs/$id/stub-argv.txt"
  fi
  "$AJ" doctor procs --agent "$AGENT"
  daemon_alive
}

# ---- run ----

echo "acceptance: $REF ($SHA) from $FROM"
echo "temp dir: $ROOT"
# The clone runs with the caller's HOME (git credentials, ssh) but no GIT_* override (scrubbed above).
run_step clone step_clone

# From here on, before any code of the clone is built or run: the isolated environment. The Go caches
# stay the caller's (pinned before HOME moves); the caller's agent-jobs, git, XDG and claude settings
# are dropped; only the temp dir counts.
# The caller's go env file is read, but go itself already runs with the sandbox's config dir (go
# creates its telemetry dir there).
caller_goenv="${GOENV:-${XDG_CONFIG_HOME:-$HOME/.config}/go/env}"
for v in GOCACHE GOMODCACHE GOPATH GOPROXY GOSUMDB GOPRIVATE GONOPROXY GONOSUMDB; do
  val="$(XDG_CONFIG_HOME="$SANDBOX_HOME/.config" GOENV="$caller_goenv" go env "$v")"
  if [ -n "$val" ]; then export "$v=$val"; fi
done
while IFS= read -r v; do unset "$v"; done < <(compgen -e | grep -E '^(AGENT_JOBS_|GIT_)' || true)
unset XDG_CONFIG_HOME XDG_DATA_HOME XDG_CACHE_HOME XDG_STATE_HOME CLAUDE_CONFIG_DIR ACCEPTANCE_CLEAN_CLONE
# Nor may the caller's Go settings change what is built or tested (the build sets its own).
unset GOENV GOFLAGS GOOS GOARCH GOEXPERIMENT
export HOME="$SANDBOX_HOME"
export AGENT_JOBS_HOME="$SANDBOX_HOME/.agent-jobs"
export AGENT_JOBS_TMUX_BIN="$STUB/tmux"
export AGENT_JOBS_CGROUP_SCOPE=off
if [ -z "$REAL_MODEL" ]; then
  export AGENT_JOBS_CLAUDE_BIN="$STUB/claude"
fi
if [ "$INSIDE_JOB" = 1 ] && [ -n "$OUTER" ]; then
  export AGENT_JOBS_TEST_OUTER_WRAPPER="$OUTER"
fi

run_step build step_build
if [ "$NO_TESTS" = 1 ]; then skip_step tests --no-tests; else run_step tests step_tests; fi
run_step install step_install
if [ "$(uname -s)" = Linux ]; then AJ="$INSTALLED"; fi
start_daemon
# The daemon is this shell's (the teardown stops it); step_daemon only checks it.
run_step daemon step_daemon
run_step status step_status
if [ -n "$REAL_MODEL" ]; then run_step real-job step_job; else run_step dry-job step_job; fi
finish
