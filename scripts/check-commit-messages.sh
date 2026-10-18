#!/usr/bin/env bash
# Refuse commits about to be pushed whose message contains Cyrillic (this repo is English-only).
# Checks origin/main..HEAD, or the last 20 commits when origin/main is unknown.
# Portable: bash 3.2 and POSIX grep (Cyrillic U+0400-U+04FF starts with UTF-8 lead byte 0xD0-0xD3).
set -euo pipefail

if git rev-parse --verify -q origin/main >/dev/null; then
  commits="$(git rev-list origin/main..HEAD)"
else
  commits="$(git rev-list -n 20 HEAD)"
fi

cyrillic="$(printf '[\320-\323]')"
bad=""
for c in $commits; do
  if git log -1 --format=%B "$c" | LC_ALL=C grep -q "$cyrillic"; then
    bad="${bad}  $(git log -1 --format='%h %s' "$c")
"
  fi
done

if [ -n "$bad" ]; then
  echo "Commit messages must be in English; these contain Cyrillic:"
  printf '%s' "$bad"
  echo "Reword them (git commit --amend / git rebase) before pushing."
  exit 1
fi
