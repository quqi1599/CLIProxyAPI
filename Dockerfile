FROM golang:1.26-bookworm AS builder

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG STRIP_BINARY=true

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./

# Download dependencies on the native platform for faster builds
RUN --mount=type=cache,target=/root/.cache/go-mod \
    go mod download

# Copy the source tree
COPY . .

# Dynamic build with CGO enabled so the server can load native plugins.
# -trimpath removes build-path details for better reproducibility.
ENV CGO_ENABLED=1
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.cache/go-mod \
    set -eu; \
    ldflags="-X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE}"; \
    if [ "${STRIP_BINARY}" = "true" ]; then \
      ldflags="-s -w ${ldflags}"; \
    fi; \
    go build \
    -buildvcs=false \
    -trimpath \
    -ldflags="${ldflags}" \
    -o ./CLIProxyAPI ./cmd/server/ && \
    test -x ./CLIProxyAPI

FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates && rm -rf /var/lib/apt/lists/*

RUN mkdir /CLIProxyAPI

COPY --from=builder /app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
