"""Native signals enter the existing receipt/finalizer owner exactly once."""

import json
from pathlib import Path
import select
import signal
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from scripts.acceptance.private_cli import application
from scripts.acceptance.private_cli import finalization
from scripts.acceptance.private_cli.contention.policy import MODERN_CONTENTION_SCENARIO
from scripts.acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.acceptance.private_cli.nested import refusal
from scripts.acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.acceptance.directories.operations import DirectoryOwner
class DriverSignalTests(unittest.TestCase):
    def test_signal_with_valid_receipts_does_not_form_a_circular_cause(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "receipt.json"
            receipt = AcceptanceReceipt(output, 3, 1, {})
            args = SimpleNamespace(output=output, scenario="nested-refused-leader", pueue=Path(sys.executable),
                                   pueued=Path(sys.executable), native_fixture=Path(sys.executable))
            original = SystemExit(143)
            with patch.object(refusal, "prepare_fixture", side_effect=original):
                with self.assertRaises(SystemExit) as caught:
                    refusal.run_refused_leader(args, output.parent, receipt)
            self.assertIs(caught.exception, original)
            self.assertEqual(caught.exception.code, 143)
            self.assertIsNone(caught.exception.__cause__, "the control error already points back to this signal")
            self.assertEqual(json.loads(output.read_text())["failure"]["exception_type"], "SystemExit")
            self.assertEqual(json.loads(output.with_suffix(".inner.json").read_text())["result"], "FAIL")

    def test_first_signal_during_cleanup_finishes_once_and_preserves_primary(self):
        for number in (signal.SIGTERM, signal.SIGINT):
            for scenario in ("ordinary", "refused"):
                phases = ("success-cleanup", "error-cleanup", "outer-cleanup") if scenario == "refused" else (
                    "success-cleanup", "error-cleanup")
                for phase in phases:
                    with self.subTest(signal=number, scenario=scenario, phase=phase), \
                            tempfile.TemporaryDirectory() as temporary:
                        output = Path(temporary) / "receipt.json"
                        child = subprocess.Popen([sys.executable, "-u", "-m",
                            "scripts.acceptance.private_cli.tests.signal_fixture", str(output), scenario, phase],
                            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                        try:
                            if child.stdout is None:
                                self.fail("owned driver stdout unavailable")
                            self.assertTrue(select.select([child.stdout], [], [], 15)[0], "cleanup readiness timed out")
                            self.assertEqual(child.stdout.readline().strip(), "ready")
                            child.send_signal(number)
                            stdout, stderr = child.communicate(input="release\n", timeout=30)
                            expected = (1 if phase == "error-cleanup" else
                                        -signal.SIGINT if number == signal.SIGINT else 128 + number)
                            self.assertEqual(child.returncode, expected, stdout + stderr)
                            inner = output.with_suffix(".inner.json") if scenario == "refused" and phase != "outer-cleanup" else output
                            self.assertTrue(inner.is_file(), "first cleanup signal prevented the finalizer's receipt")
                            observed = json.loads(inner.read_text())
                            self.assertEqual(observed["cleanup_count"], 1)
                            self.assertEqual(observed["cleanup_child_exit"], -signal.SIGTERM)
                            self.assertEqual(output.with_suffix(".count").read_text(), "1")
                            self.assertEqual(json.loads(output.read_text())["result"], "FAIL")
                            if phase == "error-cleanup":
                                self.assertEqual(observed["failure"]["exception_type"], "RuntimeError")
                                self.assertIn("fixture-primary-error", stderr)
                            elif scenario == "refused":
                                self.assertEqual(output.with_suffix(".disposed").read_text(), "closed")
                            else:
                                self.assertEqual(observed["failure"]["exception_type"],
                                    "KeyboardInterrupt" if number == signal.SIGINT else "SystemExit")
                        finally:
                            if child.poll() is None:
                                child.kill()
                            child.wait(timeout=15)
                            if child.stdin is not None:
                                child.stdin.close()
                            if child.stdout is not None:
                                child.stdout.close()
                            if child.stderr is not None:
                                child.stderr.close()

    def test_signal_after_receipt_decision_preserves_the_written_receipt(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "receipt.json"
            receipt = AcceptanceReceipt(output, 1, 1, {})
            write = receipt.write

            def signal_then_write(result, cleanup, error=None):
                handler = signal.getsignal(signal.SIGTERM)
                if not callable(handler):
                    self.fail("caught signal handler unavailable")
                handler(signal.SIGTERM, None)
                write(result, cleanup, error)

            args = SimpleNamespace(output=output)
            lifetime = FixtureLifetime(DirectoryOwner(), DirectoryOwner())
            with patch.object(receipt, "write", side_effect=signal_then_write), \
                    patch.object(finalization, "release_fixture_workers", return_value=(None, False, 0, True)), \
                    patch.object(finalization, "finish_root_cleanup", return_value=(True, {"status": "not-created"})):
                with self.assertRaises(SystemExit) as caught:
                    with application._shutdown_signals() as shutdown:
                        shutdown.begin_cleanup(None)
                        finalization.finalize_fixture(args, None, lifetime, receipt, True,
                                                      current_failure=shutdown.current_failure)
            self.assertEqual(caught.exception.code, 143)
            self.assertEqual(json.loads(output.read_text())["result"], "PASS")

    def test_deferred_signal_preserves_later_cleanup_error_as_cause_and_restores_handlers(self):
        before = {number: signal.getsignal(number) for number in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)}
        cleanup_error = OSError("owned cleanup fault")
        with self.assertRaises(SystemExit) as caught:
            with application._shutdown_signals() as shutdown:
                shutdown.begin_cleanup(None)
                handler = signal.getsignal(signal.SIGTERM)
                if not callable(handler):
                    self.fail("caught signal handler unavailable")
                handler(signal.SIGTERM, None)
                raise cleanup_error
        self.assertEqual(caught.exception.code, 143)
        self.assertIs(caught.exception.__cause__, cleanup_error)
        self.assertEqual({number: signal.getsignal(number) for number in before}, before)

    def test_native_signals_finalize_and_preserve_initiating_exit(self):
        for number in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
            for scenario in ("ordinary", "refused"):
                with self.subTest(signal=number, scenario=scenario), tempfile.TemporaryDirectory() as temporary:
                    output = Path(temporary) / "receipt.json"
                    child = subprocess.Popen([sys.executable, "-u", "-m",
                                              "scripts.acceptance.private_cli.tests.signal_fixture",
                                              str(output), scenario],
                                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                    try:
                        self.assertIsNotNone(child.stdout)
                        if child.stdout is None:
                            self.fail("owned driver stdout unavailable")
                        self.assertTrue(select.select([child.stdout], [], [], 15)[0], "driver readiness timed out")
                        self.assertEqual(child.stdout.readline().strip(), "ready")
                        child.send_signal(number)
                        stdout, stderr = child.communicate(timeout=30)
                        expected = -signal.SIGINT if number == signal.SIGINT else 128 + number
                        self.assertEqual(child.returncode, expected, stdout + stderr)
                        observed = output.with_suffix(".inner.json") if scenario == "refused" else output
                        receipt = json.loads(observed.read_text())
                        self.assertEqual(receipt["result"], "FAIL")
                        self.assertEqual(receipt["initiating_type"],
                                         "KeyboardInterrupt" if number == signal.SIGINT else "SystemExit")
                        self.assertEqual(receipt["cleanup_count"], 1)
                        self.assertEqual(receipt["cleanup_child_exit"], -signal.SIGTERM)
                        self.assertEqual(output.with_suffix(".count").read_text(), "1")
                        self.assertFalse(receipt["success"])
                        self.assertEqual(json.loads(output.read_text())["result"], "FAIL")
                    finally:
                        if child.poll() is None:
                            child.kill()
                        child.wait(timeout=15)
                        if child.stdout is not None:
                            child.stdout.close()
                        if child.stderr is not None:
                            child.stderr.close()

    def test_handlers_restore_after_the_original_interruption(self):
        numbers = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)
        before = {number: signal.getsignal(number) for number in numbers}
        with self.assertRaises(SystemExit) as caught:
            with application._shutdown_signals():
                handler = signal.getsignal(signal.SIGTERM)
                self.assertTrue(callable(handler))
                if callable(handler):
                    try:
                        handler(signal.SIGTERM, None)
                    except SystemExit:
                        # Suppression starts in the first handler, before unwinding reaches cleanup.
                        handler(signal.SIGHUP, None)
                        handler(signal.SIGINT, None)
                        raise
        self.assertEqual(caught.exception.code, 143)
        self.assertEqual({number: signal.getsignal(number) for number in numbers}, before)

    def test_receipt_write_failure_does_not_replace_the_initiating_signal(self):
        for scenario in (MODERN_CONTENTION_SCENARIO, "nested-refused-leader"):
            with self.subTest(scenario=scenario):
                self.assert_receipt_failure_preserves_signal(scenario)

    def assert_receipt_failure_preserves_signal(self, scenario):
        with tempfile.TemporaryDirectory() as temporary:
            occupied = Path(temporary) / "occupied"
            occupied.write_text("existing caller data")
            receipt = AcceptanceReceipt(occupied / "receipt.json", 3, 1, {})
            args = SimpleNamespace(scenario=scenario, output=receipt.output, pueue=Path(sys.executable),
                                   pueued=Path(sys.executable), native_fixture=Path(sys.executable))
            with patch.object(application, "parse_arguments", return_value=(args, 1, None)), \
                    patch.object(application, "prepare_receipt", return_value=(False, False, False, None, receipt)), \
                    patch.object(application, "prepare_fixture", side_effect=SystemExit(143)), \
                    patch("scripts.acceptance.private_cli.nested.refusal.prepare_fixture", side_effect=SystemExit(143)), \
                    patch.object(finalization, "release_fixture_workers", return_value=(None, False, 0, True)), \
                    patch.object(finalization, "finish_root_cleanup", return_value=(False, {"status": "retained"})):
                with self.assertRaises(SystemExit) as caught:
                    application.main()
            self.assertEqual(caught.exception.code, 143)
            cause = caught.exception.__cause__
            if scenario == "nested-refused-leader":
                self.assertIsInstance(cause, BaseExceptionGroup)
                if isinstance(cause, BaseExceptionGroup):
                    self.assertEqual(len(cause.exceptions), 2)
                    self.assertTrue(all(isinstance(error, OSError) for error in cause.exceptions))
            else:
                self.assertIsInstance(cause, OSError)
            self.assertEqual(occupied.read_text(), "existing caller data")


if __name__ == "__main__":
    unittest.main()
