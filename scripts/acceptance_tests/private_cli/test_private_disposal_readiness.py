"""Fixture disposal readiness: closure and custody release before the leased shutdown and root removal."""

from contextlib import contextmanager
import fcntl
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
from scripts.acceptance_tests.private_cli.fixtures import canonical_pool_marker
from scripts.acceptance_tests.private_cli.fixtures import create_private_pool_directory
from scripts.acceptance_tests.private_cli.fixtures import reported_as_another_user
from scripts.acceptance_tests.private_cli.fixtures import exception_capture_paths, set_up_private_capture_root
from scripts.acceptance_tests.private_cli.fixtures import write_pool_marker
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.lifetime.identity import ordinary_lifetime_owner_digest
from scripts.delegate_acceptance.private_cli import capture
from scripts.delegate_acceptance.private_cli import disposal_observer
from scripts.delegate_acceptance.private_cli import finalization
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli import leases
from scripts.delegate_acceptance.private_cli import noexec_state
from scripts.delegate_acceptance.private_cli import root_cleanup
from scripts.delegate_acceptance.private_cli import supervisor


ROOT_ID = "b" * 32
POOL_ID = "c" * 64
# The native layout the fixture reads: an identity marker and per-owner views.
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
        (self.state / ".supervisor").mkdir(mode=0o700, parents=True)
        for name in ("transition.lock", "bootstrap.lock"):
            (self.state / ".supervisor" / name).touch(mode=0o600)
        self.events = []

    def pool(self, pool_root, pool_id=POOL_ID, owners=(), views=True):
        create_private_pool_directory(pool_root)
        write_pool_marker(pool_root, canonical_pool_marker(pool_id))
        (pool_root / "images").mkdir()
        if views:
            create_private_pool_directory(pool_root / POOL_VIEWS)
            for owner in owners:
                create_private_pool_directory(pool_root / POOL_VIEWS / owner)
        return pool_root

    def invalid_pools(self, parent):
        """Pools Go's ReleasePending refuses at the root, marker, views or an owner link, with each refusal."""
        duplicate = b'{"version":1,"version":1,"pool_id":"%s"}' % POOL_ID.encode()
        cases = (("duplicate-key", duplicate, 0o600, "not the canonical version 1 record"),
                 ("world-readable", canonical_pool_marker(POOL_ID), 0o644, "unsafe owner or mode"))
        pools = [(write_pool_marker(self.pool(parent / name), raw, mode).parent, message)
                 for name, raw, mode, message in cases]
        shared_root = self.pool(parent / "group-readable-root")
        shared_root.chmod(0o755)
        shared_views = self.pool(parent / "group-readable-views")
        (shared_views / POOL_VIEWS).chmod(0o755)
        file_owner = self.pool(parent / "file-owner")
        (file_owner / POOL_VIEWS / "owner").write_bytes(b"")
        symlinked_owner = self.pool(parent / "symlinked-owner")
        (symlinked_owner / POOL_VIEWS / "owner").symlink_to(
            create_private_pool_directory(parent / "owner-elsewhere"), target_is_directory=True)
        shared_owner = self.pool(parent / "group-readable-owner", owners=("owner",))
        (shared_owner / POOL_VIEWS / "owner").chmod(0o755)
        owner_fault = "custody pool view owner owner is not a private directory of this user"
        return pools + [
            (shared_root, "custody pool root is not a private directory of this user"),
            (shared_views, "custody pool views is not a private directory of this user"),
            (file_owner, owner_fault), (symlinked_owner, owner_fault), (shared_owner, owner_fault),
        ]

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
                create_private_pool_directory(late_owner)
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

    def test_invalid_pool_with_empty_views_sends_no_shutdown(self):
        for pool, message in self.invalid_pools(self.state):
            self.events.clear()
            closure = _closure((pool, POOL_ID))
            # A second reply lets a reader that wrongly reports ready reach the shutdown this refuses.
            with self.subTest(pool=pool.name), self.native_edges([closure, closure]):
                started = time.monotonic()
                with self.assertRaisesRegex(RuntimeError, message):
                    self.shutdown()
                # A concrete fault refuses at once; it never waits out the deadline as pending release.
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


class CustodyViewsTests(DisposalFixture):
    """The views directory and every owner entry beneath it must be private directories of this user."""

    def pending(self, pool):
        return noexec_state.custody_release_pending(_closure((pool, POOL_ID)), (self.state,))

    def views_of(self, name, owners=()):
        return self.pool(self.state / name, owners=owners) / POOL_VIEWS

    def test_private_views_read_ready_or_pending_and_0500_is_private(self):
        self.assertEqual(self.pending(self.pool(self.state / "empty")), ())
        held = self.pool(self.state / "held", owners=("owner",))
        self.assertEqual(self.pending(held), (str(held / POOL_VIEWS / "owner"),))
        read_only = self.views_of("read-only", owners=("owner",))
        (read_only / "owner").chmod(0o500)
        read_only.chmod(0o500)
        self.addCleanup(read_only.chmod, 0o700)
        self.assertEqual(self.pending(read_only.parent), (str(read_only / "owner"),))

    def test_missing_views_remains_a_fixture_layout_refusal(self):
        with self.assertRaisesRegex(RuntimeError, "fixture layout assertion failed"):
            self.pending(self.pool(self.state / "no-views", views=False))

    def test_shared_or_foreign_views_is_refused(self):
        message = "custody pool views is not a private directory of this user"
        for mode in (0o755, 0o750, 0o701):
            with self.subTest(mode=oct(mode)):
                views = self.views_of("views-%o" % mode)
                views.chmod(mode)
                self.assertRaisesRegex(RuntimeError, message, self.pending, views.parent)
        views = self.views_of("foreign-views")
        with reported_as_another_user(views):
            self.assertRaisesRegex(RuntimeError, message, self.pending, views.parent)

    def test_an_owner_entry_that_is_not_a_private_directory_of_this_user_is_refused(self):
        message = "custody pool view owner owner is not a private directory of this user"
        file_owner = self.views_of("file-owner")
        (file_owner / "owner").write_bytes(b"")
        symlinked_owner = self.views_of("symlinked-owner")
        (symlinked_owner / "owner").symlink_to(
            create_private_pool_directory(self.state / "owner-elsewhere"), target_is_directory=True)
        shared_owner = self.views_of("shared-owner", owners=("owner",))
        (shared_owner / "owner").chmod(0o750)
        for views in (file_owner, symlinked_owner, shared_owner):
            with self.subTest(views=views.parent.name):
                self.assertRaisesRegex(RuntimeError, message, self.pending, views.parent)
        foreign_owner = self.views_of("foreign-owner", owners=("owner",))
        with reported_as_another_user(foreign_owner / "owner"):
            self.assertRaisesRegex(RuntimeError, message, self.pending, foreign_owner.parent)

    def test_an_owner_released_after_listing_is_absent_but_another_read_fault_is_concrete(self):
        views = self.views_of("releasing", owners=("owner",))
        owner = views / "owner"
        real_lstat = Path.lstat

        def released_first(path):
            if path == owner:
                owner.rmdir()
            return real_lstat(path)

        with patch.object(Path, "lstat", released_first):
            self.assertEqual(self.pending(views.parent), ())
        create_private_pool_directory(owner)

        def denied(path):
            if path == owner:
                raise PermissionError(13, "Permission denied", str(path))
            return real_lstat(path)

        with patch.object(Path, "lstat", denied), \
                self.assertRaisesRegex(RuntimeError, "custody pool view owner owner is unreadable"):
            self.pending(views.parent)


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
        with patch.object(root_cleanup, "authorize_managed_lifetime_closure", return_value=closure), \
                leases.DisposalLeases() as disposal:
            try:
                root_cleanup.remove_private_acceptance_root(
                    token, entries, managed_state_roots=((self.state, ()),), disposal=disposal,
                    deadline=time.monotonic() + 5,
                )
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

    def test_invalid_pool_with_empty_views_keeps_every_root_byte(self):
        for pool, message in self.invalid_pools(self.state):
            before = self.snapshot()
            with self.subTest(pool=pool.name), patch.object(root_cleanup, "rollback_owned_directories") as rollback:
                with self.assertRaisesRegex(RuntimeError, message):
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


TASK_ID = "a" * 32
OTHER_TASK_ID = "d" * 32
BOOT_ID = "linux:12345678-1234-1234-1234-123456789abc"


def _write_record(path, value):
    path.write_bytes(canonical_go_json(value))


class SupervisorClock:
    """A monotonic-only clock bound in place of the supervisor module's ``time``; any other use fails loudly."""

    def __init__(self):
        self.now = 0.0

    def monotonic(self):
        return self.now


class ObservedReadinessFixture(DisposalFixture):
    """Real canonical records and real flocks; only the public status command and daemon edges are faked."""

    def setUp(self):
        super().setUp()
        _write_record(self.state / "root.json", {
            "schema_version": 1, "root_id": ROOT_ID, "created_at": "2026-10-13T00:00:00Z"})
        self.observed = []

    def intent_only_task(self, task_id=TASK_ID):
        """Publish the durable intent of a worker whose startup failed before registration."""
        task = self.state / "tasks" / task_id
        (task / "lifetime").mkdir(parents=True)
        _write_record(task / "task.json", {"task": "ordinary fixture"})
        _write_record(task / "meta.json", {"schema_version": 1, "runner_ownership": "managed"})
        _write_record(task / "lifetime" / "intent.json", {
            "schema_version": 1, "root_id": ROOT_ID, "task_id": task_id, "owner_kind": "task",
            "owner_sha256": ordinary_lifetime_owner_digest(digest(task / "task.json"), digest(task / "meta.json")),
            "scope_kind": "process-group", "assurance": "cooperative", "scope_plan": "fixture-plan",
            "boot_id": BOOT_ID, "deadline": "2026-10-13T00:01:00Z", "created_at": "2026-10-13T00:00:04Z",
        })

    def recover_absence(self, task_id):
        """What the production guardian publishes when a public observer recovers an unregistered lifetime."""
        lifetime = self.state / "tasks" / task_id / "lifetime"
        _write_record(lifetime / "worker-wait.json", {
            "schema_version": 1, "root_id": ROOT_ID, "task_id": task_id, "registration_sha256": "",
            "child_pid": 0, "result": "absent", "observed_at": "2026-10-13T00:00:09Z",
            "absence_proof": "unregistered-disposed", "observed_boot_id": BOOT_ID,
            "intent_sha256": digest(lifetime / "intent.json"),
        })

    def transition_held(self):
        descriptor = os.open(self.state / ".supervisor" / "transition.lock", os.O_RDWR)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_SH | fcntl.LOCK_NB)
        except BlockingIOError:
            return True
        finally:
            os.close(descriptor)
        return False

    def observer(self, effect=None, root_id=ROOT_ID):
        def observe(task_id, deadline):
            self.observed.append((task_id, self.lease_held() or self.transition_held(), deadline > time.monotonic()))
            self.record("observe")
            if effect is not None:
                effect(task_id)
            return root_id
        return observe

    @contextmanager
    def daemon_edges(self):
        """Fake only the daemon evidence and native shutdown command; the closure oracle is real."""
        gone = iter([False, True, True])
        with patch.object(supervisor, "private_shutdown_evidence", return_value=(123, Path("/run/s"), (1, 2))), \
                patch.object(supervisor, "pueue_command", side_effect=lambda *_a, **_k: self.record("shutdown")), \
                patch.object(supervisor, "private_daemon_process_gone", side_effect=lambda *_a, **_k: next(gone)), \
                patch.object(supervisor, "private_shutdown_endpoint_closed", return_value=True):
            yield
        self.assertFalse(self.lease_held())
        self.assertFalse(self.transition_held())

    def observed_shutdown(self, observe, timeout=WITNESS_TIMEOUT_SECONDS, task_ids=(TASK_ID,)):
        return supervisor.shutdown_observed_private_supervisor(
            "client", self.state / ".supervisor" / "pueue.yml", {}, timeout=timeout,
            state=self.state, task_ids=task_ids, capture_root=None, observe=observe,
        )


class ObservedRecoveryTests(ObservedReadinessFixture):
    def test_intent_only_closure_is_refused_until_the_observer_yields_canonical_absence(self):
        self.intent_only_task()
        with self.daemon_edges(), self.assertRaisesRegex(Exception, "unregistered lifetime lacks recovered absence"):
            self.observed_shutdown(None)
        self.assertEqual(self.events, [])
        with self.daemon_edges():
            closure = self.observed_shutdown(self.observer(self.recover_absence))
        self.assertEqual(closure["tasks"][TASK_ID]["worker_wait"]["absence_proof"], "unregistered-disposed")
        # One observation outside both locks, then the shutdown under the read-only recheck's lease.
        self.assertEqual(self.observed, [(TASK_ID, False, True)])
        self.assertEqual(self.events, [("observe", False), ("shutdown", True)])

    def test_observer_success_without_canonical_proof_never_authorizes_and_never_resweeps(self):
        self.intent_only_task()
        with self.daemon_edges(), self.assertRaisesRegex(RuntimeError, "closure remained incomplete"):
            self.observed_shutdown(self.observer(), timeout=0.3)
        self.assertEqual(self.events, [("observe", False)])

    def test_held_views_release_only_after_the_observer_and_the_recheck(self):
        pool = self.pool(self.state / ".custody", owners=("task-owner",))
        closure = _closure((pool, POOL_ID))
        release = self.observer(lambda _task: (pool / POOL_VIEWS / "task-owner").rmdir())
        with self.daemon_edges(), patch.object(supervisor, "authorize_managed_lifetime_closure", return_value=closure):
            self.assertIs(self.observed_shutdown(release), closure)
        self.assertEqual(self.events, [("observe", False), ("shutdown", True)])

    def test_already_ready_scope_runs_no_observation(self):
        closure = _closure((self.pool(self.state / ".custody"), POOL_ID))
        with self.daemon_edges(), patch.object(supervisor, "authorize_managed_lifetime_closure", return_value=closure):
            self.observed_shutdown(self.observer())
        self.assertEqual(self.observed, [])
        self.assertEqual(self.events, [("shutdown", True)])

    def test_invalid_pool_with_empty_views_runs_no_observation_or_shutdown(self):
        for pool, message in self.invalid_pools(self.state):
            self.events.clear()
            self.observed.clear()
            with self.subTest(pool=pool.name):
                with self.daemon_edges(), \
                        patch.object(supervisor, "authorize_managed_lifetime_closure",
                                     return_value=_closure((pool, POOL_ID))), \
                        self.assertRaisesRegex(RuntimeError, message):
                    self.observed_shutdown(self.observer())
                self.assertEqual(self.observed, [])
                self.assertEqual(self.events, [])

    def test_wrong_root_failed_command_and_pool_faults_retain_without_shutdown(self):
        self.intent_only_task()

        def failed(_task):
            raise RuntimeError("delegate command failed: exit=1")

        cases = (
            (self.observer(self.recover_absence, root_id="e" * 32), "different root than the canonical closure"),
            (self.observer(failed), "delegate command failed"),
        )
        for observe, message in cases:
            self.events.clear()
            (self.state / "tasks" / TASK_ID / "lifetime" / "worker-wait.json").unlink(missing_ok=True)
            with self.subTest(message=message), self.daemon_edges(), self.assertRaisesRegex(RuntimeError, message):
                self.observed_shutdown(observe)
            self.assertNotIn("shutdown", [name for name, _held in self.events])
        self.observed.clear()
        foreign = _closure((self.pool(self.base / "foreign"), POOL_ID))
        with self.daemon_edges(), patch.object(supervisor, "authorize_managed_lifetime_closure", return_value=foreign), \
                self.assertRaisesRegex(RuntimeError, "outside the fixture-owned roots"):
            self.observed_shutdown(self.observer())
        # A concrete pool fault retains at once; it never reaches public repair.
        self.assertEqual(self.observed, [])

    def test_expired_budget_launches_no_further_observation_and_joins_the_started_one(self):
        self.intent_only_task()
        self.intent_only_task(OTHER_TASK_ID)
        clock = SupervisorClock()
        budget = 0.2
        finished = []

        def outlive_budget(_task):
            # The started observation consumes the whole budget before it returns.
            clock.now += budget
            finished.append(True)

        # Only the supervisor module's clock binding is controlled, so real preflight time cannot
        # spend the budget before the first observation starts.
        with self.daemon_edges(), patch.object(supervisor, "DEFAULT_DISPATCH_WORKERS", 1), \
                patch.object(supervisor, "time", clock), \
                self.assertRaisesRegex(TimeoutError, "budget expired before task " + OTHER_TASK_ID):
            self.observed_shutdown(self.observer(outlive_budget), timeout=budget, task_ids=(TASK_ID, OTHER_TASK_ID))
        self.assertEqual([task for task, _held, _left in self.observed], [TASK_ID])
        self.assertEqual(finished, [True])
        self.assertNotIn("shutdown", [name for name, _held in self.events])


class SweepStopTests(unittest.TestCase):
    def test_a_failure_in_any_worker_stops_queued_observations(self):
        failed = threading.Event()
        third_called = threading.Event()
        calls = []

        def observe(task_id, _deadline):
            calls.append(task_id)
            if task_id == "first":
                # Blocked while the second worker fails; a queued third must never start meanwhile.
                failed.wait(WITNESS_TIMEOUT_SECONDS)
                third_called.wait(0.5)
                return ROOT_ID
            if task_id == "second":
                failed.set()
                raise RuntimeError("second observation failed")
            third_called.set()
            return ROOT_ID

        with patch.object(supervisor, "DEFAULT_DISPATCH_WORKERS", 2), \
                self.assertRaisesRegex(RuntimeError, "second observation failed"):
            supervisor.sweep_disposal_observer(
                ("first", "second", "third"), time.monotonic() + WITNESS_TIMEOUT_SECONDS, observe)
        self.assertEqual(sorted(calls), ["first", "second"])
        self.assertFalse(third_called.is_set())


def hold_capture_start_lock(test_case, seconds):
    """Hold the shared capture start lock; a timer releases it so code that ignores a deadline fails, never hangs."""
    capture.CAPTURE_START_LOCK.acquire()
    guard = threading.Lock()
    held = [True]

    def release():
        with guard:
            if held[0]:
                held[0] = False
                capture.CAPTURE_START_LOCK.release()

    timer = threading.Timer(seconds, release)
    timer.start()
    test_case.addCleanup(timer.join)
    test_case.addCleanup(timer.cancel)
    test_case.addCleanup(release)
    return release


class ObserverCaptureDeadlineTests(unittest.TestCase):
    """The observer's absolute deadline reaches the existing private control capture owner unchanged."""

    def setUp(self):
        self.capture_root = set_up_private_capture_root(self)

    def capture_files(self):
        return sorted((path.name.rsplit(".", 1)[1], path.stat().st_size)
                      for path in self.capture_root.directory.iterdir())

    def test_absolute_deadline_bounds_launch_lock_and_never_launches_after_expiry(self):
        cases = (
            (True, 0.2, "while waiting to launch"),
            (False, -1.0, "expired before launch"),
        )
        for contended, offset, message in cases:
            with self.subTest(contended=contended), \
                    patch.object(capture.subprocess, "Popen", side_effect=AssertionError("launched")) as popen:
                release = hold_capture_start_lock(self, 2) if contended else (lambda: None)
                try:
                    started = time.monotonic()
                    with self.assertRaisesRegex(TimeoutError, message):
                        capture.run_captured_command(
                            ["private-control"], {}, None, capture_root=self.capture_root,
                            deadline=started + offset)
                    self.assertLess(time.monotonic() - started, 1)
                finally:
                    release()
                popen.assert_not_called()
            # Both refusals precede capture preparation, so no capture file exists.
            self.assertEqual(self.capture_files(), [])

    def test_deadline_consumed_while_preparing_captures_launches_nothing(self):
        """A fake clock passes the deadline while the second capture file opens; the spawn must still be refused."""
        clock = [0.0]
        open_writer = capture._open_capture_writer

        def slow_open(capture_root, name):
            writer = open_writer(capture_root, name)
            if name.endswith(".stderr"):
                clock[0] = 1.2
            return writer

        with patch.object(capture.time, "monotonic", side_effect=lambda: clock[0]), \
                patch.object(capture, "_open_capture_writer", side_effect=slow_open), \
                patch.object(capture.subprocess, "Popen", side_effect=AssertionError("launched")) as popen:
            with self.assertRaisesRegex(TimeoutError, "expired during capture preparation") as raised:
                capture.run_captured_command(["private-control"], {}, None, capture_root=self.capture_root,
                                             deadline=1.0)
        popen.assert_not_called()
        self.assertEqual(self.capture_files(), [("stderr", 0), ("stdout", 0)])
        self.assertEqual(sorted(exception_capture_paths(raised.exception, "retained_capture_paths")),
                         sorted(str(path) for path in self.capture_root.directory.iterdir()))

    def launch_and_record_wait(self, *budget, **deadline):
        """Launch through the real capture owner with a slow fake spawn; return the wait budget it used."""
        process = unittest.mock.Mock(args=["private-control"], returncode=0)
        waits = []

        def slow_spawn(*_args, **_kwargs):
            time.sleep(0.3)
            return process

        with patch.object(capture.subprocess, "Popen", side_effect=slow_spawn), \
                patch.object(capture, "_wait_private_control_process",
                             side_effect=lambda _process, timeout, _description: waits.append(timeout) or 0):
            capture.run_captured_command(["private-control"], {}, *budget, capture_root=self.capture_root, **deadline)
        return waits[0]

    def test_absolute_deadline_wait_gets_only_the_budget_remaining_after_launch(self):
        remaining = self.launch_and_record_wait(None, deadline=time.monotonic() + 1.0)
        self.assertGreater(remaining, 0)
        self.assertLessEqual(remaining, 0.7)

    def test_relative_timeout_callers_keep_a_full_post_launch_budget(self):
        self.assertEqual(self.launch_and_record_wait(3), 3)
        with patch.object(capture.subprocess, "Popen") as popen:
            for budget in ((3, time.monotonic() + 3), (None, None)):
                with self.subTest(budget=budget), self.assertRaisesRegex(Exception, "exactly one of timeout or deadline"):
                    capture.run_captured_command(["private-control"], {}, budget[0],
                                                 capture_root=self.capture_root, deadline=budget[1])
            popen.assert_not_called()


class PublicStatusObserverTests(unittest.TestCase):
    def test_contended_capture_lock_past_the_orchestrator_deadline_launches_nothing(self):
        self.capture_root = set_up_private_capture_root(self)
        observe = disposal_observer.public_status_observer(
            Path("/install/delegate"), Path("/owned/state"), {}, capture_root=self.capture_root)
        with patch.object(capture.subprocess, "Popen", side_effect=AssertionError("launched")) as popen:
            release = hold_capture_start_lock(self, 2)
            try:
                with self.assertRaisesRegex(TimeoutError, "while waiting to launch"):
                    observe(TASK_ID, time.monotonic() + 0.2)
            finally:
                release()
        popen.assert_not_called()

    def test_observer_runs_one_bounded_public_status_and_checks_its_identity(self):
        replies = iter([{"task_id": TASK_ID, "root_id": ROOT_ID}, {"task_id": OTHER_TASK_ID, "root_id": ROOT_ID},
                        {"task_id": TASK_ID}])
        with patch.object(disposal_observer, "run_json", side_effect=lambda *_a, **_k: next(replies)) as run:
            observe = disposal_observer.public_status_observer(
                Path("/install/delegate"), Path("/owned/state"), {"PATH": "/bin"}, capture_root="capture")
            self.assertEqual(observe(TASK_ID, 7.5), ROOT_ID)
            with self.assertRaisesRegex(RuntimeError, "different task"):
                observe(TASK_ID, 7.5)
            with self.assertRaisesRegex(RuntimeError, "omitted its root identity"):
                observe(TASK_ID, 7.5)
        # The orchestrator's absolute deadline passes through unchanged; no relative budget is rebuilt.
        run.assert_any_call(
            Path("/install/delegate"), {"PATH": "/bin"}, ["--root", "/owned/state", "status", TASK_ID, "--json"],
            timeout=None, phase="managed-runner-observation", capture_root="capture", deadline=7.5)


class PrimaryFinalizationTests(ObservedReadinessFixture):
    def lifetime(self, observe):
        config = self.state / ".supervisor" / "pueue.yml"
        config.write_text("shared: {}\n")
        return FixtureLifetime(
            private_config=config, managed_state_roots=[(self.state, [TASK_ID])], primary_observer=observe)

    def release(self, lifetime, budget):
        errors = []
        args = unittest.mock.Mock(scenario="capacity")
        with patch.object(finalization, "PRIMARY_FINALIZATION_SECONDS", budget):
            _binding, ready, deadline = finalization.release_fixture_workers(args, errors, lifetime, False)
        return ready, deadline, errors

    def test_failed_finalization_reaches_public_recovery_before_refusing_intent_only_closure(self):
        self.intent_only_task()
        ready, _deadline, errors = self.release(self.lifetime(self.observer(self.recover_absence)), 5)
        self.assertTrue(ready)
        self.assertEqual(errors, [])
        self.assertEqual(self.observed, [(TASK_ID, False, True)])

    def test_unbound_observer_keeps_the_passive_closure_refusal(self):
        self.intent_only_task()
        ready, _deadline, errors = self.release(self.lifetime(None), 0.2)
        self.assertFalse(ready)
        self.assertRegex(str(errors[0]), "managed workers did not close .*unregistered lifetime lacks recovered absence")

    def test_primary_shutdown_shares_the_finalization_deadline_and_sends_nothing_after_it(self):
        lifetime = self.lifetime(None)
        args = unittest.mock.Mock(pueue=Path(__file__))
        errors = []
        with patch.object(finalization, "shutdown_private_supervisor") as shutdown:
            finalization.shutdown_primary_supervisor(
                args, errors, None, lifetime, True, False, predecessor_roots=frozenset(),
                deadline=time.monotonic() - 1)
            shutdown.assert_not_called()
            self.assertRegex(str(errors[0]), "shutdown remained unverified: .*expired")
            errors.clear()
            finalization.shutdown_primary_supervisor(
                args, errors, None, lifetime, True, False, predecessor_roots=frozenset(),
                deadline=time.monotonic() + 20)
        timeout = shutdown.call_args.kwargs["timeout"]
        self.assertTrue(0 < timeout <= 20)
        self.assertEqual(errors, [])


if __name__ == "__main__":
    unittest.main()
