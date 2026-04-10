# syntax=docker/dockerfile:1.7

# --- build stage ---
FROM golang:1.25.9-alpine3.22 AS build

WORKDIR /src

# hadolint ignore=DL3018
RUN apk add --no-cache git ca-certificates && \
    apk upgrade --no-cache

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# REIN_VERSION is injected at build time by the release workflow via --build-arg.
# CI and local `docker build` without --build-arg fall back to "dev".
ARG REIN_VERSION=dev

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${REIN_VERSION}" \
      -o /out/rein ./cmd/rein

# --- runtime stage ---
FROM alpine:3.22 AS runtime

# hadolint ignore=DL3018
RUN apk add --no-cache ca-certificates tini && \
    apk upgrade --no-cache && \
    addgroup -S rein && \
    adduser -S rein -G rein

WORKDIR /app
COPY --from=build /out/rein /usr/local/bin/rein

USER rein

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --quiet --tries=1 --spider http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/rein"]
