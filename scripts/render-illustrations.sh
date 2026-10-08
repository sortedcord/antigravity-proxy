#!/bin/sh
# Render docs/illustrations/*.html to docs/assets/illustrations/*.svg.
# Requires chromium and pdftocairo (poppler-utils).
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
src="$root/docs/illustrations"
out="$root/docs/assets/illustrations"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$out"

for html in "$src"/*.html; do
  name=$(basename "$html" .html)
  chromium --headless --no-sandbox --disable-gpu --no-pdf-header-footer \
    --virtual-time-budget=3000 --print-to-pdf="$tmp/$name.pdf" "file://$html" 2>/dev/null
  pdftocairo -svg "$tmp/$name.pdf" "$tmp/$name.svg"
  # PDF units are pt (0.75 CSS px); restore CSS-pixel width/height, keep viewBox.
  awk 'NR <= 3 && /<svg / {
         match($0, /width="[0-9.]+"/);  w = substr($0, RSTART + 7, RLENGTH - 8)
         match($0, /height="[0-9.]+"/); h = substr($0, RSTART + 8, RLENGTH - 9)
         sub(/width="[0-9.]+"/,  "width=\""  int(w / 0.75 + 0.5) "\"")
         sub(/height="[0-9.]+"/, "height=\"" int(h / 0.75 + 0.5) "\"")
       } { print }' "$tmp/$name.svg" >"$out/$name.svg"
  echo "$out/$name.svg"
done
