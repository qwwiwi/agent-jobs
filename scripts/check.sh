#!/usr/bin/env bash
# Local checks (no CI): commit/PR titles, changelog fragments, formatting, vet, tests.
set -euo pipefail
cd "$(dirname "$0")/.."

# Run from a git hook, git exports GIT_DIR/GIT_INDEX_FILE/…; tests that run git in temp repos would
# then write into this repository. Drop every GIT_* variable first (TestMain does it too).
while IFS= read -r v; do unset "$v"; done < <(compgen -e | grep '^GIT_' || true)

# go test's own default is 10 minutes per package binary, and internal/jobs alone takes ~14 minutes on a
# busy host (#97): set the limit explicitly. Override with GO_TEST_TIMEOUT (a go duration).
GO_TEST_TIMEOUT="${GO_TEST_TIMEOUT:-45m}"

echo "== commit messages"
scripts/check-commit-messages.sh

if [ "${PR_TITLE+x}" = x ]; then
  echo "== PR title"
  bash scripts/check-pr-title.sh "$PR_TITLE"
fi

echo "== changelog fragments"
go run ./cmd/changelog --validate

echo "== gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "gofmt needed:"
  echo "$unformatted"
  exit 1
fi

echo "== go vet"
go vet ./...

# One run of the suite serves both steps (#172): its -json output goes to a file kept for this run only
# (manifest-check reads it below) and, rendered back to plain test output, to the log.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "== go test"
test_status=0
# pipefail makes the pipeline fail when go test does; a failing renderer or tee fails it too.
go test -count=1 -timeout "$GO_TEST_TIMEOUT" -json ./... | tee "$tmp/go-test.json" | scripts/render-test-json.sh || test_status=$?
if [ "$test_status" -ne 0 ]; then
  echo "go test failed (exit $test_status)"
  exit "$test_status"
fi

# The judge's manifest-check (examples/manifest-check/README.md): every live requirement needs a passing
# tagged test. A checkout without a checklist has nothing to check; the judge's run writes the approved one.
echo "== manifest-check"
if [ -f brief/CHECKLIST.md ]; then
  scripts/manifest-check.sh "$tmp/go-test.json"
else
  echo "skipped: no brief/CHECKLIST.md"
fi

echo "check.sh: OK"
