"""Invoke real runtime boundaries with deliberately invalid test inputs."""

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[3])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]


from collections.abc import Callable
from typing import TypeVar

Result = TypeVar("Result")


def invoke_invalid(call: Callable[..., Result], /, *args: object, **kwargs: object) -> Result:
    # Negative tests intentionally violate the static contract; the real callable must reject the unchanged inputs.
    return call(*args, **kwargs)
