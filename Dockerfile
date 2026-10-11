# syntax=docker/dockerfile:1

# ---- Build stage --------------------------------------------------------
# $BUILDPLATFORM: run the toolchain natively in CI and cross-compile with
# GOOS/GOARCH (set by BuildKit via TARGETOS/TARGETARG) — no QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /src

ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /bifrost ./cmd/bifrost

# ---- Runtime stage ------------------------------------------------------
FROM alpine:3.21

RUN apk add --no-cache ca-certificates \
 && addgroup -S bifrost \
 && adduser -S -H -G bifrost bifrost

COPY --from=build /bifrost /usr/local/bin/bifrost

# Where the config file is mounted (see docker-compose.yml).
ENV CONFIG=/config/config.yaml

USER bifrost
EXPOSE 45557

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:45557/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/bifrost"]
