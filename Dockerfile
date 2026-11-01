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
    PIPX_BIN_DIR="/home/nemo/.local/bin"

# A named volume is the intended home (docker run -v nemo-home:/home/nemo);
# declaring it keeps `docker run` without -v working too, as an anonymous
# volume instead of a read-only home.
VOLUME ["/home/nemo"]

WORKDIR /work
USER nemo

ENTRYPOINT ["/usr/local/bin/nemo", "--config", "/config.json"]
