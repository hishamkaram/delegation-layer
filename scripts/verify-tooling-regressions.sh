#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_HARNESS="$(mktemp -d)"
trap 'rm -rf "${TMP_HARNESS}"' EXIT

echo "=== Running Tooling Regression Checks (Black-Box) ==="

PASSED_CASES=0
assert_pass() {
    local name="$1"
    PASSED_CASES=$((PASSED_CASES + 1))
    echo "PASS [case ${PASSED_CASES}]: ${name}"
}

# Create disposable repository layout
DISPOSABLE_REPO="${TMP_HARNESS}/repo"
mkdir -p "${DISPOSABLE_REPO}/scripts" "${DISPOSABLE_REPO}/bin"

cp "${REPO_ROOT}/scripts/check-tool-versions.sh" "${DISPOSABLE_REPO}/scripts/"
cp "${REPO_ROOT}/scripts/install-tools.sh" "${DISPOSABLE_REPO}/scripts/"
cp "${REPO_ROOT}/scripts/verify-gates.sh" "${DISPOSABLE_REPO}/scripts/"
cp "${REPO_ROOT}/.golangci.yml" "${DISPOSABLE_REPO}/.golangci.yml"
cp "${REPO_ROOT}/Makefile" "${DISPOSABLE_REPO}/Makefile"
mkdir -p "${DISPOSABLE_REPO}/cmd"
cat << 'EOF' > "${DISPOSABLE_REPO}/cmd/dummy.go"
package main

func main() {}
EOF

create_valid_stubs() {
    cat << 'EOF' > "${DISPOSABLE_REPO}/bin/go"
#!/usr/bin/env bash
if [[ "$*" == "version" ]]; then echo "go version go1.27.1 darwin/arm64"; exit 0; fi
if [[ "$*" == "env GOTOOLCHAIN" ]]; then echo "local"; exit 0; fi
exit 0
EOF
    chmod +x "${DISPOSABLE_REPO}/bin/go"

    cat << 'EOF' > "${DISPOSABLE_REPO}/bin/golangci-lint"
#!/usr/bin/env bash
if [[ "$*" == *"-version"* || "$*" == *"version"* ]]; then
    echo "golangci-lint has version 2.13.2 built with go1.27.0 from 27774aaf on 2026-08-27T23:01:12Z"
    exit 0
fi
exit 0
EOF
    chmod +x "${DISPOSABLE_REPO}/bin/golangci-lint"

    cat << 'EOF' > "${DISPOSABLE_REPO}/bin/gofumpt"
#!/usr/bin/env bash
if [[ "$*" == *"-version"* ]]; then
    echo "v0.12.0 (go1.27.1)"
    exit 0
fi
exit 0
EOF
    chmod +x "${DISPOSABLE_REPO}/bin/gofumpt"

    cat << 'EOF' > "${DISPOSABLE_REPO}/bin/govulncheck"
#!/usr/bin/env bash
if [[ "$*" == *"-version"* ]]; then
    echo "Scanner: govulncheck@v1.8.0"
    echo "DB: https://vuln.go.dev"
    exit 0
fi
exit 0
EOF
    chmod +x "${DISPOSABLE_REPO}/bin/govulncheck"

    cat << 'EOF' > "${DISPOSABLE_REPO}/bin/goreleaser"
#!/usr/bin/env bash
if [[ "$*" == *"--version"* || "$*" == *"-version"* ]]; then
    echo "GitVersion:    2.18.1"
    exit 0
fi
exit 0
EOF
    chmod +x "${DISPOSABLE_REPO}/bin/goreleaser"
}

create_valid_stubs

# --- 1. check-tool-versions: Exact-Positive Control ---
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 0 && "${output}" == *"Tool verification passed"* ]]; then
    assert_pass "check-tool-versions exact-positive control passes all 5 tools"
else
    echo "FAIL: Expected check-tool-versions positive control to succeed, got exit ${status}: ${output}" >&2
    exit 1
fi

# --- 2. check-tool-versions: Go Compiler Guards ---
# 2a. Near/suffix version
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/go"
#!/usr/bin/env bash
if [[ "$*" == "version" ]]; then echo "go version go1.27.10 darwin/arm64"; exit 0; fi
if [[ "$*" == "env GOTOOLCHAIN" ]]; then echo "local"; exit 0; fi
exit 0
EOF
chmod +x "${DISPOSABLE_REPO}/bin/go"
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"Go version mismatch"* ]]; then
    assert_pass "check-tool-versions rejects near-version Go (go1.27.10)"
else
    echo "FAIL: Expected rejection of go1.27.10, got exit ${status}: ${output}" >&2
    exit 1
fi

# 2b. Malformed version
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/go"
#!/usr/bin/env bash
if [[ "$*" == "version" ]]; then echo "go version devel-unknown darwin/arm64"; exit 0; fi
if [[ "$*" == "env GOTOOLCHAIN" ]]; then echo "local"; exit 0; fi
exit 0
EOF
chmod +x "${DISPOSABLE_REPO}/bin/go"
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"Go version mismatch"* ]]; then
    assert_pass "check-tool-versions rejects malformed Go version"
else
    echo "FAIL: Expected rejection of malformed Go version, got exit ${status}: ${output}" >&2
    exit 1
fi

# 2c. Nonzero exit with valid-looking text
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/go"
#!/usr/bin/env bash
if [[ "$*" == "version" ]]; then echo "go version go1.27.1 darwin/arm64"; exit 42; fi
if [[ "$*" == "env GOTOOLCHAIN" ]]; then echo "local"; exit 0; fi
exit 0
EOF
chmod +x "${DISPOSABLE_REPO}/bin/go"
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 ]]; then
    assert_pass "check-tool-versions rejects Go version command failure (exit 42)"
else
    echo "FAIL: Expected rejection on Go exit 42, got exit ${status}: ${output}" >&2
    exit 1
fi
create_valid_stubs

# --- 3. check-tool-versions: golangci-lint Guards ---
# 3a. Near/suffix version
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/golangci-lint"
#!/usr/bin/env bash
echo "golangci-lint has version 2.13.20 built with go1.27.0 from 27774aaf on 2026-08-27T23:01:12Z"
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"golangci-lint version mismatch"* ]]; then
    assert_pass "check-tool-versions rejects near-version golangci-lint (2.13.20)"
else
    echo "FAIL: Expected rejection of golangci-lint 2.13.20, got exit ${status}: ${output}" >&2
    exit 1
fi

# 3b. Malformed version
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/golangci-lint"
#!/usr/bin/env bash
echo "golangci-lint has version custom-dev built with go1.27.0"
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"golangci-lint version mismatch"* ]]; then
    assert_pass "check-tool-versions rejects malformed golangci-lint version"
else
    echo "FAIL: Expected rejection of malformed golangci-lint version, got exit ${status}: ${output}" >&2
    exit 1
fi

# 3c. Nonzero exit with valid-looking text
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/golangci-lint"
#!/usr/bin/env bash
echo "golangci-lint has version 2.13.2 built with go1.27.0 from 27774aaf on 2026-08-27T23:01:12Z"
exit 42
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 ]]; then
    assert_pass "check-tool-versions rejects golangci-lint failure (exit 42)"
else
    echo "FAIL: Expected rejection on golangci-lint exit 42, got exit ${status}: ${output}" >&2
    exit 1
fi
create_valid_stubs

# --- 4. check-tool-versions: gofumpt Guards ---
# 4a. Near/suffix version
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/gofumpt"
#!/usr/bin/env bash
echo "v0.12.00 (go1.27.1)"
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"gofumpt version mismatch"* ]]; then
    assert_pass "check-tool-versions rejects near-version gofumpt (0.12.00)"
else
    echo "FAIL: Expected rejection of gofumpt 0.12.00, got exit ${status}: ${output}" >&2
    exit 1
fi

# 4b. Malformed version
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/gofumpt"
#!/usr/bin/env bash
echo "devel"
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"gofumpt version mismatch"* ]]; then
    assert_pass "check-tool-versions rejects malformed gofumpt version"
else
    echo "FAIL: Expected rejection of malformed gofumpt version, got exit ${status}: ${output}" >&2
    exit 1
fi

# 4c. Nonzero exit with valid-looking text
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/gofumpt"
#!/usr/bin/env bash
echo "v0.12.0 (go1.27.1)"
exit 42
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 ]]; then
    assert_pass "check-tool-versions rejects gofumpt failure (exit 42)"
else
    echo "FAIL: Expected rejection on gofumpt exit 42, got exit ${status}: ${output}" >&2
    exit 1
fi
create_valid_stubs

# --- 5. check-tool-versions: govulncheck Guards ---
# 5a. Near/suffix version
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/govulncheck"
#!/usr/bin/env bash
echo "Scanner: govulncheck@v1.8.00"
echo "DB: https://vuln.go.dev"
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"govulncheck version mismatch"* ]]; then
    assert_pass "check-tool-versions rejects near-version govulncheck (1.8.00)"
else
    echo "FAIL: Expected rejection of govulncheck 1.8.00, got exit ${status}: ${output}" >&2
    exit 1
fi

# 5b. Malformed version
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/govulncheck"
#!/usr/bin/env bash
echo "Scanner: govulncheck@vdev"
echo "DB: https://vuln.go.dev"
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"govulncheck version mismatch"* ]]; then
    assert_pass "check-tool-versions rejects malformed govulncheck version"
else
    echo "FAIL: Expected rejection of malformed govulncheck version, got exit ${status}: ${output}" >&2
    exit 1
fi

# 5c. Nonzero exit with valid-looking text
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/govulncheck"
#!/usr/bin/env bash
echo "Scanner: govulncheck@v1.8.0"
echo "DB: https://vuln.go.dev"
exit 42
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 ]]; then
    assert_pass "check-tool-versions rejects govulncheck failure (exit 42)"
else
    echo "FAIL: Expected rejection on govulncheck exit 42, got exit ${status}: ${output}" >&2
    exit 1
fi

# 5d. Missing prefix banner (F-01 regression)
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/govulncheck"
#!/usr/bin/env bash
echo "Scanner: govulncheck@(devel)"
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"govulncheck version mismatch"* && "${output}" == *"Required: 1.8.0"* && "${output}" == *"make tools"* ]]; then
    assert_pass "check-tool-versions rejects govulncheck missing-prefix banner with repair diagnostic"
else
    echo "FAIL: Expected rejection of govulncheck missing prefix with diagnostic, got exit ${status}: ${output}" >&2
    exit 1
fi

# 5e. Empty output with exit 0 (F-01 regression)
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/govulncheck"
#!/usr/bin/env bash
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"govulncheck version mismatch"* && "${output}" == *"Required: 1.8.0"* && "${output}" == *"make tools"* ]]; then
    assert_pass "check-tool-versions rejects empty govulncheck output with repair diagnostic"
else
    echo "FAIL: Expected rejection of empty govulncheck output with diagnostic, got exit ${status}: ${output}" >&2
    exit 1
fi
create_valid_stubs

# --- 6. check-tool-versions: goreleaser Guards ---
# 6a. Near/suffix version
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/goreleaser"
#!/usr/bin/env bash
echo "GitVersion:    2.18.10"
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"goreleaser version mismatch"* ]]; then
    assert_pass "check-tool-versions rejects near-version goreleaser (2.18.10)"
else
    echo "FAIL: Expected rejection of goreleaser 2.18.10, got exit ${status}: ${output}" >&2
    exit 1
fi

# 6b. Malformed version
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/goreleaser"
#!/usr/bin/env bash
echo "GitVersion:    custom-dev"
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"goreleaser version mismatch"* ]]; then
    assert_pass "check-tool-versions rejects malformed goreleaser version"
else
    echo "FAIL: Expected rejection of malformed goreleaser version, got exit ${status}: ${output}" >&2
    exit 1
fi

# 6c. Nonzero exit with valid-looking text
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/goreleaser"
#!/usr/bin/env bash
echo "GitVersion:    2.18.1"
exit 42
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 ]]; then
    assert_pass "check-tool-versions rejects goreleaser failure (exit 42)"
else
    echo "FAIL: Expected rejection on goreleaser exit 42, got exit ${status}: ${output}" >&2
    exit 1
fi

# 6d. Missing prefix banner (F-01 regression)
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/goreleaser"
#!/usr/bin/env bash
echo "goreleaser development build"
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"goreleaser version mismatch"* && "${output}" == *"Required: 2.18.1"* && "${output}" == *"make tools"* ]]; then
    assert_pass "check-tool-versions rejects goreleaser missing-prefix banner with repair diagnostic"
else
    echo "FAIL: Expected rejection of goreleaser missing prefix with diagnostic, got exit ${status}: ${output}" >&2
    exit 1
fi

# 6e. Empty output with exit 0 (F-01 regression)
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/goreleaser"
#!/usr/bin/env bash
exit 0
EOF
set +e
output="$(PATH="${DISPOSABLE_REPO}/bin:${PATH}" "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"goreleaser version mismatch"* && "${output}" == *"Required: 2.18.1"* && "${output}" == *"make tools"* ]]; then
    assert_pass "check-tool-versions rejects empty goreleaser output with repair diagnostic"
else
    echo "FAIL: Expected rejection of empty goreleaser output with diagnostic, got exit ${status}: ${output}" >&2
    exit 1
fi
create_valid_stubs

# --- 7. install-tools: Go Guard and Download Boundary ---
INSTALL_BIN="${TMP_HARNESS}/install_bin"
mkdir -p "${INSTALL_BIN}"
DOWNLOAD_RECORD="${TMP_HARNESS}/curl_invoked.txt"

cat << EOF > "${INSTALL_BIN}/curl"
#!/usr/bin/env bash
echo "CURL_INVOKED" >> "${DOWNLOAD_RECORD}"
exit 99
EOF
chmod +x "${INSTALL_BIN}/curl"

# 7a. Bad version rejected before download
cat << 'EOF' > "${INSTALL_BIN}/go"
#!/usr/bin/env bash
if [[ "$*" == "version" ]]; then echo "go version go1.27.10 darwin/arm64"; exit 0; fi
exit 0
EOF
chmod +x "${INSTALL_BIN}/go"

set +e
output="$(PATH="${INSTALL_BIN}:${PATH}" "${DISPOSABLE_REPO}/scripts/install-tools.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"Go version mismatch"* && ! -f "${DOWNLOAD_RECORD}" ]]; then
    assert_pass "install-tools rejects near-version Go before initiating any download"
else
    echo "FAIL: Expected install-tools to reject go1.27.10 without downloading, got exit ${status}: ${output}" >&2
    exit 1
fi

# 7b. Failed Go version command rejected before download
cat << 'EOF' > "${INSTALL_BIN}/go"
#!/usr/bin/env bash
if [[ "$*" == "version" ]]; then echo "fatal" >&2; exit 42; fi
exit 0
EOF
chmod +x "${INSTALL_BIN}/go"

set +e
output="$(PATH="${INSTALL_BIN}:${PATH}" "${DISPOSABLE_REPO}/scripts/install-tools.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && ! -f "${DOWNLOAD_RECORD}" ]]; then
    assert_pass "install-tools rejects Go command failure (exit 42) before downloading"
else
    echo "FAIL: Expected install-tools to reject failed go without downloading, got exit ${status}: ${output}" >&2
    exit 1
fi

# 7c. Valid Go control reaches the harmless recorded boundary
cat << 'EOF' > "${INSTALL_BIN}/go"
#!/usr/bin/env bash
if [[ "$*" == "version" ]]; then echo "go version go1.27.1 darwin/arm64"; exit 0; fi
exit 0
EOF
chmod +x "${INSTALL_BIN}/go"

set +e
output="$(PATH="${INSTALL_BIN}:${PATH}" "${DISPOSABLE_REPO}/scripts/install-tools.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 99 && -f "${DOWNLOAD_RECORD}" && "$(cat "${DOWNLOAD_RECORD}")" == "CURL_INVOKED" ]]; then
    assert_pass "install-tools valid control passes Go check and reaches recorded download boundary"
else
    echo "FAIL: Expected install-tools valid control to reach curl boundary (exit 99), got exit ${status}: ${output}" >&2
    exit 1
fi

# --- 8. verify-gates: Linter Exit Code Black-Box Harness ---
# Setup controlled mock golangci-lint inside DISPOSABLE_REPO/bin
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/golangci-lint"
#!/usr/bin/env bash
if grep -q "func Good" fixture.go 2>/dev/null || grep -q "CheckState" fixture.go 2>/dev/null; then
    exit 0
fi

if grep -q "Signal" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: p.Signal: Direct process signals are prohibited"
elif grep -q "Process.Kill" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: cmd.Process.Kill: Direct process signals are prohibited"
elif grep -q "syscall.Kill" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: syscall.Kill: Direct syscall signals and setsid are prohibited"
elif grep -q "syscall.Setsid" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: syscall.Setsid: Direct syscall signals and setsid are prohibited"
elif grep -q "CommandContext" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: exec.CommandContext: defaults to Process.Kill"
elif grep -q "WaitDelay" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: cmd.WaitDelay: Nonzero Cmd.WaitDelay can implicitly kill"
elif grep -q "os.Remove" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: Error return value of \`os.Remove\` is not checked"
elif grep -q "f.Sync" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: Error return value of \`f.Sync\` is not checked"
elif grep -q "switch s" fixture.go 2>/dev/null; then
    echo "fixture.go:5:2: missing cases in switch of type State: Unknown"
fi
exit ${STUB_LINT_EXIT:-1}
EOF
chmod +x "${DISPOSABLE_REPO}/bin/golangci-lint"

# 8a. Exact-1 positive control: passes all fixtures
set +e
output="$(STUB_LINT_EXIT=1 "${DISPOSABLE_REPO}/scripts/verify-gates.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 0 && "${output}" == *"All gate rejection and acceptance fixtures passed"* ]]; then
    assert_pass "verify-gates exact-1 positive control accepts linter exit 1"
else
    echo "FAIL: Expected verify-gates to succeed with linter exit 1, got exit ${status}: ${output}" >&2
    exit 1
fi

# 8b-8f. Rejection of non-1 exit codes (0, 2, 7, 42, 126)
for rejected_code in 0 2 7 42 126; do
    set +e
    output="$(STUB_LINT_EXIT=${rejected_code} "${DISPOSABLE_REPO}/scripts/verify-gates.sh" 2>&1)"
    status=$?
    set -e
    expected_msg="Expected linter exit 1, but got exit code ${rejected_code}!"
    if [[ "${rejected_code}" -eq 0 ]]; then
        expected_msg="Expected linter exit 1, but got exit code 0!"
    fi
    if [[ "${status}" -ne 0 && "${output}" == *"${expected_msg}"* ]]; then
        assert_pass "verify-gates rejects non-1 linter status (${rejected_code}) despite diagnostic"
    else
        echo "FAIL: Expected verify-gates to reject linter exit ${rejected_code}, got exit ${status}: ${output}" >&2
        exit 1
    fi
done

# Restore valid stubs for GOTOOLCHAIN checks
create_valid_stubs

# --- 9. GOTOOLCHAIN Override Protection ---
# 9a. check-tool-versions positive control
set +e
output="$(GOTOOLCHAIN=local "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 0 && "${output}" == *"Tool verification passed"* ]]; then
    assert_pass "check-tool-versions accepts GOTOOLCHAIN=local"
else
    echo "FAIL: Expected check-tool-versions to accept GOTOOLCHAIN=local, got exit ${status}: ${output}" >&2
    exit 1
fi

# 9b. check-tool-versions rejects conflicting GOTOOLCHAIN
set +e
output="$(GOTOOLCHAIN=go1.27.2 "${DISPOSABLE_REPO}/scripts/check-tool-versions.sh" 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"Conflicting GOTOOLCHAIN setting"* ]]; then
    assert_pass "check-tool-versions rejects conflicting GOTOOLCHAIN=go1.27.2"
else
    echo "FAIL: Expected check-tool-versions to reject GOTOOLCHAIN=go1.27.2, got exit ${status}: ${output}" >&2
    exit 1
fi

# 9c. Makefile positive control
set +e
output="$(make -C "${DISPOSABLE_REPO}" -n tool-versions 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 0 ]]; then
    assert_pass "Makefile accepts default/unset GOTOOLCHAIN"
else
    echo "FAIL: Expected Makefile default GOTOOLCHAIN to pass, got exit ${status}: ${output}" >&2
    exit 1
fi

# 9d. Makefile rejects ambient override
set +e
output="$(GOTOOLCHAIN=go1.27.2 make -C "${DISPOSABLE_REPO}" -n tool-versions 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"Conflicting GOTOOLCHAIN setting"* ]]; then
    assert_pass "Makefile rejects ambient GOTOOLCHAIN=go1.27.2 override"
else
    echo "FAIL: Expected Makefile to reject ambient GOTOOLCHAIN=go1.27.2, got exit ${status}: ${output}" >&2
    exit 1
fi

# 9e. Makefile rejects command-line override
set +e
output="$(make -C "${DISPOSABLE_REPO}" GOTOOLCHAIN=go1.27.2 -n tool-versions 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"Conflicting GOTOOLCHAIN setting"* ]]; then
    assert_pass "Makefile rejects command-line GOTOOLCHAIN=go1.27.2 override"
else
    echo "FAIL: Expected Makefile to reject command-line GOTOOLCHAIN=go1.27.2, got exit ${status}: ${output}" >&2
    exit 1
fi

# 9f. Makefile rejects conflicting auto setting
set +e
output="$(GOTOOLCHAIN=auto make -C "${DISPOSABLE_REPO}" -n tool-versions 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"Conflicting GOTOOLCHAIN setting"* ]]; then
    assert_pass "Makefile rejects ambient GOTOOLCHAIN=auto override"
else
    echo "FAIL: Expected Makefile to reject ambient GOTOOLCHAIN=auto, got exit ${status}: ${output}" >&2
    exit 1
fi

# --- 10. Formatter Error Propagation under Make 3.81 (F-01) ---
# 10a. Positive control: valid formatting
set +e
output="$(make -C "${DISPOSABLE_REPO}" -o tool-versions fmt-check 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 0 ]]; then
    assert_pass "fmt-check passes when files are properly formatted"
else
    echo "FAIL: Expected fmt-check to pass, got exit ${status}: ${output}" >&2
    exit 1
fi

# 10b. Formatter execution failure with empty stdout (F-01 regression)
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/gofumpt"
#!/usr/bin/env bash
if [[ "$*" == *"-version"* ]]; then
    echo "v0.12.0 (go1.27.1)"
    exit 0
fi
# Simulate formatter crash or parse failure without stdout
echo "parse error: unexpected token" >&2
exit 2
EOF
chmod +x "${DISPOSABLE_REPO}/bin/gofumpt"

set +e
output="$(make -C "${DISPOSABLE_REPO}" -o tool-versions fmt-check 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 ]]; then
    assert_pass "fmt-check propagates formatter failure even when stdout is empty"
else
    echo "FAIL: Expected fmt-check to fail on formatter error, got exit 0: ${output}" >&2
    exit 1
fi

# 10c. Unformatted files detected
cat << 'EOF' > "${DISPOSABLE_REPO}/bin/gofumpt"
#!/usr/bin/env bash
if [[ "$*" == *"-version"* ]]; then
    echo "v0.12.0 (go1.27.1)"
    exit 0
fi
echo "cmd/dummy.go"
exit 0
EOF
chmod +x "${DISPOSABLE_REPO}/bin/gofumpt"

set +e
output="$(make -C "${DISPOSABLE_REPO}" -o tool-versions fmt-check 2>&1)"
status=$?
set -e
if [[ "${status}" -ne 0 && "${output}" == *"not formatted with gofumpt"* ]]; then
    assert_pass "fmt-check rejects unformatted files listed on stdout"
else
    echo "FAIL: Expected fmt-check to report unformatted file, got exit ${status}: ${output}" >&2
    exit 1
fi
create_valid_stubs

# --- 11. Tool Invocation in Repository Paths Containing Spaces (F-02) ---
SPACED_REPO="${TMP_HARNESS}/lead scratch/repo with spaces"
mkdir -p "${SPACED_REPO}/scripts" "${SPACED_REPO}/bin" "${SPACED_REPO}/cmd"
cp "${REPO_ROOT}/scripts/check-tool-versions.sh" "${SPACED_REPO}/scripts/"
cp "${REPO_ROOT}/.golangci.yml" "${SPACED_REPO}/.golangci.yml"
cp "${REPO_ROOT}/Makefile" "${SPACED_REPO}/Makefile"
cp "${DISPOSABLE_REPO}/cmd/dummy.go" "${SPACED_REPO}/cmd/"
cp "${DISPOSABLE_REPO}/bin/"* "${SPACED_REPO}/bin/"

# 11a. fmt-check in spaced path
set +e
output="$(make -C "${SPACED_REPO}" -o tool-versions fmt-check 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 0 ]]; then
    assert_pass "make fmt-check succeeds in repository path with spaces"
else
    echo "FAIL: Expected fmt-check to succeed in spaced repo, got exit ${status}: ${output}" >&2
    exit 1
fi

# 11b. fmt in spaced path
set +e
output="$(make -C "${SPACED_REPO}" -o tool-versions fmt 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 0 ]]; then
    assert_pass "make fmt succeeds in repository path with spaces"
else
    echo "FAIL: Expected fmt to succeed in spaced repo, got exit ${status}: ${output}" >&2
    exit 1
fi

# 11c. lint in spaced path
set +e
output="$(make -C "${SPACED_REPO}" -o tool-versions lint 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 0 ]]; then
    assert_pass "make lint succeeds in repository path with spaces"
else
    echo "FAIL: Expected lint to succeed in spaced repo, got exit ${status}: ${output}" >&2
    exit 1
fi

# 11d. vuln in spaced path
set +e
output="$(make -C "${SPACED_REPO}" -o tool-versions vuln 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 0 ]]; then
    assert_pass "make vuln succeeds in repository path with spaces"
else
    echo "FAIL: Expected vuln to succeed in spaced repo, got exit ${status}: ${output}" >&2
    exit 1
fi

# 11e. config-check in spaced path
set +e
output="$(make -C "${SPACED_REPO}" -o tool-versions config-check 2>&1)"
status=$?
set -e
if [[ "${status}" -eq 0 ]]; then
    assert_pass "make config-check succeeds in repository path with spaces"
else
    echo "FAIL: Expected config-check to succeed in spaced repo, got exit ${status}: ${output}" >&2
    exit 1
fi

echo "All ${PASSED_CASES} black-box tooling regression checks passed successfully."
