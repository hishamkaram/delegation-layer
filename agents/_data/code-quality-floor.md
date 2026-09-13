# Code Quality Floor and Toolchain Standards

This document specifies the non-negotiable engineering floor for the repository. All code must pass these standards without suppression or weakening.

## 1. Toolchain & Environment
- **Go Version**: `go 1.27.0` declared in `go.mod`.
- **Compiler Pin**: Exactly `go1.27.1` in development and CI.
- **Local Toolchain Guard**: `GOTOOLCHAIN=local` is strictly enforced to prevent ambient, untracked compiler downloads.
- **Project Tools Pinning**:
  - `golangci-lint`: `v2.13.2`
  - `gofumpt`: `v0.12.0`
  - `govulncheck`: `v1.8.0`
  - `goreleaser`: `v2.18.1`
- **Isolation**: All tools install into repository-local, git-ignored `./bin/` via `make tools` with pinned SHA-256 archive validation. System/Homebrew environments are never mutated.

## 2. Static Analysis & Linter Policy
- Configuration in `.golangci.yml` using schema `version: "2"`, `linters.default: none`, and explicit linters:
  - `errcheck`: `check-type-assertions: true`, `check-blank: true` (blank assignments `_ = fn()` on error-returning calls are forbidden).
  - `govet`: `enable-all: true`, `disable: [fieldalignment]`.
  - `staticcheck`, `ineffassign`, `nilerr`, `errorlint`, `errname`, `forcetypeassert`, `contextcheck`.
  - `exhaustive`: `default-signifies-exhaustive: false` (all enum constants must be explicitly enumerated in switch statements).
  - `gocognit`: `min-complexity: 20`.
  - `gocyclo`: `min-complexity: 15`.
  - `nolintlint`: `require-explanation: true`, `require-specific: true`. No blanket or broad directory exclusions for real code.
  - `forbidigo`: Anchored patterns matching selector AST nodes:
    - `^os\.Process\.(Signal|Kill)$`
    - `^syscall\.(Kill|Setsid)$`
    - `^exec\.CommandContext$`
    - `^exec\.Cmd\.WaitDelay$`
- **Standalone Formatter**: Pinned `gofumpt 0.12.0` runs standalone via `make fmt` and `make fmt-check`. It is not duplicated inside golangci-lint.

## 3. Testing and Verification Discipline
- **Sequential Gate (`make check`)**:
  `tool-versions → fmt-check → config-check → vet → lint → verify-gates → test-race → build → smoke-cli → vuln`.
- **Race Detection**: All unit and integration tests run under `-race -count=1`.
- **Unit Testing**: Unit tests exercise error branches, writer failures, and boundaries using injected in-memory mocks without spawning background or external subprocesses.
- **Gate Rejection Fixtures**: `scripts/verify-gates.sh` proves linter enforcement against isolated temporary modules for each forbidden pattern and verifies that corrected counterparts pass.
- **No Test Weakening**: Tests must never be loosened, assertions must not be bypassed, and linters must never be suppressed to force green status.
