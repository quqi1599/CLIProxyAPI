FROM golang:1.26-bookworm AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./

# Download dependencies on the native platform for faster builds
RUN --mount=type=cache,target=/root/.cache/go-mod \
    go mod download

# Copy the source tree
COPY . .

# Static build with CGO disabled.
# -trimpath removes build-path details for better reproducibility.
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.cache/go-mod \
    set -eu; \
    ldflags="-X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE}"; \
    if [ "${STRIP_BINARY}" = "true" ]; then \
      ldflags="-s -w ${ldflags}"; \
    fi; \
    xx-go build \
    -buildvcs=false \
    -trimpath \
    -ldflags="${ldflags}" \
    -o ./CLIProxyAPI ./cmd/server/ && \
    xx-verify CLIProxyAPI

# Runtime stage
FROM alpine:3.23

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG REPOSITORY_URL=unknown

RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

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
