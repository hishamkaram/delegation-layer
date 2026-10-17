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
from scripts.delegate_acceptance.tasks.specification import _canonical_task_bytes


class CanonicalEncodingTests(unittest.TestCase):
    def test_go_json_preserves_utf8_and_escapes_html_and_line_separators(self):
        value = '雪😀<&>\u2028\u2029"\\/\n'
        expected = ('"雪😀'.encode() +
                    b'\\u003c\\u0026\\u003e\\u2028\\u2029\\"\\\\/\\n"')
        self.assertEqual(canonical_go_json(value), expected + b"\n")
        self.assertEqual(canonical_go_json(value, newline=False), expected)

    def test_task_encoding_keeps_field_order_and_go_escaping(self):
        task = {
            "brief_length": 1, "brief_sha256": "c" * 64, "budget_nanos": 1,
            "requested_config": {"permission": "read-only", "model": "雪<&>\u2028\u2029"},
            "canonical_cwd": "/workspace", "mode": "read-only", "provider": "pi:json",
            "task_id": "b" * 32, "root_id": "a" * 32, "schema_version": 1,
        }
        expected = (
            b'{"schema_version":1,"root_id":"' + b"a" * 32 +
            b'","task_id":"' + b"b" * 32 +
            b'","provider":"pi:json","mode":"read-only","canonical_cwd":"/workspace",' +
            b'"requested_config":{"model":"' + '雪'.encode() +
            b'\\u003c\\u0026\\u003e\\u2028\\u2029","permission":"read-only"},' +
            b'"budget_nanos":1,"brief_sha256":"' + b"c" * 64 + b'","brief_length":1}\n')
        self.assertEqual(_canonical_task_bytes(task), expected)
        for value, cause in ((float("nan"), ValueError), (object(), TypeError),
                             ("\ud800", UnicodeEncodeError)):
            with self.subTest(value_type=type(value).__name__):
                task["requested_config"]["model"] = value
                with self.assertRaisesRegex(
                        AcceptanceFailure, "inspection task cannot be canonically encoded") as raised:
                    _canonical_task_bytes(task)
                self.assertIsInstance(raised.exception.__cause__, cause)


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
            evidence="ordinary task", launches=[], recovered=False, registration_created=self.created,
            registration_digest=self.registration,
            root=self.root, task=self.task, termination=record, wait=wait)

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
                    allow_failure=False, authority={"mode": "guardian"}, completion=completion, deadline=self.deadline,
                    historical_aborted=False, managed_lifetime=lifetime, managed_worker_succeeded=False,
                    observation=None, ordinary_matches=True, result=result, standalone=False, task=self.task)
            lifetime["worker_wait"] = {"result": "success"}
            _validate_inspection_verdict(
                allow_failure=False, authority={"mode": "guardian"}, completion=completion, deadline=self.deadline,
                historical_aborted=False, managed_lifetime=lifetime, managed_worker_succeeded=True, observation=None,
                ordinary_matches=True, result=result, standalone=False, task=self.task)


if __name__ == "__main__":
    unittest.main()
