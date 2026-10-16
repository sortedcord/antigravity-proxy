#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

fuzz_time="${FUZZ_TIME:-15s}"

echo "Running fuzz tests (fuzztime=${fuzz_time})..."
go test ./internal/proxy -run='^$' -fuzz='^FuzzGenerationConfigSpan$' -fuzztime="$fuzz_time" -parallel=2

echo "Fuzz tests passed."
