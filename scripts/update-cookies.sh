#!/bin/bash
# Update YouTube cookies on the server.
# Run on Mac: ./scripts/update-cookies.sh
set -euo pipefail

SERVER="35.238.12.191"
REMOTE_PATH="/srv/etc/cookies.txt"
LOCAL_COOKIES="/tmp/yt-cookies.txt"

echo "=== YouTube Cookies Update ==="

# 1. Extract cookies from Chrome
echo "1. Extracting cookies from Chrome..."
yt-dlp --cookies-from-browser chrome --cookies "$LOCAL_COOKIES" --skip-download "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

if [ ! -f "$LOCAL_COOKIES" ]; then
    echo "ERROR: Failed to extract cookies"
    exit 1
fi

echo "   Cookies saved to $LOCAL_COOKIES ($(wc -c < "$LOCAL_COOKIES") bytes)"

# 2. Upload to server
echo "2. Uploading to server..."
scp "$LOCAL_COOKIES" "$SERVER:$REMOTE_PATH"

# 3. Restart container
echo "3. Restarting container..."
ssh "$SERVER" "docker restart turnip"

# 4. Cleanup
rm -f "$LOCAL_COOKIES"

echo "Done! Cookies updated."
