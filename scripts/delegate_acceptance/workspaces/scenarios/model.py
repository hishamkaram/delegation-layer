"""Stable workspace scenario constants and simple failure type."""

from __future__ import annotations

import re
from typing import NotRequired, TypedDict

DRIVER_DESCRIPTION = (
    "Native managed-workspace acceptance through the shipped Delegate CLI.\n\n"
    "This driver is deliberately fixture-only.  It uses the rebuilt Delegate and\n"
    "runner supplied by the caller, plus the real pueue pair, while the provider is\n"
    "the existing finite private fixture.  Process ownership, command capture,\n"
    "lease observation, and directory rollback remain in the shared acceptance\n"
    "helpers."
)
TASK_ID_RE = re.compile(r"^[0-9a-f]{32}$")
OID_RE = re.compile(r"^[0-9a-f]{40}([0-9a-f]{24})?$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
MAX_RECEIPT_BYTES = 4 * 1024 * 1024
MAX_RESPONSE_BYTES = 8 * 1024 * 1024
MAX_COMPACT_DEPTH = 64
MAX_COMPACT_ITEMS = 16384
MAX_COMPACT_STRING = 65536
DEFAULT_WORKSPACE_BUDGET = "5m"
DEFAULT_DISPATCH_WORKERS = 16
ADMISSION_COMMAND_TIMEOUT_SECONDS = 6 * 60
DEFAULT_CAPACITY_TASKS = 200
SUCCESS_TASK_BUDGET = "30s"
TIMEOUT_TASK_BUDGET = "30s"
TIMEOUT_CONTINUE_TASK_BUDGET = "30s"
CAPACITY_TASK_BUDGET = "30m"
WORKSPACE_OPERATION_BUDGET = "2m"
WAIT_SECONDS = ADMISSION_COMMAND_TIMEOUT_SECONDS
TIMEOUT_WAIT_SECONDS = 45
FIXTURE_SESSION = "00000000-0000-4000-8000-000000000001"
FIXTURE_FILE = "managed-workspace-fixture.txt"


class AcceptanceBlocked(RuntimeError):
    """A native prerequisite was unavailable or inconclusive."""


WorkspaceTask = TypedDict("WorkspaceTask", {
    "task_id": str,
    "workspace_id": str,
    "root_id": str,
    "workspace": dict[str, object],
    "dispatch": NotRequired[dict[str, object]],
    "parent_task_id": NotRequired[str],
    "continue": NotRequired[dict[str, object]],
})


class CaptureArtifact(TypedDict):
    name: str
    length: NotRequired[int]
    sha256: NotRequired[str]
    omitted: NotRequired[str]


class CaptureEvidence(TypedDict):
    workspace_id: str
    operation_id: str
    base_oid: str
    observed_head: str
    tree_oid: str
    root_id: str
    snapshot_ref: str
    subject: dict[str, object]
    artifacts: list[CaptureArtifact]
    turn: NotRequired[dict[str, object]]


class FileDigest(TypedDict):
    bytes: int
    sha256: str


class IndexObservation(FileDigest):
    device: int
    inode: int


class CheckoutSnapshot(TypedDict):
    head: str
    head_ref: str | None
    index: IndexObservation
    status: FileDigest
    files: dict[str, str | None]
