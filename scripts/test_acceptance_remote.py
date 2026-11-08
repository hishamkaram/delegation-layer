"""Protect acceptance cleanup when optional diagnostics fail."""

from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance_remote import CONTROL_EVIDENCE_CODE, CLOSURE_CODE, Exercise


class RemoteAcceptanceCleanupTests(unittest.TestCase):
    def setUp(self) -> None:
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.task = "a" * 32
        self.exercise = Exercise(
            Path("/candidate"),
            Path(directory.name),
            Path("/supervisor/pueue"),
            {},
            container="owned-fixture",
            tasks=[self.task],
        )
        self.interrupt_on_capture: int | None = None
        self.capture_calls = 0
        self.control = self.enterContext(
            patch.object(self.exercise, "inside", side_effect=self.inside)
        )
        self.container = self.enterContext(
            patch.object(self.exercise, "docker", side_effect=self.docker)
        )
        self.enterContext(patch.object(self.exercise, "delegate", return_value={}))

    def inside(self, *args: str, **_kwargs: object) -> bytes:
        if len(args) > 2 and args[2] == CONTROL_EVIDENCE_CODE:
            self.capture_calls += 1
            if self.interrupt_on_capture is None:
                raise RuntimeError("private-output-must-not-be-recorded")
            if self.capture_calls == self.interrupt_on_capture:
                raise KeyboardInterrupt()
            return b"{}"
        if len(args) > 2 and args[2] == CLOSURE_CODE:
            return b'{"closed":true}'
        if len(args) > 2 and "p=Path('/state/tasks')" in args[2]:
            return ('["' + self.task + '"]').encode()
        return b"1"

    @staticmethod
    def docker(*args: str, **_kwargs: object) -> bytes:
        return b"true" if args[0] == "inspect" else b""

    def assert_supported_cleanup(self) -> None:
        self.assertTrue(self.exercise.closure_verified)
        self.assertTrue(self.exercise.container_removed)
        self.container.assert_any_call("stop", "--time", "10", "owned-fixture")
        self.container.assert_any_call("rm", "owned-fixture")
        self.control.assert_any_call(
            "/candidate/delegate",
            "--root",
            "/state",
            "supervisor",
            "stop-owner",
            "--json",
        )

    def test_diagnostic_failure_preserves_supported_cleanup_without_private_output(
        self,
    ) -> None:
        self.exercise.cleanup()
        self.assert_supported_cleanup()
        self.assertEqual(
            self.exercise.evidence_failures,
            [
                "control_evidence_before-cleanup_failed",
                "control_evidence_closed_failed",
            ],
        )

    def test_interrupt_before_cleanup_propagates_after_supported_cleanup(self) -> None:
        self.interrupt_on_capture = 1
        with self.assertRaises(KeyboardInterrupt):
            self.exercise.cleanup()
        self.assert_supported_cleanup()
        self.assertEqual(self.exercise.evidence_failures, [])

    def test_interrupt_after_closure_propagates_after_supported_cleanup(self) -> None:
        self.interrupt_on_capture = 2
        with self.assertRaises(KeyboardInterrupt):
            self.exercise.cleanup()
        self.assert_supported_cleanup()
        self.assertEqual(self.exercise.evidence_failures, [])


if __name__ == "__main__":
    unittest.main()
