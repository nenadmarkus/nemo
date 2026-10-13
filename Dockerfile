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
    && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home nemo
COPY --from=build --chown=nemo:nemo /nemo /usr/local/bin/nemo

# set up golang dev
COPY --from=golang:1.27 /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:/home/nemo/go/bin:${PATH}" \
    GOPATH="/home/nemo/go"
RUN go install golang.org/x/tools/gopls@latest \
    && go install honnef.co/go/tools/cmd/staticcheck@latest

# set up C# dev
COPY --from=mcr.microsoft.com/dotnet/sdk:10.0 /usr/share/dotnet /usr/share/dotnet
ENV DOTNET_ROOT=/usr/share/dotnet \
    PATH="/usr/share/dotnet:/home/nemo/.dotnet/tools:${PATH}" \
    DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_NOLOGO=1

WORKDIR /work
USER nemo

ENTRYPOINT ["/usr/local/bin/nemo", "--config", "/config.json"]
