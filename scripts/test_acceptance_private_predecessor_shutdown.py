"""A captured predecessor supervisor is shut down once through its canonical owner."""

import json
import shutil
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import acceptance_private_cli as private_cli
import acceptance_provider_common as provider_common
from scripts.delegate_acceptance.private_cli import finalization as finalization_impl
from scripts.delegate_acceptance.private_cli import leases as leases_impl
from scripts.delegate_acceptance.private_cli import supervisor as supervisor_impl
from scripts.delegate_acceptance.private_cli import upgrade as upgrade_impl
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.processes.observation import wait_until
from test_acceptance_private_shutdown import ManagedLifetimeFixture
from test_acceptance_private_shutdown import TASK_ID
from test_acceptance_private_shutdown import write_record


class FinalizerArguments:
    scenario = "supervisor-upgrade"

    def __init__(self, pueue, output):
        self.pueue = pueue
        self.output = output


class RecordingReceipt:
    def __init__(self):
        self.written = None

    def phase(self, name):
        self.phase_name = name

    def write(self, result, cleanup, error):
        self.written = (result, cleanup, error)


class CloseFaultHandle:
    """Delegate to the real pinned backup handle; fail before closing it."""

    def __init__(self, handle):
        self.handle = handle
        self.error = OSError("injected backup close fault")

    def fileno(self):
        return self.handle.fileno()

    @property
    def closed(self):
        return self.handle.closed

    def close(self):
        raise self.error


class PredecessorShutdownTests(unittest.TestCase):
    """Stage a real pre-retarget capture and finalize through the canonical owners."""

    def setUp(self):
        self.directory = Path(tempfile.mkdtemp(prefix="dlp-", dir="/tmp")).resolve()
        self.addCleanup(shutil.rmtree, self.directory)
        self.captured = []
        self.addCleanup(self.assert_captured_handles_closed)
        self.capture_owner = private_cli.DirectoryOwner()
        self.capture_root = self.capture_owner.create(
            "private control capture root", self.directory / "c", self.directory,
        )
        self.addCleanup(self.capture_owner.rollback)
        self.base = self.capture_root.directory
        self.backup = self.base / "upgrade-legacy-control.yml"
        self.addCleanup(self.backup.unlink, missing_ok=True)
        self.fixture = ManagedLifetimeFixture(self.directory)
        self.fixture.write_task()
        self.state = self.fixture.state
        supervisor = self.state / ".supervisor"
        runtime = supervisor / "run"
        supervisor.mkdir(mode=0o700)
        runtime.mkdir(mode=0o700)
        for name in ("transition.lock", "bootstrap.lock"):
            (supervisor / name).touch(mode=0o600)
        self.endpoint = runtime / "pueue.sock"
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(self.listener.close)
        self.listener.bind(str(self.endpoint))
        self.listener.listen()
        self.daemon = subprocess.Popen(
            [sys.executable, "-c", "import sys; sys.stdin.buffer.read(1)"], stdin=subprocess.PIPE)
        self.addCleanup(self.stop_daemon)
        self.pid_path = runtime / "pueue.pid"
        self.pid_path.write_text(f"{self.daemon.pid}\n", encoding="utf-8")
        self.config = supervisor / "pueue.yml"
        self.config.write_text(
            "shared:\n"
            f"  runtime_directory: {json.dumps(str(runtime))}\n"
            f"  unix_socket_path: {json.dumps(str(self.endpoint))}\n"
            f"  pid_path: {json.dumps(str(self.pid_path))}\n",
            encoding="utf-8",
        )
        write_record(supervisor / "supervisor.identity.json", {"binding": {"endpoint": "unix:" + str(self.endpoint)}})
        self.client = self.directory / "pueue"
        self.client.write_text("fixture client", encoding="utf-8")
        self.lifetime = FixtureLifetime(
            base=self.base, base_token=self.capture_root, env={}, private_config=self.config,
            managed_state_roots=[(self.state, [TASK_ID])], attempted_task_ids=[TASK_ID],
        )
        self.calls = []

    @staticmethod
    def stop_process(process):
        if process.poll() is None:
            process.stdin.close()
            process.wait(timeout=5)

    def stop_daemon(self):
        self.stop_process(self.daemon)

    def assert_captured_handles_closed(self):
        for target in self.captured:
            self.assertTrue(target.backup_handle.closed)

    def pueue_command(self, client, config, env, args, timeout=15, phase=None, *, capture_root):
        self.calls.append((Path(config), list(args)))
        if args == ["shutdown"]:
            self.stop_daemon()
            self.listener.close()
        return ""

    def stage(self):
        owner = private_cli.DirectoryOwner()
        self.addCleanup(owner.rollback)
        capture_target = upgrade_impl.capture_predecessor_shutdown_target

        def capture(*args, **kwargs):
            # The test owns every acquired backup handle from the moment it exists.
            target = capture_target(*args, **kwargs)
            self.captured.append(target)
            self.addCleanup(target.backup_handle.close)
            return target

        with patch.object(upgrade_impl, "install_fixture"), \
                patch.object(upgrade_impl, "capture_predecessor_shutdown_target", side_effect=capture):
            staged = upgrade_impl.stage_pending_package(
                attempted_task_ids=[TASK_ID], base=self.base, config=self.config, env={}, package_owner=owner,
                real_client=self.client, real_daemon=self.directory / "pueued", state=self.state,
                supervisor_cleanups=self.lifetime.supervisor_cleanups,
            )
        return owner, staged

    def finalize(self, failure, pueue_command):
        receipt = RecordingReceipt()
        args = FinalizerArguments(self.client, self.base / "receipt.json")
        with patch.object(supervisor_impl, "pueue_command", side_effect=pueue_command):
            finalization_impl.finalize_fixture(args, failure, self.lifetime, receipt, False)
        return receipt.written

    def captured_target(self):
        self.stage()
        return self.lifetime.supervisor_cleanups[-1]["predecessor"]

    def assert_refused_before_shutdown(self, target):
        with patch.object(supervisor_impl, "pueue_command") as command:
            with self.assertRaises((RuntimeError, provider_common.AcceptanceFailure)):
                supervisor_impl.shutdown_predecessor_supervisor(
                    self.client, target, {}, task_ids=(TASK_ID,), capture_root=self.capture_root)
        command.assert_not_called()
        self.assertIsNone(self.daemon.poll())

    def publish_successor(self):
        """Model migrate progress: the captured generation exits and a successor owns the root."""
        self.stop_daemon()
        self.listener.close()
        self.endpoint.unlink()
        successor_endpoint = self.endpoint.with_name("upgrade.sock")
        self.successor_listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(self.successor_listener.close)
        self.successor_listener.bind(str(successor_endpoint))
        self.successor_listener.listen()
        self.successor = subprocess.Popen(
            [sys.executable, "-c", "import sys; sys.stdin.buffer.read(1)"], stdin=subprocess.PIPE)
        self.addCleanup(self.stop_process, self.successor)
        self.pid_path.write_text(f"{self.successor.pid}\n", encoding="utf-8")
        config = self.config.read_text(encoding="utf-8")
        self.config.write_text(
            config.replace(json.dumps(str(self.endpoint)), json.dumps(str(successor_endpoint))), encoding="utf-8")

    def successor_command(self, client, config, env, args, timeout=15, phase=None, *, capture_root):
        self.calls.append((Path(config), list(args)))
        if args == ["shutdown"]:
            self.stop_process(self.successor)
            self.successor_listener.close()
        return ""

    def lifecycle_locks_contended(self):
        """Probe both production lifecycle locks from independent descriptors."""
        contended = []
        for name in ("transition.lock", "bootstrap.lock"):
            descriptor = private_cli.os.open(
                self.state / ".supervisor" / name, private_cli.os.O_RDWR | private_cli.os.O_NOFOLLOW)
            try:
                private_cli.fcntl.flock(descriptor, private_cli.fcntl.LOCK_EX | private_cli.fcntl.LOCK_NB)
                contended.append(False)
            except BlockingIOError:
                contended.append(True)
            finally:
                private_cli.os.close(descriptor)
        return tuple(contended)

    def test_failed_retarget_finalizer_shuts_down_captured_predecessor_once(self):
        owner, staged = self.stage()
        failure = RuntimeError("supervisor migrate returned unavailable")
        with patch.object(upgrade_impl, "pueue_command", return_value="7\n"), \
                patch.object(upgrade_impl, "run_json", side_effect=failure):
            with self.assertRaises(RuntimeError) as raised:
                upgrade_impl.interrupt_pending_migration(
                    active=staged[1], capture_root=self.capture_root, client=self.client, config=self.config,
                    delegate=self.directory / "delegate", env={}, legacy_config=staged[2],
                    original_config=staged[3], package_owner=owner, pending_libexec=staged[0],
                    receipts=self.base, state=self.state,
                )
        self.assertIs(raised.exception, failure)
        self.assertIn("upgrade.sock", self.config.read_text(encoding="utf-8"))
        result, cleanup, error = self.finalize(failure, self.pueue_command)
        self.assertEqual(self.calls, [(self.backup, ["shutdown"])])
        self.assertEqual((result, error), ("FAIL", failure))
        self.assertEqual(cleanup["status"], "retained")
        self.assertEqual(cleanup["pending_supervisor_count"], 0)
        self.assertTrue(self.base.is_dir())
        self.assertIsNotNone(self.daemon.poll())

    def test_failed_predecessor_shutdown_keeps_entry_and_suppresses_primary(self):
        self.stage()
        pending = self.lifetime.supervisor_cleanups[-1]
        refused = RuntimeError("fixture shutdown refused")

        def refuse(*args, **kwargs):
            self.calls.append((Path(args[1]), list(args[3])))
            raise refused

        result, cleanup, error = self.finalize(RuntimeError("original"), refuse)
        self.assertEqual(self.calls, [(self.backup, ["shutdown"])])
        self.assertEqual(len(self.lifetime.supervisor_cleanups), 1)
        self.assertIs(self.lifetime.supervisor_cleanups[0], pending)
        self.assertTrue(pending["predecessor"].backup_handle.closed)
        self.assertEqual((result, str(error)), ("FAIL", "original"))
        self.assertEqual((cleanup["status"], cleanup["pending_supervisor_count"]), ("failed", 1))

    def test_verified_successor_releases_target_and_restores_primary_cleanup(self):
        self.stage()
        pending = self.lifetime.supervisor_cleanups[-1]
        self.stop_daemon()
        self.listener.close()
        upgrade_impl.await_pending_predecessor(self.capture_root, pending["predecessor"])
        finalization_impl.release_pending_predecessor(pending, self.lifetime.supervisor_cleanups)
        self.assertEqual(self.lifetime.supervisor_cleanups, [])
        self.assertTrue(pending["predecessor"].backup_handle.closed)
        with patch.object(finalization_impl, "shutdown_private_supervisor") as primary:
            result, cleanup, _ = self.finalize(RuntimeError("original"), self.pueue_command)
        primary.assert_called_once()
        self.assertEqual(primary.call_args.args[1], self.config)
        self.assertEqual((result, cleanup["pending_supervisor_count"]), ("FAIL", 0))

    def test_successful_release_close_fault_keeps_ledger_entry(self):
        self.stage()
        pending = self.lifetime.supervisor_cleanups[-1]
        self.stop_daemon()
        self.listener.close()
        upgrade_impl.await_pending_predecessor(self.capture_root, pending["predecessor"])
        real_handle, fault = self.inject_close_fault(pending)
        with self.assertRaises(OSError) as raised:
            finalization_impl.release_pending_predecessor(pending, self.lifetime.supervisor_cleanups)
        self.assertIs(raised.exception, fault)
        self.assertEqual(len(self.lifetime.supervisor_cleanups), 1)
        self.assertIs(self.lifetime.supervisor_cleanups[0], pending)
        self.assertFalse(real_handle.closed)

    def test_backup_tampering_refuses_before_shutdown(self):
        target = self.captured_target()
        private_cli.os.pwrite(target.backup_handle.fileno(), b"#", 0)
        self.assert_refused_before_shutdown(target)

    def test_backup_symlink_refuses_before_shutdown(self):
        target = self.captured_target()
        copy = self.base / "copy.yml"
        self.addCleanup(copy.unlink, missing_ok=True)
        copy.write_bytes(self.backup.read_bytes())
        copy.chmod(0o600)
        self.backup.unlink()
        self.backup.symlink_to(copy)
        self.assert_refused_before_shutdown(target)

    def test_backup_replacement_with_identical_bytes_refuses_before_shutdown(self):
        target = self.captured_target()
        content = self.backup.read_bytes()
        self.backup.unlink()
        self.backup.write_bytes(content)
        self.backup.chmod(0o600)
        self.assert_refused_before_shutdown(target)

    def test_backup_ownership_tampering_refuses_before_shutdown(self):
        target = self.captured_target()
        self.backup.chmod(0o644)
        self.assert_refused_before_shutdown(target)
        self.backup.chmod(0o600)
        alias = self.base / "alias.yml"
        self.addCleanup(alias.unlink, missing_ok=True)
        private_cli.os.link(self.backup, alias)
        self.assert_refused_before_shutdown(target)

    def test_socket_rebind_while_captured_listener_is_open_refuses_before_shutdown(self):
        # The captured listener stays open, as a live daemon's would. An open
        # listener keeps its socket inode allocated, so the rebind cannot reuse
        # the captured identity. An earlier fixture closed the listener first;
        # the freed inode was then reused, which a live daemon cannot cause.
        target = self.captured_target()
        self.endpoint.unlink()
        replacement = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(replacement.close)
        replacement.bind(str(self.endpoint))
        self.assert_refused_before_shutdown(target)

    def test_socket_rename_over_refuses_before_shutdown(self):
        target = self.captured_target()
        staged = self.endpoint.with_name("replacement.sock")
        replacement = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(replacement.close)
        replacement.bind(str(staged))
        staged.replace(self.endpoint)
        self.assert_refused_before_shutdown(target)

    def test_successor_generation_refuses_before_shutdown(self):
        target = self.captured_target()
        self.pid_path.write_text(f"{private_cli.os.getppid()}\n", encoding="utf-8")
        self.assert_refused_before_shutdown(target)

    def test_incomplete_managed_closure_refuses_before_shutdown(self):
        target = self.captured_target()
        self.fixture.write_task(include_wait=False)
        self.assert_refused_before_shutdown(target)


    def test_lease_is_held_for_fresh_validation_and_captured_command(self):
        target = self.captured_target()
        observed = {}
        read_pid = supervisor_impl.private_supervisor_pid

        def validate_pid(path):
            observed["validation"] = self.lifecycle_locks_contended()
            return read_pid(path)

        def command(*args, **kwargs):
            observed["command"] = self.lifecycle_locks_contended()
            return self.pueue_command(*args, **kwargs)

        with patch.object(supervisor_impl, "private_supervisor_pid", side_effect=validate_pid), \
                patch.object(supervisor_impl, "pueue_command", side_effect=command):
            supervisor_impl.shutdown_predecessor_supervisor(
                self.client, target, {}, task_ids=(TASK_ID,), capture_root=self.capture_root)
        self.assertEqual(observed, {"validation": (True, True), "command": (True, True)})
        self.assertEqual(self.calls, [(self.backup, ["shutdown"])])
        self.assertEqual(self.lifecycle_locks_contended(), (False, False))

    def test_held_bootstrap_lease_refuses_before_any_command(self):
        self.stage()
        pending = self.lifetime.supervisor_cleanups[-1]
        holder = private_cli.os.open(
            self.state / ".supervisor" / "bootstrap.lock", private_cli.os.O_RDWR | private_cli.os.O_NOFOLLOW)
        self.addCleanup(private_cli.os.close, holder)
        private_cli.fcntl.flock(holder, private_cli.fcntl.LOCK_EX | private_cli.fcntl.LOCK_NB)
        failure = RuntimeError("original")

        def expired(predicate, timeout, description):
            return wait_until(predicate, 0, description)

        with patch.object(leases_impl, "wait_until", side_effect=expired), \
                patch.object(finalization_impl, "shutdown_private_supervisor") as primary:
            result, cleanup, error = self.finalize(failure, self.pueue_command)
        self.assertEqual(self.calls, [])
        primary.assert_not_called()
        self.assertEqual(self.lifetime.supervisor_cleanups, [pending])
        self.assertEqual((result, error), ("FAIL", failure))
        self.assertEqual((cleanup["status"], cleanup["pending_supervisor_count"]), ("failed", 1))
        self.assertTrue(self.base.is_dir())
        self.assertIsNone(self.daemon.poll())

    def test_backup_replaced_during_command_fails_post_check_and_retains(self):
        self.stage()
        pending = self.lifetime.supervisor_cleanups[-1]

        def replace_during_command(*args, **kwargs):
            result = self.pueue_command(*args, **kwargs)
            content = self.backup.read_bytes()
            self.backup.unlink()
            self.backup.write_bytes(content)
            self.backup.chmod(0o600)
            return result

        failure = RuntimeError("original")
        with patch.object(finalization_impl, "shutdown_private_supervisor") as primary:
            result, cleanup, error = self.finalize(failure, replace_during_command)
        self.assertEqual(self.calls, [(self.backup, ["shutdown"])])
        primary.assert_not_called()
        self.assertEqual(self.lifetime.supervisor_cleanups, [pending])
        self.assertEqual((result, error), ("FAIL", failure))
        self.assertEqual((cleanup["status"], cleanup["pending_supervisor_count"]), ("failed", 1))
        self.assertTrue(self.base.is_dir())


    def test_late_failure_after_migrate_progress_hands_closed_predecessor_to_primary(self):
        self.stage()
        self.publish_successor()
        failure = RuntimeError("late failure after migrate progress")
        result, cleanup, error = self.finalize(failure, self.successor_command)
        self.assertEqual(self.calls, [(self.config, ["shutdown"])])
        self.assertIsNotNone(self.daemon.poll())
        self.assertIsNotNone(self.successor.poll())
        self.assertEqual((result, error), ("FAIL", failure))
        self.assertEqual((cleanup["status"], cleanup["pending_supervisor_count"]), ("retained", 0))
        self.assertTrue(self.base.is_dir())

    def test_predecessor_exited_with_open_endpoint_refuses_and_keeps_primary_skipped(self):
        self.stage()
        pending = self.lifetime.supervisor_cleanups[-1]
        self.stop_daemon()
        failure = RuntimeError("original")
        with patch.object(finalization_impl, "shutdown_private_supervisor") as primary:
            result, cleanup, error = self.finalize(failure, self.pueue_command)
        self.assertEqual(self.calls, [])
        primary.assert_not_called()
        self.assertEqual(self.lifetime.supervisor_cleanups, [pending])
        self.assertEqual((result, error), ("FAIL", failure))
        self.assertEqual((cleanup["status"], cleanup["pending_supervisor_count"]), ("failed", 1))

    def test_closed_predecessor_with_tampered_backup_is_not_a_clean_handoff(self):
        self.stage()
        pending = self.lifetime.supervisor_cleanups[-1]
        self.publish_successor()
        private_cli.os.pwrite(pending["predecessor"].backup_handle.fileno(), b"#", 0)
        failure = RuntimeError("original")
        with patch.object(finalization_impl, "shutdown_private_supervisor") as primary:
            result, cleanup, error = self.finalize(failure, self.successor_command)
        self.assertEqual(self.calls, [])
        primary.assert_not_called()
        self.assertEqual(self.lifetime.supervisor_cleanups, [pending])
        self.assertEqual((result, error), ("FAIL", failure))
        self.assertEqual((cleanup["status"], cleanup["pending_supervisor_count"]), ("failed", 1))
        self.assertIsNone(self.successor.poll())

    def test_lease_exit_fault_after_capture_leaves_finalizer_owned_entry(self):
        class FaultingTransition:
            def __init__(self, state):
                self.lease = leases_impl.private_fixture_transition(state)

            def __enter__(self):
                return self.lease.__enter__()

            def __exit__(self, *raised):
                self.lease.__exit__(*raised)
                raise OSError("injected lease exit fault")

        with patch.object(upgrade_impl, "private_fixture_transition", FaultingTransition):
            with self.assertRaisesRegex(OSError, "injected lease exit fault"):
                self.stage()
        self.assertEqual(len(self.captured), 1)
        self.assertEqual(len(self.lifetime.supervisor_cleanups), 1)
        target = self.lifetime.supervisor_cleanups[0]["predecessor"]
        self.assertIs(target, self.captured[0])
        self.assertFalse(target.backup_handle.closed)
        failure = RuntimeError("original")
        result, cleanup, error = self.finalize(failure, self.pueue_command)
        self.assertTrue(target.backup_handle.closed)
        self.assertEqual(self.calls, [(self.backup, ["shutdown"])])
        self.assertEqual((result, error), ("FAIL", failure))
        self.assertEqual((cleanup["status"], cleanup["pending_supervisor_count"]), ("retained", 0))

    def test_canonical_config_is_not_reread_after_capture(self):
        expected = self.config.read_text(encoding="utf-8")
        read_text = Path.read_text

        def guarded_read(path, *args, **kwargs):
            if self.captured and path == self.config:
                raise OSError("injected read after capture")
            return read_text(path, *args, **kwargs)

        with patch.object(Path, "read_text", guarded_read):
            _, staged = self.stage()
        self.assertEqual(staged[3], expected)
        self.assertEqual(len(self.lifetime.supervisor_cleanups), 1)
        self.assertIs(self.lifetime.supervisor_cleanups[0]["predecessor"], self.captured[0])


    def test_unknown_predecessor_disposition_fails_closed(self):
        self.stage()
        pending = self.lifetime.supervisor_cleanups[-1]
        self.publish_successor()
        failure = RuntimeError("original")
        with patch.object(finalization_impl, "shutdown_predecessor_supervisor") as predecessor, \
                patch.object(finalization_impl, "shutdown_private_supervisor") as primary:
            result, cleanup, error = self.finalize(failure, self.successor_command)
        predecessor.assert_called_once()
        primary.assert_not_called()
        self.assertEqual(self.lifetime.supervisor_cleanups, [pending])
        self.assertTrue(pending["predecessor"].backup_handle.closed)
        self.assertEqual((result, error), ("FAIL", failure))
        self.assertEqual((cleanup["status"], cleanup["pending_supervisor_count"]), ("failed", 1))


    def inject_close_fault(self, pending):
        """Swap in a handle wrapper; the test's capture cleanup still closes the real handle."""
        real_handle = pending["predecessor"].backup_handle
        fault_handle = CloseFaultHandle(real_handle)
        pending["predecessor"] = supervisor_impl.PredecessorShutdownTarget(
            **{**vars(pending["predecessor"]), "backup_handle": fault_handle})
        return real_handle, fault_handle.error

    def assert_close_fault_retains_owned_entry(self, pueue_command):
        pending = self.lifetime.supervisor_cleanups[-1]
        real_handle, _ = self.inject_close_fault(pending)
        failure = RuntimeError("original")
        with patch.object(finalization_impl, "shutdown_private_supervisor") as primary:
            result, cleanup, error = self.finalize(failure, pueue_command)
        primary.assert_not_called()
        self.assertEqual(len(self.lifetime.supervisor_cleanups), 1)
        self.assertIs(self.lifetime.supervisor_cleanups[0], pending)
        self.assertFalse(real_handle.closed)
        self.assertEqual((result, error), ("FAIL", failure))
        self.assertEqual((cleanup["status"], cleanup["pending_supervisor_count"]), ("failed", 1))

    def test_backup_close_fault_after_already_closed_keeps_ledger_entry(self):
        self.stage()
        self.publish_successor()
        self.assert_close_fault_retains_owned_entry(self.successor_command)
        self.assertEqual(self.calls, [])
        self.assertIsNone(self.successor.poll())

    def test_backup_close_fault_after_performed_shutdown_keeps_ledger_entry(self):
        self.stage()
        self.assert_close_fault_retains_owned_entry(self.pueue_command)
        self.assertEqual(self.calls, [(self.backup, ["shutdown"])])
        self.assertIsNotNone(self.daemon.poll())

    def test_invalid_canonical_config_after_handoff_refuses_primary_shutdown(self):
        self.stage()
        self.publish_successor()
        config = self.config.read_text(encoding="utf-8")
        self.config.write_text(
            config.replace(json.dumps(str(self.pid_path)), json.dumps(str(self.pid_path) + ".other")),
            encoding="utf-8")
        failure = RuntimeError("original")
        result, cleanup, error = self.finalize(failure, self.successor_command)
        self.assertEqual(self.calls, [])
        self.assertIsNone(self.successor.poll())
        self.assertEqual((result, error), ("FAIL", failure))
        self.assertEqual((cleanup["status"], cleanup["pending_supervisor_count"]), ("failed", 0))
        self.assertTrue(self.base.is_dir())


if __name__ == "__main__":
    unittest.main()
