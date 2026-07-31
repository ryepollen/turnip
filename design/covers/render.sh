#!/usr/bin/env bash
# Render a cover HTML to a square PNG via headless Chrome.
# Usage: ./render.sh offthplant [size]   (default size 3000)
set -euo pipefail
cd "$(dirname "$0")"
name="${1:?usage: render.sh <name> [size]}"
size="${2:-3000}"
scale=$(echo "scale=4; $size/1500" | bc)
chrome="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
out="out/${name}-${size}.png"
mkdir -p out
"$chrome" --headless=new --disable-gpu --hide-scrollbars \
  --force-device-scale-factor="$scale" \
  --window-size=1500,1500 \
  --default-background-color=00000000 \
  --screenshot="$PWD/$out" \
  "file://$PWD/${name}.html" >/dev/null 2>&1
echo "$out"
