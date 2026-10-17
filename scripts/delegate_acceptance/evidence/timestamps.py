"""Pure canonical RFC3339Nano parsing and comparisons."""

from __future__ import annotations

import re
from datetime import datetime, timezone
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require


_TIMESTAMP_PATTERN = re.compile(
    r"^(?P<date>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})"
    r"(?:\.(?P<fraction>\d{1,9}))?Z$")


def _canonical_timestamp(value: object, label: str) -> tuple[datetime, int]:
    """Parse the canonical UTC RFC3339Nano form used by Go records."""
    require(isinstance(value, str), f"{label} is not a timestamp")
    match = _TIMESTAMP_PATTERN.fullmatch(value)
    require(match is not None, f"{label} is not canonical UTC RFC3339Nano")
    fraction = match.group("fraction") or ""
    # RFC3339Nano omits insignificant trailing zeroes in a fractional part.
    require(not fraction or not fraction.endswith("0"),
            f"{label} is not canonical UTC RFC3339Nano")
    try:
        parsed = datetime.strptime(match.group("date"), "%Y-%m-%dT%H:%M:%S")
    except ValueError as error:
        raise AcceptanceFailure(f"{label} is not a valid timestamp") from error
    nanoseconds = int(fraction.ljust(9, "0")) if fraction else 0
    parsed = parsed.replace(microsecond=nanoseconds // 1000, tzinfo=timezone.utc)
    return parsed, nanoseconds % 1000


def _timestamp_at_or_after(left: tuple[datetime, int], right: tuple[datetime, int]) -> bool:
    return left[0] > right[0] or (left[0] == right[0] and left[1] >= right[1])


def _timestamp_before(left: tuple[datetime, int], right: tuple[datetime, int]) -> bool:
    return left[0] < right[0] or (left[0] == right[0] and left[1] < right[1])
