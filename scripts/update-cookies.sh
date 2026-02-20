#!/bin/bash
# Update YouTube cookies on the server.
# Run on Mac: ./scripts/update-cookies.sh [profile]
#
# Profiles (Arc browser):
#   Default, "Profile 2", "Profile 3"
#
# Examples:
#   ./scripts/update-cookies.sh              # Default profile
#   ./scripts/update-cookies.sh "Profile 2"  # Profile 2
set -euo pipefail

SERVER="gustafv@35.238.12.191"
REMOTE_PATH="/srv/etc/cookies.txt"
LOCAL_COOKIES="/tmp/yt-cookies.txt"

# Arc profile (default or from argument)
ARC_PROFILE="${1:-Default}"
ARC_PROFILE_PATH="$HOME/Library/Application Support/Arc/User Data/$ARC_PROFILE"

if [ ! -d "$ARC_PROFILE_PATH" ]; then
    echo "ERROR: Arc profile not found: $ARC_PROFILE_PATH"
    echo "Available profiles:"
    ls -d "$HOME/Library/Application Support/Arc/User Data/"Profile* "$HOME/Library/Application Support/Arc/User Data/Default" 2>/dev/null | xargs -I{} basename "{}"
    exit 1
fi

echo "=== YouTube Cookies Update ==="
echo "   Arc profile: $ARC_PROFILE"

# 1. Extract cookies from Arc
echo "1. Extracting cookies from Arc..."
yt-dlp --cookies-from-browser "chrome::$ARC_PROFILE_PATH" --cookies "$LOCAL_COOKIES" --skip-download "https://www.youtube.com/watch?v=dQw4w9WgXcQ"

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
ssh "$SERVER" "sudo docker restart turnip"

# 4. Cleanup
rm -f "$LOCAL_COOKIES"

echo "Done! Cookies updated."
