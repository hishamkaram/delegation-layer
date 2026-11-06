"""Actorless program-content guards for owned fixture installations."""

from contextlib import contextmanager
import json
import os
from pathlib import Path
import shutil
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.directories.operations import rollback_owned_directories
from scripts.delegate_acceptance.hermetic.case import Case
from scripts.delegate_acceptance.hermetic.suite import HermeticSuite
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.acceptance_tests.resources import track_directory_pins


@contextmanager
def forbid_program_content():
    original_open = Path.open
    original_copy = shutil.copyfile

    def reject(path):
        path = Path(path)
        if path.is_file() and os.access(path, os.X_OK) and path.suffix not in (".py", ".go", ".sh"):
            raise AssertionError("installed program content accessed: " + str(path))

    def guarded_open(path, mode="r", *args, **kwargs):
        if "r" in mode or "+" in mode:
            reject(path)
        return original_open(path, mode, *args, **kwargs)

    def guarded_copy(source, destination, *args, **kwargs):
        reject(source)
        return original_copy(source, destination, *args, **kwargs)

    with patch.object(Path, "open", guarded_open), patch.object(shutil, "copyfile", guarded_copy), \
            patch("subprocess.Popen", side_effect=AssertionError("unexpected actor launch")):
        yield


def write_program(path):
    path.write_text("#!/bin/sh\nexit 0\n")  # Authored source, never a copied installation.
    path.chmod(0o700)
    return path


def source_tree(root):
    (root / "go.mod").write_text("module fixture\n")
    for name in ("delegate", "delegate-run", "provider", "pueue-fake", "harnessprobe", "scheduler-fixture"):
        write_program(root / name)
    return root


class InstallationBoundaryTests(unittest.TestCase):
    def test_case_builds_directly_at_quoted_layout_without_reading_programs(self):
        with forbid_program_content(), tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = source_tree(root)
            suite = SimpleNamespace(source_root=source, tools=source, output=root / "evidence")
            installed = []

            def build(_source, role, target):
                installed.append((role, target))
                return write_program(target)

            with patch("scripts.delegate_acceptance.hermetic.case.build_source_command", build, create=True):
                case = Case(suite=suite, case_id="INSTALLATION")
            try:
                self.assertEqual(len(installed), 7)
                self.assertNotIn("scheduler_fixture", [role for role, _target in installed])
                self.assertEqual(case.delegate.parent, case.bin_dir)
                self.assertIn("' ; literal", str(case.runner))
                self.assertEqual(case.pueue.parent, case.supervisor_dir)
                self.assertNotIn("provider_sha256", json.loads(case.profile_config.read_text()))
            finally:
                self.assertTrue(rollback_owned_directories(case.owned_directories))

    def test_scheduler_fixture_is_built_only_for_its_compatibility_case(self):
        with forbid_program_content(), tempfile.TemporaryDirectory() as temporary:
            root = source_tree(Path(temporary))
            installed = []

            def build(_source, role, target):
                installed.append((role, target))
                return write_program(target)

            with patch("scripts.delegate_acceptance.hermetic.case.build_source_command", build):
                case = Case(suite=SimpleNamespace(source_root=root), case_id="SCHEDULER-COMPAT")
            try:
                self.assertEqual(len(installed), 8)
                self.assertEqual(installed[-1], ("scheduler_fixture", case.scheduler_fixture))
                self.assertEqual(case.scheduler_fixture.parent, case.bin_dir)
            finally:
                self.assertTrue(rollback_owned_directories(case.owned_directories))

    def test_summary_records_source_roles_without_program_hashes(self):
        with forbid_program_content(), tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source_tree(root)
            suite = HermeticSuite(root, root / "evidence")
            summary = suite.write_summary()
            self.assertEqual(summary["source_root"], str(root.resolve()))
            self.assertNotIn("sha256", json.dumps(summary["tools"]))
            self.assertIn("scheduler-fixture", summary["tools"])

    def test_failure_snapshot_preserves_data_links_and_original_programs(self):
        with forbid_program_content(), tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            case = object.__new__(Case)
            case.base = root / "private"
            case.base.mkdir()
            case.bin_dir = case.base / "cli space ' ; literal"
            case.supervisor_dir = case.base / "supervisor space ' ; literal"
            for directory, config in ((case.bin_dir, "provider-fixture.json"),
                                      (case.supervisor_dir, "fake-supervisor.json")):
                directory.mkdir()
                write_program(directory / "program.offline")
                (directory / config).write_text("{}\n")
            (case.base / "stderr").write_text("diagnostic")
            (case.base / "unavailable").symlink_to(root / "absent")
            case.case_id = "SNAPSHOT"
            case.suite = SimpleNamespace(output=root / "evidence")
            case.processes = Processes(case.base / "processes", {})
            track_directory_pins(self, (case.processes.capture_root,))
            case.retain_failure(RuntimeError("expected"))
            snapshot = case.suite.output / "cases/SNAPSHOT/private-base"
            self.assertEqual((snapshot / "stderr").read_text(), "diagnostic")
            self.assertTrue((snapshot / "unavailable").is_symlink())
            for directory in (case.bin_dir, case.supervisor_dir):
                self.assertTrue((directory / "program.offline").exists())
                self.assertFalse((snapshot / directory.name / "program.offline").exists())
                self.assertEqual(len(list((snapshot / directory.name).iterdir())), 1)
