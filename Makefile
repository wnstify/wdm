GO ?= go
APP_NAME ?= wdm
BIN_DIR ?= bin
CMD_DIR ?= ./cmd/wdm
GO_E2E_TAG ?= docker_e2e

CATALOG_DIR ?= $(if $(XDG_DATA_HOME),$(XDG_DATA_HOME),$(HOME)/.local/share)/wdm/catalogs

.PHONY: build build-linux test test-race coverage coverage-check catalog-freshness catalog-freshness-test release-notes-test dev-catalog-seed e2e lint vet fmt tidy clean help

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

catalog-freshness: ## Ensure catalog/template changes advance generated_at
	sh scripts/check-catalog-freshness.sh

catalog-freshness-test: ## Test the catalog freshness guard
	sh scripts/check-catalog-freshness_test.sh

release-notes-test: ## Test release notes extraction
	sh scripts/extract-release-notes_test.sh

dev-catalog-seed: ## Seed the in-repo catalog into the local data dir for dev testing (UNVERIFIED; FORCE=1 to overwrite)
	@if [ -f "$(CATALOG_DIR)/stable/catalog.yaml" ] && [ -z "$(FORCE)" ]; then \
		echo "Refusing: a catalog already exists at $(CATALOG_DIR)/stable/catalog.yaml."; \
		echo "Will not overwrite a real or verified catalog. Re-run with FORCE=1 to replace it."; \
		exit 1; \
	fi
	@mkdir -p "$(CATALOG_DIR)"
	@rm -rf "$(CATALOG_DIR)/stable" "$(CATALOG_DIR)/templates"
	@cp -R catalog/stable "$(CATALOG_DIR)/stable"
	@cp -R templates "$(CATALOG_DIR)/templates"
	@echo "Seeded UNVERIFIED dev catalog into $(CATALOG_DIR) (stable/ + templates/)."
	@echo "For local development testing only. Production installs use the signed 'wdm catalog update'."

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
