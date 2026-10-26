#!/usr/bin/env bash
# Local checks (no CI): English commit messages, formatting, vet, tests. Wired to .githooks/pre-push.
set -euo pipefail
cd "$(dirname "$0")/.."

# Run from a git hook, git exports GIT_DIR/GIT_INDEX_FILE/…; tests that run git in temp repos would
# then write into this repository. Drop every GIT_* variable first (TestMain does it too).
while IFS= read -r v; do unset "$v"; done < <(compgen -e | grep '^GIT_' || true)

echo "== commit messages"
scripts/check-commit-messages.sh

echo "== gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "gofmt needed:"
  echo "$unformatted"
  exit 1
fi

echo "== go vet"
go vet ./...

echo "== go test"
go test -count=1 ./...

# The judge's manifest-check (examples/manifest-check/README.md): every live requirement needs a passing
# tagged test. A checkout without a checklist has nothing to check; the judge's run writes the approved one.
echo "== manifest-check"
if [ -f brief/CHECKLIST.md ]; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  go test -count=1 -json ./... > "$tmp/go-test.json" || true # failing tests are reported by the go test step
  scripts/manifest-check.sh "$tmp/go-test.json"
else
  echo "skipped: no brief/CHECKLIST.md"
fi

echo "check.sh: OK"
