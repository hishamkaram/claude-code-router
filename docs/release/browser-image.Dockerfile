FROM mcr.microsoft.com/playwright:v1.54.1-jammy@sha256:f72b8d294edee6beadacfc1868abf746dbc31316b1927f39f393107053c58bd1

LABEL org.opencontainers.image.title="CCR managed browser preview"
LABEL org.opencontainers.image.description="Chromium runtime image for Claude Code Router managed computer-use preview"
LABEL org.opencontainers.image.source="https://github.com/hishamkaram/claude-code-router"
LABEL org.opencontainers.image.licenses="MIT"

RUN browser_binary="$(find /ms-playwright -type f -path '*/chrome-linux/chrome' -print -quit)" \
    && test -n "$browser_binary" \
    && ln -s "$browser_binary" /usr/local/bin/ccr-chromium

COPY docs/release/cua-browser-entrypoint.js /usr/local/lib/ccr-cua-browser.js
RUN command -v node \
    && printf '%s\n' '#!/bin/sh' 'exec node /usr/local/lib/ccr-cua-browser.js "$@"' > /usr/local/bin/chromium \
    && chmod 0555 /usr/local/bin/chromium /usr/local/lib/ccr-cua-browser.js

# The executor runs as an unmapped non-root UID with a read-only rootfs. Keep
# Chromium's cache, crashpad, and XDG state on the /tmp tmpfs it mounts.
ENV HOME=/tmp \
    XDG_CACHE_HOME=/tmp/.cache \
    XDG_CONFIG_HOME=/tmp/.config \
    XDG_RUNTIME_DIR=/tmp
USER 65532:65532
WORKDIR /tmp
