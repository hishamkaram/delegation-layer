#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN_DIR="${REPO_ROOT}/bin"
CONFIG_FILE="${REPO_ROOT}/.golangci.yml"
GOLANGCI_BIN="${BIN_DIR}/golangci-lint"

if [[ ! -x "${GOLANGCI_BIN}" ]]; then
    echo "ERROR: golangci-lint not found at ${GOLANGCI_BIN}. Run 'make tools' first." >&2
    exit 1
fi

if [[ ! -f "${CONFIG_FILE}" ]]; then
    echo "ERROR: Config file not found at ${CONFIG_FILE}." >&2
    exit 1
fi

export GOTOOLCHAIN=local

TMP_HARNESS="$(mktemp -d)"
# Clean up only the harness's own temp directory; no signals or broad patterns
trap 'rm -rf "${TMP_HARNESS}"' EXIT

echo "=== Running Gate Rejection & Acceptance Fixtures ==="

run_fixture_test() {
    local name="$1"
    local linter="$2"
    local expected_err="$3"
    local neg_source="$4"
    local pos_source="$5"

    local fixture_dir="${TMP_HARNESS}/${name}"
    mkdir -p "${fixture_dir}"

    # 1. Test Negative Fixture
    cat << EOF > "${fixture_dir}/go.mod"
module fixturetest

go 1.27.0
EOF
    echo "${neg_source}" > "${fixture_dir}/fixture.go"

    # Fixtures are source input; prove compilation with go build, never run them
    if ! (cd "${fixture_dir}" && go build ./... >/dev/null 2>&1); then
        echo "FAIL [${name}]: Negative fixture failed to compile with 'go build'!" >&2
        exit 1
    fi

    local lint_out
    local lint_code=0
    lint_out="$(cd "${fixture_dir}" && "${GOLANGCI_BIN}" run --config "${CONFIG_FILE}" --enable-only "${linter}" ./... 2>&1)" || lint_code=$?

    if [[ "${lint_code}" -ne 1 ]]; then
        echo "FAIL [${name}]: Expected linter exit 1, but got exit code ${lint_code}!" >&2
        echo "Linter output was:" >&2
        echo "${lint_out}" >&2
        exit 1
    fi

    if [[ -n "${expected_err}" ]] && ! echo "${lint_out}" | grep -q "${expected_err}"; then
        echo "FAIL [${name}]: Missing expected diagnostic '${expected_err}'." >&2
        echo "Linter output was:" >&2
        echo "${lint_out}" >&2
        exit 1
    fi
    echo "PASS [${name}]: Negative fixture correctly failed with expected diagnostic."

    # 2. Test Corrected Counterpart
    echo "${pos_source}" > "${fixture_dir}/fixture.go"
    if ! (cd "${fixture_dir}" && go build ./... >/dev/null 2>&1); then
        echo "FAIL [${name}]: Corrected fixture failed to compile with 'go build'!" >&2
        exit 1
    fi

    local pos_out
    local pos_code=0
    pos_out="$(cd "${fixture_dir}" && "${GOLANGCI_BIN}" run --config "${CONFIG_FILE}" --enable-only "${linter}" ./... 2>&1)" || pos_code=$?

    if [[ "${pos_code}" -ne 0 ]]; then
        echo "FAIL [${name}]: Corrected fixture failed linting (exit ${pos_code})!" >&2
        echo "Linter output was:" >&2
        echo "${pos_out}" >&2
        exit 1
    fi
    echo "PASS [${name}]: Corrected fixture passed linter."
}

# 1. Process.Signal
run_fixture_test \
    "process-signal" \
    "forbidigo" \
    "p.Signal.*Direct process signals are prohibited" \
    'package fixture
import "os"
func Bad() {
	p := &os.Process{}
	_ = p.Signal(nil)
}' \
    'package fixture
func Good() {}'

# 2. cmd.Process.Kill
run_fixture_test \
    "cmd-process-kill" \
    "forbidigo" \
    "cmd.Process.Kill.*Direct process signals are prohibited" \
    'package fixture
import "os/exec"
func Bad() {
	cmd := &exec.Cmd{}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}' \
    'package fixture
func Good() {}'

# 3. syscall.Kill
run_fixture_test \
    "syscall-kill" \
    "forbidigo" \
    "syscall.Kill.*Direct syscall signals and setsid are prohibited" \
    'package fixture
import "syscall"
func Bad() {
	_ = syscall.Kill(1, 9)
}' \
    'package fixture
func Good() {}'

# 4. syscall.Setsid
run_fixture_test \
    "syscall-setsid" \
    "forbidigo" \
    "syscall.Setsid.*Direct syscall signals and setsid are prohibited" \
    'package fixture
import "syscall"
func Bad() {
	_, _ = syscall.Setsid()
}' \
    'package fixture
func Good() {}'

# 5. exec.CommandContext
run_fixture_test \
    "exec-command-context" \
    "forbidigo" \
    "exec.CommandContext.*defaults to Process.Kill" \
    'package fixture
import (
	"context"
	"os/exec"
)
func Bad() {
	ctx := context.Background()
	_ = exec.CommandContext(ctx, "echo", "hi")
}' \
    'package fixture
import "os/exec"
func Good() {
	_ = exec.Command("echo", "hi")
}'

# 6. nonzero Cmd.WaitDelay assignment
run_fixture_test \
    "cmd-waitdelay" \
    "forbidigo" \
    "cmd.WaitDelay.*Nonzero Cmd.WaitDelay can implicitly kill" \
    'package fixture
import (
	"os/exec"
	"time"
)
func Bad() {
	cmd := &exec.Cmd{}
	cmd.WaitDelay = time.Second
}' \
    'package fixture
import "os/exec"
func Good() {
	_ = &exec.Cmd{}
}'

# 7. ignored bare filesystem error
run_fixture_test \
    "bare-fs-error" \
    "errcheck" \
    "Error return value of \`os.Remove\` is not checked" \
    'package fixture
import "os"
func Bad() {
	os.Remove("temp.txt")
}' \
    'package fixture
import "os"
func Good() error {
	if err := os.Remove("temp.txt"); err != nil {
		return err
	}
	return nil
}'

# 8. _ = f.Sync()
run_fixture_test \
    "ignored-sync-blank" \
    "errcheck" \
    "Error return value of \`f.Sync\` is not checked" \
    'package fixture
import "os"
func Bad() {
	f, err := os.Open("temp.txt")
	if err != nil {
		return
	}
	_ = f.Sync()
}' \
    'package fixture
import "os"
func Good() error {
	f, err := os.Open("temp.txt")
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return nil
}'

# 9. numeric tri-state switch missing Unknown while carrying default
run_fixture_test \
    "exhaustive-unknown" \
    "exhaustive" \
    "missing cases in switch of type.*State:.*Unknown" \
    'package fixture
type State int
const (
	Unknown State = iota
	Active
	Inactive
)
func Bad(s State) string {
	switch s {
	case Active:
		return "active"
	case Inactive:
		return "inactive"
	default:
		return "other"
	}
}' \
    'package fixture
type State int
const (
	Unknown State = iota
	Active
	Inactive
)
func Good(s State) string {
	switch s {
	case Unknown:
		return "unknown"
	case Active:
		return "active"
	case Inactive:
		return "inactive"
	default:
		return "other"
	}
}'

# 10. Known-good full fixture that must pass the whole config
echo "Testing known-good fixture against the complete configuration..."
FULL_FIXTURE_DIR="${TMP_HARNESS}/known-good"
mkdir -p "${FULL_FIXTURE_DIR}"
cat << EOF > "${FULL_FIXTURE_DIR}/go.mod"
module fixturetest

go 1.27.0
EOF

cat << "EOF" > "${FULL_FIXTURE_DIR}/fixture.go"
package fixture

import (
	"fmt"
	"io"
)

// State demonstrates an exhaustive enum pattern.
type State int

const (
	Unknown State = iota
	Ready
	Done
)

func CheckState(s State) string {
	switch s {
	case Unknown:
		return "unknown"
	case Ready:
		return "ready"
	case Done:
		return "done"
	default:
		return "other"
	}
}

func WriteSafe(w io.Writer, msg string) error {
	if _, err := fmt.Fprintln(w, msg); err != nil {
		return err
	}
	return nil
}
EOF

if ! (cd "${FULL_FIXTURE_DIR}" && go build ./... >/dev/null 2>&1); then
    echo "FAIL: Known-good fixture failed to compile!" >&2
    exit 1
fi

FULL_OUT="$(cd "${FULL_FIXTURE_DIR}" && "${GOLANGCI_BIN}" run --config "${CONFIG_FILE}" ./... 2>&1)" || {
    echo "FAIL: Known-good fixture failed full linter configuration!" >&2
    echo "${FULL_OUT}" >&2
    exit 1
}
echo "PASS: Known-good fixture passed full configuration."

echo "All gate rejection and acceptance fixtures passed."
