# syntax=docker/dockerfile:1

# ---- Build stage --------------------------------------------------------
FROM golang:1.23-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bifrost ./cmd/bifrost

# ---- Runtime stage ------------------------------------------------------
FROM alpine:3.21

RUN apk add --no-cache ca-certificates \
 && addgroup -S bifrost \
 && adduser -S -H -G bifrost bifrost

COPY --from=build /bifrost /usr/local/bin/bifrost

# Where the config file is mounted (see docker-compose.yml).
ENV CONFIG=/config/config.yaml

USER bifrost
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/bifrost"]
