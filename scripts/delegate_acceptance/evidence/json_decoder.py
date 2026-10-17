"""JSON key alias checks backed by the repository Unicode SimpleFold table."""

from __future__ import annotations

import json
from scripts.delegate_acceptance.evidence.validation import require
from pathlib import Path


_SIMPLE_FOLD_TABLE = Path(__file__).resolve().parents[2] / "_data" / "unicode-simple-fold.json"


def _load_simple_fold_table() -> dict[int, int]:
    """Load the Go Unicode SimpleFold minimum-rune translation table."""
    try:
        raw = json.loads(_SIMPLE_FOLD_TABLE.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, TypeError, ValueError) as error:
        raise RuntimeError(f"cannot load Unicode SimpleFold table: {error}") from error
    if not isinstance(raw, dict):
        raise RuntimeError("Unicode SimpleFold table is not an object")
    table: dict[int, int] = {}
    for encoded, minimum in raw.items():
        if (not isinstance(encoded, str) or not encoded.isascii() or not encoded.isdecimal()
                or isinstance(minimum, bool) or not isinstance(minimum, int)):
            raise RuntimeError("Unicode SimpleFold table has an invalid entry")
        codepoint = int(encoded)
        if not 0 <= codepoint <= 0x10FFFF or not 0 <= minimum <= 0x10FFFF:
            raise RuntimeError("Unicode SimpleFold table has an out-of-range entry")
        table[codepoint] = minimum
    return table


_SIMPLE_FOLD_TRANSLATION = _load_simple_fold_table()


def unique_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    """Decode objects with no exact or Go SimpleFold-equivalent duplicate keys.

    Go's registered provider decoders reject aliases such as ``session_id`` and
    ``Session_ID`` at every nesting level.  This uses the minimum rune from
    each Go ``unicode.SimpleFold`` cycle, rather than Python's full Unicode
    casefold, so multi-rune folds such as ``ß`` → ``ss`` remain distinct.
    Keeping this hook shared makes the Python acceptance parsers enforce the
    same fail-closed rule before provider-specific field selection.
    """
    result: dict[str, object] = {}
    folded: dict[str, str] = {}
    for key, value in pairs:
        normalized = key.translate(_SIMPLE_FOLD_TRANSLATION)
        require(normalized not in folded,
                "duplicate JSON object key: " + key)
        folded[normalized] = key
        result[key] = value
    return result
