SHELL := /bin/bash
.DEFAULT_GOAL := help

PKG       := github.com/ukaji3/nakama
BIN       := nakama
BIN_DIR   := bin
CMD_DIR   := ./cmd/nakama
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.BuildDate=$(DATE)

GO        := go
GOFLAGS   :=

.PHONY: help
help: ## このヘルプを表示
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: tidy
tidy: ## go.mod / go.sum を整理
	$(GO) mod tidy

.PHONY: build
build: ## バイナリをビルド
	mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BIN) $(CMD_DIR)

.PHONY: install
install: ## $GOPATH/bin にインストール
	$(GO) install $(GOFLAGS) -ldflags '$(LDFLAGS)' $(CMD_DIR)

.PHONY: test
test: ## ユニットテストを実行
	$(GO) test $(GOFLAGS) -race -count=1 ./...

.PHONY: cover
cover: ## カバレッジ付きテスト
	$(GO) test $(GOFLAGS) -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Open coverage.html"

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## gofmt -w
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## gofmt -s -d (CI: fail when not formatted)
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then \
		echo "::error::the following files are not gofmt -s formatted:"; \
		echo "$$out"; \
		gofmt -s -d $$out; \
		exit 1; \
	fi

.PHONY: lint
lint: ## golangci-lint (要インストール)
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not installed: https://golangci-lint.run/"; exit 1; }
	golangci-lint run ./...

.PHONY: vuln
vuln: ## govulncheck
	@command -v govulncheck >/dev/null || $(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck ./...

.PHONY: ci
ci: fmt-check vet test build ## CI ローカル相当 (fmt-check + vet + test + build)

.PHONY: clean
clean: ## 生成物の削除
	rm -rf $(BIN_DIR) coverage.out coverage.html
