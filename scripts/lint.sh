#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=2.14.0

matches_version() {
  command -v "$1" >/dev/null 2>&1 || return 1
  output=$("$1" version 2>/dev/null) || return 1
  case "$output" in
    *"version $version "*) return 0 ;;
    *) return 1 ;;
  esac
}

if [ -n "${GOLANGCI_LINT:-}" ]; then
  lint=$GOLANGCI_LINT
  if ! matches_version "$lint"; then
    printf 'GOLANGCI_LINT must point to golangci-lint v%s: %s\n' "$version" "$lint" >&2
    exit 1
  fi
elif matches_version golangci-lint; then
  lint=golangci-lint
elif [ -x "${XDG_CACHE_HOME:-$HOME/.cache}/codex-proxy/tools/golangci-lint-v$version/golangci-lint" ]; then
  lint="${XDG_CACHE_HOME:-$HOME/.cache}/codex-proxy/tools/golangci-lint-v$version/golangci-lint"
else
  cache_home="${XDG_CACHE_HOME:-${HOME:+$HOME/.cache}}"
  case "$cache_home" in
    /*) ;;
    *) echo 'Set XDG_CACHE_HOME to an absolute writable directory for pinned lint tooling' >&2; exit 1 ;;
  esac
  tool_dir="$cache_home/antigravity-proxy/tools/golangci-lint-v$version"
  lint="$tool_dir/golangci-lint"
  if ! matches_version "$lint"; then
    printf 'Installing golangci-lint v%s into %s (leaving PATH tooling unchanged)\n' "$version" "$tool_dir" >&2
    mkdir -p "$tool_dir"
    GOBIN="$tool_dir" go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$version"
    if ! matches_version "$lint"; then
      echo 'Installed lint binary does not report the expected version' >&2
      exit 1
    fi
  fi
fi

"$lint" config verify
"$lint" run ./...
format_diff=$("$lint" fmt --diff cmd internal)
if [ -n "$format_diff" ]; then
  printf '%s\n' "$format_diff" >&2
  exit 1
fi
