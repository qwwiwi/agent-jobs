#!/bin/sh
# manifest-check.sh: a reference scripts/manifest-check.sh for Go repositories (agent-jobs Wave F, F6).
#
# Every live requirement of brief/CHECKLIST.md (an R, Ri or G row with contract live) needs at least
# one test tagged with its ID that passed in this run; otherwise the script exits non-zero. A test is
# tagged by its subtest name: t.Run("[R01] login works", ...) runs as TestLogin/[R01]_login_works.
# One name may carry several tags ("[R01][R03i] ...").
#
# Usage: scripts/manifest-check.sh [go-test-json-file]
#   With a file, it reads `go test -json` output from it; without one it runs
#   `go test -json $MANIFEST_GO_TEST_ARGS` (default ./...) itself, from the repository root.
# Environment: MANIFEST_CHECKLIST (default brief/CHECKLIST.md).
# Exit: 0 every live requirement has a passing tagged test; 1 at least one has none;
#       2 bad input (no checklist, no live requirement, no test output).
#
# POSIX sh and awk only: no jq, no bash.
set -u

checklist=${MANIFEST_CHECKLIST:-brief/CHECKLIST.md}
if [ ! -f "$checklist" ]; then
  echo "manifest-check: $checklist not found" >&2
  exit 2
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/manifest-check.XXXXXX") || exit 2
trap 'rm -rf "$tmp"' EXIT
trap 'exit 2' HUP INT TERM

if [ $# -gt 0 ]; then
  json=$1
  if [ ! -f "$json" ]; then
    echo "manifest-check: $json not found" >&2
    exit 2
  fi
else
  json=$tmp/go-test.json
  # shellcheck disable=SC2086 # the argument list is split on purpose
  go test -json ${MANIFEST_GO_TEST_ARGS:-./...} >"$json" 2>"$tmp/go-test.stderr"
  echo "manifest-check: go test exit $? (failing untagged tests are check.sh's business, not this script's)"
fi

# The live requirement IDs: table rows whose ID is R<nn>, R<nn>i or G<nn> and whose contract names
# live. Cells may hold escaped pipes (\|) and backslashes (\\), as agent-jobs writes them.
awk '
/^[ \t]*\|/ {
  line = $0
  gsub(/\\\\/, "\001", line)
  gsub(/\\\|/, "\002", line)
  n = split(line, c, "|")
  if (n < 5) next
  id = c[2]
  gsub(/^[ \t]+|[ \t]+$/, "", id)
  if (id !~ /^(R[0-9][0-9]+i?|G[0-9][0-9]+)$/) next
  contract = tolower(c[4])
  gsub(/,/, " ", contract)
  m = split(contract, w, /[ \t]+/)
  for (i = 1; i <= m; i++) if (w[i] == "live") { print id; break }
}' "$checklist" | sort -u >"$tmp/live"

if [ ! -s "$tmp/live" ]; then
  echo "manifest-check: $checklist has no live requirements" >&2
  exit 2
fi

if [ ! -s "$json" ]; then
  echo "manifest-check: no go test output" >&2
  if [ -s "$tmp/go-test.stderr" ]; then tail -n 20 "$tmp/go-test.stderr" >&2; fi
  exit 2
fi

# The tagged tests that passed or failed, one line per tag: <action> TAB <ID> TAB <test name>. A test
# event of go test -json carries "Action":"pass" or "Action":"fail" and the test name in "Test".
awk '
function testname(s,    name, ch) {
  sub(/.*"Test":"/, "", s)
  name = ""
  while (length(s) > 0) {
    ch = substr(s, 1, 1)
    if (ch == "\\") { name = name substr(s, 1, 2); s = substr(s, 3); continue }
    if (ch == "\"") break
    name = name ch
    s = substr(s, 2)
  }
  return name
}
/"Test":"/ && (/"Action":"pass"/ || /"Action":"fail"/) {
  action = "pass"
  if ($0 ~ /"Action":"fail"/) action = "fail"
  name = testname($0)
  rest = name
  while (match(rest, /\[[RG][0-9][0-9]+i?\]/)) {
    printf "%s\t%s\t%s\n", action, substr(rest, RSTART + 1, RLENGTH - 2), name
    rest = substr(rest, RSTART + RLENGTH)
  }
}' "$json" >"$tmp/tagged"

total=0
missing=0
while IFS= read -r id; do
  total=$((total + 1))
  hit=$(awk -F '\t' -v id="$id" '$1 == "pass" && $2 == id { print $3; exit }' "$tmp/tagged")
  if [ -n "$hit" ]; then
    echo "ok       $id  $hit"
  else
    missing=$((missing + 1))
    failed=$(awk -F '\t' -v id="$id" '$1 == "fail" && $2 == id { print $3; exit }' "$tmp/tagged")
    if [ -n "$failed" ]; then
      echo "MISSING  $id  no passing test tagged [$id] (failed: $failed)"
    else
      echo "MISSING  $id  no passing test tagged [$id]"
    fi
  fi
done <"$tmp/live"

# Tags that name no live requirement: a typo, or a deferred or dropped row.
awk -F '\t' 'NR == FNR { live[$1] = 1; next } !($2 in live) && !seen[$2]++ { print "note     [" $2 "] tags " $3 " but is not a live requirement" }' \
  "$tmp/live" "$tmp/tagged"

echo "manifest-check: $((total - missing)) of $total live requirements have a passing tagged test"
if [ "$missing" -gt 0 ]; then
  exit 1
fi
exit 0
