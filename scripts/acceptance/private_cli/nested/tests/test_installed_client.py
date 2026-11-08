"""The nested observer uses the published installation without reading its image."""

import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance.evidence.files import write_json
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.private_cli.nested.observation import bound_private_client


class NestedInstalledClientTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve(strict=True)
        self.state = self.base / "worker-b"
        supervisor = self.state / ".supervisor"
        supervisor.mkdir(parents=True)
        installation = self.base / "installed-b"
        installation.mkdir()
        self.client = installation / "pueue"
        self.client.write_text("#!/bin/sh\nexit 0\n")
        self.client.chmod(0o100)
        self.publication = supervisor / "supervisor.identity.json"
        self.binding = {
            "client_executable": str(self.client), "executable_identity": "installed-path-v1",
            "config_path": str(supervisor / "pueue.yml"), "config_digest": "a" * 64,
            "resolved_config_sha256": "b" * 64, "endpoint": "unix:" + str(supervisor / "pueue.sock"),
            "observed_version": "pueue 4.0.4",
        }
        self.publish()

    def publish(self):
        write_json(self.publication, {"schema_version": 1, "binding": self.binding}, replace=True)

    def test_current_installed_marker_needs_no_executable_sha(self):
        environment = {"PATH": str(self.base / "incidental-installation"), "KEEP": "observer"}
        client, selected = bound_private_client(self.state, environment)
        self.assertEqual(client, self.client)
        self.assertEqual(selected, environment)
        self.assertIsNot(selected, environment)

    def test_execute_only_installed_client_is_observed_without_content_io(self):
        original_open = Path.open

        def open_record(path, *arguments, **keywords):
            self.assertNotEqual(path, self.client, "observer opened the installed program")
            return original_open(path, *arguments, **keywords)

        with patch.object(Path, "open", open_record):
            client, _environment = bound_private_client(self.state, {})
        self.assertEqual(client, self.client)
        self.assertEqual(self.client.stat().st_mode & 0o777, 0o100)

    def test_unmarked_installed_client_keeps_historical_record_data_without_rehashing(self):
        self.binding.pop("executable_identity")
        self.binding["client_sha256"] = "c" * 64
        self.publish()
        before = self.publication.read_bytes()
        self.assertEqual(bound_private_client(self.state, {})[0], self.client)
        self.assertEqual(self.publication.read_bytes(), before)

    def test_known_historical_state_copy_is_never_selected(self):
        copied = self.state / ".supervisor" / ("pueue-pair-" + "d" * 64) / "pueue"
        copied.parent.mkdir()
        copied.write_text("#!/bin/sh\nexit 0\n")
        copied.chmod(0o100)
        self.binding.pop("executable_identity")
        self.binding.update(client_executable=str(copied), client_sha256="c" * 64)
        self.publish()
        before = self.publication.read_bytes()
        with self.assertRaisesRegex(AcceptanceFailure, "historical.*copy"):
            bound_private_client(self.state, {"PATH": str(self.client.parent)})
        self.assertEqual(self.publication.read_bytes(), before)

    def test_historical_copy_layout_near_misses_remain_ordinary_installations(self):
        supervisor = self.state / ".supervisor"
        reserved = "pueue-pair-" + "d" * 64
        self.binding.pop("executable_identity")
        self.binding["client_sha256"] = "c" * 64
        for path in (supervisor / "pueue-pair-invalid" / "pueue",
                     supervisor / ("pueue-pair-" + "D" * 64) / "pueue",
                     supervisor / "extra" / reserved / "pueue",
                     supervisor / reserved / "unrelated",
                     self.base / "foreign-root" / ".supervisor" / reserved / "pueue"):
            with self.subTest(path=path):
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("#!/bin/sh\nexit 0\n")
                path.chmod(0o100)
                self.binding["client_executable"] = str(path)
                self.publish()
                self.assertEqual(bound_private_client(self.state, {})[0], path)

    def test_installed_marker_discriminates_the_historical_layout(self):
        installed = self.state / ".supervisor" / ("pueue-pair-" + "d" * 64) / "pueue"
        installed.parent.mkdir()
        installed.write_text("#!/bin/sh\nexit 0\n")
        installed.chmod(0o100)
        self.binding["client_executable"] = str(installed)
        self.publish()
        self.assertEqual(bound_private_client(self.state, {})[0], installed)

    def test_captured_resolution_replaces_only_its_owned_environment_keys(self):
        self.binding.update({
            "resolution_os": "linux", "resolution_home": str(self.base / "home-b"),
            "resolution_data_local": str(self.base / "data-b"),
            "resolution_config": str(self.base / "config-b"), "resolution_username": "worker",
        })
        self.publish()
        environment = {"HOME": "/incidental/home", "XDG_CONFIG_HOME": "/incidental/config",
                       "XDG_DATA_HOME": "/incidental/data", "XDG_RUNTIME_DIR": "/incidental/runtime",
                       "PATH": "/incidental/bin", "KEEP": "observer"}
        _client, selected = bound_private_client(self.state, environment)
        self.assertEqual(selected, {"HOME": self.binding["resolution_home"],
                                    "XDG_CONFIG_HOME": self.binding["resolution_config"],
                                    "XDG_DATA_HOME": self.binding["resolution_data_local"],
                                    "PATH": "/incidental/bin", "KEEP": "observer"})
        self.assertEqual(environment["XDG_RUNTIME_DIR"], "/incidental/runtime")
        self.binding["resolution_runtime"] = str(self.base / "runtime-b")
        self.publish()
        self.assertEqual(bound_private_client(self.state, environment)[1]["XDG_RUNTIME_DIR"],
                         self.binding["resolution_runtime"])

    def test_malformed_installed_identity_cannot_select_a_client(self):
        original = dict(self.binding)
        for change in ({"executable_identity": "future"}, {"client_sha256": "c" * 64},
                       {"daemon_sha256": "d" * 64}):
            with self.subTest(change=change):
                self.binding = {**original, **change}
                self.publish()
                with self.assertRaises(AcceptanceFailure):
                    bound_private_client(self.state, {})

    def test_client_paths_must_name_the_physical_executable(self):
        alias = self.base / "client-alias"
        alias.symlink_to(self.client)
        parent_alias = self.base / "installation-alias"
        parent_alias.symlink_to(self.client.parent, target_is_directory=True)
        for path in ("relative/pueue", str(self.client.parent / "missing"), str(self.client.parent),
                     str(alias), str(parent_alias / "pueue"), str(self.client.parent) + "/./pueue",
                     str(self.client.parent) + "//pueue", str(self.client) + "\x00"):
            with self.subTest(path=path):
                self.binding["client_executable"] = path
                self.publish()
                with self.assertRaises(AcceptanceFailure):
                    bound_private_client(self.state, {})

    def test_nonexecutable_client_cannot_fall_back_to_observer_path(self):
        self.client.chmod(0o600)
        with self.assertRaises(AcceptanceFailure):
            bound_private_client(self.state, {"PATH": os.defpath})

    def test_binding_for_another_root_cannot_select_an_observer(self):
        self.binding["config_path"] = str(self.base / "other-root" / ".supervisor" / "pueue.yml")
        self.publish()
        with self.assertRaises(AcceptanceFailure):
            bound_private_client(self.state, {})
