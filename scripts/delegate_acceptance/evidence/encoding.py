"""Pure exact Go-compatible JSON encoding."""

from __future__ import annotations

import json
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure


def canonical_go_json(value: object, newline: bool = True) -> bytes:
    """Encode bounded acceptance records with Go encoding/json semantics."""
    try:
        data = json.dumps(value, ensure_ascii=False, separators=(",", ":"),
                          allow_nan=False).encode("utf-8")
    except (TypeError, UnicodeEncodeError, ValueError) as error:
        raise AcceptanceFailure("record cannot be canonically encoded") from error
    data = (data.replace(b"&", b"\\u0026").replace(b"<", b"\\u003c")
            .replace(b">", b"\\u003e").replace("\u2028".encode(), b"\\u2028")
            .replace("\u2029".encode(), b"\\u2029"))
    return data + (b"\n" if newline else b"")
