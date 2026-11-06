"""Setup-failure ownership tests: real directory tokens and Process/Processes, fake native handle."""

from __future__ import annotations

import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import scripts.delegate_acceptance.workspaces.scenarios.application as application
import scripts.delegate_acceptance.directories.operations as directories_operations
import scripts.delegate_acceptance.private_cli.leases as private_cli_leases
import scripts.delegate_acceptance.private_cli.supervisor as private_cli_supervisor
import scripts.delegate_acceptance.processes.operations as processes_operations
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.workspaces.scenarios.context import (
    MANAGED_LIFETIME_CLOSURE_TIMEOUT, AcceptanceSetupFailure,
)
from scripts.delegate_acceptance.workspaces.scenarios.evidence import ReceiptFinalizer

TASK_ID = "0123456789abcdef0123456789abcdef"
ROOT_ID = "a" * 32


class SupervisorClock:
    """The supervisor module's time binding: sleeping advances monotonic time instantly."""

    def __init__(self):
        self.now = 0.0
        self.sleeps = []

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class RecordingOwner(DirectoryOwner):
    """A real owner that also records every token it hands out, so tests can prove none is lost."""

    def __init__(self):
        super().__init__()
        self.tokens = []

    def create(self, *arguments, **options):
        self.tokens.append(super().create(*arguments, **options))
        return self.tokens[-1]

    def adopt_created(self, *arguments, **options):
        self.tokens.append(super().adopt_created(*arguments, **options))
        return self.tokens[-1]


class SetupProcessOwnershipTests(unittest.TestCase):
    """Production Process/Processes and real directory tokens, with only the native handle faked."""

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.home = Path(self.directory.name).resolve()
        self.owner = RecordingOwner()
        self.child_exit = None
        self.write_faults = {}
        real_write = processes_operations.write_json
        test = self

        def write(path, value, *arguments, **options):
            if Path(path).name in test.write_faults:
                raise test.write_faults[Path(path).name]
            return real_write(path, value, *arguments, **options)

        class Popen:
            def __init__(self, _argv, **_options):
                self.pid, self.stdin = 4242, None

            def poll(self):
                return test.child_exit

            wait = poll

        self.registries = []
        registries = self.registries
        self.clock = SupervisorClock()

        class RecordingProcesses(Processes):
            """The production registry, remembered so teardown can observe what it still owns."""

            def __init__(self, *arguments, **options):
                super().__init__(*arguments, **options)
                registries.append(self)

        for patch in (
            mock.patch.object(Path, "home", return_value=self.home),
            mock.patch.object(application, "DirectoryOwner", return_value=self.owner),
            mock.patch.object(application, "Processes", RecordingProcesses),
            mock.patch.object(processes_operations, "write_json", side_effect=write),
            mock.patch.object(processes_operations, "subprocess", mock.Mock(Popen=Popen)),
            mock.patch.object(private_cli_supervisor, "time", self.clock),
        ):
            self.enterContext(patch)
        self.addCleanup(self._settle_children)

    def _settle_children(self):
        """Let each fake child exit naturally, then observe it through its own registry.

        Registered after the patches, so it runs while the fake handle and the temporary
        directory still exist, even when the test body failed.
        """
        self.write_faults.clear()
        if self.child_exit is None:
            self.child_exit = 0
        for registry in self.registries:
            if all(entry.result is not None for entry in registry.entries):
                continue
            self.assertEqual(registry.pending(), [])
            for entry in registry.entries:
                self.assertTrue(entry.stdout.closed and entry.stderr.closed)
                self.assertTrue((entry.directory / "completed.json").is_file())

    @staticmethod
    def _layout(owner, root_token, _binaries):
        base = root_token.directory
        owner.create("managed workspace state", base / "state", base)
        return application.WorkspaceLayout(
            base=base, state=base / "state", source=base / "source", provider_bin=base / "bin",
            waiter_directory=base / "waiters", response_directory=base / "responses",
            home_dir=base / "home", install=base / "install", libexec=base / "libexec",
            gate_path=base / "gate", fixture_session_marker=base / "marker.json", lease_nonce="n",
        )

    @staticmethod
    def _argv():
        return [str(application._git_executable()), "init"]

    def _run_git(self, context):
        context.processes.run("git-init", self._argv(), context.source)

    def _start_then_expire(self, context):
        context.processes.start("git-init", self._argv(), context.source).wait(0)

    def _make(self, *, layout=None, prepare=None):
        with (
            mock.patch.object(application, "_create_layout", side_effect=layout or self._layout),
            mock.patch.object(application, "_prepare_source",
                              side_effect=prepare or self._run_git),
        ):
            return application._make_context(mock.Mock(workspace_budget="5m", source_root=Path("/source")))

    def _setup_failure(self, **options):
        with self.assertRaises(AcceptanceSetupFailure) as raised:
            self._make(**options)
        self.addCleanup(raised.exception.release_retained)
        return raised.exception

    def _context(self):
        context, _binaries = self._make(prepare=lambda _context: None)
        self.addCleanup(context.release_retained)
        return context

    def _assert_pinned_then_released(self, holder):
        """Every recorded token is retained and live, then consumed by release with dirs kept."""
        retained = holder.retained_directories()
        self.assertEqual(len(retained), len(self.owner.tokens))
        self.assertTrue(all(kept is made for kept, made in zip(retained, self.owner.tokens)))
        for token in retained:
            token.assert_live()
        holder.release_retained()
        self.assertTrue(all(token.consumed and token.directory.is_dir() for token in retained))
        holder.release_retained()
        self.assertEqual(holder.retained_directories(), ())

    def test_started_receipt_failure_keeps_every_pin_until_release_and_the_child_still_completes(self):
        fault = OSError("started receipt write failed")
        self.write_faults["started.json"] = fault
        failure = self._setup_failure()
        self.assertIs(failure.cause, fault)
        self.assertEqual(len(self.owner.tokens), 2)
        cleanup = failure.cleanup()
        self.assertEqual((cleanup["authorized"], cleanup["decision"]),
                         (False, "process-observation-pending"))
        self.assertEqual(cleanup["retained_owned_directories"],
                         [str(token.directory) for token in self.owner.tokens])
        self.assertNotIn("retained_owned_directories_status", cleanup)
        self._assert_pinned_then_released(failure)
        self.child_exit = 0
        self.assertEqual(failure.processes.pending(), [])
        self.assertTrue((failure.processes.entries[0].directory / "completed.json").is_file())

    def test_observation_timeout_of_a_registered_child_retains_the_root(self):
        failure = self._setup_failure(prepare=self._start_then_expire)
        self.assertRegex(str(failure.cause), "process observation expired; ownership retained")
        cleanup = failure.cleanup()
        self.assertEqual((cleanup["authorized"], cleanup["decision"]),
                         (False, "process-observation-pending"))
        self._assert_pinned_then_released(failure)

    def test_a_failed_assertion_still_settles_every_registered_child(self):
        class Failing(SetupProcessOwnershipTests):
            def test_body(self):
                self._setup_failure(prepare=self._start_then_expire)
                self.fail("assertion after the child was registered")

        inner = Failing("test_body")
        result = unittest.TestResult()
        inner.run(result)
        self.assertEqual((len(result.failures), len(result.errors)), (1, 0))
        (entry,) = (entry for registry in inner.registries for entry in registry.entries)
        self.assertTrue(entry.stdout.closed and entry.stderr.closed)
        self.assertEqual(entry.result["exit_code"], 0)

    def _assert_poll_failure_retains(self, fault):
        self.child_exit = 0
        self.write_faults["completed.json"] = fault
        failure = self._setup_failure()
        self.assertIs(failure.cause, fault)
        cleanup = failure.cleanup()
        self.assertEqual((cleanup["authorized"], cleanup["decision"], cleanup["error"]),
                         (False, "setup-process-closure-unproven", str(fault)))
        self._assert_pinned_then_released(failure)
        self.write_faults.clear()
        self.assertEqual(failure.processes.pending(), [])
        self.assertTrue((failure.processes.entries[0].directory / "completed.json").is_file())

    def test_completion_receipt_oserror_retains_the_root(self):
        self._assert_poll_failure_retains(OSError("completion receipt write failed"))

    def test_completion_receipt_runtimeerror_retains_the_root(self):
        self._assert_poll_failure_retains(RuntimeError("receipt mismatch"))

    def _assert_exact_root_removed(self, failure):
        cleanup = failure.cleanup()
        self.assertEqual((cleanup["authorized"], cleanup["decision"]),
                         (True, "setup-failure-exact-root-removed"))
        self.assertFalse(failure.root_token.directory.exists())
        self.assertTrue(all(token.consumed for token in self.owner.tokens))

    def test_no_registry_failure_removes_the_exact_root(self):
        failure = self._setup_failure(layout=mock.Mock(side_effect=RuntimeError("layout refused")))
        self.assertIsNone(failure.processes)
        self._assert_exact_root_removed(failure)

    def _assert_unproven_installation_retains(self, reason):
        bounded = BoundedCommandError(reason)
        wrapped = RuntimeError("installation failed")
        wrapped.__cause__ = bounded
        failure = self._setup_failure(layout=mock.Mock(side_effect=wrapped))
        self.assertIsNone(failure.processes)
        cleanup = failure.cleanup()
        self.assertEqual((cleanup["authorized"], cleanup["decision"]),
                         (False, "setup-installation-closure-unproven"))
        self.assertTrue(failure.root_token.directory.exists())
        self._assert_pinned_then_released(failure)

    def test_unproven_installation_child_closure_retains_the_setup_root_without_a_registry(self):
        self._assert_unproven_installation_retains(CommandFailure.TERMINATION_TIMEOUT)

    def test_failed_installation_child_termination_retains_the_setup_root_without_a_registry(self):
        self._assert_unproven_installation_retains(CommandFailure.TERMINATION_FAILED)

    def test_empty_registry_failure_removes_the_exact_root(self):
        failure = self._setup_failure(prepare=mock.Mock(side_effect=RuntimeError("layout refused")))
        self.assertEqual(failure.processes.entries, [])
        self._assert_exact_root_removed(failure)

    def test_fully_observed_nonzero_exit_removes_the_exact_root(self):
        self.child_exit = 1
        failure = self._setup_failure()
        self.assertRegex(str(failure.cause), "unexpected exit 1")
        self._assert_exact_root_removed(failure)

    def test_public_setup_failure_without_a_registry_keeps_exact_removal(self):
        owner = DirectoryOwner()
        root_token = owner.adopt_created(
            "setup root", Path(tempfile.mkdtemp(prefix=".dl-workspaces-", dir=self.home)).resolve(),
            self.home)
        cleanup = AcceptanceSetupFailure(RuntimeError("early"), owner, root_token).cleanup()
        self.assertTrue(cleanup["authorized"])
        self.assertFalse(root_token.directory.exists())

    def test_interruption_before_the_handoff_leaves_the_inventory_for_terminal_release(self):
        failure = self._setup_failure(layout=mock.Mock(side_effect=RuntimeError("layout refused")))
        with mock.patch.object(failure.owner, "transfer", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                failure.cleanup()
        self._assert_pinned_then_released(failure)

    def test_interruption_after_the_handoff_keeps_the_pins_reachable(self):
        failure = self._setup_failure(prepare=mock.Mock(side_effect=RuntimeError("layout refused")))
        with mock.patch.object(failure.processes, "pending", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                failure.cleanup()
        self._assert_pinned_then_released(failure)

    def _direct_failure(self, prefix):
        owner = DirectoryOwner()
        base = Path(tempfile.mkdtemp(prefix=prefix, dir=self.home)).resolve()
        root = owner.adopt_created("setup root", base, self.home)
        child = owner.create("setup child", base / "child", base)
        failure = AcceptanceSetupFailure(RuntimeError("early"), owner, root)
        self.addCleanup(failure.release_retained)
        return failure, root, child

    def test_pre_rollback_guard_failure_leaves_every_token_to_terminal_release(self):
        failure, root, child = self._direct_failure("not-private-")
        cleanup = failure.cleanup()
        self.assertEqual((cleanup["authorized"], cleanup["decision"]),
                         (False, "setup-cleanup-failed-evidence-retained"))
        self.assertFalse(root.consumed or child.consumed)
        failure.release_retained()
        self.assertTrue(root.consumed and child.consumed and child.directory.is_dir())

    def test_interrupted_removal_leaves_consumed_and_live_tokens_without_double_release(self):
        failure, root, child = self._direct_failure(".dl-interrupted-")
        real = directories_operations.remove_owned_directory
        calls = []

        def interrupt_second(token, *arguments):
            calls.append(token)
            if len(calls) == 2:
                raise KeyboardInterrupt()
            return real(token, *arguments)

        with mock.patch.object(directories_operations, "remove_owned_directory",
                               side_effect=interrupt_second):
            with self.assertRaises(KeyboardInterrupt):
                failure.cleanup()
        self.assertEqual((child.consumed, root.consumed), (True, False))
        failure.release_retained()
        self.assertTrue(root.consumed and root.directory.is_dir() and not child.directory.exists())

    def test_release_error_aggregates_consumes_every_ordinary_token_and_deletes_nothing(self):
        failure, root, child = self._direct_failure(".dl-replaced-")
        root.directory.rename(self.home / "moved")
        root.directory.mkdir()
        with self.assertRaisesRegex(RuntimeError, "directory ownership release failed: .*setup root"):
            failure.release_retained()
        self.assertTrue(root.consumed and child.consumed)
        self.assertEqual(failure.retained_directories(), ())
        failure.release_retained()
        self.assertTrue((self.home / "moved" / "child").is_dir())

    def test_release_failure_fails_every_unfailed_verdict_and_keeps_a_failed_reason(self):
        for verdict, reason in (("passed", "ok"), ("BLOCKED", "git-unavailable"),
                                ("failed", "original reason")):
            with self.subTest(verdict):
                failure, root, child = self._direct_failure(".dl-replaced-")
                moved = self.home / ("moved-" + verdict)
                child.directory.rename(moved)
                child.directory.mkdir()
                error = ("directory ownership release failed: setup child: "
                         "owned directory was replaced: " + str(child.directory))
                finalizer = ReceiptFinalizer(self.home / "receipt.json", [], {})
                result = application._release_retained(finalizer, failure, verdict, reason)
                self.assertEqual(result, ("failed", reason if verdict == "failed"
                                          else "retained-authority-release-failed: " + error))
                self.assertEqual(finalizer.details["retained_authority_release"],
                                 {"status": "failed", "error": error})
                self.assertTrue(root.consumed and child.consumed)
                self.assertEqual(failure.retained_directories(), ())
                self.assertTrue(moved.is_dir())

    def test_partial_setup_removal_failure_lists_the_unverified_creation_inventory(self):
        failure, root, child = self._direct_failure(".dl-partial-")
        real = directories_operations.remove_owned_directory

        def remove_child_then_refuse_root(token, *arguments):
            if token is root:
                raise RuntimeError("root removal refused")
            return real(token, *arguments)

        with mock.patch.object(directories_operations, "remove_owned_directory",
                               side_effect=remove_child_then_refuse_root):
            cleanup = failure.cleanup()
        self.assertEqual((cleanup["authorized"], cleanup["decision"]),
                         (False, "setup-cleanup-failed-evidence-retained"))
        self.assertEqual(cleanup["retained_owned_directories"],
                         [str(root.directory), str(child.directory)])
        self.assertEqual(cleanup["retained_owned_directories_status"], "unverified")
        self.assertTrue(root.directory.is_dir() and not child.directory.exists())

    def test_workspace_context_refusal_retains_all_pins_until_terminal_release(self):
        context = self._context()
        cleanup = context.cleanup("failed")
        self.assertEqual((cleanup["authorized"], cleanup["decision"]),
                         (False, "managed-lifetime-closure-unproven"))
        self.assertRegex(cleanup["lifetime_error"],
                         "^managed lifetime closure remained incomplete .*managed lifetime root record")
        self.assertAlmostEqual(self.clock.now, MANAGED_LIFETIME_CLOSURE_TIMEOUT)
        self.assertGreater(len(self.clock.sleeps), 1)
        self.assertEqual(cleanup["retained_owned_directories"],
                         [str(token.directory) for token in self.owner.tokens])
        self.assertNotIn("retained_owned_directories_status", cleanup)
        self._assert_pinned_then_released(context)

    def _daemon_context(self, events, *, private_config=True, lifecycle_locks=True):
        """A context with one task, optionally a private daemon config, and a gate and cohort.

        An admitted root carries the lifecycle election and bootstrap lock files
        that first admission provisions; shutdown takes them as its transition lease.
        """
        context = self._context()
        context.add_task(TASK_ID)
        context.attempted_task_ids.append(TASK_ID)
        if private_config:
            context.private_config.parent.mkdir(mode=0o700)
            context.private_config.write_text("fixture\n", encoding="utf-8")
            if lifecycle_locks:
                for name in ("transition.lock", "bootstrap.lock"):
                    (context.private_config.parent / name).touch(mode=0o600)
        gate = mock.create_autospec(private_cli_leases.LockedReleaseGate, instance=True)
        gate.release.side_effect = lambda: events.append("gate-released")
        cohort = mock.create_autospec(private_cli_leases.LiveFIFOLeaseCohort, instance=True)
        cohort.close.side_effect = lambda: events.append("cohort-closed")
        context.gates.append(gate)
        context.cohorts.append(cohort)
        return context, cohort

    def _finalize_failed_with_closure(self, context, events, closure_results):
        """Run the real finalizer with only closure validation and native shutdown evidence faked."""
        results = iter(closure_results)
        test = self

        def validate(state, task_ids):
            events.append("closure-checked")
            test.assertEqual((state, task_ids), (context.state, (TASK_ID,)))
            for token in test.owner.tokens:
                token.assert_live()
            result = next(results)
            if isinstance(result, BaseException):
                raise result
            return result

        def shutdown_command(*arguments, **options):
            events.append("shutdown")
            for token in test.owner.tokens:
                token.assert_live()

        endpoint = context.private_config.parent / "run" / "pueue.sock"
        native = mock.Mock()
        native.evidence.return_value = (4242, endpoint, (1, 2))
        native.gone.side_effect = [False, False, True]
        native.closed.return_value = True
        native.command.side_effect = shutdown_command
        output = self.home / "receipt.json"
        with (
            mock.patch.object(private_cli_supervisor, "validate_managed_lifetime_closure",
                              side_effect=validate),
            mock.patch.object(private_cli_supervisor, "private_shutdown_evidence", native.evidence),
            mock.patch.object(private_cli_supervisor, "private_daemon_process_gone", native.gone),
            mock.patch.object(private_cli_supervisor, "private_shutdown_endpoint_closed", native.closed),
            mock.patch.object(private_cli_supervisor, "pueue_command", native.command),
            mock.patch.object(private_cli_supervisor.commands, "run_json", return_value={
                "root_id": ROOT_ID, "supervisor_owner": {"action": "stop-owner", "state": "not_initialized"}}),
        ):
            result = application._finalize_run(
                ReceiptFinalizer(output, [], {}), context, None, "failed", "original reason")
        self.assertEqual(result, ("failed", "original reason"))
        receipt = json.loads(output.read_text())
        self.assertEqual((receipt["verdict"], receipt["reason"]), ("failed", "original reason"))
        self.assertEqual(receipt["cleanup"]["retained_owned_directories"],
                         [str(token.directory) for token in self.owner.tokens])
        self.assertEqual(receipt["details"]["retained_authority_release"], {"status": "released"})
        self.assertTrue(all(token.consumed and token.directory.is_dir() for token in self.owner.tokens))
        return receipt["cleanup"], native

    def test_worker_closing_after_gate_release_still_shuts_down_the_private_daemon(self):
        events = []
        context, cohort = self._daemon_context(events)
        closed = {"root_id": ROOT_ID, "tasks": {}, "inspections": {}}
        cleanup, native = self._finalize_failed_with_closure(
            context, events, [AcceptanceFailure("worker still closing"), closed, closed, closed, closed])
        # Closure is awaited, authorized before the lease, rechecked read-only under it, then shut down.
        self.assertEqual(events, ["gate-released", "cohort-closed", "closure-checked",
                                  "closure-checked", "closure-checked", "closure-checked", "closure-checked", "shutdown"])
        cohort.remove_closed_fifos.assert_not_called()
        self.assertEqual((context.gates, context.cohorts), ([], []))
        self.assertEqual(len(self.clock.sleeps), 1)
        native.evidence.assert_called_once_with(context.private_config)
        native.command.assert_called_once()
        self.assertEqual(native.command.call_args.args,
                         (context.pueue, context.private_config, context.environment, ["shutdown"]))
        self.assertIs(native.command.call_args.kwargs["capture_root"], context.root_token)
        self.assertTrue(all(call.kwargs["capture_root"] is context.root_token
                            for call in native.gone.call_args_list))
        self.assertEqual(
            (cleanup["lifetime_closed"], cleanup["daemon_closed"], cleanup["authorized"],
             cleanup["decision"]),
            (True, True, False, "failure-evidence-retained"))
        self.assertNotIn("lifetime_error", cleanup)
        self.assertNotIn("daemon_error", cleanup)

    def test_missing_lifecycle_locks_refuse_shutdown_and_retain_the_root(self):
        events = []
        context, _cohort = self._daemon_context(events, lifecycle_locks=False)
        supervisor_entries = sorted(path.name for path in context.private_config.parent.iterdir())
        closed = {"root_id": ROOT_ID, "tasks": {}, "inspections": {}}
        cleanup, native = self._finalize_failed_with_closure(
            context, events, [closed, closed, closed])
        self.assertEqual(events, ["gate-released", "cohort-closed", "closure-checked", "closure-checked", "closure-checked"])
        native.command.assert_not_called()
        self.assertFalse((context.private_config.parent / "transition.lock").exists())
        # The refused root is left exactly as found, without even the optional election lock.
        self.assertNotIn("queue-maintenance.lock", supervisor_entries)
        self.assertEqual(sorted(path.name for path in context.private_config.parent.iterdir()), supervisor_entries)
        self.assertEqual(
            (cleanup["lifetime_closed"], cleanup["daemon_closed"], cleanup["authorized"]),
            (True, False, False))
        self.assertIn("transition.lock", cleanup["daemon_error"])

    def test_state_renamed_after_authorized_shutdown_is_still_rechecked_and_the_root_retained(self):
        events = []
        context, _cohort = self._daemon_context(events)
        moved = context.root_token.directory / "moved-state"
        # Runs before the fixture releases its pins, which require the state at its owned path.
        self.addCleanup(lambda: moved.rename(context.state) if moved.is_dir() else None)
        canonical_validate = private_cli_supervisor.validate_managed_lifetime_closure
        closed = {"root_id": ROOT_ID, "tasks": {}, "inspections": {}}

        def validate(state, task_ids):
            events.append("closure-checked")
            # Only a present state is faked closed; a missing one meets the canonical refusal.
            return closed if state.exists() else canonical_validate(state, task_ids)

        def shutdown_then_rename(*_arguments, **_options):
            events.append("shutdown")
            context.state.rename(moved)

        native = mock.Mock()
        native.gone.side_effect = [False, False, True]
        with (
            mock.patch.object(private_cli_supervisor, "validate_managed_lifetime_closure", side_effect=validate),
            mock.patch.object(private_cli_supervisor, "private_shutdown_evidence",
                              return_value=(4242, context.private_config.parent / "run" / "pueue.sock", (1, 2))),
            mock.patch.object(private_cli_supervisor, "private_daemon_process_gone", native.gone),
            mock.patch.object(private_cli_supervisor, "private_shutdown_endpoint_closed", return_value=True),
            mock.patch.object(private_cli_supervisor, "pueue_command", side_effect=shutdown_then_rename),
            mock.patch.object(private_cli_supervisor.commands, "run_json", return_value={
                "root_id": ROOT_ID, "supervisor_owner": {"action": "stop-owner", "state": "not_initialized"}}),
        ):
            cleanup = context.cleanup("passed")
        self.assertEqual((cleanup["daemon_closed"], cleanup["authorized"], cleanup["decision"]),
                         (True, False, "cleanup-failed-evidence-retained"))
        self.assertEqual(events, ["gate-released", "cohort-closed", "closure-checked", "closure-checked",
                                  "closure-checked", "closure-checked", "shutdown", "closure-checked"])
        self.assertTrue(context.root_token.directory.is_dir() and moved.is_dir())

    def test_closure_without_a_private_config_is_awaited_and_no_shutdown_is_called(self):
        events = []
        context, _cohort = self._daemon_context(events, private_config=False)
        closed = {"root_id": ROOT_ID, "tasks": {}, "inspections": {}}
        cleanup, native = self._finalize_failed_with_closure(
            context, events, [AcceptanceFailure("worker still closing"), closed, closed])
        self.assertEqual(events, ["gate-released", "cohort-closed", "closure-checked",
                                  "closure-checked", "closure-checked"])
        self.assertEqual(len(self.clock.sleeps), 1)
        native.evidence.assert_not_called()
        native.command.assert_not_called()
        self.assertEqual((cleanup["lifetime_closed"], cleanup["authorized"], cleanup["decision"]),
                         (True, False, "failure-evidence-retained"))
        self.assertNotIn("lifetime_error", cleanup)

    def test_closure_incomplete_at_the_deadline_skips_shutdown_and_retains_evidence(self):
        events = []
        context, _cohort = self._daemon_context(events)
        pending = AcceptanceFailure("worker still closing")
        cleanup, native = self._finalize_failed_with_closure(context, events, [pending] * 1000)
        self.assertEqual(MANAGED_LIFETIME_CLOSURE_TIMEOUT, 45)
        self.assertAlmostEqual(self.clock.now, 45)
        self.assertEqual(events.count("closure-checked"), len(self.clock.sleeps) + 1)
        self.assertEqual(events[:2], ["gate-released", "cohort-closed"])
        self.assertNotIn("shutdown", events)
        native.evidence.assert_not_called()
        self.assertEqual(
            (cleanup["lifetime_closed"], cleanup["daemon_closed"], cleanup["authorized"],
             cleanup["decision"]),
            (False, False, False, "managed-lifetime-closure-unproven"))
        self.assertEqual(cleanup["lifetime_error"],
                         "managed lifetime closure remained incomplete before supervisor "
                         "shutdown: worker still closing")

    def test_non_retryable_closure_error_is_reported_without_waiting(self):
        events = []
        context, _cohort = self._daemon_context(events)
        cleanup, native = self._finalize_failed_with_closure(
            context, events, [RuntimeError("state root unreadable")])
        self.assertEqual(events, ["gate-released", "cohort-closed", "closure-checked"])
        self.assertEqual(self.clock.sleeps, [])
        native.evidence.assert_not_called()
        self.assertEqual((cleanup["lifetime_closed"], cleanup["decision"], cleanup["lifetime_error"]),
                         (False, "managed-lifetime-closure-unproven", "state root unreadable"))

    def test_exceptional_cleanup_after_the_handoff_does_not_transfer_twice_or_skip_the_receipt(self):
        context = self._context()

        def transfer_then_fail(_verdict):
            context.retained_directories()
            raise RuntimeError("closure exploded")

        output = self.home / "receipt.json"
        finalizer = ReceiptFinalizer(output, [], {})
        with mock.patch.object(context, "cleanup", side_effect=transfer_then_fail):
            result = application._finalize_run(finalizer, context, None, "passed", "ok")
        self.assertEqual(result, ("failed", "cleanup-or-closure-error: RuntimeError: closure exploded"))
        receipt = json.loads(output.read_text())
        self.assertEqual(receipt["cleanup"]["retained_owned_directories"],
                         [str(token.directory) for token in self.owner.tokens])
        self.assertEqual(receipt["cleanup"]["retained_owned_directories_status"], "unverified")
        self.assertEqual(receipt["details"]["retained_authority_release"], {"status": "released"})
        self.assertTrue(all(token.consumed for token in self.owner.tokens))

    def test_exceptional_cleanup_after_partial_removal_lists_the_full_unverified_inventory(self):
        context = self._context()
        state = next(token for token in self.owner.tokens if token.directory.name == "state")

        def remove_state_then_fail(_verdict):
            context.retained_directories()
            directories_operations.remove_owned_directory(state)
            raise RuntimeError("closure exploded")

        output = self.home / "receipt.json"
        with mock.patch.object(context, "cleanup", side_effect=remove_state_then_fail):
            application._finalize_run(ReceiptFinalizer(output, [], {}), context, None, "passed", "ok")
        cleanup = json.loads(output.read_text())["cleanup"]
        self.assertEqual(cleanup["retained_owned_directories"],
                         [str(token.directory) for token in self.owner.tokens])
        self.assertEqual(cleanup["retained_owned_directories_status"], "unverified")
        self.assertTrue(context.root_token.directory.is_dir() and not state.directory.exists())

    def _main(self, *, run=None, **options):
        output = Path(tempfile.mkdtemp(dir=self.home)) / "receipt.json"
        arguments = ["--output", str(output)]
        passed = ("passed", "managed workspace fixture acceptance passed")
        with (
            mock.patch.object(application, "_create_layout", side_effect=options.get("layout") or self._layout),
            mock.patch.object(application, "_prepare_source",
                              side_effect=options.get("prepare") or self._run_git),
            mock.patch.object(application, "_run", side_effect=run or (lambda *_arguments: passed)),
            mock.patch.object(application.sys.stdout, "write"),
        ):
            code = application.main(arguments)
        return code, json.loads(output.read_text())

    def test_main_fails_with_the_original_error_and_retains_the_pending_root(self):
        self.write_faults["started.json"] = OSError("started receipt write failed")
        code, receipt = self._main()
        self.assertEqual((code, receipt["verdict"]), (1, "failed"))
        self.assertEqual(receipt["reason"], "OSError: started receipt write failed")
        cleanup = receipt["cleanup"]
        self.assertEqual((cleanup["authorized"], cleanup["decision"]),
                         (False, "process-observation-pending"))
        self.assertTrue(all(Path(path).is_dir() for path in cleanup["retained_owned_directories"]))
        self.assertEqual(receipt["details"]["retained_authority_release"], {"status": "released"})
        self.assertTrue(all(token.consumed for token in self.owner.tokens))

    def test_main_refused_cleanup_keeps_its_reason_beside_a_failed_release(self):
        code, receipt = self._main(run=self._passing_run(True), prepare=lambda _context: None)
        self.assertEqual((code, receipt["verdict"], receipt["reason"]),
                         (1, "failed", "cleanup-refused:managed-lifetime-closure-unproven"))
        release = receipt["details"]["retained_authority_release"]
        self.assertEqual(release["status"], "failed")
        self.assertRegex(release["error"], "directory ownership release failed: "
                         "managed workspace state: owned directory was replaced")
        self.assertTrue(all(token.consumed for token in self.owner.tokens))
        self.assertTrue((self.owner.tokens[0].directory / "moved-state").is_dir())

    def test_main_removes_a_safely_failed_setup_and_keeps_a_neutral_block(self):
        code, receipt = self._main(prepare=mock.Mock(side_effect=RuntimeError("layout refused")))
        self.assertEqual((code, receipt["verdict"], receipt["reason"]),
                         (1, "failed", "RuntimeError: layout refused"))
        self.assertTrue(receipt["cleanup"]["authorized"])
        self.owner = RecordingOwner()
        blocked = application.AcceptanceBlocked("git-unavailable")
        with (mock.patch.object(application, "_git_executable", side_effect=blocked),
              mock.patch.object(application, "DirectoryOwner", return_value=self.owner)):
            code, receipt = self._main()
        self.assertEqual((code, receipt["verdict"]), (3, "BLOCKED"))
        self.assertTrue(receipt["cleanup"]["authorized"])

    @staticmethod
    def _passing_run(replace_state):
        def run(_arguments, _finalizer, context):
            if replace_state:
                context.state.rename(context.base / "moved-state")
                context.state.mkdir()
            return "passed", "managed workspace fixture acceptance passed"

        return run

    def _main_with_receipt_fault(self, call, fault, *, replace_state=False):
        real, calls = ReceiptFinalizer.finalize, []

        def finalize(finalizer, *arguments):
            calls.append(arguments)
            if len(calls) == call:
                raise fault
            return real(finalizer, *arguments)

        with mock.patch.object(ReceiptFinalizer, "finalize", autospec=True, side_effect=finalize):
            with self.assertRaises(type(fault)) as raised:
                self._main(run=self._passing_run(replace_state), prepare=lambda _context: None)
        self.assertIs(raised.exception, fault)
        self.assertTrue(all(token.consumed for token in self.owner.tokens))
        return raised.exception

    def test_preliminary_receipt_write_failure_still_disposes_the_pins(self):
        error = self._main_with_receipt_fault(1, OSError("preliminary receipt write failed"))
        self.assertFalse(hasattr(error, "__notes__"))

    def test_final_receipt_write_failure_still_disposes_the_pins(self):
        error = self._main_with_receipt_fault(2, OSError("final receipt write failed"))
        self.assertFalse(hasattr(error, "__notes__"))

    def test_interrupted_receipt_write_is_not_swallowed_and_pins_are_disposed(self):
        self._main_with_receipt_fault(1, KeyboardInterrupt())

    def test_receipt_and_release_failures_are_both_preserved(self):
        for call in (1, 2):
            with self.subTest(call):
                self.owner = RecordingOwner()
                with mock.patch.object(application, "DirectoryOwner", return_value=self.owner):
                    error = self._main_with_receipt_fault(
                        call, OSError("receipt write failed"), replace_state=True)
                self.assertRegex("\n".join(error.__notes__), "retained directory authority release failed")


if __name__ == "__main__":
    unittest.main()
