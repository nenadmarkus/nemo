FROM golang:1.27 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o /nemo ./cmd/nemo


FROM ubuntu:24.04

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home nemo

COPY --from=build --chown=nemo:nemo /nemo /usr/local/bin/nemo

WORKDIR /work
USER nemo

ENTRYPOINT ["/usr/local/bin/nemo", "--config", "/config.json"]
