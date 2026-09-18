#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

if ! command -v npm >/dev/null 2>&1; then
    echo "ERROR: npm is required for the agent skill package check." >&2
    exit 1
fi

npm pack --ignore-scripts --pack-destination "${TMP_DIR}" >/dev/null
PACKAGE_TARBALL="$(find "${TMP_DIR}" -maxdepth 1 -type f -name '*.tgz' -print -quit)"
if [[ -z "${PACKAGE_TARBALL}" ]]; then
    echo "ERROR: npm pack did not produce a package archive." >&2
    exit 1
fi

if ! tar -tzf "${PACKAGE_TARBALL}" | grep -Fx 'package/skills/agent-integration/SKILL.md' >/dev/null; then
    echo "ERROR: package archive does not contain the canonical agent skill." >&2
    exit 1
fi

INSTALL_ROOT="${TMP_DIR}/install"
npm install --ignore-scripts --no-save --prefix "${INSTALL_ROOT}" "${PACKAGE_TARBALL}" >/dev/null
INSTALLER="${INSTALL_ROOT}/node_modules/.bin/delegation-layer-agent-integration"
TARGET="${TMP_DIR}/installed/agent-integration"
node "${INSTALLER}" --target "${TARGET}"
cmp "${REPO_ROOT}/skills/agent-integration/SKILL.md" "${TARGET}/SKILL.md"
node "${INSTALLER}" --target "${TARGET}"

echo "Agent skill package check passed: ${PACKAGE_TARBALL}"
