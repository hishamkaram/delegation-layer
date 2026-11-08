"""Codex provider protocol and probe limits."""

from __future__ import annotations

import re

PROVIDER = "codex:exec"
MODE = "read-only"
APPROVAL = "never"
PREDICATE_VERSION = "runtime-reported-v4"
PREDICATE_SHA256 = "04ed00ac78083189161565364b8cd9a8e0c01c7019d1fd1480c2a5f35631ff86"
OUTPUT_WRITER_CONTRACT = "process-exit-eof-v1"
WATCH_SECONDS = 150
PLANNED_NATIVE_AI_TURNS = 2
ACCEPTANCE_STATUS = "acceptance-passed"
PRELAUNCH_STATUS = "planned"
NATIVE_PROFILE_REVISION = "native-permissions-v1"

TASK_BUDGET = "120s"
CANONICAL_TASK_BUDGET = "2m0s"
OUTPUT_NAME = "codex-last-message.txt"


MAX_CONTROL_BYTES = 1 << 20


MAX_PROVIDER_BYTES = 8 << 20


MAX_INT64 = (1 << 63) - 1


MAX_ITEMS = 1024


MAX_ITEM_ID_BYTES = 256


MAX_ITEM_TYPE_BYTES = 128


OUTPUT_ARGUMENT_INDEX = 11


RUNTIME_INSPECTION_REVISION = "runtime-capability-v1"


RUNTIME_HELP_ARGS = ["exec"]


RUNTIME_REQUIRED_FLAGS = [
    "-c", "--sandbox", "--cd",
    "--output-last-message", "--json", "--color", "--skip-git-repo-check",
]


CODEX_ENVIRONMENT_KEYS = (
    "HOME", "CODEX_HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
    "TZ", "TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME",
    "XDG_DATA_DIRS", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
    "DBUS_SESSION_BUS_ADDRESS", "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME",
    "GRADLE_USER_HOME", "NPM_CONFIG_CACHE", "GOPATH", "__CF_USER_TEXT_ENCODING",
)


UUID_PATTERN = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")


TASK_PATTERN = re.compile(r"^[0-9a-f]{32}$")


RESUME_ALLOWED_ITEM_TYPES = frozenset({"agent_message", "reasoning"})


RECOGNIZED_SHELL_WRAPPERS = frozenset({"/bin/bash", "/bin/sh", "/bin/zsh"})


REJECTED_SHELL_SYNTAX = frozenset({"\x00", "\n", "\r", "$", "`", ";", "&", "|", ">", "<"})


PROBE_MARKERS = {
    "probe.v1.read_rc": "read_rc",
    "probe.v1.nonce": "nonce",
    "probe.v1.workspace_rc": "workspace_rc",
    "probe.v1.workspace_stderr": "workspace_stderr",
    "probe.v1.sibling_rc": "sibling_rc",
    "probe.v1.sibling_stderr": "sibling_stderr",
}


DENIAL_WORDS = (
    "permission denied", "operation not permitted", "not permitted",
    "permission", "denied", "read-only", "read only", "sandbox",
)
