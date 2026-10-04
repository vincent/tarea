.PHONY: all lint lint-go lint-web test test-web cover build web dev-api dev-web release clean tools

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOLANGCI_LINT_VERSION := v2.5.0
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64

all: lint test build

## tools: install pinned dev tools
tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

## lint: Go and web linters
lint: lint-go lint-web

lint-go:
	golangci-lint run ./...

lint-web:
	cd web && npm run lint && npm run check

## test: unit tests with the race detector
test:
	go test -race -timeout 120s ./...

test-web:
	cd web && npm test

## cover: coverage summary
cover:
	go test -race -timeout 120s -coverprofile=cover.out ./...
	go tool cover -func=cover.out | tail -n 1

## web: build the Svelte panel and copy it into the Go embed directory
web:
	cd web && npm ci && npm run build
	find internal/webui/dist -mindepth 1 ! -name .gitkeep -delete
	cp -R web/build/. internal/webui/dist/

## build: single static binary with the embedded panel
build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tarea ./cmd/tarea

## dev-api / dev-web: run in two terminals; Vite proxies /api to :8080
dev-api:
	go run ./cmd/tarea serve --data data

dev-web:
	cd web && npm run dev

## release: cross-compile every platform into dist/ (pure Go, CGO off)
release: web
	rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; [ "$$os" = windows ] && ext=".exe"; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/tarea-$$os-$$arch$$ext ./cmd/tarea || exit 1; \
	done
	cd dist && (sha256sum tarea-* 2>/dev/null || shasum -a 256 tarea-*) > SHA256SUMS

clean:
	rm -rf bin dist cover.out web/build web/.svelte-kit
	find internal/webui/dist -mindepth 1 ! -name .gitkeep -delete
