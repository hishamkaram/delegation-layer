"""Fixture disposal readiness: closure and custody release before the leased shutdown and root removal."""

from contextlib import contextmanager
import fcntl
import json
import os
from pathlib import Path
import shutil
import tempfile
import threading
import time
import unittest
import unittest.mock
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.acceptance_tests.private_cli.fixtures import _release_live_directory_tokens
from scripts.delegate_acceptance.private_cli import leases
from scripts.delegate_acceptance.private_cli import noexec_state
from scripts.delegate_acceptance.private_cli import root_cleanup
from scripts.delegate_acceptance.private_cli import supervisor


ROOT_ID = "b" * 32
POOL_ID = "c" * 64
# The native layout the fixture reads: an identity marker and per-owner views.
POOL_MARKER = ".identity"
POOL_VIEWS = "views"
WITNESS_TIMEOUT_SECONDS = 10


def _closure(*pools):
    return {"root_id": ROOT_ID, "tasks": {
        "%032x" % index: {"registration": {"custody_placement": {
            "version": 1, "pool_root": str(pool), "pool_id": pool_id}}}
        for index, (pool, pool_id) in enumerate(pools)
    }, "inspections": {}}


class DisposalFixture(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-disposal-readiness-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve(strict=True)
        self.parent = self.base / "parent"
        self.root = self.parent / (private_cli.PRIVATE_ACCEPTANCE_PREFIX + "test")
        self.state = self.root / "state"
        (self.state / ".supervisor").mkdir(parents=True)
        for name in ("transition.lock", "bootstrap.lock"):
            (self.state / ".supervisor" / name).touch(mode=0o600)
        self.events = []

    def pool(self, pool_root, pool_id=POOL_ID, owners=(), views=True):
        pool_root.mkdir(parents=True)
        (pool_root / POOL_MARKER).write_text(json.dumps({"version": 1, "pool_id": pool_id}))
        (pool_root / "images").mkdir()
        if views:
            (pool_root / POOL_VIEWS).mkdir()
            for owner in owners:
                (pool_root / POOL_VIEWS / owner).mkdir()
        return pool_root

    def snapshot(self):
        return sorted((str(path.relative_to(self.root)), path.is_dir()) for path in self.root.rglob("*"))

    def lease_held(self):
        """Probe the real bootstrap lock from a separate open file: an exclusive holder blocks a shared try."""
        descriptor = os.open(self.state / ".supervisor" / "bootstrap.lock", os.O_RDWR)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_SH | fcntl.LOCK_NB)
        except BlockingIOError:
            return True
        finally:
            os.close(descriptor)
        return False

    def record(self, name):
        self.events.append((name, self.lease_held()))

    @contextmanager
    def native_edges(self, closures):
        """Fake only the closure oracle, the daemon evidence and the native shutdown command."""
        replies = iter(closures)

        def closure(_state, _task_ids=()):
            self.record("closure")
            return next(replies)

        def command(*_args, **_kwargs):
            self.record("shutdown")

        def endpoint_closed(*_args):
            self.record("endpoint-closed")
            return True

        gone = iter([False, True, True])
        with patch.object(supervisor, "authorize_managed_lifetime_closure", side_effect=closure), \
                patch.object(supervisor, "private_shutdown_evidence", return_value=(123, Path("/run/s"), (1, 2))), \
                patch.object(supervisor, "pueue_command", side_effect=command), \
                patch.object(supervisor, "private_daemon_process_gone", side_effect=lambda *_a, **_k: next(gone)), \
                patch.object(supervisor, "private_shutdown_endpoint_closed", side_effect=endpoint_closed), \
                patch.object(supervisor, "pueue_snapshot") as snapshot:
            yield
        snapshot.assert_not_called()
        self.assertFalse(self.lease_held())

    def shutdown(self, timeout=WITNESS_TIMEOUT_SECONDS, env=None):
        return supervisor.shutdown_private_supervisor(
            "client", self.state / ".supervisor" / "pueue.yml", env or {}, timeout=timeout,
            state=self.state, capture_root=None,
        )

    def bootstrap_maintenance(self, action):
        """Model bootstrap maintenance: it releases views only under a shared bootstrap lock."""
        descriptor = os.open(self.state / ".supervisor" / "bootstrap.lock", os.O_RDWR)

        def run():
            try:
                fcntl.flock(descriptor, fcntl.LOCK_SH)
                self.events.append(("maintenance", False))
                action()
                fcntl.flock(descriptor, fcntl.LOCK_UN)
            finally:
                os.close(descriptor)

        worker = threading.Thread(target=run, daemon=True)
        self.addCleanup(worker.join, WITNESS_TIMEOUT_SECONDS)
        return worker


class LeasedShutdownTests(DisposalFixture):
    def test_closed_released_scope_follows_the_exact_lease_order(self):
        closure = _closure((self.pool(self.state / ".custody"), POOL_ID))
        with self.native_edges([closure, closure]):
            self.assertIs(self.shutdown(), closure)
        # Closure outside the lease, then the read-only recheck, shutdown and endpoint proof under it.
        self.assertEqual(self.events, [
            ("closure", False), ("closure", True), ("shutdown", True), ("endpoint-closed", True)])

    def test_pending_views_send_no_shutdown_and_name_the_owner(self):
        pool = self.pool(self.state / ".custody", owners=("task-owner",))
        closure = _closure((pool, POOL_ID))
        with self.native_edges([closure]), \
                self.assertRaisesRegex(RuntimeError, "1 owner namespaces: .*/views/task-owner"):
            self.shutdown(timeout=0.2)
        self.assertEqual(self.events, [("closure", False)])

    def test_release_progresses_outside_the_exclusive_lease(self):
        pool = self.pool(self.state / ".custody", owners=("inspection-owner",))
        closure = _closure((pool, POOL_ID))
        maintenance = self.bootstrap_maintenance(lambda: (pool / POOL_VIEWS / "inspection-owner").rmdir())
        with self.native_edges([closure, closure]):
            maintenance.start()
            self.shutdown()
        names = [name for name, _held in self.events]
        self.assertLess(names.index("maintenance"), self.events.index(("closure", True)))
        self.assertEqual(self.events[-3:], [("closure", True), ("shutdown", True), ("endpoint-closed", True)])

    def test_pending_under_the_recheck_leaves_the_lease_before_the_next_wait(self):
        pool = self.pool(self.state / ".custody")
        closure = _closure((pool, POOL_ID))
        late_owner = pool / POOL_VIEWS / "late-owner"
        maintenance = self.bootstrap_maintenance(late_owner.rmdir)
        calls = []

        def appears_under_lease():
            calls.append(1)
            if len(calls) == 2:
                late_owner.mkdir()
                maintenance.start()
            return closure

        with self.native_edges([closure] * 4):
            with patch.object(supervisor, "authorize_managed_lifetime_closure",
                              side_effect=lambda *_a: (self.record("closure"), appears_under_lease())[1]):
                self.shutdown()
        # Maintenance can take its shared lock only after the pending recheck left the lease.
        names = [name for name, _held in self.events]
        self.assertEqual(self.events[:2], [("closure", False), ("closure", True)])
        self.assertLess(1, names.index("maintenance"))
        self.assertEqual(names.count("shutdown"), 1)
        self.assertLess(names.index("maintenance"), names.index("shutdown"))
        self.assertEqual(self.events[-2:], [("shutdown", True), ("endpoint-closed", True)])

    def test_concrete_pool_fault_retains_without_retry(self):
        cases = (
            (_closure((self.pool(self.state / "mismatch", pool_id="d" * 64), POOL_ID)), "marker does not match"),
            (_closure((self.state / "missing", POOL_ID)), "unreadable"),
            (_closure((self.pool(self.state / "no-views", views=False), POOL_ID)), "fixture layout assertion"),
            (_closure((self.pool(self.base / "foreign"), POOL_ID)), "outside the fixture-owned roots"),
            (_closure((self.pool(self.state / "shared"), POOL_ID), (self.state / "shared", "e" * 64)),
             "conflicting identities"),
        )
        for closure, message in cases:
            self.events.clear()
            with self.subTest(message=message), self.native_edges([closure]):
                started = time.monotonic()
                with self.assertRaisesRegex(RuntimeError, message):
                    self.shutdown()
                self.assertLess(time.monotonic() - started, WITNESS_TIMEOUT_SECONDS / 2)
                self.assertEqual(self.events, [("closure", False)])

    def test_aliased_pool_root_is_never_released(self):
        real = self.pool(self.state / "real")
        alias = self.state / "alias"
        alias.symlink_to(real, target_is_directory=True)
        with self.native_edges([_closure((alias, POOL_ID))]), self.assertRaisesRegex(RuntimeError, "not canonical"):
            self.shutdown()

    def test_fallback_pool_in_the_execution_temporary_root_is_read_only(self):
        execution = self.root / "tmp"
        pool = self.pool(execution / "fallback-pool")
        closure = _closure((pool, POOL_ID))
        before = self.snapshot()
        with self.native_edges([closure, closure]):
            self.shutdown(env={"TMPDIR": str(execution)})
        self.assertEqual(self.snapshot(), before)

    def test_deliberately_stopped_closed_lifetimes_need_no_queue_done_row(self):
        # A user stop or natural budget ends Killed in the queue; canonical closure alone authorizes.
        stopped = {"root_id": ROOT_ID, "inspections": {}, "tasks": {"f" * 32: {"registration": {}}}}
        with self.native_edges([stopped, stopped]):
            self.assertIs(self.shutdown(), stopped)
        self.assertIn(("shutdown", True), self.events)

    def test_one_deadline_bounds_a_contended_lease(self):
        closure = _closure((self.pool(self.state / ".custody"), POOL_ID))
        holder = os.open(self.state / ".supervisor" / "transition.lock", os.O_RDWR)
        self.addCleanup(os.close, holder)
        fcntl.flock(holder, fcntl.LOCK_EX)
        with self.native_edges([closure]):
            started = time.monotonic()
            with self.assertRaisesRegex(RuntimeError, "historical seed lifecycle lock"):
                self.shutdown(timeout=0.3)
            # The default per-lock wait is 30 s; the shutdown's one deadline bounds it instead.
            self.assertLess(time.monotonic() - started, WITNESS_TIMEOUT_SECONDS)
        self.assertNotIn("shutdown", [name for name, _held in self.events])

    def test_shutdown_inside_a_held_transition_lease_is_refused(self):
        closure = _closure((self.pool(self.state / ".custody"), POOL_ID))
        with self.native_edges([closure]), leases.private_fixture_transition(self.state):
            with self.assertRaisesRegex(RuntimeError, "historical seed lifecycle lock"):
                self.shutdown(timeout=0.3)
        self.assertNotIn("shutdown", [name for name, _held in self.events])

    def test_pending_ledger_entry_is_kept_on_readiness_refusal(self):
        pool = self.pool(self.state / ".custody", owners=("owner",))
        config = self.state / ".supervisor" / "pueue.yml"
        config.write_text("shared: {}\n")
        pending = {"client": "client", "config": config, "env": {}, "state": self.state, "task_ids": ()}
        ledger = [pending]
        with self.native_edges([_closure((pool, POOL_ID))]), patch.object(supervisor.time, "monotonic",
                                                                         side_effect=[0, 31]):
            with self.assertRaisesRegex(RuntimeError, "custody release remained pending"):
                supervisor.shutdown_pending_private_supervisor(pending, ledger, capture_root=None)
        self.assertEqual(ledger, [pending])
        config.unlink()
        self.assertIsNone(supervisor.shutdown_pending_private_supervisor(pending, ledger, capture_root=None))


class PredecessorLeaseTests(DisposalFixture):
    def test_predecessor_shutdown_takes_one_lease_and_rechecks_its_backup(self):
        closure = _closure((self.pool(self.state / ".custody"), POOL_ID))
        backup = self.state / ".supervisor" / "pueue.yml.predecessor"
        handle = backup.open("w+b")
        self.addCleanup(handle.close)
        handle.write(b"backup\n")
        handle.flush()
        target = supervisor.PredecessorShutdownTarget(
            config=self.state / ".supervisor" / "pueue.yml", state=self.state, backup=backup,
            backup_handle=handle, backup_content=b"backup\n", pid=123, endpoint=Path("/run/s"),
            socket_identity=(1, 2))
        checks = []
        with self.native_edges([closure, closure]), \
                patch.object(supervisor, "predecessor_closed", return_value=False), \
                patch.object(supervisor, "predecessor_shutdown_evidence", return_value=(123, Path("/run/s"), (1, 2))), \
                patch.object(supervisor, "require_predecessor_backup",
                             side_effect=lambda bound: checks.append((bound, self.lease_held()))):
            disposition = supervisor.shutdown_predecessor_supervisor("client", target, {}, timeout=5, capture_root=None)
        self.assertEqual(disposition, supervisor.PREDECESSOR_SHUT_DOWN)
        # One lease: closure outside it, then the recheck, both backup checks and the shutdown under it.
        self.assertEqual(self.events, [
            ("closure", False), ("closure", True), ("shutdown", True), ("endpoint-closed", True)])
        self.assertEqual(checks, [(target, True), (target, True)])


class RecheckDeadlineTests(DisposalFixture):
    """The under-lease recheck shares the one deadline even when every outside wait is clear."""

    PENDING = ("/pool/views/late-owner",)
    # Far more readiness reads than one bounded refusal needs; reaching it means the loop ignores the deadline.
    PROBE_LIMIT = 50

    def run_ready(self, inside_results, action, budget):
        """Drive the real run_when_disposal_ready: outside reads are clear, under-lease reads follow inside_results."""
        clock = [0.0]
        reads = []

        def monotonic():
            clock[0] += 1.0
            return clock[0]

        def pending(_closure, _owned_roots):
            reads.append(self.lease_held())
            if len(reads) > self.PROBE_LIMIT:
                raise AssertionError("readiness loop did not observe the shared deadline")
            if not reads[-1]:
                return ()
            return inside_results[min(reads.count(True), len(inside_results)) - 1]

        closure = _closure()
        with patch.object(supervisor, "authorize_managed_lifetime_closure", return_value=closure), \
                patch.object(supervisor, "custody_release_pending", side_effect=pending), \
                patch.object(supervisor.time, "monotonic", side_effect=monotonic), \
                patch.object(supervisor.time, "sleep"):
            # Every clock read advances one tick, so the budget counts reads, lease lock waits included.
            deadline = supervisor.time.monotonic() + budget
            try:
                return supervisor.run_when_disposal_ready(self.state, (), (self.state,), deadline, action), reads
            finally:
                self.assertFalse(self.lease_held())

    def test_pending_recheck_past_the_deadline_refuses_without_the_lease_or_action(self):
        action = unittest.mock.Mock()
        with self.assertRaisesRegex(RuntimeError, "custody release remained pending .*/pool/views/late-owner"):
            self.run_ready([self.PENDING], action, budget=3)
        action.assert_not_called()

    def test_pending_recheck_that_clears_before_the_deadline_runs_the_action_once(self):
        action = unittest.mock.Mock(side_effect=lambda closure: (self.lease_held(), closure))
        (held, closure), reads = self.run_ready([self.PENDING, ()], action, budget=self.PROBE_LIMIT)
        action.assert_called_once()
        self.assertTrue(held)
        self.assertEqual(closure, _closure())
        self.assertEqual(reads, [False, True, False, True])


class RootRemovalReadinessTests(DisposalFixture):
    def remove(self, closure):
        owner = private_cli.DirectoryOwner()
        token = owner.adopt_created("private root", self.root, self.parent)
        entries = owner.transfer()
        with patch.object(root_cleanup, "authorize_managed_lifetime_closure", return_value=closure):
            try:
                root_cleanup.remove_private_acceptance_root(token, entries, managed_state_roots=((self.state, ()),))
            finally:
                _release_live_directory_tokens(entries)

    def test_released_owned_pool_allows_removal(self):
        self.remove(_closure((self.pool(self.state / ".custody"), POOL_ID)))
        self.assertFalse(self.root.exists())

    def test_pending_views_keep_every_root_byte(self):
        pool = self.pool(self.state / ".custody", owners=("owner",))
        before = self.snapshot()
        with patch.object(root_cleanup, "rollback_owned_directories") as rollback:
            with self.assertRaisesRegex(RuntimeError, "pending before root removal: 1 owner namespaces"):
                self.remove(_closure((pool, POOL_ID)))
            rollback.assert_not_called()
        self.assertEqual(self.snapshot(), before)

    def test_foreign_pool_is_refused_unread(self):
        foreign = self.pool(self.base / "foreign", owners=("owner",))
        with patch.object(noexec_state, "_pool_marker_id") as marker, \
                patch.object(root_cleanup, "rollback_owned_directories") as rollback:
            with self.assertRaisesRegex(RuntimeError, "outside the fixture-owned roots"):
                self.remove(_closure((foreign, POOL_ID)))
            marker.assert_not_called()
            rollback.assert_not_called()
        self.assertTrue(self.root.is_dir())
        shutil.rmtree(foreign)


if __name__ == "__main__":
    unittest.main()
