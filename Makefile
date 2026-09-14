SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c

ifneq ($(origin GOTOOLCHAIN),undefined)
  ifneq ($(GOTOOLCHAIN),local)
    $(error Conflicting GOTOOLCHAIN setting '$(GOTOOLCHAIN)' rejected. GOTOOLCHAIN must be unset or 'local' to prevent compiler mismatch)
  endif
endif
override GOTOOLCHAIN := local
export GOTOOLCHAIN

REPO_ROOT := $(shell pwd)
BIN_DIR := $(REPO_ROOT)/bin
GOLANGCI_BIN := $(BIN_DIR)/golangci-lint
GOFUMPT_BIN := $(BIN_DIR)/gofumpt
GOVULN_BIN := $(BIN_DIR)/govulncheck
GORELEASER_BIN := $(BIN_DIR)/goreleaser

.PHONY: tools tool-versions fmt fmt-check config-check vet lint verify-gates verify-tooling-regressions test-race build smoke-cli vuln check acceptance-protocol supervisor-fixtures acceptance-supervisor

tools:
	./scripts/install-tools.sh

tool-versions:
	./scripts/check-tool-versions.sh

fmt: tool-versions
	find . -type f -name '*.go' -not -path './bin/*' -not -path './dist/*' -exec "$(GOFUMPT_BIN)" -w {} +

fmt-check: tool-versions
	@files=$$(find . -type f -name '*.go' -not -path './bin/*' -not -path './dist/*' -exec "$(GOFUMPT_BIN)" -l {} +) || exit $$?; \
	if [ -n "$$files" ]; then \
		echo "ERROR: The following Go files are not formatted with gofumpt:" >&2; \
		echo "$$files" >&2; \
		echo "Action: Run 'make fmt' to format them." >&2; \
		exit 1; \
	fi

config-check: tool-versions
	"$(GOLANGCI_BIN)" config verify
	"$(GORELEASER_BIN)" check

vet: tool-versions
	go vet ./...

lint: tool-versions
	"$(GOLANGCI_BIN)" run ./...

verify-tooling-regressions: tool-versions
	./scripts/verify-tooling-regressions.sh

verify-gates: tool-versions
	./scripts/verify-gates.sh
	./scripts/verify-tooling-regressions.sh

test-race: tool-versions
	go test -race -count=1 ./...

build: tool-versions
	@mkdir -p bin dist/delegate-darwin-amd64 dist/delegate-darwin-arm64 dist/delegate-linux-amd64 dist/delegate-linux-arm64
	CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.version=dev" -o bin/delegate ./cmd/delegate
	CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.version=dev" -o bin/delegate-run ./cmd/delegate-run
	CGO_ENABLED=0 go build -buildvcs=false -o bin/protocolfixture ./internal/testutil/protocolfixture
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.version=dev" -o dist/delegate-darwin-amd64/delegate ./cmd/delegate
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.version=dev" -o dist/delegate-darwin-amd64/delegate-run ./cmd/delegate-run
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.version=dev" -o dist/delegate-darwin-arm64/delegate ./cmd/delegate
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.version=dev" -o dist/delegate-darwin-arm64/delegate-run ./cmd/delegate-run
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.version=dev" -o dist/delegate-linux-amd64/delegate ./cmd/delegate
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.version=dev" -o dist/delegate-linux-amd64/delegate-run ./cmd/delegate-run
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.version=dev" -o dist/delegate-linux-arm64/delegate ./cmd/delegate
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.version=dev" -o dist/delegate-linux-arm64/delegate-run ./cmd/delegate-run

smoke-cli: build
	./scripts/smoke-cli.sh
	@if [ -x /bin/bash ]; then \
		echo "Executing smoke test under /bin/bash..."; \
		/bin/bash ./scripts/smoke-cli.sh; \
	fi

acceptance-protocol: build
	./scripts/acceptance-protocol.sh

supervisor-fixtures: tool-versions
	@mkdir -p bin/phase2tools
	CGO_ENABLED=0 go build -buildvcs=false -o bin/phase2tools/delegate ./internal/testutil/phase2cli/cmd/delegate
	CGO_ENABLED=0 go build -buildvcs=false -o bin/phase2tools/delegate-run ./internal/testutil/phase2cli/cmd/delegate-run
	CGO_ENABLED=0 go build -buildvcs=false -o bin/phase2tools/provider ./internal/testutil/phase2fixture/cmd/provider
	CGO_ENABLED=0 go build -buildvcs=false -o bin/phase2tools/pueue-fake ./internal/testutil/phase2supervisor/cmd/pueue
	CGO_ENABLED=0 go build -buildvcs=false -o bin/phase2tools/phase2probe ./internal/testutil/phase2probe

acceptance-supervisor: supervisor-fixtures
	./scripts/acceptance-supervisor.sh

# Note: vuln requires access to the public vulnerability database (https://vuln.go.dev);
# the mandatory behavioral and unit tests remain hermetic.
vuln: tool-versions
	"$(GOVULN_BIN)" ./...

check:
	$(MAKE) tool-versions
	$(MAKE) fmt-check
	$(MAKE) config-check
	$(MAKE) vet
	$(MAKE) lint
	$(MAKE) verify-gates
	$(MAKE) test-race
	$(MAKE) build
	$(MAKE) smoke-cli
	$(MAKE) vuln
