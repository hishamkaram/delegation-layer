"""Cleanup ownership checks for the CCR fixture gate."""

import json
from pathlib import Path
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import acceptance_ccr_fixture as gate
from acceptance_supervisor_common import DirectoryOwner
from scripts.acceptance_tests.resources import track_directory_pins


class FixtureCleanupTests(unittest.TestCase):
    @staticmethod
    def fake_run(shutdown_result=True, finalize_result=True, finalize_error=None,
                 output=None):
        calls = []

        class Run:
            def __init__(self):
                self.output = output

            def safe_shutdown(self):
                calls.append("shutdown")
                return shutdown_result

            def finalize(self, success):
                calls.append(("finalize", success))
                if finalize_error is not None:
                    raise finalize_error
                return finalize_result

        return Run(), calls

    def test_success_finalizes_once_after_proven_shutdown(self):
        run, calls = self.fake_run()

        gate.cleanup_fixture(run, True)

        self.assertEqual(calls, ["shutdown", ("finalize", True)])

    def test_failure_still_finalizes_once_and_cleanup_failure_is_fatal(self):
        run, calls = self.fake_run(shutdown_result=False)

        with self.assertRaisesRegex(RuntimeError, "fixture cleanup failed"):
            gate.cleanup_fixture(run, False)

        self.assertEqual(calls, ["shutdown", ("finalize", False)])

    def test_finalize_failure_is_fatal(self):
        run, calls = self.fake_run(finalize_result=False)

        with self.assertRaisesRegex(RuntimeError, "fixture cleanup failed"):
            gate.cleanup_fixture(run, True)

        self.assertEqual(calls, ["shutdown", ("finalize", True)])

    def test_behavioral_success_is_not_published_when_finalization_fails(self):
        for failure in (False, OSError("injected finalization failure")):
            with self.subTest(failure=repr(failure)), \
                    tempfile.TemporaryDirectory(prefix="ccr-fixture-finalize-unit-") as temporary:
                output = Path(temporary) / "evidence"
                output.mkdir()
                run, calls = self.fake_run(
                    finalize_result=failure is not False,
                    finalize_error=failure if isinstance(failure, BaseException) else None,
                    output=output,
                )
                run.provider_executable = "/ccr"
                run.direct = lambda *_args, **_kwargs: None
                run.exercise = lambda _fixture: {"status": "passed", "behavior": "complete"}
                fixture = SimpleNamespace(server=SimpleNamespace(server_port=43210))

                class FixtureContext:
                    def __enter__(self):
                        return fixture

                    def __exit__(self, *_args):
                        return None

                def fixture_acceptance(_args, directories):
                    track_directory_pins(self, directories)
                    return run

                argv = ["acceptance_ccr_fixture.py", "--output", str(output)]
                with patch.object(gate, "choose_output", return_value=output), \
                        patch.object(gate, "FixtureAcceptance", side_effect=fixture_acceptance), \
                        patch.object(gate, "Fixture", return_value=FixtureContext()), \
                        patch.dict(gate.os.environ, {}, clear=False), \
                        patch.object(sys, "argv", argv):
                    with self.assertRaisesRegex(RuntimeError, "fixture cleanup failed"):
                        gate.main()

                self.assertEqual(calls, ["shutdown", ("finalize", True)])
                self.assertFalse((output / "success.json").exists())
                failure_receipt = json.loads((output / "failure.json").read_text())
                self.assertTrue(failure_receipt["behavior_completed"])
                self.assertTrue(failure_receipt["passed_receipt_absent"])
                cleanup = json.loads((output / "failure-cleanup.json").read_text())
                self.assertTrue(cleanup["natural_daemon_shutdown"])
                self.assertFalse(cleanup["artifact_finalization_completed"])
                self.assertTrue(cleanup["passed_receipt_absent"])

    def test_constructor_failure_rolls_back_fixture_profile(self):
        with tempfile.TemporaryDirectory(prefix="ccr-fixture-constructor-unit-") as temporary:
            output = Path(temporary) / "evidence"
            output.mkdir()
            owner = DirectoryOwner()
            profile_token = owner.create("CCR fixture profile", output / "profile", output)
            profile = profile_token.directory
            owner.create("CCR fixture data", profile_token.directory / "data", profile_token.directory)
            directories = owner.transfer()

            with patch.object(gate.NativeAcceptance, "__init__",
                              side_effect=RuntimeError("injected constructor failure")):
                with self.assertRaisesRegex(RuntimeError, "injected constructor failure"):
                    gate.FixtureAcceptance(SimpleNamespace(), directories)

            self.assertFalse(profile.exists())
            self.assertTrue(output.is_dir())

    def test_finalize_removes_fixture_profile_after_native_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="ccr-fixture-finalize-profile-unit-") as temporary:
            output = Path(temporary) / "evidence"
            output.mkdir()
            owner = DirectoryOwner()
            profile_token = owner.create("CCR fixture profile", output / "profile", output)
            profile = profile_token.directory
            run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
            run.fixture_directories = (profile_token,)

            with patch.object(gate.NativeAcceptance, "finalize", return_value=True):
                self.assertTrue(run.finalize(True))

            self.assertFalse(profile.exists())
            self.assertEqual(run.fixture_directories, ())

    def test_finalize_retains_fixture_profile_until_native_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="ccr-fixture-retain-profile-unit-") as temporary:
            output = Path(temporary) / "evidence"
            output.mkdir()
            owner = DirectoryOwner()
            profile_token = owner.create("CCR fixture profile", output / "profile", output)
            track_directory_pins(self, (profile_token,))
            profile = profile_token.directory
            run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
            run.fixture_directories = (profile_token,)

            with patch.object(gate.NativeAcceptance, "finalize", return_value=False):
                self.assertFalse(run.finalize(False))

            self.assertTrue(profile.is_dir())


if __name__ == "__main__":
    unittest.main()
