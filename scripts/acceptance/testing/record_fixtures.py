"""Checked traversal of valid test records without copying or normalizing them."""

from __future__ import annotations

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[3])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]



def _value_at(value: object, path: tuple[str | int, ...]) -> object:
    for key in path:
        if isinstance(key, str):
            if not isinstance(value, dict) or any(not isinstance(name, str) for name in value):
                raise AssertionError(f"fixture path {key!r} requires a string-keyed object")
            if key not in value:
                raise AssertionError(f"fixture object has no field {key!r}")
            value = value[key]
        elif type(key) is int:
            if not isinstance(value, list) or not -len(value) <= key < len(value):
                raise AssertionError(f"fixture path index {key} requires an existing array element")
            value = value[key]
        else:
            raise AssertionError("fixture path requires string keys or integer indices")
    return value


def object_at(value: object, *path: str | int) -> dict[str, object]:
    record = _value_at(value, path)
    if not isinstance(record, dict) or any(not isinstance(key, str) for key in record):
        raise AssertionError("fixture value is not a string-keyed object")
    return record


def array_at(value: object, *path: str | int) -> list[object]:
    items = _value_at(value, path)
    if not isinstance(items, list):
        raise AssertionError("fixture value is not an array")
    return items
