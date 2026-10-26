#!/bin/sh
# manifest-check.sh for agent-jobs itself (Wave F, F6): every live requirement of brief/CHECKLIST.md
# needs a test tagged with its ID that passed in this run. The check is the reference script
# examples/manifest-check/manifest-check.sh (tested by its own go tests); this wrapper prepares Go for
# the judge's run, which happens in a plain copy of the tree (no .git), with a scrubbed environment
# (PATH kept) and a throwaway HOME, so the Go module and build caches start cold, and runs the tagged
# tests first to stay well inside acceptance_timeout_minutes.
#
# Usage: scripts/manifest-check.sh [go-test-json-file]   (see the reference script)
# Environment: MANIFEST_CHECKLIST, MANIFEST_GO_TEST_ARGS (passed on); MANIFEST_DOWNLOAD_TIMEOUT, the
#   seconds `go mod download` may take with a cold module cache (default 300).
# Exit: as the reference script; 2 also when the modules cannot be downloaded (a cold module cache
#   needs network access to the Go module proxy, proxy.golang.org unless GOPROXY says otherwise).
#
# POSIX sh only.
set -u

# Run from the repository root, as the reference script expects (the judge's wrapper does).
root=$(cd "$(dirname "$0")/.." && pwd) || exit 2

if [ $# -eq 0 ]; then
  # Never fetch another Go toolchain: the installed go must satisfy go.mod.
  GOTOOLCHAIN=local
  export GOTOOLCHAIN
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/manifest-repo.XXXXXX") || exit 2
  trap 'rm -rf "$tmp"' EXIT
  trap 'exit 2' HUP INT TERM

  # Both runs below judge against one copy of the checklist taken before any test runs: a test that
  # edits or removes the working tree's checklist cannot drop a requirement from the second run.
  checklist=${MANIFEST_CHECKLIST:-brief/CHECKLIST.md}
  if [ -f "$checklist" ]; then
    cp "$checklist" "$tmp/CHECKLIST.md" || exit 2
    MANIFEST_CHECKLIST=$tmp/CHECKLIST.md
    export MANIFEST_CHECKLIST
  fi

  # Fetch the modules first, bounded, so a cold cache without network fails here with a clear line
  # (exit 2) instead of as build failures of every package (every requirement MISSING, exit 1).
  # A watchdog stops the download after the limit, so no timeout(1) is needed.
  limit=${MANIFEST_DOWNLOAD_TIMEOUT:-300}
  case $limit in
    '' | *[!0-9]*)
      echo "manifest-check: MANIFEST_DOWNLOAD_TIMEOUT must be a whole number of seconds, not '$limit'" >&2
      exit 2
      ;;
  esac
  go mod download &
  pid=$!
  (
    trap - EXIT HUP INT TERM
    i=0
    while [ "$i" -lt "$limit" ]; do
      sleep 1
      kill -0 "$pid" 2>/dev/null || exit 0
      i=$((i + 1))
    done
    : >"$tmp/timed-out"
    kill "$pid"
  ) >/dev/null 2>&1 &
  watchdog=$!
  wait "$pid"
  status=$?
  kill "$watchdog" 2>/dev/null
  if [ "$status" -ne 0 ] && [ -f "$tmp/timed-out" ]; then
    echo "manifest-check: go mod download did not finish within ${limit}s (MANIFEST_DOWNLOAD_TIMEOUT): a cold module cache needs network access to the Go module proxy" >&2
    exit 2
  fi
  if [ "$status" -ne 0 ]; then
    echo "manifest-check: go mod download failed (exit $status): a cold module cache needs network access to the Go module proxy" >&2
    exit 2
  fi

  # Fast path: run only the top-level tests whose body holds a requirement tag ("[R01]", "[G02]"), so
  # the long untagged suites do not run. The reference still decides what passed in this run. A tag
  # this scan misses (one outside a Test function, say) leaves its requirement MISSING: then the whole
  # suite runs, so the scan can make the check faster, never stricter.
  if [ -z "${MANIFEST_GO_TEST_ARGS:-}" ]; then
    tagged=$(find . -name '.?*' -prune -o -name '*_test.go' -type f -exec awk '
      FNR == 1 { cur = "" }
      /^func Test[A-Za-z0-9_]*\(/ { cur = $2; sub(/\(.*/, "", cur) }
      /^}/ { cur = "" }
      cur != "" && /"[^"]*\[[RG][0-9][0-9]+i?\]/ { print cur }' {} + | sort -u | paste -s -d '|' -)
    if [ -n "$tagged" ]; then
      MANIFEST_GO_TEST_ARGS="-run ^($tagged)\$ ./..."
      export MANIFEST_GO_TEST_ARGS
      /bin/sh "$root/examples/manifest-check/manifest-check.sh"
      status=$?
      if [ "$status" -ne 1 ]; then
        exit "$status"
      fi
      echo "manifest-check: a live requirement has no passing test among the tagged tests; running the whole suite"
      unset MANIFEST_GO_TEST_ARGS
    fi
  fi
fi

/bin/sh "$root/examples/manifest-check/manifest-check.sh" "$@"
exit $?
