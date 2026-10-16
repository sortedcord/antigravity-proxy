#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

echo "Checking module hygiene..."
status_before=$(git status --porcelain go.mod go.sum 2>/dev/null || true)
go mod tidy
status_after=$(git status --porcelain go.mod go.sum 2>/dev/null || true)

if [ "$status_before" != "$status_after" ]; then
  echo "Error: go.mod or go.sum is not tidy. Run 'go mod tidy' and commit the changes." >&2
  git diff go.mod go.sum >&2 || true
  git checkout go.mod go.sum 2>/dev/null || true
  exit 1
fi

echo "Module hygiene check passed."
