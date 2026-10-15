#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

python3 scripts/ci/check-security.py

if command -v govulncheck >/dev/null 2>&1; then
  echo "Running govulncheck..."
  govulncheck ./...
fi
