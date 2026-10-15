#!/bin/sh
# Build snapshot release archives locally and verify contents and checksums.
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

dist_dir="dist"
rm -rf "$dist_dir"
mkdir -p "$dist_dir"

package="antigravity-proxy/internal/buildinfo"
ldflags="-s -w -X $package.Version=$version -X $package.Commit=$commit -X $package.Date=$date"

targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"

for target in $targets; do
  os="${target%%/*}"
  arch="${target##*/}"
  ext=""
  if [ "$os" = "windows" ]; then
    ext=".exe"
  fi

  target_dir="$dist_dir/build_${os}_${arch}"
  mkdir -p "$target_dir"
  binary_name="antigravity-proxy$ext"
  binary_path="$target_dir/$binary_name"

  printf 'Building %s/%s...\n' "$os" "$arch"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath \
    -ldflags="$ldflags" \
    -o "$binary_path" ./cmd/antigravity-proxy

  # Stage archive contents
  stage_dir="$dist_dir/stage_${os}_${arch}"
  rm -rf "$stage_dir"
  mkdir -p "$stage_dir"
  cp "$binary_path" "$stage_dir/"
  cp LICENSE README.md "$stage_dir/"
  mkdir -p "$stage_dir/docs"
  cp docs/deployment.md docs/versioning.md "$stage_dir/docs/"

  archive_base="antigravity-proxy_${version}_${os}_${arch}"
  if [ "$os" = "windows" ]; then
    archive_name="${archive_base}.zip"
    archive_path="$dist_dir/$archive_name"
    python3 -c "
import zipfile, os
stage = '$stage_dir'
with zipfile.ZipFile('$archive_path', 'w', zipfile.ZIP_DEFLATED) as z:
    for root, _, files in os.walk(stage):
        for f in files:
            p = os.path.join(root, f)
            arcname = os.path.relpath(p, stage)
            z.write(p, arcname)
"
  else
    archive_name="${archive_base}.tar.gz"
    archive_path="$dist_dir/$archive_name"
    tar -czf "$archive_path" -C "$stage_dir" .
  fi

  rm -rf "$stage_dir" "$target_dir"
done

# Generate SHA256 checksums
(
  cd "$dist_dir"
  sha256sum antigravity-proxy_* > checksums.txt
)

printf 'Generated snapshot archives in %s:\n' "$dist_dir"
ls -lh "$dist_dir"

# Verify host archive extraction and execution
host_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
host_arch="$(uname -m)"
case "$host_arch" in
  x86_64) host_arch="amd64" ;;
  aarch64|arm64) host_arch="arm64" ;;
esac

host_archive="$dist_dir/antigravity-proxy_${version}_${host_os}_${host_arch}.tar.gz"
if [ -f "$host_archive" ]; then
  test_extract=$(mktemp -d)
  trap 'rm -rf "$test_extract"' EXIT
  tar -xzf "$host_archive" -C "$test_extract"

  # Verify file presence and strict absence of untracked secrets
  for f in antigravity-proxy LICENSE README.md docs/deployment.md docs/versioning.md; do
    if [ ! -f "$test_extract/$f" ]; then
      printf 'Missing expected file in archive: %s\n' "$f" >&2
      exit 1
    fi
  done

  # Verify no auth files, environment secrets, or extra binaries
  if find "$test_extract" -name "*.auth.json" -o -name "*.env" -o -name "*.log" | grep -q .; then
    printf 'Forbidden files found in archive!\n' >&2
    exit 1
  fi

  # Execute and verify version output and full lifecycle smoke test
  python3 scripts/test-packaged-binary.py "$test_extract/antigravity-proxy"
  rm -rf "$test_extract"
  trap - EXIT
fi

printf 'Snapshot packaging and artifact verification passed.\n'
