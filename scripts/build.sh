#!/bin/sh
# Build native binaries with explicit identity; release tooling may override metadata.
set -eu
cd "$(dirname "$0")/.."

base_version=$(tr -d '\n\r' < internal/buildinfo/VERSION)
case "$base_version" in
  v*) ;;
  *) base_version="v$base_version" ;;
esac
version=${VERSION:-${base_version}-dev}
commit=${COMMIT:-$(git rev-parse HEAD 2>/dev/null || printf unknown)}
if [ -z "${COMMIT:-}" ] && [ -n "$(git status --porcelain 2>/dev/null)" ]; then
  commit="${commit}-dirty"
fi
date=${BUILD_DATE:-$(git show -s --format=%cI HEAD 2>/dev/null || printf unknown)}
# These values enter linker arguments; reject whitespace and arbitrary flag content.
case "$version$commit$date" in
  *[!a-zA-Z0-9.+:_-]*) printf '%s\n' 'invalid build metadata' >&2; exit 1 ;;
esac
output=${1:-bin/antigravity-proxy}
mkdir -p "$(dirname "$output")"
package=antigravity-proxy/internal/buildinfo
CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w -X $package.Version=$version -X $package.Commit=$commit -X $package.Date=$date" \
  -o "$output" ./cmd/antigravity-proxy
