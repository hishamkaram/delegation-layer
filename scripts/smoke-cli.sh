#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN_DIR="${REPO_ROOT}/bin"
BINARY="${BIN_DIR}/delegate"

if [[ ! -x "${BINARY}" ]]; then
    echo "ERROR: Binary not found at ${BINARY}. Build it first with 'make build'." >&2
    exit 1
fi

# Record native platform receipt details
echo "=== Native Smoke CLI Test ==="
echo "Platform uname: $(uname -a)"
echo "Go host OS/Arch: $(go env GOHOSTOS)/$(go env GOHOSTARCH)"
echo "Binary path: ${BINARY}"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

PASSED_CASES=0

assert_case() {
    local name="$1"
    local expected_code="$2"
    local stdout_mode="$3" # "empty", "usage", "version", "providers"
    local stderr_mode="$4" # "empty", "error"
    local expected_err_substr="$5"
    shift 5

    local stdout_file="${TMP_DIR}/stdout.log"
    local stderr_file="${TMP_DIR}/stderr.log"

    local actual_code=0
    if [[ $# -gt 0 ]]; then
        "${BINARY}" "$@" >"${stdout_file}" 2>"${stderr_file}" || actual_code=$?
    else
        "${BINARY}" >"${stdout_file}" 2>"${stderr_file}" || actual_code=$?
    fi

    # Check exit code
    if [[ "${actual_code}" -ne "${expected_code}" ]]; then
        echo "FAIL [${name}]: expected exit code ${expected_code}, got ${actual_code}" >&2
        echo "Stderr output was:" >&2
        cat "${stderr_file}" >&2
        exit 1
    fi

    # Check stdout
    case "${stdout_mode}" in
        empty)
            if [[ -s "${stdout_file}" ]]; then
                echo "FAIL [${name}]: expected empty stdout, got: $(cat "${stdout_file}")" >&2
                exit 1
            fi
            ;;
        usage)
            if ! grep -q "Usage: delegate" "${stdout_file}"; then
                echo "FAIL [${name}]: stdout missing usage text. Got: $(cat "${stdout_file}")" >&2
                exit 1
            fi
            ;;
        version)
            if ! grep -q "^delegate " "${stdout_file}"; then
                echo "FAIL [${name}]: stdout missing version string. Got: $(cat "${stdout_file}")" >&2
                exit 1
            fi
            ;;
        providers)
            if ! grep -q '"schema_version":1' "${stdout_file}" || ! grep -q 'antigravity:print' "${stdout_file}"; then
                echo "FAIL [${name}]: stdout missing provider discovery metadata. Got: $(cat "${stdout_file}")" >&2
                exit 1
            fi
            ;;
    esac

    # Check stderr
    case "${stderr_mode}" in
        empty)
            if [[ -s "${stderr_file}" ]]; then
                echo "FAIL [${name}]: expected empty stderr, got: $(cat "${stderr_file}")" >&2
                exit 1
            fi
            ;;
        error)
            if [[ -n "${expected_err_substr}" ]] && ! grep -q "${expected_err_substr}" "${stderr_file}"; then
                echo "FAIL [${name}]: stderr missing expected substring '${expected_err_substr}'. Got: $(cat "${stderr_file}")" >&2
                exit 1
            fi
            if ! grep -q "Usage: delegate" "${stderr_file}"; then
                echo "FAIL [${name}]: stderr missing usage guidance. Got: $(cat "${stderr_file}")" >&2
                exit 1
            fi
            ;;
    esac

    PASSED_CASES=$((PASSED_CASES + 1))
    echo "PASS [${PASSED_CASES}/12]: ${name}"
}

# 1. No arguments: exit 0, usage on stdout, empty stderr
assert_case "no arguments" 0 usage empty ""

# 2. help: exit 0, usage on stdout, empty stderr
assert_case "help command" 0 usage empty "" help

# 3. --help: exit 0, usage on stdout, empty stderr
assert_case "--help flag" 0 usage empty "" --help

# 4. -h: exit 0, usage on stdout, empty stderr
assert_case "-h flag" 0 usage empty "" -h

# 5. version: exit 0, version on stdout, empty stderr
assert_case "version command" 0 version empty "" version

# 6. --version: exit 0, version on stdout, empty stderr
assert_case "--version flag" 0 version empty "" --version

# 7. providers discovery: exit 0, bounded JSON on stdout, empty stderr
assert_case "providers discovery" 0 providers empty "" providers --json

# 8. unknown command: exit 2, error+usage on stderr, empty stdout
assert_case "unknown command" 2 empty error 'error: unknown command or flag "unknown-cmd"' unknown-cmd

# 9. unknown flag: exit 2, error+usage on stderr, empty stdout
assert_case "unknown flag" 2 empty error 'error: unknown command or flag "--invalid-flag"' --invalid-flag

# 10. extra argument to help: exit 2, error+usage on stderr, empty stdout
assert_case "extra argument to help" 2 empty error 'unexpected extra argument "extra-arg" for help' help extra-arg

# 11. extra argument to version: exit 2, error+usage on stderr, empty stdout
assert_case "extra argument to version" 2 empty error 'unexpected extra argument "extra-arg" for version' version extra-arg

# 12. extra argument to providers: exit 2, error+usage on stderr, empty stdout
assert_case "extra argument to providers" 2 empty error 'unexpected extra argument "extra-arg" for providers' providers extra-arg

if [[ "${PASSED_CASES}" -ne 12 ]]; then
    echo "FAIL: Expected 12 smoke test cases to execute, but only ${PASSED_CASES} passed." >&2
    exit 1
fi

echo "All smoke tests passed (${PASSED_CASES}/12 cases executed)."
