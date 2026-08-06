# Valorant Discord Bot — build & run helpers
#
# Usage:
#   make run                 # local: load .env and run
#   make build               # native binary → bin/valorant-bot
#   make build-all           # cross-compile linux/darwin targets → dist/
#   make build-pi            # linux/arm64 (64-bit Pi)
#   make build-pi32          # linux/arm (32-bit Pi / armv7)
#   make build-linux         # linux/amd64 (VPS/server)
#   make docker-build        # container image
#   make docker-up           # docker compose up -d

APP       := valorant-bot
CMD       := ./cmd/bot
BIN_DIR   := bin
DIST_DIR  := dist
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)

.PHONY: all run build build-all build-pi build-pi32 build-linux build-darwin \
	clean test docker-build docker-up docker-down help

all: build

help:
	@echo "Targets:"
	@echo "  run           Run locally (requires .env)"
	@echo "  build         Native binary → $(BIN_DIR)/$(APP)"
	@echo "  build-all     Cross-compile → $(DIST_DIR)/"
	@echo "  build-pi      linux/arm64 (Raspberry Pi 64-bit)"
	@echo "  build-pi32    linux/arm   (Raspberry Pi 32-bit)"
	@echo "  build-linux   linux/amd64 (server/VPS)"
	@echo "  build-darwin  darwin/arm64 + darwin/amd64"
	@echo "  test          go test ./..."
	@echo "  docker-build  Build Docker image"
	@echo "  docker-up     Start via docker compose"
	@echo "  docker-down   Stop compose stack"
	@echo "  clean         Remove bin/ and dist/"

run:
	@test -f .env || { echo "missing .env — copy from .env.example or deploy/env.local.example"; exit 1; }
	@set -a; . ./.env; set +a; go run $(CMD)

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP) $(CMD)
	@echo "→ $(BIN_DIR)/$(APP)"

build-pi:
	@mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" \
		-o $(DIST_DIR)/$(APP)-linux-arm64 $(CMD)
	@echo "→ $(DIST_DIR)/$(APP)-linux-arm64"

build-pi32:
	@mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" \
		-o $(DIST_DIR)/$(APP)-linux-armv7 $(CMD)
	@echo "→ $(DIST_DIR)/$(APP)-linux-armv7"

build-linux:
	@mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" \
		-o $(DIST_DIR)/$(APP)-linux-amd64 $(CMD)
	@echo "→ $(DIST_DIR)/$(APP)-linux-amd64"

build-darwin:
	@mkdir -p $(DIST_DIR)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" \
		-o $(DIST_DIR)/$(APP)-darwin-arm64 $(CMD)
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" \
		-o $(DIST_DIR)/$(APP)-darwin-amd64 $(CMD)
	@echo "→ $(DIST_DIR)/$(APP)-darwin-{arm64,amd64}"

build-all: build-pi build-pi32 build-linux build-darwin
	@echo "all targets in $(DIST_DIR)/"

test:
	go test ./...

docker-build:
	docker build -t $(APP):$(VERSION) -t $(APP):latest .

docker-up:
	docker compose up -d

docker-down:
	docker compose down

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)
