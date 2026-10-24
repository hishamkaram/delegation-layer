"""One validity rule for a candidate history at a head, with and without the observed store boundary."""

from __future__ import annotations

from dataclasses import FrozenInstanceError, fields, replace
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    MAX_CANDIDATE_COMMITS, STAGE, CandidateHistoryAtHead, RawCandidateSnapshot, StoreBoundary,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_store_replay import BOUNDARY, raw


X = "a" * 64
GROUPS = [{"manifest.json": b"m", "locator.json": b"l"}, {"attempt-0001.intent.json": b"i"},
          {"attempt-0001.claim.Linux-amd64.json": b"c"}]
RAW_FIELDS = ["location", "candidate_id", "head", "head_tree", "boundary", "transactions"]
HISTORY_FIELDS = ["location", "candidate_id", "head", "head_tree", "transactions"]


def at_head(snapshot: RawCandidateSnapshot) -> CandidateHistoryAtHead:
    """The same verified history without its boundary observation."""
    return CandidateHistoryAtHead(location=snapshot.location, candidate_id=snapshot.candidate_id,
                                  head=snapshot.head, head_tree=snapshot.head_tree,
                                  transactions=snapshot.transactions)


class HistoryValueTests(unittest.TestCase):
    def setUp(self):
        self.snapshot = raw(X, GROUPS)
        self.history = at_head(self.snapshot)

    def build(self, kind: type, **changes: object) -> object:
        values = {name: changes.get(name, getattr(self.snapshot, name)) for name in HISTORY_FIELDS}
        if kind is RawCandidateSnapshot:
            values["boundary"] = changes.get("boundary", BOUNDARY)
        return kind(**values)

    def assertRefused(self, code: str, kind: type, **changes: object) -> None:
        with self.assertRaises(VerificationError) as caught:
            self.build(kind, **changes)
        self.assertEqual((caught.exception.code, caught.exception.stage), (code, STAGE))

    def test_the_raw_snapshot_constructor_is_unchanged(self):
        self.assertEqual([field.name for field in fields(RawCandidateSnapshot)], RAW_FIELDS)
        positional = RawCandidateSnapshot(*(getattr(self.snapshot, name) for name in RAW_FIELDS))
        self.assertEqual(positional, self.snapshot)
        self.assertEqual(positional.boundary, BOUNDARY)

    def test_the_history_value_is_distinct_and_has_no_boundary(self):
        self.assertEqual([field.name for field in fields(CandidateHistoryAtHead)], HISTORY_FIELDS)
        self.assertFalse(hasattr(self.history, "boundary"))
        self.assertFalse(issubclass(CandidateHistoryAtHead, RawCandidateSnapshot))
        self.assertFalse(issubclass(RawCandidateSnapshot, CandidateHistoryAtHead))
        self.assertNotEqual(self.history, self.snapshot)
        with self.assertRaises(TypeError):
            CandidateHistoryAtHead(*(getattr(self.snapshot, name) for name in RAW_FIELDS))
        for value in (self.history, self.snapshot):
            with self.subTest(value=type(value).__name__), self.assertRaises(FrozenInstanceError):
                value.head = "9" * 40

    def test_both_values_derive_the_same_directory(self):
        added = {entry for item in self.snapshot.transactions for entry in item.transition.added}
        self.assertEqual(self.history.entries, added)
        self.assertEqual(self.snapshot.entries, added)
        self.assertEqual(at_head(raw(X, [])).entries, frozenset())

    def test_both_values_share_one_validity_table(self):
        first, second, third = self.snapshot.transactions
        cases = {
            "location": ("store_snapshot_invalid", dict(location="owner/store")),
            "head": ("store_snapshot_invalid", dict(head="H" * 40)),
            "head_tree": ("store_snapshot_invalid", dict(head_tree="3" * 39)),
            "transaction list": ("store_snapshot_invalid", dict(transactions=list(self.snapshot.transactions))),
            "foreign item": ("store_snapshot_invalid", dict(transactions=(first, "transaction"))),
            "duplicate commit": ("store_snapshot_invalid", dict(transactions=(first, first))),
            "reordered": ("store_snapshot_invalid", dict(transactions=(first, third, second))),
            "missing first": ("store_snapshot_invalid", dict(transactions=(second, third))),
            "wrong parent tree": ("store_snapshot_invalid",
                                  dict(transactions=(first, replace(second, parent_tree="4" * 40), third))),
            "wrong tree": ("store_snapshot_invalid", dict(transactions=(replace(first, tree="4" * 40),))),
            "beyond the bound": ("store_snapshot_invalid", dict(transactions=tuple(
                replace(first, commit=f"{index:040x}") for index in range(1, MAX_CANDIDATE_COMMITS + 2)))),
            "candidate id": ("store_candidate_id_invalid", dict(candidate_id="x")),
        }
        for kind in (RawCandidateSnapshot, CandidateHistoryAtHead):
            self.assertEqual(self.build(kind).transactions, self.snapshot.transactions)
            for label, (code, changes) in cases.items():
                with self.subTest(kind=kind.__name__, case=label):
                    self.assertRefused(code, kind, **changes)

    def test_only_the_raw_snapshot_requires_its_observed_boundary(self):
        for boundary in (None, (42, (7,)), "boundary"):
            with self.subTest(boundary=boundary):
                self.assertRefused("store_snapshot_invalid", RawCandidateSnapshot, boundary=boundary)
                self.assertRefused("store_snapshot_invalid", RawCandidateSnapshot, boundary=boundary,
                                   candidate_id="x")
        self.assertEqual(self.build(RawCandidateSnapshot, boundary=StoreBoundary(1, (3, 7))).boundary,
                         StoreBoundary(1, (3, 7)))
        with self.assertRaises(TypeError):
            CandidateHistoryAtHead(boundary=BOUNDARY, **{name: getattr(self.snapshot, name)
                                                         for name in HISTORY_FIELDS})


if __name__ == "__main__":
    unittest.main()
