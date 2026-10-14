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

.PHONY: tools tool-versions fmt fmt-check config-check vet lint verify-gates verify-tooling-regressions verify-skills verify-acceptance-ownership skill-package-check plugin-package-check test-race test-native-harness test-evidence-tools build smoke-cli vuln check acceptance-protocol supervisor-fixtures acceptance-supervisor acceptance-private-cli acceptance-agy codex-acceptance-tools acceptance-codex acceptance-codex-timeout-resume acceptance-codex-review claude-acceptance-tools acceptance-claude acceptance-claude-interactive acceptance-pi acceptance-opencode acceptance-native acceptance-native-lifetime

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

verify-skills:
	python3 scripts/verify_skills.py
	python3 -m unittest discover -s scripts -p 'test_verify_skills.py' -v

verify-acceptance-ownership:
	python3 scripts/verify_acceptance_ownership.py
	python3 -m unittest discover -s scripts -p 'test_verify_acceptance_ownership.py' -v

skill-package-check:
	./scripts/check-skill-package.sh

plugin-package-check:
	./scripts/check-plugin-package.sh

test-race: tool-versions
	go test -race -count=1 -timeout=15m ./...

test-native-harness:
	python3 -m unittest discover -s scripts -p 'test_acceptance_*.py' -v

test-evidence-tools:
	python3 -m unittest discover -s scripts -p 'test_handoff_evidence.py' -v
	python3 -m unittest discover -s scripts -p 'test_verify_release_archive.py' -v

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
	@mkdir -p bin/harness-tools
	CGO_ENABLED=0 go build -buildvcs=false -o bin/harness-tools/delegate ./internal/testutil/harnesscli/cmd/delegate
	CGO_ENABLED=0 go build -buildvcs=false -o bin/harness-tools/delegate-run ./internal/testutil/harnesscli/cmd/delegate-run
	CGO_ENABLED=0 go build -buildvcs=false -o bin/harness-tools/provider ./internal/testutil/fakeprovider/cmd/provider
	CGO_ENABLED=0 go build -buildvcs=false -o bin/harness-tools/pueue-fake ./internal/testutil/fakesupervisor/cmd/pueue
	CGO_ENABLED=0 go build -buildvcs=false -o bin/harness-tools/harnessprobe ./internal/testutil/harnessprobe

acceptance-supervisor: supervisor-fixtures
	./scripts/acceptance-supervisor.sh

acceptance-private-cli: build
	@receipt_dir=$$(mktemp -d "$(BIN_DIR)/private-cli-acceptance.XXXXXX"); \
	echo "Private CLI acceptance evidence: $$receipt_dir/receipt.json"; \
	python3 scripts/acceptance_private_cli.py \
		--delegate "$(BIN_DIR)/delegate" \
		--runner "$(BIN_DIR)/delegate-run" \
		--pueue "$${DELEGATE_TEST_PUEUE:-$(BIN_DIR)/test-supervisor/pueue}" \
		--pueued "$${DELEGATE_TEST_PUEUED:-$(BIN_DIR)/test-supervisor/pueued}" \
		--parallel-tasks 200 \
		--dispatch-workers 4 \
		--output "$$receipt_dir/receipt.json"

acceptance-agy: build test-native-harness
	./scripts/acceptance_native.sh antigravity:print

codex-acceptance-tools: tool-versions
	@mkdir -p bin/codex-acceptance
	CGO_ENABLED=0 go build -buildvcs=false -o bin/codex-acceptance/delegate ./internal/testutil/codexacceptance/cmd/delegate
	CGO_ENABLED=0 go build -buildvcs=false -o bin/codex-acceptance/delegate-run ./internal/testutil/codexacceptance/cmd/delegate-run

acceptance-codex: build test-native-harness
	./scripts/acceptance_native.sh codex:exec

acceptance-codex-timeout-resume: build test-native-harness
	./scripts/acceptance_native.sh codex:exec --permission workspace-write --scenario timeout-resume

acceptance-codex-review: build test-native-harness
	./scripts/acceptance_native.sh codex:review

claude-acceptance-tools: tool-versions
	@mkdir -p bin/claude-acceptance
	CGO_ENABLED=0 go build -buildvcs=false -o bin/claude-acceptance/delegate ./internal/testutil/claudeacceptance/cmd/delegate
	CGO_ENABLED=0 go build -buildvcs=false -o bin/claude-acceptance/delegate-run ./internal/testutil/claudeacceptance/cmd/delegate-run

acceptance-claude: build test-native-harness
	./scripts/acceptance_native.sh claude:print

acceptance-claude-interactive: build test-native-harness
	./scripts/acceptance_native.sh claude:print --permission read-only --scenario interactive

acceptance-pi: build test-native-harness
	./scripts/acceptance_native.sh pi:json

acceptance-opencode: build test-native-harness
	./scripts/acceptance_native.sh opencode:run

# Individual native gates return 2 when the executable, supervisor, or native
# login prerequisite is unavailable. The aggregate keeps that explicit state
# neutral while still failing on a real acceptance failure (exit 1).
acceptance-native: build test-native-harness
	./scripts/acceptance_native_all.sh

acceptance-native-lifetime: tool-versions
	python3 -m unittest discover -s scripts -p 'test_native_lifetime_proof.py' -v
	python3 scripts/native_lifetime_proof.py --output "$${DELEGATE_NATIVE_LIFETIME_RECEIPT:-bin/native-lifetime-proof/receipt.json}"

# Note: vuln requires access to the public vulnerability database (https://vuln.go.dev);
# the mandatory behavioral and unit tests remain hermetic.
vuln: tool-versions
	"$(GOVULN_BIN)" ./...

check:
	$(MAKE) tool-versions
	$(MAKE) verify-skills
	$(MAKE) verify-acceptance-ownership
	$(MAKE) plugin-package-check
	$(MAKE) fmt-check
	$(MAKE) config-check
	$(MAKE) vet
	$(MAKE) lint
	$(MAKE) verify-gates
	$(MAKE) test-race
	$(MAKE) test-native-harness
	$(MAKE) test-evidence-tools
	$(MAKE) build
	$(MAKE) smoke-cli
	$(MAKE) vuln

.PHONY: inspection-fixtures acceptance-inspection
inspection-fixtures: tool-versions
	@mkdir -p bin/inspection-fixture
	CGO_ENABLED=0 go build -buildvcs=false -o bin/inspection-fixture/delegate ./internal/testutil/inspectionfixture/cmd/delegate
	CGO_ENABLED=0 go build -buildvcs=false -o bin/inspection-fixture/delegate-run ./internal/testutil/inspectionfixture/cmd/delegate-run
	CGO_ENABLED=0 go build -buildvcs=false -o bin/inspection-fixture/inspection-helper ./internal/testutil/inspectionfixture/cmd/inspection-helper
	CGO_ENABLED=0 go build -buildvcs=false -o bin/inspection-fixture/provider ./internal/testutil/fakeprovider/cmd/provider

acceptance-inspection: inspection-fixtures test-native-harness
	./scripts/acceptance-inspection.sh
