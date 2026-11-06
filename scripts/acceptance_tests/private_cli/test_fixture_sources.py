"""Source inputs replace installed-copy inputs before any fixture is created."""

from contextlib import redirect_stderr
import io
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.private_cli import arguments, fixture_setup, installation
from scripts.delegate_acceptance.private_cli import scheduler_acceptance as scheduler
from scripts.delegate_acceptance.workspaces.scenarios import application as workspaces


class FixtureSourceArgumentsTests(unittest.TestCase):
    def private_args(self, extra=()):
        argv = ["acceptance-private-cli", "--pueue", "/selected/pueue", "--pueued", "/selected/pueued",
                "--output", "/unused/receipt.json", *extra]
        with patch.object(sys, "argv", argv):
            return arguments.parse_arguments()[0]

    def test_current_checkout_is_default_and_an_explicit_source_is_preserved(self):
        self.assertEqual(self.private_args().source_root, installation.REPOSITORY)
        selected = self.private_args(("--source-root", "/baseline", "--upgraded-source-root", "/candidate"))
        self.assertEqual((selected.source_root, selected.upgraded_source_root), (Path("/baseline"), Path("/candidate")))

    def test_retired_private_inputs_refuse_instead_of_building_unrelated_current_source(self):
        for flag in ("--delegate", "--runner", "--upgraded-delegate", "--upgraded-runner"):
            errors = io.StringIO()
            with self.subTest(flag=flag), redirect_stderr(errors), self.assertRaises(SystemExit) as raised:
                self.private_args((flag, "/arbitrary/installed-program"))
            self.assertEqual(raised.exception.code, 2)
            self.assertIn("installed binary inputs are retired", errors.getvalue())
            self.assertIn("--source-root", errors.getvalue())

    def test_retired_workspace_binary_inputs_fail_before_output_or_fixture_creation(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "absent-parent" / "receipt.json"
            for flag in ("--delegate", "--runner", "--pueue", "--pueued"):
                errors = io.StringIO()
                with self.subTest(flag=flag), redirect_stderr(errors), self.assertRaises(SystemExit):
                    workspaces._parse_args(["--output", str(output), flag, "/arbitrary/installed-program"])
                self.assertIn("installed binary inputs are retired", errors.getvalue())
                self.assertFalse(output.parent.exists())

    def test_invalid_source_stops_before_allocating_private_fixture_resources(self):
        args = SimpleNamespace(source_root=Path("/missing-source-root"), upgraded_source_root=None)
        with patch.object(fixture_setup.os, "access", return_value=True), \
                patch.object(fixture_setup.tempfile, "mkdtemp") as create:
            with self.assertRaisesRegex(RuntimeError, "source root is unavailable"):
                fixture_setup.prepare_fixture(args, "nonce", False, None, None, None)
        create.assert_not_called()

    def test_scheduler_preserves_explicit_source_and_supervisor_selection(self):
        selected = scheduler.parse_arguments(["--source-root", "/checkout", "--pueue", "/selected/pueue",
                                              "--pueued", "/selected/pueued"])
        self.assertEqual(selected.source_root, Path("/checkout"))
        self.assertEqual((selected.pueue, selected.pueued), (Path("/selected/pueue"), Path("/selected/pueued")))
        self.assertIsNone(selected.upgraded_source_root)
        for flag in ("--delegate", "--runner"):
            errors = io.StringIO()
            with self.subTest(flag=flag), redirect_stderr(errors), self.assertRaises(SystemExit):
                scheduler.parse_arguments([flag, "/arbitrary/installed-program"])
            self.assertIn("installed binary inputs are retired", errors.getvalue())


if __name__ == "__main__":
    unittest.main()
