"""Validate lifetime budget modes without weakening auxiliary-owner bounds."""

import unittest

from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.lifetime.policy import _lifetime_deadline


class LifetimeBudgetTests(unittest.TestCase):
    def test_finite_legacy_deadline_is_required_for_every_owner(self):
        for owner in ("task", "inspection", "workspace"):
            with self.subTest(owner=owner):
                finite = {"deadline": "2026-10-13T00:01:00Z"}
                deadline = _lifetime_deadline(finite, owner, "fixture")
                self.assertIsNotNone(deadline)
                self.assertEqual(_lifetime_deadline({**finite, "unlimited": False}, owner, "fixture"), deadline)
                with self.assertRaises(AcceptanceFailure):
                    _lifetime_deadline({"deadline": ""}, owner, "fixture")

    def test_only_task_owner_can_have_explicit_unlimited_deadline(self):
        record = {"deadline": "", "unlimited": True}
        self.assertIsNone(_lifetime_deadline(record, "task", "fixture"))
        for owner in ("inspection", "workspace"):
            with self.subTest(owner=owner):
                with self.assertRaisesRegex(AcceptanceFailure, "task owner"):
                    _lifetime_deadline(record, owner, "fixture")
        with self.assertRaisesRegex(AcceptanceFailure, "no deadline"):
            _lifetime_deadline({**record, "deadline": "2026-10-13T00:01:00Z"}, "task", "fixture")

    def test_unlimited_marker_requires_boolean_type(self):
        for marker in (None, 0, 1, "true", [], {}):
            with self.subTest(marker=marker):
                with self.assertRaisesRegex(AcceptanceFailure, "boolean"):
                    _lifetime_deadline({"deadline": "", "unlimited": marker}, "task", "fixture")
