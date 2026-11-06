"""Actorless checks for independently built layouts and verified archive installation."""

import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.private_cli import installation
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, BoundedCommandResult, CommandFailure, command_closure_unproven


class FixtureInstallationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-installation-contract-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.source = self.root / "source"
        self.source.mkdir()
        (self.source / "go.mod").write_text("module example.invalid/candidate\n")
        for name in ("delegate", "delegate-run"):
            directory = self.source / "cmd" / name
            directory.mkdir(parents=True)
            (directory / "main.go").write_text("package main\nfunc main() {}\n")
        self.owner = DirectoryOwner()
        self.addCleanup(self.owner.rollback)

    @staticmethod
    def materialize_build(arguments, _environment, **_options):
        # Original test output models the build edge; no installed file is read/copied.
        target = Path(arguments[arguments.index("-o") + 1])
        target.write_text("#!/bin/sh\nexit 0\n")
        target.chmod(0o700)
        return BoundedCommandResult(0, b"")

    def test_builds_both_commands_from_selected_source_directly_into_each_owned_layout(self):
        first = self.owner.create("first package", self.root / "first", self.root).directory
        second = self.owner.create("second package", self.root / "second", self.root).directory
        with patch.object(installation, "run_bounded_command", side_effect=self.materialize_build) as build:
            adjacent, adjacent_bin, adjacent_libexec = installation.install_package_layout(
                self.owner, self.source, first, False)
            linked, canonical, libexec = installation.install_package_layout(
                self.owner, self.source, second, True)
        self.assertEqual((adjacent, adjacent_bin, adjacent_libexec),
                         (first / "install", first / "install", first / "libexec"))
        self.assertEqual(canonical, second / "Cellar/delegation-layer/0.1.0/bin")
        self.assertEqual(libexec, canonical.parent / "libexec")
        self.assertEqual(build.call_count, 4)
        for call, target, role in zip(build.call_args_list, (adjacent_bin, adjacent_bin, canonical, canonical),
                                     ("delegate", "delegate-run", "delegate", "delegate-run")):
            self.assertEqual(call.args[0], ["go", "build", "-buildvcs=false", "-ldflags",
                                           "-X main.version=dev", "-o", str(target / role), "./cmd/" + role])
            self.assertEqual(call.kwargs["cwd"], self.source)
            self.assertEqual(call.args[1]["GOTOOLCHAIN"], "local")
            self.assertEqual(call.args[1]["CGO_ENABLED"], "0")
            self.assertNotIn("GOOS", call.args[1])
            self.assertNotIn("GOARCH", call.args[1])
            self.assertNotIn("PRIVATE_TOKEN", call.args[1])
        for name in ("delegate", "delegate-run"):
            self.assertTrue((linked / name).is_symlink())
            self.assertEqual((linked / name).resolve(strict=True), canonical / name)
            self.assertFalse((canonical / name).is_symlink())
            self.assertFalse(os.path.samestat((adjacent / name).stat(), (canonical / name).stat()))

    def test_missing_source_command_and_occupied_destination_fail_before_building(self):
        destination = self.owner.create("installation", self.root / "install", self.root).directory
        command = self.source / "cmd/delegate-run/main.go"
        command.unlink()
        with patch.object(installation, "run_bounded_command") as build:
            with self.assertRaisesRegex(RuntimeError, "both Delegate commands"):
                installation.build_candidate_pair(self.source, destination)
            build.assert_not_called()
        command.write_text("package main\n")
        (destination / "delegate-run").symlink_to(self.root / "missing")
        with patch.object(installation, "run_bounded_command") as build:
            with self.assertRaisesRegex(RuntimeError, "occupied"):
                installation.build_candidate_pair(self.source, destination)
            build.assert_not_called()

    def test_successful_build_must_produce_real_executables(self):
        destination = self.owner.create("installation", self.root / "install", self.root).directory
        for kind in ("missing", "nonexecutable", "symlink"):
            with self.subTest(kind=kind):
                def invalid(arguments, _environment, **_options):
                    target = Path(arguments[arguments.index("-o") + 1])
                    if kind == "nonexecutable":
                        target.write_text("unusable build output\n")
                    elif kind == "symlink":
                        target.symlink_to(self.root / "missing")
                    return BoundedCommandResult(0, b"")
                with patch.object(installation, "run_bounded_command", side_effect=invalid):
                    with self.assertRaisesRegex(RuntimeError, "did not install executable"):
                        installation.build_candidate_pair(self.source, destination)
                (destination / "delegate").unlink(missing_ok=True)

    def test_build_failure_reports_status_without_private_output_and_stops_before_second_build(self):
        destination = self.owner.create("installation", self.root / "install", self.root).directory
        with patch.object(installation, "run_bounded_command",
                          return_value=BoundedCommandResult(7, b"PRIVATE stdout", b"PRIVATE stderr")) as build:
            with self.assertRaisesRegex(RuntimeError, "failed with exit 7") as error:
                installation.build_candidate_pair(self.source, destination)
        self.assertNotIn("PRIVATE", str(error.exception))
        self.assertEqual(build.call_count, 1)

    def test_supervisor_uses_original_archive_installer_once_for_host_destination(self):
        with patch.object(installation, "run_bounded_command", return_value=BoundedCommandResult(0, b"")) as run:
            installation.install_supervisor_pair(self.root)
        self.assertEqual(run.call_args.args[0], [sys.executable,
                         str(installation.REPOSITORY / "scripts/fetch_pueue_assets.py"), "--host", str(self.root)])
        self.assertEqual(run.call_args.kwargs["cwd"], installation.REPOSITORY)

    def test_bounded_installer_failure_is_explicit(self):
        failure = BoundedCommandError(CommandFailure.TIMEOUT)
        with patch.object(installation, "run_bounded_command", side_effect=failure):
            with self.assertRaisesRegex(RuntimeError, "command_timeout") as error:
                installation.install_supervisor_pair(self.root)
        self.assertIs(error.exception.__cause__, failure)

    def test_adjacent_candidate_is_one_owned_build_with_independent_archive_pair(self):
        directory = self.root / "adjacent"
        with patch.object(installation, "build_candidate_pair") as build, \
                patch.object(installation, "install_supervisor_pair") as supervisor:
            sources = installation.install_adjacent_candidate(self.owner, self.source, directory)
        build.assert_called_once_with(self.source, directory)
        supervisor.assert_called_once_with(directory)
        self.assertEqual(sources, {name: directory / name for name in
                                  ("delegate", "delegate-run", "pueue", "pueued")})

    def test_closure_refusal_uses_the_child_owner_verdict_through_wrappers(self):
        for reason in (CommandFailure.TERMINATION_TIMEOUT, CommandFailure.TERMINATION_FAILED):
            failure = BoundedCommandError(reason)
            wrapped = RuntimeError("installation failed")
            wrapped.__cause__ = failure
            with self.subTest(reason=reason):
                self.assertTrue(command_closure_unproven(wrapped))
                failure.exit_code = -9
                self.assertFalse(command_closure_unproven(wrapped))
        failure = BoundedCommandError(CommandFailure.TIMEOUT, -9)
        failure.__cause__ = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)
        self.assertFalse(command_closure_unproven(failure))
        self.assertFalse(command_closure_unproven(RuntimeError("unrelated setup failure")))


if __name__ == "__main__":
    unittest.main()
