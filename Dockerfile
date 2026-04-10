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

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/rein ./cmd/rein

# --- runtime stage ---
FROM alpine:3.20 AS runtime

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
