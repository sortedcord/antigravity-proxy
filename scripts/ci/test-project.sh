#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

echo "Validating Docker container tag rules..."
python3 scripts/test-docker-tags.py

echo "Validating accounts, admin, and routing configuration..."
go test -race -run '^(TestCLI|TestConfig|TestLogin|TestAdmin|TestModel)' ./cmd/antigravity-proxy ./internal/config ./internal/proxy

echo "Project checks passed."
