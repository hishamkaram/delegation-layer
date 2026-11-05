#!/usr/bin/env bash
# Black-box regression for the build and smoke stages of `make check`.
# It runs the current Makefile with GNU Make in a disposable layout where
# recording stubs replace compilers, validators, and gate scripts, so no real
# build, provider, network, or recursive tooling regression runs.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_HARNESS="$(mktemp -d)"
trap 'rm -rf "${TMP_HARNESS}"' EXIT

LAYOUT="${TMP_HARNESS}/repo"
STUB_PATH="${TMP_HARNESS}/stub-path"
RECORD="${TMP_HARNESS}/record.log"
mkdir -p "${LAYOUT}/scripts" "${LAYOUT}/bin" "${LAYOUT}/diagrams" "${STUB_PATH}"
cp "${REPO_ROOT}/Makefile" "${LAYOUT}/Makefile"

# write_stub PATH BODY writes an executable Bash stub that appends to RECORD.
write_stub() {
    printf '#!/usr/bin/env bash\nset -euo pipefail\n%s\n' "$2" > "$1"
    chmod +x "$1"
}

# The compiler stub records each build's target platform and output path and
# creates the output; STUB_FAIL_BUILD makes the first build fail after recording.
write_stub "${STUB_PATH}/go" "
if [[ \"\$1\" != build ]]; then echo \"go \$1\" >> '${RECORD}'; exit 0; fi
output=''
while [[ \$# -gt 0 ]]; do
    if [[ \"\$1\" == -o ]]; then output=\"\$2\"; fi
    shift
done
echo \"build \${GOOS:-host}/\${GOARCH:-host} \${output}\" >> '${RECORD}'
if [[ -n \"\${STUB_FAIL_BUILD:-}\" ]]; then exit 1; fi
: > \"\${output}\""
write_stub "${STUB_PATH}/python3" "echo \"python3 \$1\" >> '${RECORD}'"
write_stub "${STUB_PATH}/sha256sum" "echo sha256sum >> '${RECORD}'"
write_stub "${LAYOUT}/bin/golangci-lint" "echo \"golangci-lint \$1\" >> '${RECORD}'"
write_stub "${LAYOUT}/bin/goreleaser" "echo goreleaser >> '${RECORD}'"
write_stub "${LAYOUT}/bin/govulncheck" "echo govulncheck >> '${RECORD}'"
write_stub "${LAYOUT}/bin/gofumpt" "echo gofumpt >> '${RECORD}'"
write_stub "${LAYOUT}/scripts/check-tool-versions.sh" ":"
for gate in check-plugin-package check-skill-package verify-gates verify-tooling-regressions; do
    write_stub "${LAYOUT}/scripts/${gate}.sh" "echo ${gate} >> '${RECORD}'"
done
# STUB_FAIL_SMOKE makes the first smoke run fail after recording.
write_stub "${LAYOUT}/scripts/smoke-cli.sh" "
echo smoke-cli >> '${RECORD}'
if [[ -n \"\${STUB_FAIL_SMOKE:-}\" ]]; then exit 1; fi"

BUILD_RECORDS=(
    "build host/host bin/delegate"
    "build host/host bin/delegate-run"
    "build host/host bin/protocolfixture"
    "build darwin/amd64 dist/delegate-darwin-amd64/delegate"
    "build darwin/amd64 dist/delegate-darwin-amd64/delegate-run"
    "build darwin/arm64 dist/delegate-darwin-arm64/delegate"
    "build darwin/arm64 dist/delegate-darwin-arm64/delegate-run"
    "build linux/amd64 dist/delegate-linux-amd64/delegate"
    "build linux/amd64 dist/delegate-linux-amd64/delegate-run"
    "build linux/arm64 dist/delegate-linux-arm64/delegate"
    "build linux/arm64 dist/delegate-linux-arm64/delegate-run"
)
GATES_BEFORE_BUILD=(
    "go run" "python3 scripts/verify_skills.py" sha256sum
    "python3 scripts/verify_acceptance_ownership.py" check-plugin-package
    check-skill-package "go run" "golangci-lint config" goreleaser "go vet"
    "golangci-lint run" "python3 scripts/static_tools.py" "python3 scripts/static_analysis.py"
    "python3 scripts/static_tools.py" "python3 scripts/verify_static_gates.py"
    verify-gates verify-tooling-regressions "go test"
    "python3 -m"
)

PASSED_CASES=0

# run_make TARGET [VAR=VALUE...] runs one Make target in a fresh layout with the
# stubs first on PATH and inherited Make state removed.
run_make() {
    local target="$1"
    shift
    rm -rf "${LAYOUT}/dist" "${LAYOUT}/bin/delegate" "${LAYOUT}/bin/delegate-run" "${LAYOUT}/bin/protocolfixture"
    : > "${RECORD}"
    set +e
    output="$(env -u MAKEFLAGS -u MFLAGS -u MAKELEVEL "$@" GOTOOLCHAIN=local PATH="${STUB_PATH}:${PATH}" \
        make --no-print-directory -C "${LAYOUT}" "${target}" 2>&1)"
    status=$?
    set -e
}

# expect_run NAME EXIT_KIND EXPECTED_RECORD... requires `zero` or `nonzero` exit
# and exactly EXPECTED_RECORD, in order, in the stub record.
expect_run() {
    local name="$1" exit_kind="$2"
    shift 2
    local expected actual
    expected="$(printf '%s\n' "$@")"
    actual="$(cat "${RECORD}")"
    if [[ ("${exit_kind}" == zero && "${status}" -eq 0) || ("${exit_kind}" == nonzero && "${status}" -ne 0) ]] \
        && [[ "${actual}" == "${expected}" ]]; then
        PASSED_CASES=$((PASSED_CASES + 1))
        echo "PASS [check-build ${PASSED_CASES}]: ${name}"
        return
    fi
    echo "FAIL: ${name}: exit ${status} (expected ${exit_kind}); record diff, expected < actual >:" >&2
    diff <(echo "${expected}") <(echo "${actual}") >&2 || true
    echo "${output}" >&2
    exit 1
}

run_make check
expect_run "check runs every gate in order and each of the 11 builds once before smoke" zero \
    "${GATES_BEFORE_BUILD[@]}" "${BUILD_RECORDS[@]}" smoke-cli smoke-cli govulncheck

run_make build
expect_run "standalone build compiles all 11 outputs" zero "${BUILD_RECORDS[@]}"
for record in "${BUILD_RECORDS[@]}"; do
    if [[ ! -f "${LAYOUT}/${record##* }" ]]; then
        echo "FAIL: standalone build did not produce ${record##* }" >&2
        exit 1
    fi
done

run_make smoke-cli
expect_run "standalone smoke-cli builds its prerequisite once, then smokes" zero \
    "${BUILD_RECORDS[@]}" smoke-cli smoke-cli

run_make check STUB_FAIL_BUILD=1
expect_run "check stops at a build failure before smoke and vuln" nonzero \
    "${GATES_BEFORE_BUILD[@]}" "${BUILD_RECORDS[0]}"

run_make check STUB_FAIL_SMOKE=1
expect_run "check stops at a smoke failure before vuln" nonzero \
    "${GATES_BEFORE_BUILD[@]}" "${BUILD_RECORDS[@]}" smoke-cli

echo "All ${PASSED_CASES} check build-stage regression checks passed."
