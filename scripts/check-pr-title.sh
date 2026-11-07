#!/usr/bin/env bash
# Can also be sourced: check_pr_title "$title". Never evaluate title text as shell code.
check_pr_title() {
  local title="${1-}" cyrillic
  cyrillic="$(printf '[\320-\323]')"
  if [ -z "${title//[[:space:]]/}" ] || [[ "$title" == *$'\n'* || "$title" == *$'\r'* ]]; then
    echo "PR title must be a non-empty single line." >&2
    return 1
  fi
  if printf '%s\n' "$title" | LC_ALL=C grep -q "$cyrillic"; then
    echo "PR title must be in English; it contains Cyrillic (same rule as commit messages)." >&2
    return 1
  fi
  if [[ "$title" == *'$('* || "$title" == *'`'* ]]; then
    echo 'PR title must not contain shell substitution syntax: $( or backticks.' >&2
    return 1
  fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -euo pipefail
  if [ "$#" -eq 1 ]; then
    check_pr_title "$1"
  elif [ "$#" -eq 0 ] && [ "${PR_TITLE+x}" = x ]; then
    check_pr_title "$PR_TITLE"
  else
    echo 'Usage: check-pr-title.sh TITLE (or set PR_TITLE)' >&2
    exit 1
  fi
fi
