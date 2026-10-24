"""The one exception-preservation policy for a fixture cleanup that runs when a scope ends."""

from __future__ import annotations

from collections.abc import Callable, Iterator
from contextlib import contextmanager


@contextmanager
def closing_preserving_primary(close: Callable[[], object]) -> Iterator[None]:
    """Run ``close`` exactly once when the scope exits.

    A cleanup fault alone propagates unchanged. After a scope failure, that failure stays the
    raised error and the cleanup fault becomes its cause, grouped with any explicit cause it had.
    """
    try:
        yield
    except BaseException as primary:
        try:
            close()
        except BaseException as fault:
            primary.__cause__ = fault if primary.__cause__ is None else BaseExceptionGroup(
                "fixture cleanup failed after an explicitly caused error", [primary.__cause__, fault])
        raise
    close()
