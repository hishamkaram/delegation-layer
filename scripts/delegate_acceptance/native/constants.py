"""Immutable native acceptance budgets, profiles, and validation limits."""

PROFILES = {
    "ccr:print": {
        "executable": "ccr", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
    },
    "antigravity:print": {
        "executable": "agy", "modes": ("read-only", "workspace-write"),
        "default_mode": "workspace-write",
    },
    "claude:print": {
        "executable": "claude", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
        "authentication_status": ("auth", "status"),
    },
    "codex:exec": {
        "executable": "codex", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
    },
    "codex:review": {
        "executable": "codex", "modes": ("read-only",),
        "default_mode": "read-only",
    },
    "opencode:run": {
        "executable": "opencode", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
    },
    "pi:json": {
        "executable": "pi", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
    },
}
# A live provider may need to establish a remote session before it can perform
# the bounded fixture. Keep the gate finite, while giving that first turn the
# same ceiling already used by the model-discovery acceptance path.
TASK_BUDGET = "5m"
CANONICAL_TASK_BUDGET = "5m0s"
TASK_BUDGET_NANOS = 300_000_000_000
WATCH_SECONDS = 330
TIMEOUT_RESUME_SOURCE_BUDGET = "3m"
CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET = "3m0s"
TIMEOUT_RESUME_SOURCE_BUDGET_NANOS = 180_000_000_000
TIMEOUT_SOURCE_RUNNER_RESULTS = frozenset({"Success", "Failed"})
# Codex's Linux workspace-write sandbox temporarily materializes these missing
# protected metadata paths. Own them in the isolated timeout fixture so a
# forced provider stop cannot turn interrupted sandbox cleanup into test data.
CODEX_TIMEOUT_BASELINE_DIRECTORIES = (".agents", ".aws", ".codex")
TIMEOUT_LEASE_SETUP_SECONDS = WATCH_SECONDS
TIMEOUT_LEASE_STATUS_INTERVAL_SECONDS = 5
TIMEOUT_LEASE_OBSERVATION_SECONDS = 30
TIMEOUT_LEASE_MAX_BYTES = 512
TIMEOUT_LEASE_MODE = 0o600
# model-discovery-v1 allows provider CLIs to populate a remote model/cache
# catalog on their first run; this outer wait adds supervisor bootstrap time.
MODEL_DISCOVERY_TASK_BUDGET = "5m0s"
MODEL_DISCOVERY_TASK_BUDGET_NANOS = 300_000_000_000
MODEL_DISCOVERY_TIMEOUT_SECONDS = 330
MAX_RAW_BYTES = 8 * 1024 * 1024
MAX_CONTROL_BYTES = 1 << 20
AUTHENTICATION_STATUS_CODES = frozenset({401, 403})
MODEL_DISCOVERY_EXIT_CODES = {
    "available": 0,
    "partial": 0,
    "blocked": 2,
    "unavailable": 2,
    "failed": 1,
}
MODEL_DISCOVERY_REQUIRED_KEYS = {
    "schema_version", "command", "provider", "observed_at", "status",
    "reason_code", "complete", "source", "models", "efforts",
}
MODEL_DISCOVERY_OPTIONAL_KEYS = frozenset()
MODEL_INFO_REQUIRED_KEYS = {"id", "efforts"}
MODEL_INFO_OPTIONAL_KEYS = {"name", "default_effort"}
FAILURE_DIAGNOSTICS_BYTES = 256 * 1024
FAILURE_DIAGNOSTICS_MAX_ENTRIES = 4096
FAILURE_DIAGNOSTICS_MAX_TASKS = 512
FAILURE_DIAGNOSTICS_MAX_OMISSIONS = 64
FAILURE_DIAGNOSTICS_TEXT_BYTES = 256
# Native configuration/session locations, never inline credentials or config.
# Keep the shared fixture environment isolated; only this live gate opts in.
NATIVE_DISCOVERY_ENVIRONMENT = frozenset({
    "CODEX_HOME", "CLAUDE_CONFIG_DIR", "ANTHROPIC_CONFIG_DIR",
    "PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR",
    "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_TUI_CONFIG",
    "OPENCODE_PERMISSION", "OPENCODE_AUTO_SHARE",
    "GEMINI_HOME", "GEMINI_CLI_HOME", "ANTIGRAVITY_HOME",
    "ANTIGRAVITY_CONFIG_HOME", "AGY_HOME", "AGY_CONFIG_HOME",
    "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS",
    "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
    "DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK", "TMP", "TEMP",
    "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME",
    "GRADLE_USER_HOME", "NPM_CONFIG_CACHE", "GOPATH",
})
