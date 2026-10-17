"""The two-current-binary proof is selected explicitly before legacy setup."""

from contextlib import redirect_stderr
import io
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.private_cli import application, arguments
from scripts.delegate_acceptance.private_cli.contention_policy import MODERN_CONTENTION_SCENARIO
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime


def invocation(output):
    return [
        "acceptance-private-cli", "--delegate", "/baseline/delegate",
        "--runner", "/baseline/delegate-run", "--upgraded-delegate", "/candidate/delegate",
        "--upgraded-runner", "/candidate/delegate-run", "--pueue", "/fixture/pueue",
        "--pueued", "/fixture/pueued", "--parallel-tasks", "2",
        "--scenario", MODERN_CONTENTION_SCENARIO, "--output", str(output),
    ]


class ModernEntryTests(unittest.TestCase):
    def test_current_binaries_do_not_select_the_legacy_mode(self):
        with tempfile.TemporaryDirectory() as temporary:
            with patch.object(sys, "argv", invocation(Path(temporary) / "receipt.json")):
                args, workers, parser = arguments.parse_arguments()
            legacy, _, _, _, receipt = arguments.prepare_receipt(args, workers, parser)
            self.assertFalse(legacy)
            self.assertEqual(args.scenario, MODERN_CONTENTION_SCENARIO)
            self.assertEqual(workers, 2)
            self.assertFalse(receipt.output.exists())

    def test_partial_candidate_inputs_fail_before_fixture_setup(self):
        for missing in ("--upgraded-delegate", "--upgraded-runner"):
            with self.subTest(missing=missing), tempfile.TemporaryDirectory() as temporary:
                argv = invocation(Path(temporary) / "receipt.json")
                offset = argv.index(missing)
                del argv[offset:offset + 2]
                with patch.object(sys, "argv", argv), redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as error:
                        arguments.parse_arguments()
                self.assertEqual(error.exception.code, 2)
                self.assertEqual(list(Path(temporary).iterdir()), [])

    def test_modern_branch_keeps_one_fixture_owner_and_finalizer(self):
        with tempfile.TemporaryDirectory() as temporary:
            argv = invocation(Path(temporary) / "receipt.json")
            installed = object()
            calls = []

            def prepared(args, nonce, legacy, lifetime, response_directory, receipt):
                self.assertFalse(legacy)
                self.assertIsInstance(lifetime, FixtureLifetime)
                calls.append(("prepare", lifetime))
                return installed

            def modern(lifetime, actual, response_directory, receipt):
                self.assertIs(actual, installed)
                self.assertIs(lifetime, calls[0][1])
                calls.append(("modern", lifetime))

            def finalize(args, failure, lifetime, receipt, success):
                self.assertIsNone(failure)
                self.assertTrue(success)
                self.assertIs(lifetime, calls[0][1])
                calls.append(("finalize", lifetime))

            with patch.object(sys, "argv", argv), \
                    patch.object(application, "prepare_fixture", side_effect=prepared), \
                    patch.object(application, "run_prepared_modern_contention", side_effect=modern), \
                    patch.object(application, "prepare_initial_observations", side_effect=AssertionError("legacy initialization")), \
                    patch.object(application, "finalize_fixture", side_effect=finalize):
                application.main()
            self.assertEqual([name for name, _ in calls], ["prepare", "modern", "finalize"])
