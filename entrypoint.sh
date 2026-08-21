#!/bin/sh
# Container entrypoint wrapper.
#
# Starts the bgutil POT provider HTTP server in the background so yt-dlp can
# fetch YouTube "proof-of-origin" tokens. Without it, YouTube intermittently
# rejects requests for the web client, and yt-dlp calls fail or hang until the
# 5-minute timeout kills them ("signal: killed") — which looked like "some
# videos work, some don't". The yt-dlp plugin auto-discovers the server on
# 127.0.0.1:4416, so no yt-dlp arguments change.
#
# The server is best-effort: if it never built or keeps crashing, yt-dlp simply
# degrades to the old no-token behavior (public videos still download). We keep
# it alive with a respawn loop; the loop inherits the container's stdout/stderr
# so its output shows up in `docker logs`.
if [ -f /srv/bgutil/server/build/main.js ]; then
    (
        while true; do
            node /srv/bgutil/server/build/main.js
            echo "[entrypoint] bgutil POT server exited, restarting in 5s"
            sleep 5
        done
    ) &
else
    echo "[entrypoint] bgutil POT server not built, yt-dlp runs without POT tokens"
fi

# Hand off to the base image init (drops privileges, runs the passed command).
exec /init.sh "$@"
