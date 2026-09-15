#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
PUEUE_TEST_CLIENT="${DELEGATE_TEST_PUEUE:-${REPO_ROOT}/bin/test-supervisor/pueue}"
PUEUE_TEST_DAEMON="${DELEGATE_TEST_PUEUED:-${REPO_ROOT}/bin/test-supervisor/pueued}"
for binary in "${PUEUE_TEST_CLIENT}" "${PUEUE_TEST_DAEMON}"; do
    if [[ "${binary}" != /* || ! -x "${binary}" ]]; then
        echo "ERROR: An absolute executable pueue/pueued test pair is required. Run scripts/install-test-supervisor.sh first." >&2
        exit 1
    fi
done
if ! command -v python3 >/dev/null 2>&1; then
    echo "ERROR: Python 3 is required for finite supervisor acceptance." >&2
    exit 1
fi
mkdir -p "${REPO_ROOT}/bin/supervisor-acceptance"
ACCEPTANCE_OUTPUT="$(mktemp -d "${REPO_ROOT}/bin/supervisor-acceptance/run.XXXXXX")"
export PYTHONDONTWRITEBYTECODE=1
echo "Supervisor acceptance evidence: ${ACCEPTANCE_OUTPUT}"
python3 "${SCRIPT_DIR}/acceptance_supervisor_hermetic.py" \
    --tools "${REPO_ROOT}/bin/harness-tools" --output "${ACCEPTANCE_OUTPUT}/hermetic"
python3 "${SCRIPT_DIR}/acceptance_supervisor_native.py" \
    --tools "${REPO_ROOT}/bin/harness-tools" --pueue "${PUEUE_TEST_CLIENT}" \
    --pueued "${PUEUE_TEST_DAEMON}" --output "${ACCEPTANCE_OUTPUT}/native"
