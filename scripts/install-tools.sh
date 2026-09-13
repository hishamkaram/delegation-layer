#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN_DIR="${REPO_ROOT}/bin"

mkdir -p "${BIN_DIR}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

export GOTOOLCHAIN=local

# Verify Go version
GO_VERSION_OUTPUT="$(go version)"
GO_VERSION_TOKEN="$(echo "${GO_VERSION_OUTPUT}" | awk '{print $3}')"
if [[ "${GO_VERSION_TOKEN}" != "go1.27.1" ]]; then
    echo "ERROR: Go version mismatch. Required: go1.27.1. Found: ${GO_VERSION_TOKEN}" >&2
    echo "Action: Install Go 1.27.1 and ensure GOTOOLCHAIN=local is respected." >&2
    exit 1
fi

OS="$(uname -s)"
ARCH="$(uname -m)"

case "${OS}" in
    Darwin)
        GOLANGCI_OS="darwin"
        GORELEASER_OS="Darwin"
        ;;
    Linux)
        GOLANGCI_OS="linux"
        GORELEASER_OS="Linux"
        ;;
    *)
        echo "ERROR: Unsupported operating system: ${OS}. Supported: Darwin, Linux." >&2
        exit 1
        ;;
esac

case "${ARCH}" in
    x86_64|amd64)
        GOLANGCI_ARCH="amd64"
        GORELEASER_ARCH="x86_64"
        ;;
    arm64|aarch64)
        GOLANGCI_ARCH="arm64"
        GORELEASER_ARCH="arm64"
        ;;
    *)
        echo "ERROR: Unsupported architecture: ${ARCH}. Supported: x86_64/amd64, arm64/aarch64." >&2
        exit 1
        ;;
esac

# Pinned SHA256 checksums from official release checksum manifests
# golangci-lint v2.13.2
GOLANGCI_VERSION="2.13.2"
GOLANGCI_TAR="golangci-lint-${GOLANGCI_VERSION}-${GOLANGCI_OS}-${GOLANGCI_ARCH}.tar.gz"
GOLANGCI_URL="https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_VERSION}/${GOLANGCI_TAR}"

get_golangci_sha256() {
    case "$1" in
        darwin-amd64) echo "8a13aaf9cbbb1dee52824e862cf0d0720e5bb97c1f4260d1e51623a09492b57b" ;;
        darwin-arm64) echo "f4bf83f0b64f055c42b28fc9a38861839f69c096e61c788e72dfaae412011789" ;;
        linux-amd64)  echo "2277d43b98ec0054280f2ac26b53268bae97682444678a59a657dd565da021d6" ;;
        linux-arm64)  echo "a2a4e0065aa41be71f7c5ac90f271b61751331e5d04314e62afe4027855f0893" ;;
        *)
            echo "ERROR: Unknown golangci-lint target key: $1" >&2
            return 1
            ;;
    esac
}

# GoReleaser v2.18.1
GORELEASER_VERSION="2.18.1"
GORELEASER_TAR="goreleaser_${GORELEASER_OS}_${GORELEASER_ARCH}.tar.gz"
GORELEASER_URL="https://github.com/goreleaser/goreleaser/releases/download/v${GORELEASER_VERSION}/${GORELEASER_TAR}"

get_goreleaser_sha256() {
    case "$1" in
        Darwin-x86_64) echo "623e9ba517ace49c3d6b57bcfe8f5fe33ca45313ee93261c1854464cca94d861" ;;
        Darwin-arm64)  echo "8e912c5cc78896d791b7530e672d4a4ef9c00ebff7375de410fae1b459825ea3" ;;
        Linux-x86_64)  echo "0c6122af0ad8fd65638889bf7d3757148b2f80eeff9f079682f0655df66ec8e8" ;;
        Linux-arm64)   echo "93dba7614308e167158bd26978e8275971fd4b9147e7f3c687a64f5939d42d27" ;;
        *)
            echo "ERROR: Unknown goreleaser target key: $1" >&2
            return 1
            ;;
    esac
}

compute_sha256() {
    local file="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "${file}" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "${file}" | awk '{print $1}'
    else
        echo "ERROR: Neither sha256sum nor shasum is available." >&2
        exit 1
    fi
}

echo "Installing tools into ${BIN_DIR}..."

# 1. golangci-lint 2.13.2
GOLANGCI_KEY="${GOLANGCI_OS}-${GOLANGCI_ARCH}"
EXPECTED_GOLANGCI_SHA="$(get_golangci_sha256 "${GOLANGCI_KEY}")"
echo "Downloading golangci-lint v${GOLANGCI_VERSION} (${GOLANGCI_KEY})..."
curl -sSL -f -o "${TMP_DIR}/${GOLANGCI_TAR}" "${GOLANGCI_URL}"
ACTUAL_GOLANGCI_SHA="$(compute_sha256 "${TMP_DIR}/${GOLANGCI_TAR}")"
if [[ "${ACTUAL_GOLANGCI_SHA}" != "${EXPECTED_GOLANGCI_SHA}" ]]; then
    echo "ERROR: SHA256 mismatch for ${GOLANGCI_TAR}!" >&2
    echo "  Expected: ${EXPECTED_GOLANGCI_SHA}" >&2
    echo "  Actual:   ${ACTUAL_GOLANGCI_SHA}" >&2
    exit 1
fi
tar -xzf "${TMP_DIR}/${GOLANGCI_TAR}" -C "${TMP_DIR}"
cp "${TMP_DIR}/golangci-lint-${GOLANGCI_VERSION}-${GOLANGCI_OS}-${GOLANGCI_ARCH}/golangci-lint" "${BIN_DIR}/golangci-lint"
chmod 0755 "${BIN_DIR}/golangci-lint"
echo "Installed golangci-lint v${GOLANGCI_VERSION}"

# 2. GoReleaser 2.18.1
GORELEASER_KEY="${GORELEASER_OS}-${GORELEASER_ARCH}"
EXPECTED_GORELEASER_SHA="$(get_goreleaser_sha256 "${GORELEASER_KEY}")"
echo "Downloading GoReleaser v${GORELEASER_VERSION} (${GORELEASER_KEY})..."
curl -sSL -f -o "${TMP_DIR}/${GORELEASER_TAR}" "${GORELEASER_URL}"
ACTUAL_GORELEASER_SHA="$(compute_sha256 "${TMP_DIR}/${GORELEASER_TAR}")"
if [[ "${ACTUAL_GORELEASER_SHA}" != "${EXPECTED_GORELEASER_SHA}" ]]; then
    echo "ERROR: SHA256 mismatch for ${GORELEASER_TAR}!" >&2
    echo "  Expected: ${EXPECTED_GORELEASER_SHA}" >&2
    echo "  Actual:   ${ACTUAL_GORELEASER_SHA}" >&2
    exit 1
fi
tar -xzf "${TMP_DIR}/${GORELEASER_TAR}" -C "${TMP_DIR}" goreleaser
cp "${TMP_DIR}/goreleaser" "${BIN_DIR}/goreleaser"
chmod 0755 "${BIN_DIR}/goreleaser"
echo "Installed GoReleaser v${GORELEASER_VERSION}"

# 3. gofumpt 0.12.0
echo "Installing gofumpt v0.12.0..."
GOBIN="${BIN_DIR}" go install mvdan.cc/gofumpt@v0.12.0
echo "Installed gofumpt v0.12.0"

# 4. govulncheck 1.8.0
echo "Installing govulncheck v1.8.0..."
GOBIN="${BIN_DIR}" go install golang.org/x/vuln/cmd/govulncheck@v1.8.0
echo "Installed govulncheck v1.8.0"

# Verify all installed tools
echo "Verifying tool versions..."
"${SCRIPT_DIR}/check-tool-versions.sh"
echo "All tools installed and verified successfully."
