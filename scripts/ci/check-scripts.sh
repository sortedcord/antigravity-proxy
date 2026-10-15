#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

echo "Checking shell scripts syntax..."
for script in scripts/*.sh scripts/ci/*.sh; do
  if [ -f "$script" ]; then
    printf '  checking %s...\n' "$script"
    sh -n "$script"
    if command -v shellcheck >/dev/null 2>&1; then
      shellcheck -x "$script"
    fi
  fi
done

echo "Checking Python scripts syntax..."
for script in scripts/*.py scripts/ci/*.py; do
  if [ -f "$script" ]; then
    printf '  checking %s...\n' "$script"
    python3 -m py_compile "$script"
  fi
done

echo "Script checks passed."
