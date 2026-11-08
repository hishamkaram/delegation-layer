"""Hermetic oracles for a native acceptance behavior cohort."""

import sys
from pathlib import Path
from argparse import Namespace
from contextlib import ExitStack
from io import FileIO
import os
import stat
import subprocess
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch
from typing import Callable


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.native.tests.support import NoCommands
from scripts.acceptance.native.tests.support import task_owner

from scripts.acceptance.directories import operations as directories_operations
from scripts.acceptance.supervisor import observation as supervisor_observation
from scripts.acceptance.tasks import operations as tasks_operations


from scripts.acceptance.native.tests.support import adjacent_runner_bundle
from scripts.acceptance.native.tests.support import invoke_with_invalid_arguments
from scripts.acceptance.testing.resources import track_directory_pins
from scripts.acceptance.native import workflow as gate
from scripts.acceptance.native.timeouts import TIMEOUT_SOURCE_TOOL_TIMEOUT_MS
from scripts.acceptance.native.timeouts import TimeoutLeaseOwner
from scripts.acceptance.native.timeouts import _TimeoutLeaseSetupError


class NativeAcceptanceOracleTests(unittest.TestCase):
    def _timeout_lease_operation(self, root: Path, resources: ExitStack) -> gate.NativeAcceptance:
        output = root / "evidence"
        workspace = root / "workspace"
        briefs = root / "briefs"
        output.mkdir(parents=True)
        owner = directories_operations.DirectoryOwner()
        owned_directories = None

        def release_pins():
            if owned_directories is None:
                owner.release()
            else:
                for token in owned_directories:
                    directories_operations.release_owned_directory(token)

        resources.callback(release_pins)
        workspace_token = owner.create("timeout workspace", workspace, root)
        briefs_token = owner.create("timeout briefs", briefs, root)
        timeout_state = TimeoutLeaseOwner(workspace_token.directory, True, owner)
        for lease in timeout_state.leases.values():
            resources.callback(lease.close)
        raw_token = owner.create("raw process captures", output / "processes", output)
        operation = object.__new__(gate.NativeAcceptance)
        operation.success_candidate = None
        operation.output = output
        operation.workspace = workspace_token.directory
        operation.briefs = briefs_token.directory
        operation.owned_directories = owner.transfer()
        owned_directories = operation.owned_directories
        self.enterContext(patch.object(operation, "processes", SimpleNamespace(capture_root=raw_token), create=True))
        operation.timeout_state = timeout_state
        return operation

    @staticmethod
    def _timeout_owner(capture: Callable[[Path], bool]) -> TimeoutLeaseOwner:
        owner = TimeoutLeaseOwner(Path("/"), False, directories_operations.DirectoryOwner())
        owner.capture = Mock(side_effect=capture)
        return owner

    @staticmethod
    def _timeout_writer(lease: gate.TimeoutFifoLease, resources: ExitStack) -> FileIO:
        descriptor = os.open(lease.path, os.O_WRONLY | os.O_NONBLOCK)
        return resources.enter_context(os.fdopen(descriptor, "wb", buffering=0))

    def test_timeout_fixture_closes_readers_and_writer_after_early_assertion(self):
        with self.assertRaisesRegex(AssertionError, "injected early assertion"):
            with tempfile.TemporaryDirectory(prefix="timeout-failed-assertion-") as directory, ExitStack() as resources:
                operation = self._timeout_lease_operation(Path(directory), resources)
                leases = list(operation.timeout_state.leases.values())
                writer = self._timeout_writer(leases[0], resources)
                self.fail("injected early assertion")
        self.assertTrue(writer.closed)
        self.assertTrue(all(lease.fd is None for lease in leases))
        self.assertFalse(Path(directory).exists())

    def test_timeout_fixture_closes_first_reader_when_second_acquisition_fails(self):
        opened = []
        constructor = gate.TimeoutFifoLease

        def acquire(*args):
            if opened:
                raise OSError("injected second FIFO acquisition failure")
            lease = constructor(*args)
            opened.append(lease)
            return lease

        with self.assertRaisesRegex(OSError, "second FIFO acquisition failure"):
            with tempfile.TemporaryDirectory(prefix="timeout-partial-acquisition-") as directory, \
                    ExitStack() as resources, patch("scripts.acceptance.native.timeouts.TimeoutFifoLease", side_effect=acquire):
                self._timeout_lease_operation(Path(directory), resources)
        self.assertEqual(len(opened), 1)
        self.assertIsNone(opened[0].fd)
        self.assertFalse(Path(directory).exists())

    def test_timeout_constructor_preserves_primary_fault_and_reports_reader_close_fault(self):
        opened = []
        constructor = gate.TimeoutFifoLease
        primary = OSError("injected second FIFO acquisition failure")
        cleanup_error = OSError("reader close fault")
        with tempfile.TemporaryDirectory(prefix="timeout-partial-close-fault-") as directory, ExitStack() as resources:
            owner = directories_operations.DirectoryOwner()
            resources.callback(owner.release)

            def acquire(*args):
                if opened:
                    raise primary
                lease = constructor(*args)
                resources.callback(lease.close)  # Original closer remains the test's final recovery owner.
                resources.enter_context(patch.object(lease, "close", side_effect=cleanup_error))
                opened.append(lease)
                return lease

            with patch("scripts.acceptance.native.timeouts.TimeoutFifoLease", side_effect=acquire), \
                    self.assertRaises(_TimeoutLeaseSetupError) as caught:
                TimeoutLeaseOwner(Path(directory), True, owner)
            self.assertIs(caught.exception.__cause__, primary)
            self.assertIs(caught.exception.owner.leases["parent"], opened[0])
            self.assertEqual(caught.exception.cleanup_failures, (cleanup_error,))
            self.assertIsNotNone(opened[0].fd)
            self.assertTrue(opened[0].path.is_fifo())
            self.assertIn(str(opened[0].path.parent), str(caught.exception))
        self.assertIsNone(opened[0].fd)

    def test_native_constructor_retains_timeout_ancestors_when_reader_close_is_unproven(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-constructor-retain-") as directory:
            root = Path(directory)
            output, home = root / "evidence", root / "home"
            output.mkdir()
            delegate, runner = adjacent_runner_bundle(root)
            args = Namespace(
                provider="opencode:run", mode="workspace-write", scenario="timeout-resume",
                discover_models=False, output=str(output), delegate=str(delegate), runner=str(runner),
                pueue="/bin/sh", pueued="/bin/sh", provider_executable="/bin/sh", model=None, effort=None)
            opened = []
            constructor, close = gate.TimeoutFifoLease, os.close
            primary = OSError("injected second FIFO acquisition failure")
            close_error = OSError("injected result after native close")
            faulted = False
            selected_fd = None

            def acquire(*arguments):
                nonlocal selected_fd
                if opened:
                    raise primary
                lease = constructor(*arguments)
                opened.append(lease)
                selected_fd = lease.fd
                return lease

            # Retain the observed number before close() retires it.
            def report_after_close(fd):
                nonlocal faulted
                close(fd)
                if selected_fd is not None and fd == selected_fd and not faulted:
                    faulted = True
                    raise close_error

            before_descriptors = len(os.listdir("/dev/fd"))
            with patch.object(gate.Path, "home", return_value=home), \
                    patch.object(gate, "reject_tmp"), patch.object(gate, "require_discovery"), \
                    patch("scripts.acceptance.native.timeouts.TimeoutFifoLease", side_effect=acquire), \
                    patch("scripts.acceptance.native.timeouts.os.close", side_effect=report_after_close), \
                    self.assertRaises(_TimeoutLeaseSetupError) as caught:
                gate.NativeAcceptance(args)
            partial = caught.exception
            self.assertIs(partial.__cause__, primary)
            self.assertEqual(partial.cleanup_failures, (close_error,))
            self.assertTrue(partial.owner.directory.is_dir())
            self.assertTrue(partial.owner.directory.parent.is_dir())
            self.assertTrue(partial.owner.directory.parent.parent.is_dir())
            self.assertTrue(opened[0].path.is_fifo())
            self.assertFalse(partial.owner.cleanup())
            self.assertTrue(opened[0].path.is_fifo())
            self.assertTrue(faulted)
            self.assertEqual(len(os.listdir("/dev/fd")), before_descriptors)
            # The injected failure followed a real successful close, and no
            # process was launched. Only that proof permits final test disposal.

    def test_timeout_cleanup_reports_close_error_without_retrying_a_retired_descriptor(self):
        # The fault occurs after a real successful native close. This independent
        # proof permits this test's final TemporaryDirectory disposal; the
        # production owner must still retain uncertainty on every cleanup retry.
        with self.assertRaisesRegex(OSError, "injected result after native close"):
            with tempfile.TemporaryDirectory(prefix="timeout-reader-close-error-") as directory, ExitStack() as resources:
                operation = self._timeout_lease_operation(Path(directory), resources)
                parent = operation.timeout_state.leases["parent"]
                descriptor = parent.fd
                close = os.close
                attempts = []

                def close_then_report_error(fd):
                    attempts.append(fd)
                    close(fd)
                    if fd == descriptor:
                        raise OSError("injected result after native close")

                with patch("scripts.acceptance.native.timeouts.os.close", side_effect=close_then_report_error):
                    self.assertFalse(operation._cleanup_timeout_leases())
                    self.assertFalse(operation._cleanup_timeout_leases())
                    with self.assertRaisesRegex(OSError, "injected result after native close"):
                        parent.close()
                self.assertEqual(attempts.count(descriptor), 1)
                self.assertIsNone(parent.fd)
                self.assertTrue(parent.path.is_fifo())
                self.assertIsNone(operation.timeout_state.leases["child"].fd)
                self.assertFalse(operation.timeout_state.leases["child"].path.exists())

    def test_timeout_liveness_requires_exact_fifo_inode_and_distinct_ready_evidence(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-ready-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent, resources)
            child_writer = self._timeout_writer(child, resources)
            try:
                parent_writer.write(parent.expected)
                child_writer.write(child.expected)
                self.assertTrue(operation._capture_timeout_liveness())
                receipt = gate.read_json(operation.output / "timeout-liveness.json")
                self.assertTrue(receipt["ready"])
                self.assertFalse(receipt["closed"])
                self.assertNotEqual(parent.inode, child.inode)
                for name, lease in operation.timeout_state.leases.items():
                    metadata = lease.path.lstat()
                    self.assertEqual(metadata.st_dev, lease.device)
                    self.assertEqual(metadata.st_ino, lease.inode)
                    self.assertTrue(stat.S_ISFIFO(metadata.st_mode))
                    self.assertEqual(receipt["resources"][name]["inode"], lease.inode)
            finally:
                parent_writer.close()
                child_writer.close()
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_marker_only_forgery(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-marker-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent, resources)
            child_writer = self._timeout_writer(child, resources)
            try:
                parent_writer.write(parent.expected)
                child_writer.write(child.expected)
                parent_writer.close()
                child_writer.close()
                with self.assertRaisesRegex(gate.AcceptanceFailure, "closed before"):
                    operation._capture_timeout_liveness()
                self.assertFalse((operation.output / "timeout-liveness.json").exists())
            finally:
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_malformed_fifo_evidence(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-malformed-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            parent = operation.timeout_state.leases["parent"]
            writer = self._timeout_writer(parent, resources)
            try:
                writer.write(b"marker-only\n")
                with self.assertRaisesRegex(gate.AcceptanceFailure, "malformed"):
                    operation._timeout_liveness_ready()
            finally:
                writer.close()
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_marker_file_alone_is_not_readiness(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-marker-file-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            operation.timeout_state.marker_file.write_text("timeout-marker:forged\n")
            self.assertFalse(operation._timeout_liveness_ready())
            self.assertFalse((operation.output / "timeout-liveness.json").exists())
            operation._cleanup_timeout_leases()

    def test_timeout_liveness_one_side_eof_is_insufficient(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-one-eof-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent, resources)
            child_writer = self._timeout_writer(child, resources)
            parent_writer.write(parent.expected)
            child_writer.write(child.expected)
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                parent_writer.close()
                self.assertFalse(operation._timeout_liveness_closed())
                self.assertFalse(operation.timeout_state.leases["child"].eof)
            finally:
                child_writer.close()
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_requires_both_fifo_eof_for_success(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-both-eof-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent, resources)
            child_writer = self._timeout_writer(child, resources)
            parent_writer.write(parent.expected)
            child_writer.write(child.expected)
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                parent_writer.close()
                child_writer.close()
                self.assertTrue(operation._timeout_liveness_closed())
                operation.wait_timeout_leases_closed()
                receipt = gate.read_json(operation.output / "timeout-liveness.json")
                self.assertTrue(receipt["closed"])
                self.assertTrue(all(item["closed"] for item in receipt["resources"].values()))
            finally:
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_observes_eof_without_a_poll_event(self):
        class SilentPoll:
            def register(self, _fd, _events):
                return None

            def poll(self, _timeout):
                return []

        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-silent-eof-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            parent = operation.timeout_state.leases["parent"]
            writer = self._timeout_writer(parent, resources)
            try:
                writer.write(parent.expected)
                parent.observe()
                self.assertTrue(parent.ready)
                self.assertFalse(parent.eof)
                writer.close()
                writer = None
                with patch.object(gate.select, "poll", return_value=SilentPoll()):
                    parent.observe()
                self.assertTrue(parent.eof)
            finally:
                if writer is not None:
                    writer.close()
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_cleanup_removes_only_owned_fifos_and_artifacts(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-cleanup-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            operation.timeout_state.marker_file.write_text("owned\n")
            operation.timeout_state.resume_file.write_text("owned\n")
            paths = [lease.path for lease in operation.timeout_state.leases.values()]

            self.assertTrue(operation._cleanup_timeout_leases())
            self.assertTrue(all(not path.exists() for path in paths))
            self.assertTrue(operation.timeout_state.directory.is_dir())
            self.assertFalse(operation.timeout_state.marker_file.exists())
            self.assertFalse(operation.timeout_state.resume_file.exists())
            self.assertTrue(all(lease.fd is None for lease in operation.timeout_state.leases.values()))

    def test_finalize_retains_timeout_leases_when_shutdown_is_unproven(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-finalize-retain-") as directory, ExitStack() as resources:
            root = Path(directory)
            operation = self._timeout_lease_operation(root, resources)
            base_owner = directories_operations.DirectoryOwner()
            operation.base_parent = root / "private"
            base_parent_token = base_owner.create(
                "private parent", operation.base_parent, root)
            track_directory_pins(self, (base_parent_token,))
            base_token = base_owner.create(
                "private base", operation.base_parent / "run", operation.base_parent)
            track_directory_pins(self, (base_token,))
            operation.base = base_token.directory
            operation.owned_directories = (
                *base_owner.transfer(), *operation.owned_directories,
            )
            operation.closed = False
            operation.safe_shutdown = Mock(return_value=False)
            try:
                self.assertFalse(operation.finalize(True))
                self.assertTrue(operation.timeout_state.directory.is_dir())
                self.assertTrue(all(lease.path.is_fifo()
                                    for lease in operation.timeout_state.leases.values()))
                receipt = gate.read_json(operation.output / "cleanup.json")
                self.assertEqual(receipt["status"], "retained")
                self.assertFalse(receipt["supervisor_shutdown_observed"])
            finally:
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_fifo_replacement_and_preserves_replacement(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-replace-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent, resources)
            child_writer = self._timeout_writer(child, resources)
            parent_writer.write(parent.expected)
            child_writer.write(child.expected)
            replacement = parent.path
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                replacement.unlink()
                replacement.write_text("replacement")
                with self.assertRaisesRegex(gate.AcceptanceFailure, "replaced"):
                    operation._timeout_liveness_closed()
                self.assertTrue(replacement.is_file())
            finally:
                parent_writer.close()
                child_writer.close()
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_an_extra_writer_until_it_closes(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-extra-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent, resources)
            child_writer = self._timeout_writer(child, resources)
            extra_writer = None
            parent_writer.write(parent.expected)
            child_writer.write(child.expected)
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                extra_writer = self._timeout_writer(child, resources)
                parent_writer.close()
                child_writer.close()
                self.assertFalse(operation._timeout_liveness_closed())
                extra_writer.close()
                extra_writer = None
                self.assertTrue(operation._timeout_liveness_closed())
            finally:
                if extra_writer is not None:
                    extra_writer.close()
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_accepts_a_fragmented_marker_and_rejects_a_repeat(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-repeat-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            parent = operation.timeout_state.leases["parent"]
            writer = self._timeout_writer(parent, resources)
            try:
                split = len(parent.expected) // 2
                writer.write(parent.expected[:split])
                self.assertFalse(operation._timeout_liveness_ready())
                self.assertFalse(parent.ready)
                writer.write(parent.expected[split:])
                self.assertFalse(operation._timeout_liveness_ready())
                self.assertTrue(parent.ready)
                # A retried shell call reopens the same lease and repeats its marker.
                writer.close()
                writer = self._timeout_writer(parent, resources)
                writer.write(parent.expected)
                with self.assertRaisesRegex(gate.AcceptanceFailure, "extra or malformed"):
                    operation._timeout_liveness_ready()
                self.assertFalse((operation.output / "timeout-liveness.json").exists())
            finally:
                writer.close()
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_isolated_between_concurrent_acceptance_runs(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-isolation-") as directory, ExitStack() as resources:
            root = Path(directory)
            first = self._timeout_lease_operation(root / "first", resources)
            second = self._timeout_lease_operation(root / "second", resources)
            first_parent = first.timeout_state.leases["parent"]
            first_child = first.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(first_parent, resources)
            child_writer = self._timeout_writer(first_child, resources)
            try:
                parent_writer.write(first_parent.expected)
                child_writer.write(first_child.expected)
                self.assertTrue(first._capture_timeout_liveness())
                self.assertFalse(second._timeout_liveness_ready())
                self.assertNotEqual(first_parent.inode, second.timeout_state.leases["parent"].inode)
                forged = self._timeout_writer(second.timeout_state.leases["parent"], resources)
                try:
                    with self.assertRaisesRegex(gate.AcceptanceFailure, "malformed"):
                        forged.write(first_parent.expected)
                        second._timeout_liveness_ready()
                finally:
                    forged.close()
            finally:
                parent_writer.close()
                child_writer.close()
                first._cleanup_timeout_leases()
                second._cleanup_timeout_leases()

    def test_timeout_brief_uses_harness_fifo_leases_and_portable_shell_syntax(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-brief-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            brief = operation.timeout_brief("timeout-source").read_text()
            command = brief.split("`", 2)[1]

            self.assertIn(str(operation.timeout_state.parent_fifo), command)
            self.assertIn(str(operation.timeout_state.child_fifo), command)
            self.assertIn(operation.timeout_state.parent_ready_marker, command)
            self.assertIn(operation.timeout_state.child_ready_marker, command)
            self.assertTrue(command.startswith("set -eu; "))
            self.assertIn("exec 3>", command)
            self.assertIn("exec 4>", command)
            self.assertIn("exec sleep 600", command)
            self.assertIn("wait", command)
            for forbidden in ("process_ref", "/proc", "pgrep", "$!", "ps -"):
                self.assertNotIn(forbidden, command)
            syntax = subprocess.run(["sh", "-n", "-c", command], capture_output=True,
                                    text=True, check=False)
            self.assertEqual(syntax.returncode, 0, syntax.stderr)
            operation._cleanup_timeout_leases()

    def test_timeout_brief_requires_one_foreground_call_that_outlives_the_budget(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-brief-call-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            brief = operation.timeout_brief("timeout-source").read_text()
            operation._cleanup_timeout_leases()
        self.assertEqual(brief.count("`"), 2)
        self.assertIn("Make exactly one native shell tool call, in the foreground", brief)
        self.assertIn(f"set it to {TIMEOUT_SOURCE_TOOL_TIMEOUT_MS} milliseconds", brief)
        self.assertGreater(TIMEOUT_SOURCE_TOOL_TIMEOUT_MS * 1_000_000,
                           gate.TIMEOUT_RESUME_SOURCE_BUDGET_NANOS)
        self.assertIn("Do not run the command in the background", brief)
        self.assertIn("do not run the command again or retry it", brief)
        self.assertIn("do not reply before the supervisor budget ends the turn", brief)

    def test_timeout_leases_are_inside_the_native_workspace_write_boundary(self):
        for provider in ("codex:exec", "opencode:run"):
            with self.subTest(provider=provider):
                self._assert_timeout_leases_inside_workspace(provider)

    def _assert_timeout_leases_inside_workspace(self, provider: str) -> None:
        with tempfile.TemporaryDirectory(prefix="native-timeout-root-unit-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            home = root / "home"
            delegate, runner = adjacent_runner_bundle(root)
            args = Namespace(
                provider=provider, mode="workspace-write", scenario="timeout-resume",
                discover_models=False, output=str(output), delegate=str(delegate),
                runner=str(runner), pueue="/bin/sh", pueued="/bin/sh",
                provider_executable="/bin/sh", model=None, effort=None,
            )
            with patch.object(gate.Path, "home", return_value=home), \
                    patch.object(gate, "reject_tmp"), \
                    patch.object(gate, "require_discovery"):
                operation = gate.NativeAcceptance(args)
            try:
                self.assertEqual(
                    operation.timeout_state.directory.parent,
                    operation.workspace,
                )
                self.assertTrue(all(
                    lease.path.is_relative_to(operation.workspace)
                    for lease in operation.timeout_state.leases.values()
                ))
                self.assertEqual((operation.provider, operation.scenario),
                                 (provider, "timeout-resume"))
                self.assertTrue(operation.ops.managed_runner)
                if provider == "opencode:run":
                    self.assertIsNone(operation.git)
            finally:
                self.assertTrue(operation._cleanup_timeout_leases())
                self.assertTrue(directories_operations.rollback_owned_directories(
                    operation.owned_directories))

    def test_timeout_resume_brief_names_the_previous_provider_session_neutrally(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-resume-brief-") as directory, ExitStack() as resources:
            operation = self._timeout_lease_operation(Path(directory), resources)
            resume = operation.timeout_brief("timeout-resume", resume=True).read_text()
            source = operation.timeout_brief("timeout-source").read_text()

            self.assertIn("Continue the exact previous provider session.", resume)
            self.assertNotIn("Codex", resume)
            self.assertNotIn("Codex", source)
            self.assertIn(f"`{operation.timeout_state.resume_marker}` followed by one newline", resume)
            self.assertIn(operation.timeout_state.marker_file.name, resume)
            self.assertIn(operation.timeout_state.resume_file.name, resume)
            self.assertIn("`DELEGATION_TIMEOUT_RESUMED`", resume)
            command = source.split("`", 2)[1]
            for lease in operation.timeout_state.leases.values():
                self.assertIn(str(lease.path), command)
            self.assertIn(operation.timeout_state.parent_ready_marker, command)
            self.assertIn(operation.timeout_state.child_ready_marker, command)
            self.assertIn(operation.timeout_state.marker, command)
            self.assertIn("exec sleep 600", command)
            operation._cleanup_timeout_leases()

    def test_timeout_liveness_fifo_unavailability_is_blocked(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-platform-") as directory:
            root = Path(directory)
            with patch.object(gate.sys, "platform", "freebsd"):
                with self.assertRaises(gate.BlockedFailure):
                    gate.TimeoutFifoLease(root / "unsupported.fifo", "timeout", "ready")

    def test_timeout_liveness_setup_allows_slow_queued_and_running_startup(self):
        task_id = "e" * 32
        clock = [0.0]
        statuses = []

        def status(_name, observed_task, expected, timeout):
            self.assertEqual(observed_task, task_id)
            self.assertEqual(expected, {0, 1})
            self.assertGreater(timeout, 0)
            statuses.append("queued" if len(statuses) % 2 == 0 else "running")
            return {"liveness": "running", "publication": "pending",
                    "status": statuses[-1]}

        def sleep(delay):
            clock[0] += delay

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "a" * 32
        self.enterContext(patch.object(operation, "ops", SimpleNamespace(root_id=None, status=status), create=True))
        operation.output = Path(tempfile.gettempdir())
        operation.timeout_state = self._timeout_owner(lambda _output: clock[0] >= 31.0)

        operation.record_timeout_liveness(
            task_id, timeout=40.0, sleep_fn=sleep, monotonic_fn=lambda: clock[0])

        self.assertGreaterEqual(clock[0], 31.0)
        self.assertGreaterEqual(len(statuses), 6)
        self.assertEqual(operation.ops.root_id, operation.root_id)

    def test_timeout_liveness_setup_reports_terminal_delegate_state_immediately(self):
        task_id = "f" * 32
        clock = [0.0]

        def status(_name, observed_task, expected, timeout):
            self.assertEqual(observed_task, task_id)
            self.assertEqual(expected, {0, 1})
            self.assertGreater(timeout, 0)
            return {
                "liveness": "ended", "publication": "rejected", "status": "failed",
                "failure": {"code": "provider_failed", "stage": "run-provider",
                            "next_action": "stop_and_report"},
            }

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "a" * 32
        self.enterContext(patch.object(operation, "ops", SimpleNamespace(root_id=None, status=status), create=True))
        operation.output = Path(tempfile.gettempdir())
        operation.timeout_state = self._timeout_owner(lambda _output: False)

        with self.assertRaisesRegex(
                gate.AcceptanceFailure,
                "ended before both liveness leases.*failure.code='provider_failed'"):
            operation.record_timeout_liveness(
                task_id, timeout=40.0, sleep_fn=lambda delay: clock.__setitem__(0, clock[0] + delay),
                monotonic_fn=lambda: clock[0])

        self.assertEqual(clock[0], 0.0)

    def test_timeout_liveness_setup_stops_on_durable_timeout_state(self):
        task_id = "1" * 32
        clock = [0.0]

        def status(_name, observed_task, expected, timeout):
            self.assertEqual(observed_task, task_id)
            self.assertEqual(expected, {0, 1})
            self.assertGreater(timeout, 0)
            return {"liveness": "undetermined", "publication": "pending",
                    "status": "timed_out"}

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "a" * 32
        self.enterContext(patch.object(operation, "ops", SimpleNamespace(root_id=None, status=status), create=True))
        operation.output = Path(tempfile.gettempdir())
        operation.timeout_state = self._timeout_owner(lambda _output: False)

        with self.assertRaisesRegex(
                gate.AcceptanceFailure,
                "ended before both liveness leases.*status='timed_out'"):
            operation.record_timeout_liveness(
                task_id, timeout=40.0, sleep_fn=lambda delay: clock.__setitem__(0, clock[0] + delay),
                monotonic_fn=lambda: clock[0])

        self.assertEqual(clock[0], 0.0)

    def test_timeout_liveness_setup_uses_one_bounded_deadline(self):
        task_id = "0" * 32
        clock = [0.0]
        calls = []

        def status(_name, observed_task, expected, timeout):
            self.assertEqual(observed_task, task_id)
            self.assertEqual(expected, {0, 1})
            self.assertGreater(timeout, 0)
            self.assertLessEqual(timeout, 3.0 - clock[0])
            calls.append((clock[0], timeout))
            clock[0] += 0.4
            return {"liveness": "running", "publication": "pending"}

        def sleep(delay):
            clock[0] += delay

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "a" * 32
        self.enterContext(patch.object(operation, "ops", SimpleNamespace(root_id=None, status=status), create=True))
        operation.output = Path(tempfile.gettempdir())
        operation.timeout_state = self._timeout_owner(lambda _output: False)

        with self.assertRaisesRegex(gate.AcceptanceFailure, "bounded deadline"):
            operation.record_timeout_liveness(
                task_id, timeout=3.0, sleep_fn=sleep, monotonic_fn=lambda: clock[0])

        self.assertEqual(len(calls), 1)
        self.assertLessEqual(clock[0], 3.0)

    def test_provider_status_polling_passes_remaining_timeout_to_client(self):
        root = "a" * 32
        task = "b" * 32
        clock = [0.0]
        calls = []
        running = {"label": f"delegate:{root}:{task}", "status": {"Running": {}}}
        done = {"label": f"delegate:{root}:{task}",
                "status": {"Done": {"result": "Success"}}}
        responses = iter((running, done))

        def client(_name, _operation, timeout):
            calls.append((clock[0], timeout))
            clock[0] += 0.4
            row = next(responses)
            return SimpleNamespace(json=lambda: {"tasks": {"7": row}})

        def sleep(delay):
            clock[0] += delay

        observed = supervisor_observation.wait_runner_terminal(
            client, root, task, "Success", timeout=1.0,
            sleep_fn=sleep, monotonic_fn=lambda: clock[0])

        self.assertEqual(observed, done)
        self.assertEqual(len(calls), 2)
        self.assertGreater(calls[0][1], calls[1][1])
        self.assertLessEqual(calls[0][1], 1.0)
        self.assertLessEqual(calls[1][1], 1.0 - calls[1][0])

    def test_runner_polling_clamps_slow_calls_and_pauses_to_the_deadline(self):
        root, task = "a" * 32, "b" * 32
        clock, calls, sleeps = [0.0], [], []
        running = {"label": f"delegate:{root}:{task}", "status": {"Running": {}}}

        def slow_client(_name, _operation, timeout):
            calls.append((clock[0], timeout))
            clock[0] += 3.0
            return SimpleNamespace(json=lambda: {"tasks": {"7": running}})

        def sleep(delay):
            sleeps.append(delay)
            clock[0] += delay

        with self.assertRaisesRegex(RuntimeError, "not observed within acceptance deadline"):
            supervisor_observation.wait_runner_terminal(
                slow_client, root, task, "Success", timeout=10.0,
                sleep_fn=sleep, monotonic_fn=lambda: clock[0], poll_seconds=5)
        # The second call overruns the deadline; no pause or later call follows it.
        self.assertEqual(calls, [(0.0, 10.0), (8.0, 2.0)])
        self.assertEqual(sleeps, [5])
        self.assertEqual(clock[0], 11.0)
        clock[0], calls[:], sleeps[:] = 0.0, [], []
        with self.assertRaisesRegex(RuntimeError, "not observed within acceptance deadline"):
            supervisor_observation.wait_runner_terminal(
                slow_client, root, task, "Success", timeout=12.0,
                sleep_fn=sleep, monotonic_fn=lambda: clock[0], poll_seconds=5)
        self.assertEqual(calls, [(0.0, 12.0), (8.0, 4.0)])
        self.assertEqual(sleeps, [5, 1.0])
        self.assertEqual(clock[0], 12.0)

    def test_runner_polling_keeps_the_positional_contract_and_refuses_bad_intervals(self):
        root, task = "a" * 32, "b" * 32
        clock, calls, sleeps = [0.0], [], []
        running = {"label": f"delegate:{root}:{task}", "status": {"Running": {}}}

        def client(_name, _operation, timeout):
            calls.append(timeout)
            return SimpleNamespace(json=lambda: {"tasks": {"7": running}})

        def sleep(delay):
            sleeps.append(delay)
            clock[0] += delay

        # The legacy seven-positional invocation; an eighth positional is refused.
        legacy_arguments = (client, root, task, "Success", 0.25, sleep, lambda: clock[0])
        invalid_positional_arguments = legacy_arguments + (5,)
        with self.assertRaisesRegex(RuntimeError, "not observed"):
            supervisor_observation.wait_runner_terminal(*legacy_arguments)
        # The legacy 0.1s cadence holds; the last pause is clamped to the deadline.
        self.assertEqual((len(calls), sleeps[:2]), (3, [0.1, 0.1]))
        self.assertAlmostEqual(sleeps[2], 0.05)
        self.assertAlmostEqual(clock[0], 0.25)
        with self.assertRaises(TypeError):
            invoke_with_invalid_arguments(supervisor_observation.wait_runner_terminal, invalid_positional_arguments, {})
        for interval in (0, -1, float("inf"), float("nan"), True):
            with self.subTest(interval=interval), \
                    self.assertRaisesRegex(gate.AcceptanceFailure, "poll interval"):
                supervisor_observation.wait_runner_terminal(
                    client, root, task, "Success", timeout=1.0, poll_seconds=interval)
        self.assertEqual(len(calls), 3)

    def test_task_ledgers_forward_their_poll_interval_to_the_canonical_observer(self):
        observed = []

        def observer(*_args, poll_seconds, **_options):
            observed.append(poll_seconds)
            raise RuntimeError("observer reached")

        legacy = tasks_operations.NativeTaskOps(
            NoCommands(), Path("/delegate"), Path("/runner"), Path("/pueue"),
            Path("/base"), Path("/state"), Path("/evidence"))
        explicit = tasks_operations.NativeTaskOps(
            NoCommands(), Path("/delegate"), Path("/runner"), Path("/pueue"),
            Path("/base"), Path("/state"), Path("/evidence"), runner_poll_seconds=5)
        native = gate.NativeAcceptanceTaskOps(task_owner(self,
            provider="codex:exec", processes=NoCommands(), delegate=Path("/delegate"), runner=Path("/runner"),
            pueue=Path("/pueue"), base=Path("/base"), state=Path("/state"),
            output=Path("/evidence")))
        with patch("scripts.acceptance.tasks.operations.wait_runner_terminal", observer):
            for ops in (legacy, explicit, native):
                ops.root_id = "a" * 32
                with self.assertRaisesRegex(RuntimeError, "observer reached"):
                    ops.wait_task("fresh", "b" * 32)
        self.assertEqual(observed, [0.1, 5, 5])

    def test_runner_polling_returns_only_the_exact_observed_terminal_row(self):
        root, task = "a" * 32, "b" * 32
        clock = [0.0]
        killed = {"label": f"delegate:{root}:{task}", "status": {"Done": {"result": "Killed"}}}
        done = {"label": f"delegate:{root}:{task}", "status": {"Done": {"result": "Success"}}}
        responses = iter(({**killed, "status": {"Queued": {}}}, done, killed))

        def client(_name, _operation, timeout):
            self.assertGreater(timeout, 0)
            row = next(responses)
            return SimpleNamespace(json=lambda: {"tasks": {"7": row}})

        def sleep(delay):
            clock[0] += delay

        observed = supervisor_observation.wait_runner_terminal(
            client, root, task, "Success", timeout=10.0,
            sleep_fn=sleep, monotonic_fn=lambda: clock[0], poll_seconds=5)
        self.assertIs(observed, done)
        self.assertEqual(clock[0], 5.0)
        with self.assertRaisesRegex(gate.AcceptanceFailure, "supervisor result 'Killed'"):
            supervisor_observation.wait_runner_terminal(
                client, root, task, "Success", timeout=10.0,
                sleep_fn=sleep, monotonic_fn=lambda: clock[0], poll_seconds=5)

    def test_native_runner_wait_bounds_retained_status_captures(self):
        with tempfile.TemporaryDirectory(prefix="native-runner-poll-") as directory:
            root = Path(directory)
            label = "delegate:" + "a" * 32 + ":" + "b" * 32
            gate.write_json(root / "status.json",
                            {"tasks": {"7": {"label": label, "status": {"Running": {}}}}})
            pueue = root / "pueue"
            pueue.write_text(f"#!/bin/sh\nexec cat '{root / 'status.json'}'\n")
            pueue.chmod(0o700)
            owner = task_owner(self,
                provider="codex:exec",
                processes=gate.Processes(root / "processes", {"PATH": "/usr/bin:/bin"}),
                delegate=pueue, runner=pueue, pueue=pueue, base=root, state=root / "state",
                output=root / "evidence")
            ops = gate.NativeAcceptanceTaskOps(owner)
            ops.bind_supervisor(root / "pueue.yml")
            ops.root_id = "a" * 32
            clock, sleeps = [0.0], []
            virtual = SimpleNamespace(monotonic=lambda: clock[0],
                                      sleep=lambda delay: (sleeps.append(delay),
                                                           clock.__setitem__(0, clock[0] + delay)))
            with patch("scripts.acceptance.tasks.operations.time", virtual), \
                    self.assertRaisesRegex(RuntimeError, "not observed"):
                ops.wait_task("fresh", "b" * 32)
            captures = sorted(owner.processes.directory.iterdir())

        expected_calls = int(ops.watch_seconds / ops.runner_poll_seconds)
        self.assertEqual((ops.watch_seconds, ops.runner_poll_seconds), (330, 5))
        self.assertEqual(sleeps, [5] * expected_calls)
        self.assertEqual([path.name for path in captures],
                         [f"{index:03d}-runner-status" for index in range(1, expected_calls + 1)])
        self.assertEqual(len(owner.processes.entries), expected_calls)
