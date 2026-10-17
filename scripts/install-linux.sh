#!/usr/bin/env bash
# Install agent-jobs as a systemd USER service on Linux, for the current user only (no sudo).
#
#   scripts/install-linux.sh --instance thrall            # build, install, enable, (re)start
#   scripts/install-linux.sh --instance thrall --uninstall
#
# Idempotent: an unchanged binary/unit leaves a running daemon alone. Touches only
# ~/.local/bin/agent-jobs, ~/.config/systemd/user/agent-jobs@.service, ~/.config/agent-jobs/<instance>.env
# (created once, never overwritten), the unit agent-jobs@<instance>.service and the agent's skill
# <skills dir>/agent-jobs/SKILL.md (default ${CLAUDE_CONFIG_DIR:-~/.claude}/skills). State (~/.agent-jobs)
# and the env file are kept on --uninstall.
#
# Test hooks: AGENT_JOBS_SYSTEMCTL (systemctl replacement), --binary PATH (install a prebuilt binary).
set -euo pipefail

usage() {
  cat <<'USAGE'
usage: install-linux.sh --instance AGENT [--binary PATH] [--skills-dir DIR | --no-skill] [--force-skill] [--uninstall]
  --instance AGENT  agent name = systemd instance (agent-jobs@AGENT); default $AGENT_JOBS_AGENT
  --binary PATH     install this prebuilt binary instead of building from the repo
  --skills-dir DIR  the agent's Claude Code skills dir; the agent-jobs skill goes to DIR/agent-jobs/SKILL.md
                    (default ${CLAUDE_CONFIG_DIR:-$HOME/.claude}/skills)
  --no-skill        do not install (or remove) the agent-jobs skill
  --force-skill     replace a SKILL.md that differs from what this installer last wrote (edited
                    locally); the old file is kept as SKILL.md.bak.<time>
  --uninstall       stop + disable agent-jobs@AGENT; remove the unit template and the binary
                    when no other agent-jobs@ instance is enabled (state and env file are kept)
USAGE
}

INSTANCE="${AGENT_JOBS_AGENT:-}"
BINARY=""
UNINSTALL=0
SKILLS_DIR=""
NO_SKILL=0
FORCE_SKILL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --instance) [ $# -ge 2 ] || { usage >&2; exit 2; }; INSTANCE="$2"; shift 2 ;;
    --instance=*) INSTANCE="${1#*=}"; shift ;;
    --binary) [ $# -ge 2 ] || { usage >&2; exit 2; }; BINARY="$2"; shift 2 ;;
    --binary=*) BINARY="${1#*=}"; shift ;;
    --skills-dir) [ $# -ge 2 ] || { usage >&2; exit 2; }; SKILLS_DIR="$2"; shift 2 ;;
    --skills-dir=*) SKILLS_DIR="${1#*=}"; shift ;;
    --no-skill) NO_SKILL=1; shift ;;
    --force-skill) FORCE_SKILL=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "install-linux: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ "$(uname -s)" != "Linux" ]; then
  echo "install-linux: Linux only (macOS: see launchd/ in README)" >&2
  exit 1
fi
if [ "$(id -u)" = "0" ]; then
  echo "install-linux: run as the agent's user, not root (systemd --user unit)" >&2
  exit 1
fi
# Same rule as jobs.StateDir: the instance is the agent name.
if ! [[ "$INSTANCE" =~ ^[a-z0-9][a-z0-9_-]{0,31}$ ]]; then
  echo "install-linux: invalid or missing --instance '${INSTANCE}' (want [a-z0-9_-], max 32)" >&2
  exit 2
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UNIT_SRC="$REPO_ROOT/deploy/systemd/agent-jobs@.service"
CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
BIN_DIR="$HOME/.local/bin"
BIN="$BIN_DIR/agent-jobs"
UNIT_DIR="$CONFIG_HOME/systemd/user"
UNIT_DST="$UNIT_DIR/agent-jobs@.service"
ENV_DIR="$CONFIG_HOME/agent-jobs"
ENV_FILE="$ENV_DIR/$INSTANCE.env"
UNIT="agent-jobs@$INSTANCE.service"
SYSTEMCTL="${AGENT_JOBS_SYSTEMCTL:-systemctl}"
[ -n "$SKILLS_DIR" ] || SKILLS_DIR="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/skills"
SKILL_SRC="$REPO_ROOT/skills/agent-jobs/SKILL.md"
# Ownership of the installed skill is tracked by its canonical directory, one record per instance
# ($ENV_DIR/<instance>.skill); the destination's .installed-sha256 is the hash this installer wrote.
SKILL_RECORD="$ENV_DIR/$INSTANCE.skill"
set_skill_paths() {
  SKILL_DIR="$(realpath -m -- "$1")"
  SKILL_DST="$SKILL_DIR/SKILL.md"
  SKILL_HASH="$SKILL_DIR/.installed-sha256"
}
set_skill_paths "$SKILLS_DIR/agent-jobs"

sha256() { sha256sum -- "$1" | cut -d' ' -f1; }

# refuse_skill_alias: the destination must never be the repository's own skill (a --skills-dir of
# "$REPO_ROOT/skills" would make uninstall delete the tracked source).
refuse_skill_alias() {
  local src_dir
  src_dir="$(realpath -m -- "$(dirname "$SKILL_SRC")")"
  if [ "$SKILL_DIR" = "$src_dir" ] || [ "$(realpath -m -- "$SKILL_DST")" = "$(realpath -m -- "$SKILL_SRC")" ]; then
    echo "install-linux: skill destination $SKILL_DIR is the repository's own skill; choose another --skills-dir" >&2
    exit 2
  fi
}

# skill_ours: the destination is what this installer wrote (recorded hash) or the shipped file.
skill_ours() {
  { [ -f "$SKILL_HASH" ] && [ "$(sha256 "$SKILL_DST")" = "$(cat "$SKILL_HASH")" ]; } ||
    { [ -f "$SKILL_SRC" ] && cmp -s "$SKILL_SRC" "$SKILL_DST"; }
}

# skill_users prints the other instances whose record names the same canonical skill directory.
skill_users() {
  local f
  for f in "$ENV_DIR"/*.skill; do
    [ -f "$f" ] && [ "$f" != "$SKILL_RECORD" ] || continue
    if [ "$(cat "$f")" = "$SKILL_DIR" ]; then basename "$f" .skill; fi
  done | tr '\n' ' '
}

# systemctl --user needs the user manager's bus; non-login shells (tmux, ssh -c) often lack it.
if [ -z "${XDG_RUNTIME_DIR:-}" ] && [ -d "/run/user/$(id -u)" ]; then
  XDG_RUNTIME_DIR="/run/user/$(id -u)"
  export XDG_RUNTIME_DIR
fi

# The unit resolves its env file via %E (the user manager's XDG_CONFIG_HOME, normally ~/.config).
if [ -n "${XDG_CONFIG_HOME:-}" ] && [ "$XDG_CONFIG_HOME" != "$HOME/.config" ]; then
  echo "WARNING: XDG_CONFIG_HOME=$XDG_CONFIG_HOME differs from $HOME/.config; the unit and env file go there," >&2
  echo "         so the systemd user manager must see the same XDG_CONFIG_HOME (else it ignores them)." >&2
fi

sc() {
  echo "+ systemctl --user $*"
  "$SYSTEMCTL" --user "$@"
}

# One installer at a time (two concurrent runs would race on the binary and the unit).
mkdir -p "$BIN_DIR"
exec 9>"$BIN_DIR/.agent-jobs.install.lock"
if ! flock -n 9; then
  echo "install-linux: another install-linux.sh is running (lock $BIN_DIR/.agent-jobs.install.lock)" >&2
  exit 1
fi

# running_wrappers prints the number of job wrappers of this user started from $BIN. Every running job's
# PreToolUse guard hook is "$BIN guard …": removing $BIN makes the hook exit 127, which Claude Code
# treats as non-blocking, so the job would run unguarded.
running_wrappers() {
  local n
  n="$(pgrep -u "$(id -u)" -f -- "$BIN run-wrapper " 2>/dev/null | wc -l)" || true
  echo "${n:-0}"
}

# other_instances lists agent-jobs@ instances other than $UNIT that are enabled (wants symlink) or
# loaded/active right now (list-units), so the shared template and binary stay while one needs them.
other_instances() {
  {
    if [ -d "$UNIT_DIR/default.target.wants" ]; then
      find "$UNIT_DIR/default.target.wants" -maxdepth 1 -name 'agent-jobs@*.service' -printf '%f\n' 2>/dev/null || true
    fi
    "$SYSTEMCTL" --user list-units --plain --no-legend 'agent-jobs@*.service' 2>/dev/null |
      awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^agent-jobs@.*\.service$/) { print $i; break } }' || true
  } | grep -vxF -- "$UNIT" | sort -u | tr '\n' ' ' || true
}

if [ "$UNINSTALL" = 1 ]; then
  echo "== uninstall $UNIT"
  # disable --now stops only the daemon (KillMode=process): running jobs go on WITHOUT timeout
  # enforcement or collection until a daemon for this agent starts again.
  sc disable --now "$UNIT" || echo "  (unit was not enabled/active)"
  others="$(other_instances)"
  rc=0
  # The skill this instance installed (its record; else the given/default dir) is removed when no
  # other instance records the same directory and it is still what this installer wrote.
  if [ "$NO_SKILL" = 1 ]; then
    echo "  skill: left as is (--no-skill)"
  else
    if [ -f "$SKILL_RECORD" ]; then set_skill_paths "$(cat "$SKILL_RECORD")"; fi
    refuse_skill_alias
    users="$(skill_users)"
    if [ ! -f "$SKILL_DST" ]; then
      :
    elif [ -n "$users" ]; then
      echo "  skill: also installed for $users: keeping $SKILL_DST"
    elif skill_ours; then
      rm -f "$SKILL_DST" "$SKILL_HASH"
      rmdir "$SKILL_DIR" 2>/dev/null || true
      echo "  removed $SKILL_DST"
    else
      echo "  skill: $SKILL_DST differs from what this installer wrote (edited locally?): kept"
    fi
    rm -f "$SKILL_RECORD"
  fi
  if [ -n "$others" ]; then
    echo "  other instances still enabled or active ($others): keeping $UNIT_DST and $BIN"
  else
    if [ -f "$UNIT_DST" ]; then rm -f "$UNIT_DST"; echo "  removed $UNIT_DST"; fi
    n="$(running_wrappers)"
    if [ "$n" -gt 0 ]; then
      echo "  $n jobs still running — cancel or wait, then rerun --uninstall (keeping $BIN: it is their guard hook)" >&2
      rc=1
    elif [ -f "$BIN" ]; then
      rm -f "$BIN"
      echo "  removed $BIN"
    fi
  fi
  sc daemon-reload
  echo "  kept: env file $ENV_FILE (if any) and state (~/.agent-jobs/$INSTANCE or \$AGENT_JOBS_HOME)"
  if [ "$rc" = 0 ]; then echo "uninstall done"; else echo "uninstall incomplete"; fi
  exit "$rc"
fi

if [ ! -f "$UNIT_SRC" ]; then
  echo "install-linux: unit template not found: $UNIT_SRC" >&2
  exit 1
fi
mkdir -p "$BIN_DIR" "$UNIT_DIR" "$ENV_DIR"

changed=0

echo "== binary $BIN"
# Never write over the running binary in place (ETXTBSY, and the guard hook of a running job points at
# this path): build/copy next to it, then rename atomically.
tmp="$BIN_DIR/.agent-jobs.new.$$"
trap 'rm -f "$tmp"' EXIT
if [ -n "$BINARY" ]; then
  if [ ! -f "$BINARY" ]; then
    echo "install-linux: --binary not found: $BINARY" >&2
    exit 1
  fi
  cp "$BINARY" "$tmp"
  echo "  using prebuilt $BINARY"
else
  if ! command -v go >/dev/null 2>&1 && [ -x "$HOME/.local/go/bin/go" ]; then
    export PATH="$HOME/.local/go/bin:$PATH"
  fi
  if ! command -v go >/dev/null 2>&1; then
    echo "install-linux: go not found (need Go 1.27; or pass --binary)" >&2
    exit 1
  fi
  echo "  building from $REPO_ROOT"
  version="$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
  echo "  version $version"
  (cd "$REPO_ROOT" && go build -trimpath -ldflags "-X main.version=$version" -o "$tmp" ./cmd/agent-jobs)
fi
chmod 0755 "$tmp"
if [ -f "$BIN" ] && cmp -s "$tmp" "$BIN"; then
  rm -f "$tmp"
  echo "  unchanged"
else
  mv -f "$tmp" "$BIN"
  changed=1
  echo "  installed"
fi
trap - EXIT

echo "== unit $UNIT_DST"
if [ -f "$UNIT_DST" ] && cmp -s "$UNIT_SRC" "$UNIT_DST"; then
  echo "  unchanged"
else
  utmp="$UNIT_DST.new.$$"
  install -m 0644 "$UNIT_SRC" "$utmp"
  mv -f "$utmp" "$UNIT_DST"
  changed=1
  echo "  installed"
fi

echo "== skill $SKILL_DST"
# Installed per agent: teaches the MCP tools and when to do work yourself, use a subagent or start a job.
# Updated automatically only while the destination is still what this installer wrote (its recorded
# hash); a locally edited SKILL.md is left alone unless --force-skill (then backed up). Never needs a
# daemon restart.
if [ "$NO_SKILL" = 1 ]; then
  echo "  skipped (--no-skill)"
elif [ ! -f "$SKILL_SRC" ]; then
  echo "install-linux: skill not found: $SKILL_SRC" >&2
  exit 1
else
  refuse_skill_alias
  write_skill() {
    mkdir -p "$SKILL_DIR"
    stmp="$SKILL_DST.new.$$"
    install -m 0644 "$SKILL_SRC" "$stmp"
    mv -f "$stmp" "$SKILL_DST"
  }
  if [ ! -e "$SKILL_DST" ]; then
    write_skill
    echo "  installed"
  elif cmp -s "$SKILL_SRC" "$SKILL_DST"; then
    echo "  unchanged"
  elif skill_ours; then
    write_skill
    echo "  updated"
  elif [ "$FORCE_SKILL" = 1 ]; then
    bak="$SKILL_DST.bak.$(date +%Y%m%d-%H%M%S)"
    cp -p -- "$SKILL_DST" "$bak"
    write_skill
    echo "  replaced (--force-skill); the previous file is $bak"
  else
    echo "  NOTICE: $SKILL_DST differs from what this installer last wrote (edited locally?): left as is;" >&2
    echo "          rerun with --force-skill to replace it (the old file is backed up)" >&2
  fi
  if cmp -s "$SKILL_SRC" "$SKILL_DST"; then
    sha256 "$SKILL_DST" >"$SKILL_HASH"
  fi
  printf '%s\n' "$SKILL_DIR" >"$SKILL_RECORD"
fi

echo "== env file $ENV_FILE"
if [ -e "$ENV_FILE" ]; then
  echo "  exists, left as is"
else
  (
  umask 077
  cat >"$ENV_FILE" <<ENVFILE
# Environment for agent-jobs@$INSTANCE (systemd EnvironmentFile: KEY=VALUE lines, no 'export').
# AGENT_JOBS_HOME=$HOME/.agent-jobs
# AGENT_JOBS_CLAUDE_BIN=$HOME/.local/bin/claude
# AGENT_JOBS_TMUX_BIN=/usr/bin/tmux
ENVFILE
  )
  echo "  created (all settings commented out)"
fi

echo "== state (agent-jobs init --agent $INSTANCE)"
# Same environment as the service: the env file may move the state root.
# Parsed, never sourced: only KEY=VALUE lines (systemd EnvironmentFile syntax, optional quotes).
(
  while IFS= read -r line || [ -n "$line" ]; do
    [[ "$line" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] || continue
    key="${line%%=*}"
    val="${line#*=}"
    if [[ "$val" =~ ^\"(.*)\"$ ]] || [[ "$val" =~ ^\'(.*)\'$ ]]; then val="${BASH_REMATCH[1]}"; fi
    export "$key=$val"
  done <"$ENV_FILE"
  "$BIN" init --agent "$INSTANCE"
) | sed 's/^/  /'

echo "== systemd"
sc daemon-reload
sc enable "$UNIT"
if [ "$changed" = 1 ] || ! "$SYSTEMCTL" --user is-active --quiet "$UNIT"; then
  # restart = start when inactive; KillMode=process keeps running jobs alive across it.
  sc restart "$UNIT"
else
  echo "  $UNIT already active and nothing changed: left running"
fi

if command -v loginctl >/dev/null 2>&1; then
  linger="$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null || true)"
  if [ "$linger" != "yes" ]; then
    echo "WARNING: lingering is off for $(id -un): the user manager (and this daemon) stops at logout." >&2
    echo "         Enable once with: loginctl enable-linger $(id -un)" >&2
  fi
fi

echo "done: $UNIT  (status: systemctl --user status $UNIT; logs: journalctl --user -u $UNIT)"
