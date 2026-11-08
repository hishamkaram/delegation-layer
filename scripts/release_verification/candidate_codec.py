"""Strict canonical JSON field readers for development-candidate records."""

from __future__ import annotations
from scripts.acceptance.evidence.source import text

import json
import re

from scripts.acceptance.native_evidence.contract import EvidenceError
from scripts.acceptance.native_evidence.contract import canonical_json
from scripts.acceptance.evidence.source import HEX_SHA256
from scripts.acceptance.evidence.source import VerificationError


VERSION_RE = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")


def encode_record(value: dict[str, object], stage: str) -> bytes:
    """Return the only accepted byte encoding of a candidate record."""
    try:
        return canonical_json(value)
    except (EvidenceError, RecursionError):
        raise VerificationError("candidate_record_unencodable", stage) from None


def _unique_pairs(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def _reject_constant(value: str) -> None:
    raise ValueError("non-finite JSON constant")


def load_record(data: bytes, *, max_bytes: int, stage: str) -> dict[str, object]:
    """Decode one canonical object with the shared strict JSON reader.

    The reader rejects duplicate keys, non-finite constants, invalid UTF-8 and
    nesting beyond the interpreter's recursion limit; the record schema owns
    every accepted shape.
    """
    if len(data) > max_bytes:
        raise VerificationError("candidate_record_too_large", stage)
    try:
        value = json.loads(data.decode("utf-8"), object_pairs_hook=_unique_pairs,
                           parse_constant=_reject_constant)
    except (ValueError, UnicodeError, RecursionError):
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




def sha256_hex(value: object, stage: str) -> str:
    return text(value, HEX_SHA256, stage)




def positive_int(value: object, stage: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 1:
        raise VerificationError("candidate_field_invalid", stage)
    return value


def object_list(value: object, *, max_items: int, stage: str) -> list[object]:
    if not isinstance(value, list) or len(value) > max_items:
        raise VerificationError("candidate_field_invalid", stage)
    return value
