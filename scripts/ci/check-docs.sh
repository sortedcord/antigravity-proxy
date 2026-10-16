#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

python3 scripts/ci/check-docs.py
