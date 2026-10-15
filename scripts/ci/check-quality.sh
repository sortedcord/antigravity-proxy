#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

echo "Checking code formatting..."
unformatted=$(gofmt -l cmd internal)
if [ -n "$unformatted" ]; then
  printf 'Unformatted files:\n%s\n' "$unformatted" >&2
  exit 1
fi

echo "Running go vet..."
go vet ./...

echo "Checking module hygiene..."
./scripts/ci/check-modules.sh

echo "Running strict linters..."
./scripts/lint.sh

echo "Quality checks passed."
