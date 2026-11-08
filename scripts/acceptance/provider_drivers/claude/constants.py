"""Claude runtime and stream protocol bounds."""

from __future__ import annotations

import re

PROVIDER = "claude:print"
MODE = "read-only"
PREDICATE_VERSION = "native-permissions-v1"
PREDICATE_SHA256 = "8049da50895cfdbdacf9f81678c1a2d4c1f15137e2b89b6a9255d6d1e3873642"
NATIVE_PROFILE_REVISION = "native-permissions-v1"
TASK_BUDGET = "120s"
CANONICAL_TASK_BUDGET = "2m0s"
WATCH_SECONDS = 150
PLANNED_NATIVE_AI_TURNS = 2
ACCEPTANCE_STATUS = "acceptance-passed"
PRELAUNCH_STATUS = "planned"

APPROVAL = "plan"

MAX_CONTROL_BYTES = 1 << 20
MAX_PROVIDER_BYTES = 8 << 20
CLAUDE_XDG_DIRECTORY = "claude"
RUNTIME_INSPECTION_REVISION = "runtime-capability-v1"
RUNTIME_HELP_ARGS: list[str] | None = None
RUNTIME_REQUIRED_FLAGS = (
    "--print", "--input-format", "--output-format", "--verbose", "--permission-mode",
    "--permission-prompts", "--resume", "--session-id",
)
NATIVE_ENVIRONMENT_KEYS = (
    "HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
    "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING",
)
NATIVE_DISCOVERY_ENVIRONMENT_KEYS = (
    "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS",
    "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
    "CLAUDE_CONFIG_DIR", "ANTHROPIC_CONFIG_DIR",
)
PATH_VALIDATION_ENVIRONMENT_KEYS = frozenset({
    "HOME", "TMPDIR", "TMP", "TEMP", *NATIVE_DISCOVERY_ENVIRONMENT_KEYS,
})
UUID_PATTERN = re.compile(
    r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
TASK_PATTERN = re.compile(r"^[0-9a-f]{32}$")
ROOT_PATTERN = re.compile(r"^[0-9a-f]{32}$")
KNOWN_CONTENT_TYPES = frozenset({"text", "thinking", "redacted_thinking", "tool_use", "tool_result"})
ERROR_SUBTYPES = frozenset({
    "error_max_turns", "error_max_budget_usd", "error_during_execution",
    "error_max_structured_output_retries",
})
