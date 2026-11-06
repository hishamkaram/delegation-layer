# Code Quality Floor and Toolchain Standards

This document specifies the non-negotiable engineering floor for the repository. All code must pass these standards without suppression or weakening.

## 1. Toolchain & Environment
- **Go Version**: `go 1.27.0` declared in `go.mod`.
- **Compiler Pin**: Exactly the release in the root [`.go-version`](../../.go-version)
  file, one stable `X.Y.Z` line, in development and CI. The two repository
  tool guard scripts validate that exact format and fail on a missing or
  malformed pin before any download. Every Delegate `actions/setup-go` step
  only reads the same file; it is not a pin format validator. The CCR
  integration workflow pins the separate external CCR builder compiler
  independently; it must satisfy the pinned CCR checkout's `go.mod` minimum.
- **Python Toolchain**: Python 3.12 for repository checks and acceptance scripts; CI selects it explicitly.
- **Local Toolchain Guard**: `GOTOOLCHAIN=local` is strictly enforced to prevent ambient, untracked compiler downloads.
- **Project Tools Pinning**:
  - `golangci-lint`: `v2.14.0`
  - `gofumpt`: `v0.12.0`
  - `govulncheck`: `v1.8.0`
  - `goreleaser`: `v2.18.1`
- **Isolation**: All tools install into repository-local, git-ignored `./bin/` via `make tools`. Downloaded archives use pinned SHA-256 validation; gofumpt and govulncheck use version-pinned `go install` module builds. System/Homebrew environments are never mutated.
  `make tools-go` installs only the core Go tools needed by the platform gate.
- **Static tools**: `scripts/static_tools.py` owns installation and version checks for
  ShellCheck 0.11.0 and jactionlint 2.4.0. Ruff's exact pin and wheel hashes live in
  `tools/lint/requirements.txt`; ESLint and its dependencies use the separate
  locked `tools/lint/package.json`, never the published package's dependencies.
  `make lint-static` uses the Git source inventory for Python, JavaScript,
  and shell paths. Root `ruff.toml` and `eslint.config.mjs` own their rules.
- **Type and workflow security checks**: `make tools` also installs the pinned
  ty and zizmor wheels from `tools/diagnostics/requirements.txt`. Their historical
  installation directory is `bin/diagnostics/`; both are mandatory in
  `make lint-static`, with exact versions checked by `static-tool-versions`.
  `make verify-static-gates` proves their rejection and acceptance paths.
  These shared checks run once on Linux in the main CI gate, and findings fail
  that gate. `make diagnostics` remains a focused type/security subset;
  `make diagnostic-tools` installs just that subset.

## 2. Static Analysis & Linter Policy
- Configuration in `.golangci.yml` using schema `version: "2"`, `linters.default: none`, and explicit linters:
  - `errcheck`: `check-type-assertions: true`, `check-blank: true` (blank assignments `_ = fn()` on error-returning calls are forbidden).
  - `govet`: `enable-all: true`, `disable: [fieldalignment]`.
  - `staticcheck`, `unused`, `ineffassign`, `nilerr`, `errorlint`, `errname`, `forcetypeassert`, `contextcheck`.
  - `exhaustive`: `default-signifies-exhaustive: false` (all enum constants must be explicitly enumerated in switch statements).
  - `gocognit`: `min-complexity: 20`.
  - `gocyclo`: `min-complexity: 15`.
  - `bodyclose`, `sqlclosecheck`, `rowserrcheck`, `reassign`,
    `gocheckcompilerdirectives`, `loggercheck`, and `durationcheck`.
  - `nolintlint`: `require-explanation: true`, `require-specific: true`. No blanket or broad directory exclusions for real code.
  - `forbidigo`: Anchored patterns matching selector AST nodes:
    - `^os\.Process\.(Signal|Kill)$`
    - `^syscall\.(Kill|Setsid)$`
    - `^unix\.Kill$`, with one path-and-message-scoped exclusion for the
      guardian implementation under `internal/lifetime/`
    - `^exec\.CommandContext$`
    - `^exec\.Cmd\.WaitDelay$`
- **Standalone Formatter**: Pinned `gofumpt 0.12.0` runs standalone via `make fmt` and `make fmt-check`. It is not duplicated inside golangci-lint.
- **Python**: Ruff checks correctness without automatic fixes. `F401` is never
  automatically fixed; facade exports must be verified before an import is
  removed or explicitly re-exported. ty targets Python 3.12 and all platforms,
  with both repository and scripts import roots; explicit input paths and
  negative fixtures prove coverage. This is static analysis, not native-host proof.
- **Workflows**: jactionlint uses its correctness profile for the actionlint
  checks plus current GitHub syntax. It checks every workflow, including release,
  with no ignores, baselines or network access. zizmor checks workflow security
  separately with its default regular profile, strict collection, no external
  configuration, and no ignores.

## 3. Testing and Verification Discipline
- **Sequential Gate:** The root [Makefile](../../Makefile) `check` target is
  authoritative: shared static checks run before its `check-platform` target.
  CI runs the complete gate on Linux and the platform gate on macOS. Go vet,
  lint, builds, race tests and Python/Node runtime tests remain required on both
  hosts; Ruff, ty, ESLint, ShellCheck, jactionlint, zizmor and their rejection fixtures run
  once on Linux. Keep this contract aligned by referring to those recipes
  rather than copying their command sequence here.
- **Race Detection**: The full Go unit and integration gate requires
  `-race -count=1` via `make test-race`. The narrower `make check-docs` target
  runs one Go documentation-consistency test with `-count=1` and without
  `-race`; it does not establish runtime or race coverage. Runtime changes
  retain the full Go race requirement.
- **Unit Testing**: Pure unit tests exercise local error branches, writer
  failures, and boundaries with injected in-memory mocks and no background or
  external subprocesses.
- **Lifecycle Composition**: Durable lifecycle changes use the
  [production-store and real-consumer tests](maintainability-contract.md#lifecycle-tests).
  Fake-only coverage establishes isolated protocol ordering, not durable
  recovery or a real consumer verdict.
- **Gate Rejection Fixtures**: `scripts/verify-gates.sh` proves linter enforcement against isolated temporary modules for each forbidden pattern and verifies that corrected counterparts pass.
- **Cross-language fixtures**: `make verify-static-gates` proves Ruff, ESLint,
  ShellCheck, jactionlint and zizmor rejection and acceptance using finite
  temporary inputs. The same mandatory target proves ty resolves both import
  forms and rejects known type errors, including under hidden paths.
- **Goroutine leaks**: goleak v1.3.0 supplements `-race`. The shared test helper
  checks after suite cleanup, including failed suites, without replacing their
  failure code. Each adopted package proves the hook with a finite re-executed
  leak fixture. Helper processes exercising deliberate ownership transfer must
  retain their explicit exit behavior. A clean goroutine scan is not evidence
  that independent child processes exited; native closure tests remain required.
  Initial adoption covers execution, scheduler host, Pueue control and application
  composition. This does not establish leak-check coverage for the remaining
  goroutine owners (guardian, lifetime, supervisor, scheduler runtime, transport,
  inspection probes, verified execution and provider inspection).
- **No Test Weakening**: Tests must never be loosened, assertions must not be bypassed, and linters must never be suppressed to force green status.
