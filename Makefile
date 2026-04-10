.PHONY: help build run test lint docker clean tidy

BINARY := rein
PKG    := ./cmd/rein
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

help:
	@echo "Rein. Rein in your LLM spend."
	@echo ""
	@echo "Targets:"
	@echo "  build    Build the binary into ./bin/rein"
	@echo "  run      Run rein locally on :8080"
	@echo "  test     Run the test suite"
	@echo "  lint     Run golangci-lint"
	@echo "  docker   Build the docker image"
	@echo "  tidy     Run go mod tidy"
	@echo "  clean    Remove build artifacts"

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

run:
	@# Dev mode uses the in-memory keystore so no encryption key is needed and
	@# no rein.db file is left behind between runs. For a local SQLite dev loop,
	@# export REIN_DB_URL=sqlite:./rein.db and REIN_ENCRYPTION_KEY=$$(openssl rand -hex 32).
	REIN_ADMIN_TOKEN=dev-token REIN_DB_URL=memory go run $(PKG)

test:
	go test ./... -race -coverprofile=coverage.out

lint:
	golangci-lint run ./...

docker:
	docker build -t ghcr.io/archilea/rein:$(VERSION) .

tidy:
	go mod tidy

clean:
	rm -rf bin dist coverage.out
