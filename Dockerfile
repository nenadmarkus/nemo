FROM golang:1.27 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o /nemo ./cmd/nemo


FROM buildpack-deps:noble

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    jq \
    ripgrep \
    fd-find \
    tree \
    less \
    vim-tiny \
    nano \
    unzip \
    zip \
    file \
    poppler-utils \
    curl \
    wget \
    git-lfs \
    openssh-client \
    procps \
    psmisc \
    lsof \
    net-tools \
    iputils-ping \
    dnsutils \
    rsync \
    patch \
    diffutils \
    make \
    gcc \
    g++ \
    python3 \
    python3-pip \
    python3-venv \
    pipx \
    && rm -rf /var/lib/apt/lists/*

# Node.js LTS from NodeSource; Ubuntu's nodejs is too old for current tooling.
RUN curl -fsSL https://deb.nodesource.com/setup_26.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && node --version \
    && npm --version

# set up headless Chromium (Playwright): the browser bundles go to /opt, not
# $HOME, because $HOME is replaced by an empty volume at run time and would
# shadow anything built into it. For the same reason the playwright CLI and
# library are installed globally into /usr/local instead of the npm prefix
# under $HOME.
RUN npm install -g --prefix /usr/local playwright@1.64.1 \
    && PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright /usr/local/bin/playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/* /root/.npm

# Chromium runs here as an unprivileged user with no capabilities and no setuid
# helper, so its sandbox is unavailable and a bare launch aborts. The wrapper
# supplies the flags that make the browser usable from a shell; Playwright and
# other CDP clients launch the browser themselves and do not use it. There is
# no D-Bus or UPower in the container, so Chromium logs a dozen harmless errors
# on every run; --log-level=3 mutes them, and a later --log-level=0 on the
# command line brings them back.
RUN set -eu; \
    chrome="$(find /opt/ms-playwright -maxdepth 4 -type f -name chrome -perm -u+x | sort | tail -n 1)"; \
    test -x "$chrome"; \
    printf '%s\n' '#!/bin/sh' "exec \"$chrome\" --headless --no-sandbox --disable-dev-shm-usage --disable-gpu --log-level=3 \"\$@\"" > /usr/local/bin/chromium; \
    chmod 0755 /usr/local/bin/chromium

# Fail the build, not the agent's first screenshot, if a shared library the
# browser needs is missing.
RUN mkdir -p /tmp/smoke-home \
    && printf '<p>headless ok</p>' > /tmp/smoke.html \
    && HOME=/tmp/smoke-home chromium --dump-dom file:///tmp/smoke.html | grep -q 'headless ok' \
    && chromium --version \
    && rm -rf /tmp/smoke.html /tmp/smoke-home

RUN useradd --create-home nemo
COPY --from=build --chown=nemo:nemo /nemo /usr/local/bin/nemo

# set up golang dev: the toolchain is shared and read-only, so its tools go
# to /usr/local/bin — not $HOME, which is replaced by a writable volume at
# run time. Build state stays out of /home/nemo too, so a fresh home volume
# is empty and owned by nemo.
COPY --from=golang:1.27 /usr/local/go /usr/local/go
RUN GOBIN=/usr/local/bin GOPATH=/root/go /usr/local/go/bin/go install golang.org/x/tools/gopls@latest \
    && GOBIN=/usr/local/bin GOPATH=/root/go /usr/local/go/bin/go install honnef.co/go/tools/cmd/staticcheck@latest

# set up C# dev
COPY --from=mcr.microsoft.com/dotnet/sdk:10.0 /usr/share/dotnet /usr/share/dotnet

# Toolchain state and user-level installs live under $HOME, which is the one
# writable, persistent place in the container (a named volume mounted over
# /home/nemo). That keeps the read-only rootfs while letting the agent run
# `go install`, `dotnet tool install -g`, `npm install -g`, `pipx install`.
# NODE_PATH and PLAYWRIGHT_BROWSERS_PATH are the exception: they point into the
# image, at the global npm packages and the Chromium bundles baked in above.
ENV PATH="/usr/local/go/bin:/usr/share/dotnet:/home/nemo/.local/bin:/home/nemo/.npm-global/bin:/home/nemo/.dotnet/tools:/home/nemo/go/bin:${PATH}" \
    GOPATH="/home/nemo/go" \
    GOCACHE="/home/nemo/.cache/go-build" \
    GOTOOLCHAIN="auto" \
    XDG_CACHE_HOME="/home/nemo/.cache" \
    DOTNET_ROOT="/usr/share/dotnet" \
    DOTNET_CLI_HOME="/home/nemo" \
    DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_NOLOGO=1 \
    NUGET_PACKAGES="/home/nemo/.nuget/packages" \
    NPM_CONFIG_PREFIX="/home/nemo/.npm-global" \
    NPM_CONFIG_CACHE="/home/nemo/.npm" \
    PIP_CACHE_DIR="/home/nemo/.cache/pip" \
    PIPX_HOME="/home/nemo/.local/pipx" \
    PIPX_BIN_DIR="/home/nemo/.local/bin" \
    NODE_PATH="/usr/local/lib/node_modules" \
    PLAYWRIGHT_BROWSERS_PATH="/opt/ms-playwright"

# A named volume is the intended home (docker run -v nemo-home:/home/nemo);
# declaring it keeps `docker run` without -v working too, as an anonymous
# volume instead of a read-only home.
VOLUME ["/home/nemo"]

WORKDIR /work
USER nemo

ENTRYPOINT ["/usr/local/bin/nemo", "--config", "/config.json"]
