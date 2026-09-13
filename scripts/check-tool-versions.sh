#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN_DIR="${REPO_ROOT}/bin"

if [[ "${GOTOOLCHAIN:-}" != "" && "${GOTOOLCHAIN:-}" != "local" ]]; then
    echo "ERROR: Conflicting GOTOOLCHAIN setting: '${GOTOOLCHAIN}'. Must be unset or 'local'." >&2
    exit 1
fi
export GOTOOLCHAIN=local

# 1. Check Go compiler
if ! command -v go >/dev/null 2>&1; then
    echo "ERROR: 'go' binary not found on PATH." >&2
    echo "Action: Install Go 1.27.1 and add it to PATH." >&2
    exit 1
fi

ACTUAL_GOTOOLCHAIN="$(go env GOTOOLCHAIN)"
if [[ "${ACTUAL_GOTOOLCHAIN}" != "local" ]]; then
    echo "ERROR: Effective GOTOOLCHAIN is not 'local'. Found: ${ACTUAL_GOTOOLCHAIN}" >&2
    exit 1
fi

if ! GO_VERSION_OUTPUT="$(go version 2>&1)"; then
    echo "ERROR: 'go version' command failed." >&2
    exit 1
fi

GO_VERSION_TOKEN="$(echo "${GO_VERSION_OUTPUT}" | awk '{print $3}')"
if [[ "${GO_VERSION_TOKEN}" != "go1.27.1" ]]; then
    echo "ERROR: Go version mismatch. Required: go1.27.1. Found: ${GO_VERSION_TOKEN}" >&2
    echo "Action: Install Go 1.27.1 and ensure GOTOOLCHAIN=local is respected." >&2
    exit 1
fi
echo "Go compiler: ${GO_VERSION_OUTPUT}"

# 2. Check golangci-lint
GOLANGCI_BIN="${BIN_DIR}/golangci-lint"
if [[ ! -x "${GOLANGCI_BIN}" ]]; then
    echo "ERROR: golangci-lint not found at ${GOLANGCI_BIN}." >&2
    echo "Action: Run 'make tools' to install pinned tools into ./bin." >&2
    exit 1
fi
if ! GOLANGCI_OUT="$("${GOLANGCI_BIN}" version 2>&1)"; then
    echo "ERROR: '${GOLANGCI_BIN} version' failed: ${GOLANGCI_OUT}" >&2
    exit 1
fi
GOLANGCI_TOKEN="$(echo "${GOLANGCI_OUT}" | awk '{print $4}')"
if [[ "${GOLANGCI_TOKEN}" != "2.13.2" ]]; then
    echo "ERROR: golangci-lint version mismatch at ${GOLANGCI_BIN}. Required: 2.13.2. Found: ${GOLANGCI_TOKEN}" >&2
    echo "Action: Run 'make tools' to re-install pinned tools." >&2
    exit 1
fi
echo "golangci-lint: ${GOLANGCI_OUT}"

# 3. Check gofumpt
GOFUMPT_BIN="${BIN_DIR}/gofumpt"
if [[ ! -x "${GOFUMPT_BIN}" ]]; then
    echo "ERROR: gofumpt not found at ${GOFUMPT_BIN}." >&2
    echo "Action: Run 'make tools' to install pinned tools into ./bin." >&2
    exit 1
fi
if ! GOFUMPT_OUT="$("${GOFUMPT_BIN}" -version 2>&1)"; then
    echo "ERROR: '${GOFUMPT_BIN} -version' failed: ${GOFUMPT_OUT}" >&2
    exit 1
fi
GOFUMPT_TOKEN="$(echo "${GOFUMPT_OUT}" | awk '{print $1}' | sed 's/^v//')"
if [[ "${GOFUMPT_TOKEN}" != "0.12.0" ]]; then
    echo "ERROR: gofumpt version mismatch at ${GOFUMPT_BIN}. Required: 0.12.0. Found: ${GOFUMPT_TOKEN}" >&2
    echo "Action: Run 'make tools' to re-install pinned tools." >&2
    exit 1
fi
echo "gofumpt: ${GOFUMPT_OUT}"

# 4. Check govulncheck
GOVULN_BIN="${BIN_DIR}/govulncheck"
if [[ ! -x "${GOVULN_BIN}" ]]; then
    echo "ERROR: govulncheck not found at ${GOVULN_BIN}." >&2
    echo "Action: Run 'make tools' to install pinned tools into ./bin." >&2
    exit 1
fi
if ! GOVULN_OUT="$("${GOVULN_BIN}" -version 2>&1)"; then
    echo "ERROR: '${GOVULN_BIN} -version' failed: ${GOVULN_OUT}" >&2
    exit 1
fi
GOVULN_TOKEN="$(echo "${GOVULN_OUT}" | awk '/^Scanner: govulncheck@v/ { sub(/^Scanner: govulncheck@v/, ""); print $1; exit }')"
if [[ "${GOVULN_TOKEN}" != "1.8.0" ]]; then
    echo "ERROR: govulncheck version mismatch at ${GOVULN_BIN}. Required: 1.8.0. Found: ${GOVULN_TOKEN}" >&2
    echo "Action: Run 'make tools' to re-install pinned tools." >&2
    exit 1
fi
echo "govulncheck: $(echo "${GOVULN_OUT}" | awk '/^Scanner:/ { print; exit }')"

# 5. Check GoReleaser
GORELEASER_BIN="${BIN_DIR}/goreleaser"
if [[ ! -x "${GORELEASER_BIN}" ]]; then
    echo "ERROR: goreleaser not found at ${GORELEASER_BIN}." >&2
    echo "Action: Run 'make tools' to install pinned tools into ./bin." >&2
    exit 1
fi
if ! GORELEASER_OUT="$("${GORELEASER_BIN}" --version 2>&1)"; then
    echo "ERROR: '${GORELEASER_BIN} --version' failed: ${GORELEASER_OUT}" >&2
    exit 1
fi
GORELEASER_TOKEN="$(echo "${GORELEASER_OUT}" | awk '$1 == "GitVersion:" { print $2; exit }')"
if [[ "${GORELEASER_TOKEN}" != "2.18.1" ]]; then
    echo "ERROR: goreleaser version mismatch at ${GORELEASER_BIN}. Required: 2.18.1. Found: ${GORELEASER_TOKEN}" >&2
    echo "Action: Run 'make tools' to re-install pinned tools." >&2
    exit 1
fi
echo "goreleaser: $(echo "${GORELEASER_OUT}" | awk '$1 == "GitVersion:" { sub(/^[[:space:]]+/, ""); print; exit }')"

echo "Tool verification passed."
