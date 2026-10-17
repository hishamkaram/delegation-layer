"""Pure closure and inspection decisions consume facts without reading storage."""

from copy import deepcopy
from datetime import datetime, timezone
from pathlib import Path
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.inspections.result_policy import _validate_inspection_verdict
from scripts.delegate_acceptance.lifetime.policy import _validate_lifetime_termination


class FoundationPolicyTests(unittest.TestCase):
    def setUp(self):
        self.created = (datetime(2026, 10, 16, tzinfo=timezone.utc), 0)
        self.deadline = (datetime(2026, 10, 16, 0, 1, tzinfo=timezone.utc), 0)
        self.root = "a" * 32
        self.task = "b" * 32
        self.registration = "c" * 64

    def termination(self, cause="completed"):
        return {
            "schema_version": 1, "root_id": self.root, "task_id": self.task,
            "registration_sha256": self.registration, "launch_count": 0,
            "launch_set_sha256": sha(canonical_go_json([])), "cause": cause,
            "scope_empty": True, "observed_at": "2026-10-16T00:00:30Z",
        }

    def validate_termination(self, record, wait):
        return _validate_lifetime_termination(
            "ordinary task", [], False, self.created, self.registration,
            self.root, self.task, record, wait)

    def test_previous_boot_closure_does_not_invent_a_native_wait(self):
        with patch.object(Path, "open", side_effect=AssertionError("unexpected I/O")):
            self.validate_termination(self.termination("previous-boot"), None)
            with self.assertRaisesRegex(AcceptanceFailure, "worker Wait is missing"):
                self.validate_termination(self.termination(), None)

    def test_termination_rejects_changed_launches_and_nonempty_scope(self):
        record = self.termination()
        changes = (("launch_count", 1), ("launch_set_sha256", "d" * 64),
                   ("scope_empty", False))
        for key, value in changes:
            with self.subTest(key=key):
                changed = deepcopy(record)
                changed[key] = value
                with self.assertRaisesRegex(AcceptanceFailure, "termination binding mismatch"):
                    self.validate_termination(changed, {"result": "success"})

    def test_recovered_absence_cannot_be_projected_as_inspection_success(self):
        result = {"reason": "eligible"}
        completion = {"native_exit": "successful-exit"}
        lifetime = {"worker_wait": {"result": "absent"}, "termination": self.termination()}
        with patch.object(Path, "open", side_effect=AssertionError("unexpected I/O")):
            with self.assertRaisesRegex(AcceptanceFailure, "successful inspection proof is incomplete"):
                _validate_inspection_verdict(
                    False, {"mode": "guardian"}, completion, self.deadline,
                    False, lifetime, False, None, True, result, False, self.task)
            lifetime["worker_wait"] = {"result": "success"}
            _validate_inspection_verdict(
                False, {"mode": "guardian"}, completion, self.deadline,
                False, lifetime, True, None, True, result, False, self.task)


if __name__ == "__main__":
    unittest.main()
