#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

coverage_dir="${COVERAGE_DIR:-}"
if [ -n "$coverage_dir" ]; then
  mkdir -p "$coverage_dir"
  go test -race -covermode=atomic -coverprofile="$coverage_dir/coverage.out" ./...
else
  go test -race -cover ./...
fi
