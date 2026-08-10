# dokkup
#
# The container targets work with Apple's `container` runtime and with Docker.
# The two CLIs agree on everything used here (build, run, exec, stop, rm), so the
# only difference is which binary gets invoked.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BINARY      := dokkup
CMD_PKG     := ./cmd/dokkup
DIST        := dist
WEB         := web
WEB_OUT     := internal/server/static/dist

DEVENV_IMAGE := dokkup-devenv:local
DEVENV_NAME  := dokkup-devenv
DEVENV_DIR   := devenv

# Apple's runtime is preferred where present: on Apple Silicon it needs no
# virtual machine of its own and starts noticeably faster.
RUNTIME ?= $(shell command -v container 2>/dev/null || command -v docker 2>/dev/null)

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w \
             -X main.version=$(VERSION) \
             -X main.commit=$(COMMIT) \
             -X main.date=$(DATE)

# Budgets from CONTRIBUTING.md. Enforced, not aspirational.
MAX_BINARY_MB := 25

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

.PHONY: web
web: ## Build the frontend
	cd $(WEB) && bun install --frozen-lockfile && bun run build

.PHONY: build
build: web ## Build the binary with the frontend embedded
	mkdir -p $(DIST)
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY) $(CMD_PKG)
	@echo "built $(DIST)/$(BINARY) ($(VERSION))"

.PHONY: build-linux
build-linux: web ## Cross-compile for Linux amd64 and arm64
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY)_linux_amd64 $(CMD_PKG)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY)_linux_arm64 $(CMD_PKG)

.PHONY: budget
budget: build ## Fail if the binary exceeds its size budget
	@size=$$(( $$(wc -c < $(DIST)/$(BINARY)) / 1024 / 1024 )); \
	echo "binary: $${size} MB (budget $(MAX_BINARY_MB) MB)"; \
	if [ "$${size}" -gt "$(MAX_BINARY_MB)" ]; then \
		echo "over budget -- see the weight budget in CONTRIBUTING.md" >&2; \
		exit 1; \
	fi

.PHONY: clean
clean: ## Remove build output
	rm -rf $(DIST) $(WEB_OUT) $(WEB)/node_modules $(WEB)/.svelte-kit

##@ Development

.PHONY: dev
dev: ## Backend with reload plus the frontend dev server
	@echo "frontend: http://localhost:5173   api: http://localhost:8080"
	@$(MAKE) -j2 dev-api dev-web

.PHONY: dev-api
dev-api:
	DOKKUP_DEV=1 go run $(CMD_PKG) serve --listen 127.0.0.1:8080

.PHONY: dev-web
dev-web:
	cd $(WEB) && bun install && bun run dev

##@ Quality

.PHONY: fmt
fmt: ## Format Go sources
	gofmt -s -w $$(git ls-files '*.go')

.PHONY: lint
lint: ## Lint Go and the frontend
	gofmt -l $$(git ls-files '*.go') | (! grep .) || (echo "run 'make fmt'" >&2; exit 1)
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run \
		|| echo "golangci-lint not installed, skipping"
	cd $(WEB) && bun install && bun run check

.PHONY: test
test: ## Go tests against the in-memory Dokku fake -- no container needed
	go test -race -shuffle=on ./...

.PHONY: test-integration
test-integration: devenv-up ## Tests against the real Dokku in the dev environment
	go test -race -tags=integration -count=1 ./...

##@ Development environment

.PHONY: devenv-build
devenv-build: ## Build the dev environment image
	@test -n "$(RUNTIME)" || { echo "no container runtime found (install Apple 'container' or Docker)" >&2; exit 1; }
	$(RUNTIME) build -t $(DEVENV_IMAGE) $(DEVENV_DIR)

.PHONY: devenv-up
devenv-up: devenv-build ## Start a real Dokku locally (first run installs it, takes a few minutes)
	@if $(RUNTIME) inspect $(DEVENV_NAME) >/dev/null 2>&1; then \
		echo "$(DEVENV_NAME) already exists; starting it"; \
		$(RUNTIME) start $(DEVENV_NAME) >/dev/null 2>&1 || true; \
	else \
		$(RUNTIME) run -d --cap-add ALL --name $(DEVENV_NAME) \
			-p 2222:22 -p 8081:80 -p 8443:443 \
			-v "$(CURDIR)":/workspace \
			$(DEVENV_IMAGE); \
	fi
	@$(MAKE) --no-print-directory devenv-wait

.PHONY: devenv-wait
devenv-wait: ## Block until Dokku is installed and answering
	@echo "waiting for Dokku (first run installs it; follow with 'make devenv-logs')"
	@for i in $$(seq 1 180); do \
		if $(RUNTIME) exec $(DEVENV_NAME) dokku apps:list >/dev/null 2>&1; then \
			echo "Dokku ready: $$($(RUNTIME) exec $(DEVENV_NAME) dokku version)"; \
			exit 0; \
		fi; \
		sleep 5; \
	done; \
	echo "Dokku did not become ready; check 'make devenv-logs'" >&2; \
	exit 1

.PHONY: devenv-shell
devenv-shell: ## A shell inside the dev environment
	$(RUNTIME) exec -ti $(DEVENV_NAME) bash

.PHONY: devenv-logs
devenv-logs: ## Follow the Dokku install log
	$(RUNTIME) exec $(DEVENV_NAME) journalctl -u dokku-bootstrap -f --no-pager

.PHONY: devenv-down
devenv-down: ## Stop and remove the dev environment
	-$(RUNTIME) stop $(DEVENV_NAME)
	-$(RUNTIME) rm $(DEVENV_NAME)
