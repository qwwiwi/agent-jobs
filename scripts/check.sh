#!/usr/bin/env bash
# Local checks (no CI): formatting, vet, tests. Wired to .githooks/pre-push.
set -euo pipefail
cd "$(dirname "$0")/.."

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

echo "check.sh: OK"
