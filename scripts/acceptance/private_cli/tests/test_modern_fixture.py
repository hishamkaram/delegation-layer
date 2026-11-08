"""Finite control-fixture transport and atomic event publication contracts."""

import json
import os
import unittest
from unittest.mock import patch

from scripts.acceptance.private_cli.tests.modern_fixtures import ModernControlFixture
from scripts.acceptance.private_cli.tests import modern_application_fixture as modern_application_fixture
from scripts.acceptance.private_cli import control_fixture as control_fixture
from scripts.acceptance.private_cli.contention.controls import control_gate


class ModernFiniteFixtureTests(unittest.TestCase):
    def setUp(self):
        self.fixture = ModernControlFixture(self)

    def test_failed_process_construction_refuses_access_and_cleans_owned_directories(self):
        fixture = object.__new__(ModernControlFixture)
        cleanup_owner = unittest.TestCase()
        self.addCleanup(cleanup_owner.doCleanups)
        failure = OSError("injected process owner construction failure")
        with patch("scripts.acceptance.private_cli.tests.modern_fixtures.Processes", side_effect=failure):
            with self.assertRaises(OSError) as raised:
                fixture.__init__(cleanup_owner)
        self.assertIs(raised.exception, failure)
        with self.assertRaisesRegex(RuntimeError, "process owner is not initialized"):
            self.assertIsNotNone(fixture.processes)
        self.assertTrue(cleanup_owner.doCleanups())
        self.assertFalse(fixture.root.directory.exists())

    def test_later_construction_failure_drains_the_created_process_owner_before_cleanup(self):
        fixture = object.__new__(ModernControlFixture)
        cleanup_owner = unittest.TestCase()
        self.addCleanup(cleanup_owner.doCleanups)
        failure = OSError("injected bundle construction failure")
        with patch.object(ModernControlFixture, "install", side_effect=failure):
            with self.assertRaises(OSError) as raised:
                fixture.__init__(cleanup_owner)
        self.assertIs(raised.exception, failure)
        processes = fixture.processes
        with patch.object(processes, "drain", wraps=processes.drain) as drain:
            self.assertTrue(cleanup_owner.doCleanups())
        drain.assert_called_once_with(40)
        self.assertTrue(processes.finished_naturally())
        self.assertFalse(fixture.root.directory.exists())

    def test_lock_deadline_retains_contention_cause_and_closes_descriptor(self):
        contention = BlockingIOError("lock held")
        with (
            patch.object(modern_application_fixture.os, "open", return_value=42),
            patch.object(modern_application_fixture.os, "close") as close,
            patch.object(modern_application_fixture.fcntl, "flock", side_effect=contention),
            patch.object(modern_application_fixture.time, "monotonic", side_effect=[0, 20]),
        ):
            with self.assertRaisesRegex(RuntimeError, "lock deadline expired") as raised:
                modern_application_fixture.acquire_lock("fixture.lock", 1)
        self.assertIs(raised.exception.__cause__, contention)
        close.assert_called_once_with(42)

    def test_event_is_complete_create_once_and_never_overwrites_a_previous_receipt(self):
        event = self.fixture.controls.directory / "event.json"
        record: dict[str, object] = {"nested": {"lease_records": ["one", "two"]}}
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
