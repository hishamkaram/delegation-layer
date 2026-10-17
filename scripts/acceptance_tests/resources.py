"""Test scopes own any directory pins left after their assertions."""

from collections.abc import Iterable
from unittest import TestCase

from scripts.delegate_acceptance.directories.operations import DirectoryOwner, OwnedDirectory


def track_directory_pins(test_case: TestCase, entries: Iterable[OwnedDirectory]) -> None:
    for entry in entries:
        if not isinstance(entry, OwnedDirectory):
            raise TypeError("test directory pins require an owned directory token")
        test_case.addCleanup(_consume_remaining_pins, entry)


def _consume_remaining_pins(entry: OwnedDirectory) -> None:
    # Test assertions may consume the token or remove its pathname first.
    # Discard only remaining pins; this never claims filesystem cleanup.
    if not entry._consumed:
        entry.consume()


class DirectoryPinOwner(DirectoryOwner):
    """Keep a test's final pin cleanup attached to canonical token transfers."""

    def __init__(self, test_case: TestCase):
        super().__init__()
        self._test_case = test_case

    def adopt_created(self, label, directory, parent) -> OwnedDirectory:
        entry = super().adopt_created(label, directory, parent)
        track_directory_pins(self._test_case, (entry,))
        return entry

    def transfer(self) -> tuple[OwnedDirectory, ...]:
        entries = super().transfer()
        track_directory_pins(self._test_case, entries)
        return entries
