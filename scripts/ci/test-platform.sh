#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

os="$(uname -s 2>/dev/null || echo unknown)"
case "$os" in
  MINGW*|MSYS*|CYGWIN*|Windows_NT*)
    go test -v ./internal/atomicfile ./internal/status
    go build -trimpath -o antigravity-proxy.exe ./cmd/antigravity-proxy
    python3 scripts/test-packaged-binary.py ./antigravity-proxy.exe
    ;;
  *)
    ./scripts/build.sh
    python3 scripts/test-packaged-binary.py ./bin/antigravity-proxy
    ;;
esac
