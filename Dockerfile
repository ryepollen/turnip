FROM umputun/baseimage:buildgo-latest as build

ARG GIT_BRANCH
ARG GITHUB_SHA
ARG CI

ENV CGO_ENABLED=0

ADD . /build
WORKDIR /build

RUN \
    if [ -z "$CI" ] ; then \
    echo "runs outside of CI" && version=$(git rev-parse --abbrev-ref HEAD)-$(git log -1 --format=%h)-$(date +%Y%m%dT%H:%M:%S); \
    else version=${GIT_BRANCH}-${GITHUB_SHA:0:7}-$(date +%Y%m%dT%H:%M:%S); fi && \
    echo "version=$version" && \
    cd app && go build -o /build/feed-master -ldflags "-X main.revision=${version} -s -w"



FROM umputun/baseimage:app-latest

# enables automatic changelog generation by tools like Dependabot
LABEL org.opencontainers.image.source="https://github.com/umputun/feed-master"

COPY --from=build /build/feed-master /srv/feed-master
COPY app/webapp /srv/webapp
COPY entrypoint.sh /srv/entrypoint.sh
RUN \
    chown -R app:app /srv && \
    chmod +x /srv/feed-master /srv/entrypoint.sh
RUN apk --no-cache add ca-certificates ffmpeg python3 py3-pip deno nodejs npm git
# yt-dlp: latest. POT provider plugin: pinned to match the server built below —
# the bgutil plugin and its server must be the same version (see POTVER).
RUN pip3 install --break-system-packages --no-cache-dir --no-deps -U yt-dlp && \
    pip3 install --break-system-packages --no-cache-dir --no-deps "bgutil-ytdlp-pot-provider==1.3.2"
# bgutil POT provider server: gives yt-dlp YouTube proof-of-origin tokens.
# Built here, started in the background by entrypoint.sh (HTTP server on :4416).
RUN git clone --single-branch --branch 1.3.2 --depth 1 \
        https://github.com/Brainicism/bgutil-ytdlp-pot-provider.git /srv/bgutil && \
    cd /srv/bgutil/server && npm ci && npx tsc
RUN npm install -g vot-cli
WORKDIR /srv

CMD ["/srv/feed-master"]
ENTRYPOINT ["/srv/entrypoint.sh"]
