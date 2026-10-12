BINARY := viti-talos
# viti dispatches plugins by binary name, so the "t" shorthand is a second
# name for the same binary rather than a cobra alias.
ALIAS  := viti-t
PKG    := github.com/vitistack/vitictl-talos

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS ?= -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: fix
fix: golangci-lint ## Auto-fix lint issues.
	$(GOLANGCI_LINT) run --fix

.PHONY: gosec
gosec: gosec-bin ## Run security analysis.
	$(GOSEC) ./...

.PHONY: govulncheck
govulncheck: govulncheck-bin ## Check dependencies for known vulnerabilities.
	$(GOVULNCHECK) ./...

.PHONY: update-deps
update-deps: ## Update dependencies to latest versions.
	go get -u ./...
	go mod tidy

##@ Build

.PHONY: build
build: fmt vet ## Build the plugin binary into bin/.
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

.PHONY: install
install: build ## Install the plugin (and its tl alias) onto GOBIN.
	install -m 0755 bin/$(BINARY) $(GOBIN)/$(BINARY)
	ln -sf $(GOBIN)/$(BINARY) $(GOBIN)/$(ALIAS)
	@echo "installed $(GOBIN)/$(BINARY) (+ $(ALIAS) symlink) — run 'viti talos --help' or 'viti t --help'"

.PHONY: uninstall
uninstall: ## Remove the installed binary and its alias.
	rm -f $(GOBIN)/$(BINARY) $(GOBIN)/$(ALIAS)

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf bin cover.out

##@ Tools

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

GOLANGCI_LINT     ?= $(LOCALBIN)/golangci-lint
GOSEC             ?= $(LOCALBIN)/gosec
GOVULNCHECK       ?= $(LOCALBIN)/govulncheck
# Track upstream: resolve @latest to a concrete version so the cached binaries
# below are keyed on the real version and refresh when a new release lands.
# Falls back to a known-good version when the module proxy is unreachable.
GOLANGCI_LINT_VER ?= $(shell go list -m -f '{{.Version}}' github.com/golangci/golangci-lint/v2@latest 2>/dev/null)
GOLANGCI_LINT_VER := $(or $(GOLANGCI_LINT_VER),v2.13.1)
# Pinned to gosec main: v2.29.0 bundles golang.org/x/tools v0.49.0, which cannot read Go 1.27.2 export data. Switch back to latest once v2.29.1+ is released.
GOSEC_VER         ?= v2.29.1-0.20261005092323-d2b649ec0182
GOVULNCHECK_VER   ?= latest

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN)
	@test -x $(GOLANGCI_LINT)-$(GOLANGCI_LINT_VER) || { \
		GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VER) && \
		mv -f $(GOLANGCI_LINT) $(GOLANGCI_LINT)-$(GOLANGCI_LINT_VER); }
	@ln -sf $(GOLANGCI_LINT)-$(GOLANGCI_LINT_VER) $(GOLANGCI_LINT)

.PHONY: gosec-bin
gosec-bin: $(LOCALBIN)
	@test -x $(GOSEC)-$(GOSEC_VER) || { \
		GOBIN=$(LOCALBIN) go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VER) && \
		mv -f $(GOSEC) $(GOSEC)-$(GOSEC_VER); }
	@ln -sf $(GOSEC)-$(GOSEC_VER) $(GOSEC)

.PHONY: govulncheck-bin
govulncheck-bin: $(LOCALBIN)
	@test -x $(GOVULNCHECK) || GOBIN=$(LOCALBIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VER)

##@ Pre-commit

# Make targets precheck runs, in order (mirrors what CI runs on push).
PRECHECK_TARGETS ?= build lint test gosec govulncheck
# Helm charts precheck lints (skipped when helm is not installed).
PRECHECK_HELM_CHARTS ?= $(wildcard charts/*/)

# Snapshot of the working tree (tracked diff + untracked files) used to detect
# files rewritten by go mod tidy / code generation / go fmt during precheck.
PRECHECK_TREE_STATE = { git diff HEAD; git ls-files --others --exclude-standard; } | git hash-object --stdin

.PHONY: precheck
precheck: ## Run every check CI runs (tidy, build, lint, test, security scans, helm lint) before commit/push.
	@set -e; \
	before=$$($(PRECHECK_TREE_STATE)); \
	if [ -f go.mod ]; then \
		printf '%b\n' "\033[0;36m==> go mod tidy + verify\033[0m"; \
		go mod tidy; \
		go mod verify; \
	fi; \
	for t in $(PRECHECK_TARGETS); do \
		printf '%b\n' "\033[0;36m==> make $$t\033[0m"; \
		$(MAKE) --no-print-directory $$t; \
	done; \
	if [ -n "$(strip $(PRECHECK_HELM_CHARTS))" ]; then \
		if command -v helm >/dev/null 2>&1; then \
			for c in $(PRECHECK_HELM_CHARTS); do \
				printf '%b\n' "\033[0;36m==> helm lint $$c\033[0m"; \
				helm lint $$c; \
			done; \
		else \
			printf '%b\n' "\033[0;33mhelm not installed, skipping helm lint\033[0m"; \
		fi; \
	fi; \
	after=$$($(PRECHECK_TREE_STATE)); \
	if [ "$$before" != "$$after" ]; then \
		printf '%b\n' "\033[0;33m\033[1mprecheck modified files (go.mod/go.sum, generated code, CRDs/RBAC or formatting).\033[0m"; \
		printf '%b\n' "\033[0;33mReview and stage them before committing:\033[0m"; \
		git status --short; \
		exit 1; \
	fi; \
	printf '%b\n' "\033[0;32m\033[1mAll prechecks passed - ready to commit and push.\033[0m"
