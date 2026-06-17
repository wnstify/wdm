GO ?= go
APP_NAME ?= wdm
BIN_DIR ?= bin
CMD_DIR ?= ./cmd/wdm
GO_E2E_TAG ?= docker_e2e

.PHONY: build build-linux test test-race coverage coverage-check e2e lint vet fmt tidy clean help

build: ## Build local binary
	$(GO) build -o $(BIN_DIR)/$(APP_NAME) $(CMD_DIR)

build-linux: ## Build Linux amd64 binary
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/$(APP_NAME)-linux-amd64 $(CMD_DIR)

test: ## Run unit tests
	$(GO) test ./...

test-race: ## Run tests with race detector and shuffled order
	$(GO) test -race -shuffle=on ./...

coverage: ## Run quick local coverage summary
	$(GO) test -count=1 -cover ./...

coverage-check: ## Run offline coverage gate
	@GO=$(GO) scripts/check-coverage.sh

e2e: ## Run Docker e2e tests
	$(GO) test -tags $(GO_E2E_TAG) ./tests/e2e/...

lint: ## Run golangci-lint
	golangci-lint run

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format Go code
	$(GO) fmt ./...

tidy: ## Tidy go.mod/go.sum
	$(GO) mod tidy

clean: ## Remove build output
	rm -rf $(BIN_DIR)

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "; printf "Available targets:\n"} /^[a-zA-Z0-9_.-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
