"""Hermetic oracles for a native acceptance behavior cohort."""
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch
from typing import Callable

import acceptance_provider_common as provider_common
import acceptance_supervisor_common as supervisor_common
from scripts.acceptance_tests.native.support import adjacent_runner_bundle
from scripts.acceptance_tests.resources import track_directory_pins
from scripts.delegate_acceptance.native import (
    acceptance as gate,
)
from scripts.delegate_acceptance.native.timeouts import (
    TIMEOUT_SOURCE_TOOL_TIMEOUT_MS,
    TimeoutLeaseOwner,
)


class NativeAcceptanceOracleTests(unittest.TestCase):
    def _timeout_lease_operation(self, root: Path) -> object:
        output = root / "evidence"
        workspace = root / "workspace"
        briefs = root / "briefs"
        output.mkdir(parents=True)
        owner = supervisor_common.DirectoryOwner()
        workspace_token = owner.create("timeout workspace", workspace, root)
        briefs_token = owner.create("timeout briefs", briefs, root)
        timeout_state = TimeoutLeaseOwner(workspace_token.directory, True, owner)
        raw_token = owner.create("raw process captures", output / "processes", output)
        operation = object.__new__(gate.NativeAcceptance)
        operation.success_candidate = None
        operation.output = output
        operation.workspace = workspace_token.directory
        operation.briefs = briefs_token.directory
        operation.owned_directories = owner.transfer()
        track_directory_pins(self, operation.owned_directories)
        operation.processes = SimpleNamespace(capture_root=raw_token)
        operation.timeout_state = timeout_state
        return operation

    @staticmethod
    def _timeout_owner(capture: Callable[[Path], bool]) -> TimeoutLeaseOwner:
        owner = TimeoutLeaseOwner(Path("/"), False, supervisor_common.DirectoryOwner())
        owner.capture = capture
        return owner

    @staticmethod
    def _timeout_writer(lease: gate.TimeoutFifoLease) -> int:
        return os.open(lease.path, os.O_WRONLY | os.O_NONBLOCK)

    def test_timeout_liveness_requires_exact_fifo_inode_and_distinct_ready_evidence(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-ready-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            try:
                os.write(parent_writer, parent.expected)
                os.write(child_writer, child.expected)
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
                os.close(parent_writer)
                os.close(child_writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_marker_only_forgery(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-marker-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            try:
                os.write(parent_writer, parent.expected)
                os.write(child_writer, child.expected)
                os.close(parent_writer)
                os.close(child_writer)
                with self.assertRaisesRegex(gate.AcceptanceFailure, "closed before"):
                    operation._capture_timeout_liveness()
                self.assertFalse((operation.output / "timeout-liveness.json").exists())
            finally:
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_malformed_fifo_evidence(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-malformed-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_state.leases["parent"]
            writer = self._timeout_writer(parent)
            try:
                os.write(writer, b"marker-only\n")
                with self.assertRaisesRegex(gate.AcceptanceFailure, "malformed"):
                    operation._timeout_liveness_ready()
            finally:
                os.close(writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_marker_file_alone_is_not_readiness(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-marker-file-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            operation.timeout_state.marker_file.write_text("timeout-marker:forged\n")
            self.assertFalse(operation._timeout_liveness_ready())
            self.assertFalse((operation.output / "timeout-liveness.json").exists())
            operation._cleanup_timeout_leases()

    def test_timeout_liveness_one_side_eof_is_insufficient(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-one-eof-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            os.write(parent_writer, parent.expected)
            os.write(child_writer, child.expected)
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                os.close(parent_writer)
                self.assertFalse(operation._timeout_liveness_closed())
                self.assertFalse(operation.timeout_state.leases["child"].eof)
            finally:
                os.close(child_writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_requires_both_fifo_eof_for_success(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-both-eof-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            os.write(parent_writer, parent.expected)
            os.write(child_writer, child.expected)
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                os.close(parent_writer)
                os.close(child_writer)
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

        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-silent-eof-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_state.leases["parent"]
            writer = self._timeout_writer(parent)
            try:
                os.write(writer, parent.expected)
                parent.observe()
                self.assertTrue(parent.ready)
                self.assertFalse(parent.eof)
                os.close(writer)
                writer = None
                with patch.object(gate.select, "poll", return_value=SilentPoll()):
                    parent.observe()
                self.assertTrue(parent.eof)
            finally:
                if writer is not None:
                    os.close(writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_cleanup_removes_only_owned_fifos_and_artifacts(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-cleanup-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
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
        with tempfile.TemporaryDirectory(prefix="native-timeout-finalize-retain-") as directory:
            root = Path(directory)
            operation = self._timeout_lease_operation(root)
            base_owner = supervisor_common.DirectoryOwner()
            operation.base_parent = root / "private"
            operation.base_parent_token = base_owner.create(
                "private parent", operation.base_parent, root)
            track_directory_pins(self, (operation.base_parent_token,))
            base_token = base_owner.create(
                "private base", operation.base_parent / "run", operation.base_parent)
            track_directory_pins(self, (base_token,))
            operation.base = base_token.directory
            operation.owned_directories = (
                *base_owner.transfer(), *operation.owned_directories,
            )
            operation.closed = False
            operation.safe_shutdown = lambda: False
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
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-replace-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            os.write(parent_writer, parent.expected)
            os.write(child_writer, child.expected)
            replacement = parent.path
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                replacement.unlink()
                replacement.write_text("replacement")
                with self.assertRaisesRegex(gate.AcceptanceFailure, "replaced"):
                    operation._timeout_liveness_closed()
                self.assertTrue(replacement.is_file())
            finally:
                os.close(parent_writer)
                os.close(child_writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_an_extra_writer_until_it_closes(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-extra-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_state.leases["parent"]
            child = operation.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            extra_writer = None
            os.write(parent_writer, parent.expected)
            os.write(child_writer, child.expected)
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                extra_writer = self._timeout_writer(child)
                os.close(parent_writer)
                os.close(child_writer)
                self.assertFalse(operation._timeout_liveness_closed())
                os.close(extra_writer)
                extra_writer = None
                self.assertTrue(operation._timeout_liveness_closed())
            finally:
                if extra_writer is not None:
                    os.close(extra_writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_accepts_a_fragmented_marker_and_rejects_a_repeat(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-repeat-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_state.leases["parent"]
            writer = self._timeout_writer(parent)
            try:
                split = len(parent.expected) // 2
                os.write(writer, parent.expected[:split])
                self.assertFalse(operation._timeout_liveness_ready())
                self.assertFalse(parent.ready)
                os.write(writer, parent.expected[split:])
                self.assertFalse(operation._timeout_liveness_ready())
                self.assertTrue(parent.ready)
                # A retried shell call reopens the same lease and repeats its marker.
                os.close(writer)
                writer = self._timeout_writer(parent)
                os.write(writer, parent.expected)
                with self.assertRaisesRegex(gate.AcceptanceFailure, "extra or malformed"):
                    operation._timeout_liveness_ready()
                self.assertFalse((operation.output / "timeout-liveness.json").exists())
            finally:
                os.close(writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_isolated_between_concurrent_acceptance_runs(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-isolation-") as directory:
            root = Path(directory)
            first = self._timeout_lease_operation(root / "first")
            second = self._timeout_lease_operation(root / "second")
            first_parent = first.timeout_state.leases["parent"]
            first_child = first.timeout_state.leases["child"]
            parent_writer = self._timeout_writer(first_parent)
            child_writer = self._timeout_writer(first_child)
            try:
                os.write(parent_writer, first_parent.expected)
                os.write(child_writer, first_child.expected)
                self.assertTrue(first._capture_timeout_liveness())
                self.assertFalse(second._timeout_liveness_ready())
                self.assertNotEqual(first_parent.inode, second.timeout_state.leases["parent"].inode)
                forged = self._timeout_writer(second.timeout_state.leases["parent"])
                try:
                    with self.assertRaisesRegex(gate.AcceptanceFailure, "malformed"):
                        os.write(forged, first_parent.expected)
                        second._timeout_liveness_ready()
                finally:
                    os.close(forged)
            finally:
                os.close(parent_writer)
                os.close(child_writer)
                first._cleanup_timeout_leases()
                second._cleanup_timeout_leases()

    def test_timeout_brief_uses_harness_fifo_leases_and_portable_shell_syntax(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-brief-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
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
        with tempfile.TemporaryDirectory(prefix="native-timeout-brief-call-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
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
            args = SimpleNamespace(
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
                self.assertTrue(supervisor_common.rollback_owned_directories(
                    operation.owned_directories))

    def test_timeout_resume_brief_names_the_previous_provider_session_neutrally(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-resume-brief-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
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
        operation.ops = SimpleNamespace(root_id=None, status=status)
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
        operation.ops = SimpleNamespace(root_id=None, status=status)
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
        operation.ops = SimpleNamespace(root_id=None, status=status)
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
        operation.ops = SimpleNamespace(root_id=None, status=status)
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

        observed = provider_common.wait_runner_terminal(
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
            provider_common.wait_runner_terminal(
                slow_client, root, task, "Success", timeout=10.0,
                sleep_fn=sleep, monotonic_fn=lambda: clock[0], poll_seconds=5)
        # The second call overruns the deadline; no pause or later call follows it.
        self.assertEqual(calls, [(0.0, 10.0), (8.0, 2.0)])
        self.assertEqual(sleeps, [5])
        self.assertEqual(clock[0], 11.0)
        clock[0], calls[:], sleeps[:] = 0.0, [], []
        with self.assertRaisesRegex(RuntimeError, "not observed within acceptance deadline"):
            provider_common.wait_runner_terminal(
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
            provider_common.wait_runner_terminal(*legacy_arguments)
        # The legacy 0.1s cadence holds; the last pause is clamped to the deadline.
        self.assertEqual((len(calls), sleeps[:2]), (3, [0.1, 0.1]))
        self.assertAlmostEqual(sleeps[2], 0.05)
        self.assertAlmostEqual(clock[0], 0.25)
        with self.assertRaises(TypeError):
            provider_common.wait_runner_terminal(*invalid_positional_arguments)
        for interval in (0, -1, float("inf"), float("nan"), True):
            with self.subTest(interval=interval), \
                    self.assertRaisesRegex(gate.AcceptanceFailure, "poll interval"):
                provider_common.wait_runner_terminal(
                    client, root, task, "Success", timeout=1.0, poll_seconds=interval)
        self.assertEqual(len(calls), 3)

    def test_task_ledgers_forward_their_poll_interval_to_the_canonical_observer(self):
        observed = []

        def observer(*_args, poll_seconds, **_options):
            observed.append(poll_seconds)
            raise RuntimeError("observer reached")

        legacy = provider_common.NativeTaskOps(
            object(), Path("/delegate"), Path("/runner"), Path("/pueue"),
            Path("/base"), Path("/state"), Path("/evidence"))
        explicit = provider_common.NativeTaskOps(
            object(), Path("/delegate"), Path("/runner"), Path("/pueue"),
            Path("/base"), Path("/state"), Path("/evidence"), runner_poll_seconds=5)
        native = gate.NativeAcceptanceTaskOps(SimpleNamespace(
            processes=object(), delegate=Path("/delegate"), runner=Path("/runner"),
            pueue=Path("/pueue"), base=Path("/base"), state=Path("/state"),
            output=Path("/evidence")))
        with patch("scripts.delegate_acceptance.tasks.operations.wait_runner_terminal", observer):
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

        observed = provider_common.wait_runner_terminal(
            client, root, task, "Success", timeout=10.0,
            sleep_fn=sleep, monotonic_fn=lambda: clock[0], poll_seconds=5)
        self.assertIs(observed, done)
        self.assertEqual(clock[0], 5.0)
        with self.assertRaisesRegex(gate.AcceptanceFailure, "supervisor result 'Killed'"):
            provider_common.wait_runner_terminal(
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
            owner = SimpleNamespace(
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
            with patch("scripts.delegate_acceptance.tasks.operations.time", virtual), \
                    self.assertRaisesRegex(RuntimeError, "not observed"):
                ops.wait_task("fresh", "b" * 32)
            captures = sorted(owner.processes.directory.iterdir())

        expected_calls = int(ops.watch_seconds / ops.runner_poll_seconds)
        self.assertEqual((ops.watch_seconds, ops.runner_poll_seconds), (330, 5))
        self.assertEqual(sleeps, [5] * expected_calls)
        self.assertEqual([path.name for path in captures],
                         [f"{index:03d}-runner-status" for index in range(1, expected_calls + 1)])
        self.assertEqual(len(owner.processes.entries), expected_calls)
