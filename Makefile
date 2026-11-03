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
# Repository validators; their focused targets add their own tests, while check runs
# only these because test-python already runs every Python test.
VERIFY_SKILLS := python3 scripts/verify_skills.py
VERIFY_ACCEPTANCE_OWNERSHIP := python3 scripts/verify_acceptance_ownership.py

.PHONY: tools tool-versions fmt fmt-check config-check vet lint verify-gates verify-tooling-regressions verify-skills verify-diagram-checksums verify-acceptance-ownership skill-package-check plugin-package-check test-race test-python test-native-harness test-evidence-tools build smoke-cli vuln check acceptance-protocol supervisor-fixtures acceptance-supervisor acceptance-private-cli acceptance-private-cli-default-stop acceptance-supervisor-handoff acceptance-supervisor-upgrade-recovery acceptance-workspaces acceptance-agy codex-acceptance-tools acceptance-codex acceptance-codex-timeout-resume acceptance-codex-review claude-acceptance-tools acceptance-claude acceptance-claude-interactive acceptance-pi acceptance-opencode acceptance-native acceptance-native-lifetime

tools: tools-go
	python3 scripts/static_tools.py install

.PHONY: tools-go check-platform
tools-go:
	./scripts/install-tools.sh

.PHONY: static-tool-versions lint-static verify-static-gates diagnostic-tools diagnostics
static-tool-versions:
	python3 scripts/static_tools.py verify

lint-static: static-tool-versions
	python3 scripts/static_analysis.py all

verify-static-gates: static-tool-versions
	python3 scripts/verify_static_gates.py

diagnostic-tools:
	python3 scripts/static_tools.py install --diagnostic

diagnostics:
	python3 scripts/static_tools.py verify --diagnostic
	python3 scripts/verify_static_gates.py --diagnostic
	python3 scripts/static_analysis.py diagnostics

tool-versions:
	./scripts/check-tool-versions.sh

fmt: tool-versions
	@formatter_sources=$$(mktemp) || exit $$?; trap 'rm -f "$$formatter_sources"' EXIT; \
	go run ./cmd/source-inventory --kind go --allow-empty >"$$formatter_sources" || exit $$?; \
	if [ -s "$$formatter_sources" ]; then xargs -0 "$(GOFUMPT_BIN)" -w <"$$formatter_sources"; fi

fmt-check: tool-versions
	@formatter_sources=$$(mktemp) || exit $$?; trap 'rm -f "$$formatter_sources"' EXIT; \
	go run ./cmd/source-inventory --kind go --allow-empty >"$$formatter_sources" || exit $$?; \
	files=$$(if [ -s "$$formatter_sources" ]; then xargs -0 "$(GOFUMPT_BIN)" -l <"$$formatter_sources"; fi) || exit $$?; \
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
	$(VERIFY_SKILLS)
	python3 -m unittest discover -s scripts -p 'test_verify_skills.py' -v
	python3 -m unittest discover -s scripts -p 'test_skill_examples.py' -v

verify-diagram-checksums:
	@if command -v sha256sum >/dev/null 2>&1; then \
		cd diagrams && sha256sum -c SHA256SUMS; \
	elif command -v shasum >/dev/null 2>&1; then \
		cd diagrams && shasum -a 256 -c SHA256SUMS; \
	else \
		echo "ERROR: sha256sum or shasum is required to verify diagram integrity." >&2; \
		exit 1; \
	fi

verify-acceptance-ownership:
	$(VERIFY_ACCEPTANCE_OWNERSHIP)
	python3 -m unittest discover -s scripts -p 'test_verify_acceptance_ownership.py' -v

skill-package-check:
	./scripts/check-skill-package.sh

plugin-package-check:
	./scripts/check-plugin-package.sh

.PHONY: check-docs
# Content validators use the project compiler, not the runtime tool/native matrix.
check-docs:
	$(VERIFY_SKILLS)
	$(MAKE) verify-diagram-checksums plugin-package-check
	python3 -m unittest discover -s scripts -p 'test_verify_skills.py' -v
	python3 -m unittest discover -s scripts -p 'test_skill_examples.py' -v
	python3 -m unittest discover -s scripts -p 'test_acceptance_skill_routing.py' -v
	python3 -m unittest discover -s scripts/acceptance_tests/native -t scripts -p 'test_native_acceptance_aggregate.py' -k test_public_option_docs_cover_ccr_required_model_and_effort -v
	go test -count=1 ./internal/app -run '^TestProviderGuideMatchesNativeCatalog$$'

# -timeout is the per-test-binary aggregate hang watchdog, not a latency requirement;
# functional and cleanup deadlines stay inside each test.
test-race: tool-versions
	go test -race -count=1 -timeout=30m ./...

# The one standard discovery of every Python test below scripts; check runs Python tests only here.
# The focused targets below stay for direct contributor and acceptance use.
test-python:
	python3 -m unittest discover -s scripts -t scripts -p 'test_*.py' -v

test-native-harness:
	python3 -m unittest discover -s scripts -p 'test_acceptance_*.py' -v
	python3 -m unittest discover -s scripts/acceptance_tests -t scripts -p 'test_*.py' -v

test-evidence-tools:
	python3 -m unittest discover -s scripts -p 'test_native_acceptance_evidence.py' -v
	python3 -m unittest discover -s scripts -p 'test_verify_release_archive.py' -v
	python3 -m unittest discover -s scripts -p 'test_release_*.py' -v
	python3 -m unittest discover -s scripts -p 'test_published_install*.py' -v
	python3 -m unittest discover -s scripts -p 'test_candidate_*.py' -v
	python3 -m unittest discover -s scripts -p 'test_homebrew_formula_render.py' -v
	python3 -m unittest discover -s scripts -p 'test_actions_artifacts.py' -v

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
	CGO_ENABLED=0 go test -c -buildvcs=false -o bin/harness-tools/scheduler-fixture ./internal/scheduler

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

.PHONY: acceptance-scheduler
acceptance-scheduler: build supervisor-fixtures
	@receipt_dir=$$(mktemp -d "$(BIN_DIR)/scheduler-acceptance.XXXXXX"); \
	echo "Scheduler acceptance output: $$receipt_dir/results"; \
	python3 scripts/acceptance_scheduler.py \
		--delegate "$(BIN_DIR)/delegate" --runner "$(BIN_DIR)/delegate-run" \
		--pueue "$${DELEGATE_TEST_PUEUE:-$(BIN_DIR)/test-supervisor/pueue}" \
		--pueued "$${DELEGATE_TEST_PUEUED:-$(BIN_DIR)/test-supervisor/pueued}" \
		--output "$$receipt_dir/results"

acceptance-private-cli-default-stop: build
	@receipt_dir=$$(mktemp -d "$(BIN_DIR)/private-cli-acceptance.XXXXXX"); \
	echo "Default stop acceptance evidence: $$receipt_dir/receipt.json"; \
	python3 scripts/acceptance_private_cli.py \
		--delegate "$(BIN_DIR)/delegate" \
		--runner "$(BIN_DIR)/delegate-run" \
		--pueue "$${DELEGATE_TEST_PUEUE:-$(BIN_DIR)/test-supervisor/pueue}" \
		--pueued "$${DELEGATE_TEST_PUEUED:-$(BIN_DIR)/test-supervisor/pueued}" \
		--scenario default-stop \
		--parallel-tasks 2 \
		--runtime-only \
		--output "$$receipt_dir/receipt.json"

.PHONY: nested-survival-fixture acceptance-nested-survival
nested-survival-fixture: tool-versions
	go test -c -o "$(BIN_DIR)/nested-lifetime-fixture" ./internal/lifetime

# The expectation is an assertion, never a backend selector. Exact Linux proof
# is requested explicitly; normal CI hosts assert their cooperative backend.
acceptance-nested-survival: build nested-survival-fixture
	@for topology in shared-warm other-warm other-cold; do \
		receipt_dir=$$(mktemp -d "$(BIN_DIR)/nested-survival.XXXXXX") || exit $$?; \
		echo "Nested survival $$topology evidence: $$receipt_dir/receipt.json"; \
		python3 scripts/acceptance_private_cli.py \
			--delegate "$(BIN_DIR)/delegate" --runner "$(BIN_DIR)/delegate-run" \
			--pueue "$${DELEGATE_TEST_PUEUE:-$(BIN_DIR)/test-supervisor/pueue}" \
			--pueued "$${DELEGATE_TEST_PUEUED:-$(BIN_DIR)/test-supervisor/pueued}" \
			--scenario nested-survival --nested-topology "$$topology" \
			--expected-backend "$${DELEGATE_NESTED_EXPECTED_BACKEND:-process-group}" \
			--native-fixture "$(BIN_DIR)/nested-lifetime-fixture" \
			--parallel-tasks 3 --dispatch-workers 1 --runtime-only \
			--output "$$receipt_dir/receipt.json" || exit $$?; \
	done

acceptance-supervisor-handoff: build
	@receipt_dir=$$(mktemp -d "$(BIN_DIR)/private-cli-acceptance.XXXXXX"); \
	echo "Supervisor handoff acceptance evidence: $$receipt_dir/receipt.json"; \
	python3 scripts/acceptance_private_cli.py \
		--delegate "$(BIN_DIR)/delegate" \
		--runner "$(BIN_DIR)/delegate-run" \
		--pueue "$${DELEGATE_TEST_PUEUE:-$(BIN_DIR)/test-supervisor/pueue}" \
		--pueued "$${DELEGATE_TEST_PUEUED:-$(BIN_DIR)/test-supervisor/pueued}" \
		--scenario external-inspection-handoff \
		--parallel-tasks 2 \
		--dispatch-workers 2 \
		--output "$$receipt_dir/receipt.json"

acceptance-supervisor-upgrade-recovery: build
	@receipt_dir=$$(mktemp -d "$(BIN_DIR)/private-cli-acceptance.XXXXXX"); \
	echo "Supervisor upgrade recovery evidence: $$receipt_dir/receipt.json"; \
	python3 scripts/acceptance_private_cli.py \
		--delegate "$(BIN_DIR)/delegate" --runner "$(BIN_DIR)/delegate-run" \
		--pueue "$${DELEGATE_TEST_PUEUE:-$(BIN_DIR)/test-supervisor/pueue}" \
		--pueued "$${DELEGATE_TEST_PUEUED:-$(BIN_DIR)/test-supervisor/pueued}" \
		--scenario supervisor-upgrade-recovery --parallel-tasks 2 \
		--output "$$receipt_dir/receipt.json"

# Fixture-only managed workspace regressions (lifecycle, lineage, checkpoint); not real provider acceptance.
# Each scenario writes its own receipt under bin/managed-workspace-acceptance.XXXXXX; the script owns cleanup.
acceptance-workspaces: build
	@for scenario in lifecycle lineage checkpoint; do \
		receipt_dir=$$(mktemp -d "$(BIN_DIR)/managed-workspace-acceptance.XXXXXX") || exit $$?; \
		echo "Managed workspace $$scenario acceptance evidence: $$receipt_dir/receipt.json"; \
		python3 scripts/acceptance_workspaces.py \
			--delegate "$(BIN_DIR)/delegate" \
			--runner "$(BIN_DIR)/delegate-run" \
			--pueue "$${DELEGATE_TEST_PUEUE:-$(BIN_DIR)/test-supervisor/pueue}" \
			--pueued "$${DELEGATE_TEST_PUEUED:-$(BIN_DIR)/test-supervisor/pueued}" \
			--scenario "$$scenario" \
			--output "$$receipt_dir/receipt.json" || exit $$?; \
	done

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

.PHONY: acceptance-independent-startup
acceptance-independent-startup: tool-versions
	./scripts/install-test-supervisor.sh
	DELEGATE_INDEPENDENT_EXPECTED_BACKEND="$${DELEGATE_INDEPENDENT_EXPECTED_BACKEND:?set the expected native backend}" \
	DELEGATE_TEST_PUEUE="$(BIN_DIR)/test-supervisor/pueue" \
	DELEGATE_TEST_PUEUED="$(BIN_DIR)/test-supervisor/pueued" \
	go test -race -count=1 -timeout=4m -v \
		-run '^TestIndependentStartup(NativeProof|PeerProof|CustodyNativeProof|OwnStatePairProof)$$' \
		./internal/lifetime ./internal/scheduler/host ./internal/supervisor
	go test -race -count=1 -timeout=4m -v \
		-run '^Test(Infrastructure|ValidateInfrastructure|StatePairContentStaging|Lease|Adopt|CloseSerializesWithBorrower|Origin|ReadOrigin|Startup|OwnerPublication|OwnerJournal|RootPin|IntentJournal|Context|IntentRequires|Endpoint|DecodeStrictByteSlice|WithoutProcessScopeMarker|ConfigureProcessScopeEnvironment)' \
		./internal/supervisor ./internal/supervisor/infrastructure ./internal/taskdir ./internal/task ./internal/lifetime

acceptance-native-lifetime: tool-versions
	python3 -m unittest discover -s scripts -p 'test_native_lifetime_proof.py' -v
	python3 scripts/native_lifetime_proof.py --output "$${DELEGATE_NATIVE_LIFETIME_RECEIPT:-bin/native-lifetime-proof/receipt.json}"

# Note: vuln requires access to the public vulnerability database (https://vuln.go.dev);
# the mandatory behavioral and unit tests remain hermetic.
vuln: tool-versions
	"$(GOVULN_BIN)" ./...

check:
	$(MAKE) lint-static verify-static-gates
	$(MAKE) check-platform

check-platform:
	$(MAKE) tool-versions
	$(VERIFY_SKILLS)
	$(MAKE) verify-diagram-checksums
	$(VERIFY_ACCEPTANCE_OWNERSHIP)
	$(MAKE) plugin-package-check
	$(MAKE) skill-package-check
	$(MAKE) fmt-check
	$(MAKE) config-check
	$(MAKE) vet
	$(MAKE) lint
	$(MAKE) verify-gates
	$(MAKE) test-race
	$(MAKE) test-python
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
