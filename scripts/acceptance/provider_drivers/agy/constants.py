"""Antigravity acceptance contract constants."""


PROVIDER = "antigravity:print"

MODE = "workspace-write"

PREDICATE_VERSION = "print-record-v2"

PREDICATE_SHA256 = "54713d2c06eef693c09d8f08419f6dd2fd4f4802742e71d113df0dfbf1f9c43f"

TASK_BUDGET = "120s"

CANONICAL_TASK_BUDGET = "2m0s"

L3_NATIVE_TIMEOUT = "3s"

WATCH_SECONDS = 150

FINALIZATION_SECONDS = 30

TIMEOUT_MARKER = b"[agy] print timeout"

AUTHENTICATION_DIAGNOSTIC = "authentication-unavailable"





MAX_STATUS_BYTES = 8 * 1024 * 1024

MAX_PROVIDER_BYTES = 8 * 1024 * 1024
