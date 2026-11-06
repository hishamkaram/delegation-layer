"""Invoke real runtime boundaries with deliberately invalid test inputs."""

from collections.abc import Callable
from typing import TypeVar

Result = TypeVar("Result")


def invoke_invalid(call: Callable[..., Result], /, *args: object, **kwargs: object) -> Result:
    # Negative tests intentionally violate the static contract; the real callable must reject the unchanged inputs.
    return call(*args, **kwargs)
