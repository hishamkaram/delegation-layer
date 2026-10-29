"""Acceptance disposal holds the maintenance election from authorized shutdown through root deletion."""

from contextlib import contextmanager
import errno
import fcntl
import os
from pathlib import Path
import sys
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.acceptance_tests.private_cli.fixtures import _release_live_directory_tokens
from scripts.delegate_acceptance.directories import operations
from scripts.delegate_acceptance.private_cli import finalization
from scripts.delegate_acceptance.private_cli import leases
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli import root_cleanup
from scripts.delegate_acceptance.private_cli import supervisor
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.processes.operations import Processes

ELECTION = "queue-maintenance.lock"
BARRIER_SECONDS = 10
CLOSED = {"root_id": "root", "tasks": {}, "inspections": {}}
# The holder takes each lock from its own open, publishes its ready marker, and
# keeps the locks until the test publishes the release marker or its bound ends.
HOLDER = """
import fcntl, os, sys, time
supervisor, ready, release = sys.argv[1:4]
held = []
for spec in sys.argv[4:]:
    name, mode = spec.split(":")
    descriptor = os.open(os.path.join(supervisor, name), os.O_RDWR | os.O_NOFOLLOW)
    fcntl.flock(descriptor, fcntl.LOCK_EX if mode == "EX" else fcntl.LOCK_SH)
    held.append(descriptor)
open(ready, "x").close()
deadline = time.monotonic() + 30
while not os.path.exists(release):
    if time.monotonic() >= deadline:
        sys.exit(3)
    time.sleep(0.01)
"""
# An owned probe child reports each lock from its own open: "busy" means EWOULDBLOCK.
PROBE = """
import fcntl, os, sys
for path in sys.argv[1:]:
    descriptor = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        print("free")
    except BlockingIOError:
        print("busy")
    finally:
        os.close(descriptor)
"""


def _lock_free(path, mode):
    """Probe from a fresh open; this process's own held lease also reads as busy."""
    descriptor = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
    try:
        fcntl.flock(descriptor, mode | fcntl.LOCK_NB)
        return True
    except BlockingIOError:
        return False
    finally:
        os.close(descriptor)


class DisposalLeaseFixture(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-disposal-lease-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve(strict=True)
        self.parent = self.base / "parent"
        self.root = self.parent / (private_cli.PRIVATE_ACCEPTANCE_PREFIX + "lease")
        self.state = self.root / "state"
        self.supervisor = self.state / ".supervisor"
        self.make_state(self.state)
        self.process_owner = private_cli.DirectoryOwner()
        self.addCleanup(self.process_owner.rollback)
        self.processes = Processes(self.base / "processes", {"PATH": "/usr/bin:/bin"}, owner=self.process_owner)
        self.holders = 0
        self.disposal = leases.DisposalLeases()
        self.addCleanup(self.disposal.release_all)

    @staticmethod
    def make_state(state):
        (state / ".supervisor").mkdir(mode=0o700, parents=True)
        for name in ("transition.lock", "bootstrap.lock"):
            (state / ".supervisor" / name).touch(mode=0o600)
        return state

    def probe(self, *states):
        """Return what an owned child observes on each state's election lock."""
        paths = [state / ".supervisor" / ELECTION for state in states]
        return self.processes.run("flock-probe", [sys.executable, "-I", "-c", PROBE, *paths], self.base) \
            .output().decode().split()

    @contextmanager
    def holder(self, *specs, supervisor_dir=None):
        """Run one owned child holding ``specs`` until this context releases and waits for it."""
        supervisor_dir = self.supervisor if supervisor_dir is None else supervisor_dir
        for spec in specs:
            (supervisor_dir / spec.split(":")[0]).touch(mode=0o600)
        self.holders += 1
        ready = self.base / ("ready-%d" % self.holders)
        release = self.base / ("release-%d" % self.holders)
        process = self.processes.start(
            "lock-holder", [sys.executable, "-I", "-c", HOLDER, supervisor_dir, ready, release, *specs], self.base
        )
        try:
            wait_until(lambda: ready.exists() or process.poll() is not None, BARRIER_SECONDS, "lock holder entry")
            self.assertTrue(ready.exists(), "lock holder exited before holding its locks")
            yield process
        finally:
            release.touch()
            process.wait(timeout=BARRIER_SECONDS)

    @contextmanager
    def worker_behind_holder(self, target, *specs, exiting=None):
        """Start ``target`` while an owned holder keeps ``specs``; release the holder, then join the worker.

        ``exiting`` is set just before the join for a worker that waits on its test leaving this context.
        """
        worker = threading.Thread(target=target)
        try:
            with self.holder(*specs):
                worker.start()
                yield worker
        finally:
            # The holder is already released and waited; always join before lease and directory cleanup.
            if exiting is not None:
                exiting.set()
            if worker.ident is not None:
                worker.join(BARRIER_SECONDS)

    def owned_entries(self):
        owner = private_cli.DirectoryOwner()
        token = owner.adopt_created("private root", self.root, self.parent)
        entries = owner.transfer()
        self.addCleanup(_release_live_directory_tokens, entries)
        return token, entries

    def remove(self, *, disposal, deadline, states=None):
        token, entries = self.owned_entries()
        managed = tuple((state, ()) for state in (states or (self.state,)))
        with patch.object(root_cleanup, "authorize_managed_lifetime_closure", return_value=CLOSED):
            kwargs = {} if disposal is None else {"disposal": disposal, "deadline": deadline}
            root_cleanup.remove_private_acceptance_root(token, entries, managed_state_roots=managed, **kwargs)

    def run_ready(self, action, deadline, state=None):
        with patch.object(supervisor, "authorize_managed_lifetime_closure", return_value=CLOSED):
            return supervisor.run_when_disposal_ready(
                self.state if state is None else state, (), (self.root,), deadline, action, disposal=self.disposal
            )


class DisposalLeaseTests(DisposalLeaseFixture):
    def test_unleased_supervisor_root_is_retained_while_elected_owner_runs(self):
        with self.holder(ELECTION + ":EX"):
            with self.assertRaisesRegex(RuntimeError, "no disposal lease"):
                self.remove(disposal=None, deadline=None)
            self.assertTrue(self.supervisor.is_dir())

    def test_elected_owner_blocks_action_without_bootstrap_held_while_waiting(self):
        waiting = threading.Event()
        attempts = []
        real_wait = leases.wait_until

        def observed_wait(predicate, **kwargs):
            def attempt():
                acquired = predicate()
                attempts.append((kwargs["description"], acquired))
                if not acquired and kwargs["description"].endswith(ELECTION):
                    waiting.set()
                return acquired

            return real_wait(attempt, **kwargs)

        action = Mock(return_value="shut down")
        outcome = {}
        def target():
            outcome["value"] = self.run_ready(action, time.monotonic() + BARRIER_SECONDS)

        with patch.object(leases, "wait_until", observed_wait), \
                self.worker_behind_holder(target, ELECTION + ":EX", "bootstrap.lock:SH") as worker:
            self.assertTrue(waiting.wait(BARRIER_SECONDS), "disposal never waited for the elected owner")
            self.assertTrue(_lock_free(self.supervisor / "transition.lock", fcntl.LOCK_EX))
            self.assertTrue(_lock_free(self.supervisor / "bootstrap.lock", fcntl.LOCK_SH))
            action.assert_not_called()
        self.assertFalse(worker.is_alive())
        self.assertEqual(outcome, {"value": "shut down"})
        action.assert_called_once_with(CLOSED)
        self.assertEqual([description for description, _ in attempts if not description.endswith(ELECTION)],
                         ["disposal lease transition.lock", "disposal lease bootstrap.lock"])
        for name in leases.DISPOSAL_LOCK_ORDER:
            self.assertFalse(_lock_free(self.supervisor / name, fcntl.LOCK_SH), name + " was not kept held")

    def test_post_start_assertion_failure_still_releases_holder_and_joins_worker(self):
        exiting = threading.Event()
        outcome = {}

        def target():
            outcome["value"] = self.run_ready(
                lambda closure: exiting.wait(BARRIER_SECONDS), time.monotonic() + BARRIER_SECONDS
            )

        with self.assertRaisesRegex(AssertionError, "injected post-start failure"):
            with self.worker_behind_holder(target, ELECTION + ":EX", exiting=exiting) as worker:
                raise AssertionError("injected post-start failure")
        self.assertFalse(worker.is_alive(), "worker was not joined after a post-start failure")
        self.assertEqual(outcome, {"value": True})

    def test_expired_deadline_attempts_each_lock_once_and_never_runs_action(self):
        attempts = []
        real_wait = leases.wait_until

        def counted_wait(predicate, **kwargs):
            return real_wait(lambda: attempts.append(kwargs["description"]) or predicate(), **kwargs)

        action = Mock()
        with self.holder(ELECTION + ":EX"), patch.object(leases, "wait_until", counted_wait):
            with self.assertRaisesRegex(RuntimeError, "observation expired: disposal lease " + ELECTION):
                self.run_ready(action, time.monotonic() - 1)
        action.assert_not_called()
        self.assertEqual(attempts, ["disposal lease " + ELECTION])
        self.assertFalse(self.disposal.holds(self.state))
        self.assertTrue(self.supervisor.is_dir())

    def test_lease_is_threaded_from_shutdown_through_deletion_without_reacquisition(self):
        opened = []
        real_open = leases._open_held_disposal

        def counted_open(state):
            opened.append(state)
            return real_open(state)

        with patch.object(leases, "_open_held_disposal", counted_open):
            self.assertEqual(self.run_ready(lambda closure: closure, time.monotonic() + BARRIER_SECONDS), CLOSED)
            # A second open of a held lock would conflict with this process's own lease.
            self.remove(disposal=self.disposal, deadline=time.monotonic())
        self.assertEqual(opened, [self.state])
        self.assertFalse(self.root.exists())

    def wait_on_pending_second_root(self, *, pending_at_entry):
        """Hold the shut-down root, then make a second root wait once on custody; return probe observations."""
        second = self.make_state(self.root / "second")
        # The probe opens the second root's election lock before that root is first held.
        (second / ".supervisor" / ELECTION).touch(mode=0o600)
        self.assertEqual(self.run_ready(lambda closure: closure, time.monotonic() + BARRIER_SECONDS), CLOSED)
        locked_pending = iter(() if pending_at_entry else ({"pool": ("owner",)},))
        observed = []
        with patch.object(supervisor, "disposal_ready_now", return_value=None if pending_at_entry else CLOSED), \
                patch.object(supervisor, "custody_release_pending", side_effect=lambda *_: next(locked_pending, {})), \
                patch.object(supervisor, "require_custody_release_budget"), \
                patch.object(supervisor, "await_custody_release",
                             side_effect=lambda *_: observed.append(self.probe(self.state, second))):
            self.run_ready(lambda closure: closure, time.monotonic() + BARRIER_SECONDS, state=second)
        return second, observed

    def assert_both_roots_deleted_without_reacquisition(self, second):
        self.assertTrue(self.disposal.holds(self.state) and self.disposal.holds(second))
        with patch.object(leases, "_open_held_disposal", side_effect=AssertionError("held root reopened")):
            self.remove(disposal=self.disposal, deadline=time.monotonic(), states=(self.state, second))
        self.assertFalse(self.root.exists())

    def test_second_root_pending_at_entry_keeps_shut_down_root_held(self):
        second, observed = self.wait_on_pending_second_root(pending_at_entry=True)
        self.assertEqual(observed, [["busy", "free"]])
        self.assert_both_roots_deleted_without_reacquisition(second)

    def test_second_root_pending_under_its_lease_releases_only_itself(self):
        second, observed = self.wait_on_pending_second_root(pending_at_entry=False)
        self.assertEqual(observed, [["busy", "free"], ["busy", "free"]])
        self.assert_both_roots_deleted_without_reacquisition(second)

    def test_same_root_reentry_with_pending_custody_reacquires_without_self_deadlock(self):
        self.run_ready(lambda closure: closure, time.monotonic() + BARRIER_SECONDS)
        locked_pending = iter(({"pool": ("owner",)},))
        held_while_waiting = []
        with patch.object(supervisor, "disposal_ready_now", return_value=None), \
                patch.object(supervisor, "custody_release_pending", side_effect=lambda *_: next(locked_pending, {})), \
                patch.object(supervisor, "require_custody_release_budget"), \
                patch.object(supervisor, "await_custody_release",
                             side_effect=lambda *_: held_while_waiting.append(self.disposal.holds(self.state))):
            self.assertEqual(self.run_ready(lambda closure: "again", time.monotonic() + BARRIER_SECONDS), "again")
        self.assertEqual(held_while_waiting, [False, False])
        self.assertTrue(self.disposal.holds(self.state))

    def test_second_root_deadline_failure_keeps_shut_down_root_until_owner_exit(self):
        second = self.make_state(self.root / "second")
        self.run_ready(lambda closure: closure, time.monotonic() + BARRIER_SECONDS)
        with self.holder(ELECTION + ":EX", supervisor_dir=second / ".supervisor"):
            with self.assertRaisesRegex(RuntimeError, "observation expired: disposal lease " + ELECTION):
                self.run_ready(Mock(), time.monotonic() + 0.2, state=second)
            self.assertEqual(self.probe(self.state, second), ["busy", "busy"])
        self.assertTrue(self.disposal.holds(self.state) and not self.disposal.holds(second))
        self.disposal.release_all()
        self.assertEqual(self.probe(self.state, second), ["free", "free"])

    def test_late_write_during_deletion_retains_bytes_and_reports(self):
        real_remove = operations._remove_entry_at

        def late_writer(parent_fd, name, *args, **kwargs):
            real_remove(parent_fd, name, *args, **kwargs)
            if name == "bootstrap.lock":
                os.close(os.open("late-wake", os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600, dir_fd=parent_fd))

        with patch.object(operations, "_remove_entry_at", late_writer):
            with self.assertRaisesRegex(RuntimeError, "directory rollback failed"):
                self.remove(disposal=self.disposal, deadline=time.monotonic() + BARRIER_SECONDS)
        self.assertTrue(any(path.name == "late-wake" for path in self.parent.rglob("*")))


class DisposalLeaseIdentityTests(DisposalLeaseFixture):
    def assert_refused(self, *created_absent):
        with self.assertRaises((RuntimeError, OSError)):
            self.disposal.hold(self.state, time.monotonic() + 1)
        self.assertFalse(self.disposal.holds(self.state))
        for path in created_absent:
            self.assertFalse(os.path.lexists(path), str(path))

    def test_missing_supervisor_is_never_manufactured(self):
        for name in ("transition.lock", "bootstrap.lock"):
            (self.supervisor / name).unlink()
        self.supervisor.rmdir()
        self.assert_refused(self.supervisor)

    def test_symlinked_or_shared_supervisor_is_refused(self):
        target = self.base / "elsewhere"
        self.supervisor.rename(target)
        self.supervisor.symlink_to(target)
        self.assert_refused(target / ELECTION)
        self.supervisor.unlink()
        target.rename(self.supervisor)
        self.supervisor.chmod(0o755)
        self.assert_refused(self.supervisor / ELECTION)

    def test_missing_lifecycle_lock_refusal_never_creates_the_election_lock(self):
        for name in ("transition.lock", "bootstrap.lock"):
            with self.subTest(missing=name):
                (self.supervisor / name).unlink()
                self.assert_refused(self.supervisor / ELECTION, self.supervisor / name)
                (self.supervisor / name).touch(mode=0o600)

    def test_unsafe_or_missing_locks_are_refused(self):
        (self.supervisor / ELECTION).symlink_to(self.supervisor / "bootstrap.lock")
        self.assert_refused()
        (self.supervisor / ELECTION).unlink()
        (self.supervisor / ELECTION).touch(mode=0o644)
        self.assert_refused()
        (self.supervisor / ELECTION).chmod(0o600)
        (self.supervisor / "transition.lock").unlink()
        self.assert_refused(self.supervisor / "transition.lock")

    def test_missing_election_is_created_private_once_and_existing_winner_is_reused(self):
        self.disposal.hold(self.state, time.monotonic() + 1)
        created = os.stat(self.supervisor / ELECTION, follow_symlinks=False)
        self.assertEqual(oct(created.st_mode & 0o777), oct(0o600))
        self.disposal.release_all()
        descriptor = os.open(self.supervisor, os.O_RDONLY | os.O_DIRECTORY)
        try:
            winner = leases._create_election_lock(descriptor)
            try:
                self.assertEqual(os.fstat(winner).st_ino, created.st_ino)
            finally:
                os.close(winner)
        finally:
            os.close(descriptor)

    def test_replaced_supervisor_after_hold_revokes_deletion_authority(self):
        self.disposal.hold(self.state, time.monotonic() + 1)
        self.supervisor.rename(self.state / "replaced")
        self.supervisor.mkdir(mode=0o700)
        with self.assertRaisesRegex(RuntimeError, "was replaced"):
            self.remove(disposal=self.disposal, deadline=time.monotonic() + 1)
        self.assertTrue(self.root.is_dir())

    def test_failing_body_still_releases_and_waits_for_its_holder(self):
        with self.assertRaisesRegex(ValueError, "body failed"):
            with self.holder(ELECTION + ":EX") as process:
                raise ValueError("body failed")
        self.assertEqual(process.result["exit_code"], 0)
        self.assertTrue(_lock_free(self.supervisor / ELECTION, fcntl.LOCK_EX))


class FinalizerLeaseTeardownTests(DisposalLeaseFixture):
    def owned_root_lifetime(self, second):
        """Return a lifetime owning the real private root with both held states as its managed roots."""
        lifetime = FixtureLifetime(base=self.root, managed_state_roots=[(self.state, []), (second, [])])
        lifetime.base_token = lifetime.ownership.adopt_created("private root", self.root, self.parent)
        self.addCleanup(_release_live_directory_tokens, (lifetime.base_token,))
        return lifetime

    def finalize_with_close_fault(self, failure, receipt):
        """Finalize an owned root while two states are held and one lock descriptor's close faults after closing."""
        second = self.make_state(self.root / "second")
        lifetime = self.owned_root_lifetime(second)
        for state in (self.state, second):
            self.disposal.hold(state, time.monotonic() + BARRIER_SECONDS)
        held = {os.stat(state / ".supervisor").st_ino for state in (self.state, second)}
        held |= {os.stat(state / ".supervisor" / name).st_ino
                 for state in (self.state, second) for name in leases.DISPOSAL_LOCK_ORDER}
        faulty = os.stat(self.supervisor / "transition.lock").st_ino
        real_close = os.close
        attempted = set()

        def close(descriptor):
            identity = os.fstat(descriptor).st_ino
            attempted.add(identity)
            real_close(descriptor)
            if identity == faulty:
                raise OSError(errno.EIO, "injected lease close fault")

        args = SimpleNamespace(scenario="lease-teardown", output=self.base / "receipt.json", pueue=None)
        with patch.object(finalization, "DisposalLeases", return_value=self.disposal), \
                patch.object(root_cleanup, "authorize_managed_lifetime_closure", return_value=CLOSED), \
                patch.object(leases.os, "close", close):
            try:
                finalization.finalize_fixture(args, failure, lifetime, receipt, failure is None)
            finally:
                self.assertTrue(held <= attempted, "a held lease descriptor was not closed")

    def failed_receipt_error(self, receipt, private_root):
        """Return the error of the one FAIL receipt, which keeps the actual owned-root state."""
        receipt.write.assert_called_once()
        result, evidence, receipt_error = receipt.write.call_args.args
        self.assertEqual(result, "FAIL")
        self.assertEqual(evidence["status"], "failed")
        self.assertEqual(evidence["private_root"], {"status": private_root, "verified": True})
        return receipt_error

    def test_lease_close_fault_after_removal_writes_one_failed_receipt_then_raises(self):
        receipt = Mock()
        with self.assertRaisesRegex(RuntimeError, "disposal lease release failed: .*injected lease close fault"):
            self.finalize_with_close_fault(None, receipt)
        self.assertIn("disposal lease release failed", str(self.failed_receipt_error(receipt, "removed")))
        self.assertFalse(self.root.exists())

    def test_lease_close_fault_keeps_the_retained_root_and_primary_failure_in_its_receipt(self):
        failure = RuntimeError("primary scenario failure")
        receipt = Mock()
        self.finalize_with_close_fault(failure, receipt)
        self.assertIs(self.failed_receipt_error(receipt, "retained"), failure)
        for state in (self.state, self.root / "second"):
            self.assertTrue(all((state / ".supervisor" / name).is_file() for name in leases.DISPOSAL_LOCK_ORDER))


if __name__ == "__main__":
    unittest.main()
