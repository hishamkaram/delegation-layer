"""Finite control-fixture transport and atomic event publication contracts."""

import json
import os
import unittest
from unittest.mock import patch

from scripts.acceptance_tests.private_cli.modern_fixtures import ModernControlFixture
from scripts.delegate_acceptance.private_cli import control_fixture
from scripts.delegate_acceptance.private_cli.contention_controls import control_gate


class ModernFiniteFixtureTests(unittest.TestCase):
    def setUp(self):
        self.fixture = ModernControlFixture(self)

    def test_event_is_complete_create_once_and_never_overwrites_a_previous_receipt(self):
        event = self.fixture.controls.directory / "event.json"
        record = {"nested": {"lease_records": ["one", "two"]}}
        control_fixture.publish_event(event, record)
        self.assertEqual(json.loads(event.read_text()), record)
        with self.assertRaises(FileExistsError):
            control_fixture.publish_event(event, {"changed": True})
        self.assertEqual(json.loads(event.read_text()), record)
        self.assertEqual(list(event.parent.glob("*.staging")), [])

    def test_version_help_and_another_root_do_not_enter_the_leased_status_gate(self):
        gate = control_gate(self.fixture.controls, "ungated")
        config = str(self.fixture.supervisor / "pueue.yml")
        (gate.directory / "request.json").write_text(json.dumps({"config": config, "nonce": "test"}))
        for arguments in (
            ["--version"],
            ["-c", config, "--version"],
            ["--help"],
            ["-c", "/another/config", "status", "--json"],
        ):
            with (
                self.subTest(arguments=arguments),
                patch.dict(os.environ, {"TMPDIR": str(gate.directory)}),
                patch.object(control_fixture.os, "execv") as execute,
                patch.object(control_fixture, "wait_for_release", side_effect=AssertionError("unexpected gate")),
            ):
                control_fixture.run_client("/usr/bin/true", self.fixture.controls.directory, arguments)
                execute.assert_called_once_with("/usr/bin/true", ["/usr/bin/true", *arguments])
            self.assertFalse(gate.receipt.exists())

    def test_fixture_wait_expires_without_leaving_a_child_or_signalling_any_process(self):
        missing = self.fixture.controls.directory / "never-released"
        with (
            patch.object(control_fixture.time, "monotonic", side_effect=[0, 2]),
            patch.object(control_fixture.time, "sleep") as sleep,
        ):
            with self.assertRaisesRegex(RuntimeError, "deadline expired"):
                control_fixture.wait_for_release(missing, 1)
        sleep.assert_not_called()
        self.assertFalse(self.fixture.processes.entries)
