#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN_DIR="${REPO_ROOT}/bin"
FIXTURE_BIN="${BIN_DIR}/protocolfixture"

echo "=== Protocol Acceptance: 49-Case Fault Matrix ==="
echo "Platform: $(uname -a)"
echo "Go host: $(go env GOHOSTOS)/$(go env GOHOSTARCH)"

cd "${REPO_ROOT}"
mkdir -p "${BIN_DIR}"
CGO_ENABLED=0 go build -buildvcs=false -o "${FIXTURE_BIN}" ./internal/testutil/protocolfixture

echo "Binary: ${FIXTURE_BIN}"

# 1. Run full 49-case acceptance suite under -race -count=1
echo "Running 49-case fault matrix acceptance suite under race detector..."
go test -race -count=1 -v ./internal/testutil/protocolfixture

# 2. Live CLI verification through standalone subprocess invocations
echo "Running live CLI workflow verification..."
PROTOCOL_TMP_ROOT="$(mktemp -d)"
cleanup() {
    protocol_exit=$?
    trap - EXIT
    if ! rm -rf -- "${PROTOCOL_TMP_ROOT}"; then
        exit 1
    fi
    exit "${protocol_exit}"
}
trap cleanup EXIT

STORE_ROOT="${PROTOCOL_TMP_ROOT}/store"
WORKSPACE_DIR="${PROTOCOL_TMP_ROOT}/workspace"
mkdir -m 700 "${STORE_ROOT}" "${WORKSPACE_DIR}"
TASK_ID="0123456789abcdef0123456789abcdef"
BRIEF_FILE="${WORKSPACE_DIR}/brief.md"
printf '%s\n' 'Live acceptance brief content' > "${BRIEF_FILE}"
SINK_LOG="${PROTOCOL_TMP_ROOT}/sink.log"
EXPECTED="${PROTOCOL_TMP_ROOT}/expected"
printf '  exact live answer Ω\n\n' > "${EXPECTED}"

# Prepare
"${FIXTURE_BIN}" prepare -root "${STORE_ROOT}" -task-id "${TASK_ID}" -canonical-cwd "${WORKSPACE_DIR}" -brief-file "${BRIEF_FILE}" >/dev/null

# Claim admission
"${FIXTURE_BIN}" claim -root "${STORE_ROOT}" -task-id "${TASK_ID}" -kind admission -fake-sink-event "${SINK_LOG}" >/dev/null

# Start, capture, seal and publish under one continuous runner lease.
"${FIXTURE_BIN}" seal -root "${STORE_ROOT}" -task-id "${TASK_ID}" -raw-stdout-file "${EXPECTED}" -fake-sink-event "${SINK_LOG}" -publish >/dev/null

# Repeated independent collection must preserve exact bytes and create no work.
cp "${SINK_LOG}" "${PROTOCOL_TMP_ROOT}/sink-before"
cp "${STORE_ROOT}/tasks/${TASK_ID}/outcome.json" "${PROTOCOL_TMP_ROOT}/outcome-before"
for attempt in 1 2; do
    "${FIXTURE_BIN}" collect -root "${STORE_ROOT}" -task-id "${TASK_ID}" -raw-output > "${PROTOCOL_TMP_ROOT}/answer-${attempt}"
    cmp "${EXPECTED}" "${PROTOCOL_TMP_ROOT}/answer-${attempt}"
done
cmp "${PROTOCOL_TMP_ROOT}/sink-before" "${SINK_LOG}"
cmp "${PROTOCOL_TMP_ROOT}/outcome-before" "${STORE_ROOT}/tasks/${TASK_ID}/outcome.json"
awk -F: -v task="${TASK_ID}" '
    NF != 3 || $1 !~ /^[0-9]+$/ || $3 != task { bad = 1 }
    $2 == "admission_sink" { admissions++ }
    $2 == "runner_sink" { starts++ }
    END { exit (bad || NR != 2 || admissions != 1 || starts != 1) }
' "${SINK_LOG}"

# Inspect
INSPECT_JSON="$("${FIXTURE_BIN}" inspect -root "${STORE_ROOT}" -task-id "${TASK_ID}")"
if ! echo "${INSPECT_JSON}" | grep -q '"publication":2'; then
    echo "FAIL: Expected publication committed (2) in inspection output: ${INSPECT_JSON}" >&2
    exit 1
fi

# Scavenge
SCAVENGE_JSON="$("${FIXTURE_BIN}" scavenge -root "${STORE_ROOT}")"
if ! echo "${SCAVENGE_JSON}" | grep -q '"status":"scavenged"'; then
    echo "FAIL: Scavenge failed: ${SCAVENGE_JSON}" >&2
    exit 1
fi

echo "=== All 49 Fault Matrix Cases Verified (G01-G14, R01-R14, E01-E09, L01-L09, S01-S03) ==="
