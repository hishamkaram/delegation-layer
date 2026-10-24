"""Typed block reason codec for raw native case receipts and compact case receipts."""

from __future__ import annotations

from .contract import (
    BLOCK_REASON_KEY, COMPACT_CASE_LEGACY_SCHEMA_VERSION, COMPACT_CASE_SCHEMA_VERSION,
    BlockReason, EvidenceError,
)


_CASE_STATUSES = frozenset({"PASS", "BLOCKED", "FAIL"})


def parse_block_reason(value: object) -> BlockReason:
    """Return the reason named by one serialized value; any other value is refused."""
    if type(value) is not str:
        raise EvidenceError("block_reason_invalid")
    try:
        return BlockReason(value)
    except ValueError:
        raise EvidenceError("block_reason_invalid") from None


def source_block_reason(
        source: dict[str, object] | None, status: str) -> tuple[BlockReason | None, str | None]:
    """Read the reason a raw case receipt carries; return it with any evidence error code.

    A BLOCKED receipt without the field (older harness) or with an unusable one is
    UNCLASSIFIED, never neutral. A PASS or FAIL receipt must not carry the field.
    """
    carried = source is not None and BLOCK_REASON_KEY in source
    if status != "BLOCKED":
        return None, "source_block_reason_unexpected" if carried else None
    if not carried:
        return BlockReason.UNCLASSIFIED, None
    try:
        return parse_block_reason(source[BLOCK_REASON_KEY]), None
    except EvidenceError:
        return BlockReason.UNCLASSIFIED, "source_block_reason_invalid"


def encode_block_reason(status: str, reason: BlockReason | None) -> str | None:
    """Serialize the version 2 field, enforcing: non-null exactly for BLOCKED.

    Only a BLOCKED status with a canonical reason member, or a PASS or FAIL
    status with no reason, is valid; every other pair is refused.
    """
    if not isinstance(status, str) or status not in _CASE_STATUSES:
        raise EvidenceError("block_reason_status_mismatch")
    if status == "BLOCKED":
        if isinstance(reason, BlockReason):
            return reason.value
    elif reason is None:
        return None
    raise EvidenceError("block_reason_status_mismatch")


def compact_block_reason(case: dict[str, object]) -> BlockReason | None:
    """Validate and return the reason one compact case receipt carries.

    Version 1 is closed without the field, so a BLOCKED v1 case is explicitly
    UNCLASSIFIED. Version 2 requires the field.
    """
    version, status = case.get("schema_version"), case.get("status")
    if not isinstance(status, str) or status not in _CASE_STATUSES:
        raise EvidenceError("aggregate_case_status_invalid")
    if type(version) is int and version == COMPACT_CASE_LEGACY_SCHEMA_VERSION:
        if BLOCK_REASON_KEY in case:
            raise EvidenceError("aggregate_case_block_reason_unexpected")
        return BlockReason.UNCLASSIFIED if status == "BLOCKED" else None
    if type(version) is not int or version != COMPACT_CASE_SCHEMA_VERSION:
        raise EvidenceError("aggregate_case_schema_invalid")
    if BLOCK_REASON_KEY not in case:
        raise EvidenceError("aggregate_case_block_reason_missing")
    carried = case[BLOCK_REASON_KEY]
    if status != "BLOCKED":
        if carried is not None:
            raise EvidenceError("aggregate_case_block_reason_unexpected")
        return None
    try:
        return parse_block_reason(carried)
    except EvidenceError:
        raise EvidenceError("aggregate_case_block_reason_invalid") from None
