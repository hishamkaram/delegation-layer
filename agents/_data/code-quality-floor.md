# Code Quality Floor and Toolchain Standards

This document specifies the non-negotiable engineering floor for the repository. All code must pass these standards without suppression or weakening.

## 1. Toolchain & Environment
- **Go Version**: `go 1.27.0` declared in `go.mod`.
- **Compiler Pin**: Exactly `go1.27.2` in development and CI.
- **Python Toolchain**: Python 3.12 for repository checks and acceptance scripts; CI selects it explicitly.
- **Local Toolchain Guard**: `GOTOOLCHAIN=local` is strictly enforced to prevent ambient, untracked compiler downloads.
- **Project Tools Pinning**:
  - `golangci-lint`: `v2.14.0`
  - `gofumpt`: `v0.12.0`
  - `govulncheck`: `v1.8.0`
  - `goreleaser`: `v2.18.1`
- **Isolation**: All tools install into repository-local, git-ignored `./bin/` via `make tools`. Downloaded archives use pinned SHA-256 validation; gofumpt and govulncheck use version-pinned `go install` module builds. System/Homebrew environments are never mutated.

## 2. Static Analysis & Linter Policy
- Configuration in `.golangci.yml` using schema `version: "2"`, `linters.default: none`, and explicit linters:
  - `errcheck`: `check-type-assertions: true`, `check-blank: true` (blank assignments `_ = fn()` on error-returning calls are forbidden).
  - `govet`: `enable-all: true`, `disable: [fieldalignment]`.
  - `staticcheck`, `unused`, `ineffassign`, `nilerr`, `errorlint`, `errname`, `forcetypeassert`, `contextcheck`.
  - `exhaustive`: `default-signifies-exhaustive: false` (all enum constants must be explicitly enumerated in switch statements).
  - `gocognit`: `min-complexity: 20`.
  - `gocyclo`: `min-complexity: 15`.
  - `nolintlint`: `require-explanation: true`, `require-specific: true`. No blanket or broad directory exclusions for real code.
  - `forbidigo`: Anchored patterns matching selector AST nodes:
    - `^os\.Process\.(Signal|Kill)$`
    - `^syscall\.(Kill|Setsid)$`
    - `^unix\.Kill$`, with one path-and-message-scoped exclusion for the
      guardian implementation under `internal/lifetime/`
    - `^exec\.CommandContext$`
    - `^exec\.Cmd\.WaitDelay$`
- **Standalone Formatter**: Pinned `gofumpt 0.12.0` runs standalone via `make fmt` and `make fmt-check`. It is not duplicated inside golangci-lint.

## 3. Testing and Verification Discipline
- **Sequential Gate:** The root [Makefile](../../Makefile) `check` target is
  authoritative. Keep this contract aligned by referring to that recipe
  rather than copying its command sequence here.
- **Race Detection**: All unit and integration tests run under `-race -count=1`.
- **Unit Testing**: Pure unit tests exercise local error branches, writer
  failures, and boundaries with injected in-memory mocks and no background or
  external subprocesses.
- **Lifecycle Composition**: Durable lifecycle changes must satisfy the
  [canonical transition-matrix and composed-test
  contract](maintainability-contract.md#durable-transition-matrix-and-composed-tests).
  Fake-only coverage establishes isolated protocol ordering, not durable
  recovery or a real consumer verdict.
- **Gate Rejection Fixtures**: `scripts/verify-gates.sh` proves linter enforcement against isolated temporary modules for each forbidden pattern and verifies that corrected counterparts pass.
- **No Test Weakening**: Tests must never be loosened, assertions must not be bypassed, and linters must never be suppressed to force green status.
