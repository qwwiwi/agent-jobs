#!/usr/bin/env bash
# Install agent-jobs as a systemd USER service on Linux, for the current user only (no sudo).
#
#   scripts/install-linux.sh --instance thrall            # build, install, enable, (re)start
#   scripts/install-linux.sh --instance thrall --uninstall
#
# Idempotent: an unchanged binary/unit leaves a running daemon alone. Touches only
# ~/.local/bin/agent-jobs, ~/.config/systemd/user/agent-jobs@.service, ~/.config/agent-jobs/<instance>.env
# (created once, never overwritten) and the unit agent-jobs@<instance>.service. State (~/.agent-jobs)
# and the env file are kept on --uninstall.
#
# Test hooks: AGENT_JOBS_SYSTEMCTL (systemctl replacement), --binary PATH (install a prebuilt binary).
set -euo pipefail

usage() {
  cat <<'USAGE'
usage: install-linux.sh --instance AGENT [--binary PATH] [--uninstall]
  --instance AGENT  agent name = systemd instance (agent-jobs@AGENT); default $AGENT_JOBS_AGENT
  --binary PATH     install this prebuilt binary instead of building from the repo
  --uninstall       stop + disable agent-jobs@AGENT; remove the unit template and the binary
                    when no other agent-jobs@ instance is enabled (state and env file are kept)
USAGE
}

INSTANCE="${AGENT_JOBS_AGENT:-}"
BINARY=""
UNINSTALL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --instance) [ $# -ge 2 ] || { usage >&2; exit 2; }; INSTANCE="$2"; shift 2 ;;
    --instance=*) INSTANCE="${1#*=}"; shift ;;
    --binary) [ $# -ge 2 ] || { usage >&2; exit 2; }; BINARY="$2"; shift 2 ;;
    --binary=*) BINARY="${1#*=}"; shift ;;
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

# systemctl --user needs the user manager's bus; non-login shells (tmux, ssh -c) often lack it.
if [ -z "${XDG_RUNTIME_DIR:-}" ] && [ -d "/run/user/$(id -u)" ]; then
  XDG_RUNTIME_DIR="/run/user/$(id -u)"
  export XDG_RUNTIME_DIR
fi

sc() {
  echo "+ systemctl --user $*"
  "$SYSTEMCTL" --user "$@"
}

if [ "$UNINSTALL" = 1 ]; then
  echo "== uninstall $UNIT"
  # disable --now stops only the daemon (KillMode=process): running jobs finish on their own and are
  # collected by the next daemon for this agent, if one is ever started again.
  sc disable --now "$UNIT" || echo "  (unit was not enabled/active)"
  others=""
  if [ -d "$UNIT_DIR/default.target.wants" ]; then
    others="$(find "$UNIT_DIR/default.target.wants" -maxdepth 1 -name 'agent-jobs@*.service' ! -name "$UNIT" -printf '%f ' 2>/dev/null || true)"
  fi
  if [ -n "$others" ]; then
    echo "  other instances still enabled ($others): keeping $UNIT_DST and $BIN"
  else
    if [ -f "$UNIT_DST" ]; then rm -f "$UNIT_DST"; echo "  removed $UNIT_DST"; fi
    if [ -f "$BIN" ]; then rm -f "$BIN"; echo "  removed $BIN"; fi
  fi
  sc daemon-reload
  echo "  kept: env file $ENV_FILE (if any) and state (~/.agent-jobs/$INSTANCE or \$AGENT_JOBS_HOME)"
  echo "uninstall done"
  exit 0
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
  (cd "$REPO_ROOT" && go build -trimpath -o "$tmp" ./cmd/agent-jobs)
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
  install -m 0644 "$UNIT_SRC" "$UNIT_DST"
  changed=1
  echo "  installed"
fi

echo "== env file $ENV_FILE"
if [ -e "$ENV_FILE" ]; then
  echo "  exists, left as is"
else
  umask 077
  cat >"$ENV_FILE" <<ENVFILE
# Environment for agent-jobs@$INSTANCE (systemd EnvironmentFile: KEY=VALUE lines, no 'export').
# AGENT_JOBS_HOME=$HOME/.agent-jobs
# AGENT_JOBS_CLAUDE_BIN=$HOME/.local/bin/claude
# AGENT_JOBS_TMUX_BIN=/usr/bin/tmux
ENVFILE
  echo "  created (all settings commented out)"
fi

echo "== state (agent-jobs init --agent $INSTANCE)"
# Same environment as the service: the env file may move the state root.
(
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
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
