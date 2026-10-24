"""Strict canonical JSON field readers for development-candidate records."""

from __future__ import annotations

import re

from ..native_evidence.contract import EvidenceError, canonical_json
from ..native_evidence.public_contract import PublicProofError, parse_json
from .contract import HEX_SHA256, REVISION_RE, VerificationError


VERSION_RE = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")


IDENTIFIER_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,63}$")


# Opaque identities produced by other owners, such as invocation and resource IDs.
OPAQUE_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")


def encode_record(value: dict[str, object], stage: str) -> bytes:
    """Return the only accepted byte encoding of a candidate record."""
    try:
        return canonical_json(value)
    except (EvidenceError, RecursionError):
        raise VerificationError("candidate_record_unencodable", stage) from None


def load_record(data: bytes, *, max_bytes: int, stage: str) -> dict[str, object]:
    """Decode one canonical object with the shared strict JSON reader.

    The reader rejects duplicate keys, non-finite constants, invalid UTF-8 and
    nesting beyond the interpreter's recursion limit; the record schema owns
    every accepted shape.
    """
    if len(data) > max_bytes:
        raise VerificationError("candidate_record_too_large", stage)
    try:
        value = parse_json(data)
    except PublicProofError:
        raise VerificationError("candidate_record_invalid", stage) from None
    if not isinstance(value, dict) or encode_record(value, stage) != data:
        raise VerificationError("candidate_record_not_canonical", stage)
    return value


def require_header(value: dict[str, object], *, schema_version: int, kind: str,
                   error: str, stage: str) -> None:
    """Require the exact integer schema version; JSON true or 1.0 is not 1."""
    version = value.get("schema_version")
    if (isinstance(version, bool) or not isinstance(version, int) or version != schema_version
            or value.get("kind") != kind):
        raise VerificationError(error, stage)


def exact_fields(value: object, fields: frozenset[str], stage: str) -> dict[str, object]:
    if not isinstance(value, dict) or set(value) != fields:
        raise VerificationError("candidate_record_fields_invalid", stage)
    return value


def text(value: object, pattern: re.Pattern[str], stage: str) -> str:
    if not isinstance(value, str) or pattern.fullmatch(value) is None:
        raise VerificationError("candidate_field_invalid", stage)
    return value


def sha256_hex(value: object, stage: str) -> str:
    return text(value, HEX_SHA256, stage)


def revision(value: object, stage: str) -> str:
    return text(value, REVISION_RE, stage)


def positive_int(value: object, stage: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 1:
        raise VerificationError("candidate_field_invalid", stage)
    return value


def non_negative_int(value: object, stage: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise VerificationError("candidate_field_invalid", stage)
    return value


def object_list(value: object, *, max_items: int, stage: str) -> list[object]:
    if not isinstance(value, list) or len(value) > max_items:
        raise VerificationError("candidate_field_invalid", stage)
    return value


def identifier_tuple(value: object, *, max_items: int, stage: str) -> tuple[str, ...]:
    """Return a sorted, duplicate-free tuple of bounded identifiers."""
    items = tuple(text(item, IDENTIFIER_RE, stage)
                  for item in object_list(value, max_items=max_items, stage=stage))
    if list(items) != sorted(set(items)):
        raise VerificationError("candidate_field_invalid", stage)
    return items
