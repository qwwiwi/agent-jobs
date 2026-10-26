#!/bin/sh
# manifest-check.sh for agent-jobs itself (Wave F, F6): every live requirement of brief/CHECKLIST.md
# needs a test tagged with its ID that passed in this run. The check is the reference script
# examples/manifest-check/manifest-check.sh (tested by its own go tests); this wrapper only prepares Go
# for the judge's run, which happens in a plain copy of the tree (no .git), with a scrubbed environment
# (PATH kept) and a throwaway HOME, so the Go module and build caches start cold.
#
# Usage: scripts/manifest-check.sh [go-test-json-file]   (see the reference script)
# Environment: MANIFEST_CHECKLIST, MANIFEST_GO_TEST_ARGS (passed on); MANIFEST_DOWNLOAD_TIMEOUT, the
#   seconds `go mod download` may take with a cold module cache (default 300; needs `timeout`).
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
  # Fetch the modules first, bounded, so a cold cache without network fails here with a clear line
  # (exit 2) instead of as build failures of every package (every requirement MISSING, exit 1).
  limit=${MANIFEST_DOWNLOAD_TIMEOUT:-300}
  if command -v timeout >/dev/null 2>&1; then
    timeout "$limit" go mod download
  else
    go mod download
  fi
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "manifest-check: go mod download failed (exit $status): a cold module cache needs network access to the Go module proxy" >&2
    exit 2
  fi
fi

exec /bin/sh "$root/examples/manifest-check/manifest-check.sh" "$@"
