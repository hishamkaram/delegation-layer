"""Threaded ownership tests and the lifetime of the workers they start.

ConcurrentReplacementTests are the concurrent replacement fixtures. The
lifetime tests run those and the journal-resumer fixture with a failure
injected after a worker started. Each fixture's own patch exit and temporary
root cleanup are guarded: the guard first records which started workers are
still alive, then releases and joins them while that patch is still installed
and that root still exists, so a defective fixture is reported without its
workers racing the teardown.
"""

from contextlib import nullcontext
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch
import sys


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)


from scripts.acceptance.directories import journal as directory_journal
from scripts.acceptance.directories import operations as directory_operations
# The module, not its TestCase, so discovery does not run those tests twice.
import scripts.acceptance.directories.tests.test_ownership as ownership_tests


class ConcurrentReplacementTests(unittest.TestCase):
    def test_concurrent_path_replacement_is_not_deleted(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            owner = directory_operations.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            moved = threading.Event()
            replacement_ready = threading.Event()
            errors = []
            original_rename = directory_operations._rename_noreplace

            def rename_then_pause(source_fd, source, destination_fd, destination):
                result = original_rename(source_fd, source, destination_fd, destination)
                if source == "owned" and destination.startswith(".delegate-cleanup-"):
                    moved.set()
                    if not replacement_ready.wait(5):
                        raise RuntimeError("replacement fixture did not continue")
                return result

            def cleanup():
                try:
                    directory_operations.remove_owned_directory(token)
                except BaseException as error:  # Preserve the worker failure for the test thread.
                    errors.append(error)

            with patch.object(directory_operations, "_rename_noreplace", side_effect=rename_then_pause):
                worker = threading.Thread(target=cleanup)
                worker.start()
                try:
                    self.assertTrue(moved.wait(5))
                    replacement = root / "owned"
                    replacement.mkdir()
                    (replacement / "keep.txt").write_text("replacement")
                finally:  # Release, then join unconditionally before the patch and temporary root exit.
                    replacement_ready.set()
                    worker.join()

            self.assertFalse(worker.is_alive())
            self.assertEqual(errors, [])
            self.assertEqual((root / "owned" / "keep.txt").read_text(), "replacement")

    def test_concurrent_preisolation_replacement_is_quarantined_not_deleted(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            owner = directory_operations.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            rename_ready = threading.Event()
            replacement_ready = threading.Event()
            errors = []
            original_rename = directory_operations._rename_noreplace

            def pause_before_rename(source_fd, source, destination_fd, destination):
                if source == "owned" and destination.startswith(".delegate-cleanup-"):
                    rename_ready.set()
                    if not replacement_ready.wait(5):
                        raise RuntimeError("replacement fixture did not continue")
                return original_rename(source_fd, source, destination_fd, destination)

            def cleanup():
                try:
                    directory_operations.remove_owned_directory(token)
                except BaseException as error:  # Preserve the worker failure for the test thread.
                    errors.append(error)

            with patch.object(directory_operations, "_rename_noreplace", side_effect=pause_before_rename):
                worker = threading.Thread(target=cleanup)
                worker.start()
                try:
                    self.assertTrue(rename_ready.wait(5))
                    token.directory.rename(root / "original")
                    token.directory.mkdir()
                    (token.directory / "keep.txt").write_text("replacement")
                finally:  # Release, then join unconditionally before the patch and temporary root exit.
                    replacement_ready.set()
                    worker.join()

            self.assertFalse(worker.is_alive())
            self.assertEqual(len(errors), 1)
            self.assertRegex(str(errors[0]), "replaced.*quarantine")
            retained = tuple(root.glob(".delegate-cleanup-*/keep.txt"))
            self.assertEqual(len(retained), 1)
            self.assertEqual(retained[0].read_text(), "replacement")
            self.assertTrue((root / "original").is_dir())


REAL_PATCH = patch
REAL_TEMPORARY_DIRECTORY = tempfile.TemporaryDirectory
REAL_EVENT_WAIT = threading.Event.wait
REAL_THREAD_START = threading.Thread.start
REAL_THREAD_JOIN = threading.Thread.join
REAL_WRITE_TEXT = Path.write_text
REPLACEMENT_FIXTURES = (
    "test_concurrent_path_replacement_is_not_deleted",
    "test_concurrent_preisolation_replacement_is_quarantined_not_deleted",
)
RESUMER_FIXTURE = "test_cleanup_journal_serializes_concurrent_resumers"
# The fixture patches whose exit is guarded; every other patch passes through unchanged.
GUARDED_FIXTURE_PATCHES = ((directory_operations, "_rename_noreplace"), (directory_journal, "_cleanup_empty"))
FIXTURE_PATCH = "fixture patch"
TEMPORARY_ROOT = "temporary root"


class _ObservedRun:
    """The exact workers one fixture run started, held until an owner joins them, and the guarded boundaries.

    A started worker that returns from an Event wait then holds on its own
    finish barrier; only an untimed join on that worker opens it, so a fixture
    that skips the join or joins with a timeout still has the worker running
    when it reaches a boundary. At each guarded boundary the run records which
    workers are alive, then remediates: it releases every barrier a tracked
    worker waits on and every hold, and joins every started worker, before the
    real boundary runs.
    """

    def __init__(self, fail_start_number=None):
        self.started = []
        self.finish = {}
        self.waiting = {}
        self.lock = threading.Lock()
        self.closing = False
        self.events = []
        self.observed = {}
        self.closed = {}
        self.fail_start_number = fail_start_number
        self.start_calls = 0

    def start(self, thread):
        self.start_calls += 1
        if self.start_calls == self.fail_start_number:
            raise RuntimeError("injected start failure")
        self.finish[thread] = threading.Event()
        REAL_THREAD_START(thread)
        self.started.append(thread)

    def join(self, thread, timeout=None):
        if timeout is None and thread in self.finish:
            self.finish[thread].set()
        return REAL_THREAD_JOIN(thread, timeout)

    def wait(self, event, timeout=None):
        current = threading.current_thread()
        finish = self.finish.get(current)
        if finish is not None:
            with self.lock:
                if self.closing:
                    event.set()
                else:
                    self.waiting[current] = event
        released = REAL_EVENT_WAIT(event, timeout)
        if finish is not None:
            REAL_EVENT_WAIT(finish)
        return released

    def alive(self):
        return [worker.is_alive() for worker in self.started]

    def remediate(self, boundary):
        self.observed.setdefault(boundary, self.alive())
        self.events.append("observed " + boundary)
        self.close()
        self.closed.setdefault(boundary, self.alive())
        self.events.append("remediated " + boundary)

    def close(self):
        with self.lock:
            self.closing = True
            for event in self.waiting.values():
                event.set()
        for finish in self.finish.values():
            finish.set()
        for worker in self.started:
            REAL_THREAD_JOIN(worker)


class _GuardedExit:
    """One real fixture context whose exit first lets the run observe and remediate."""

    def __init__(self, run, boundary, context):
        self.run = run
        self.boundary = boundary
        self.context = context

    def __enter__(self):
        return self.context.__enter__()

    def __exit__(self, *exc_info):
        self.run.remediate(self.boundary)
        try:
            return self.context.__exit__(*exc_info)
        finally:
            self.run.events.append("exited " + self.boundary)


class _FixturePatch:
    """Stands in for a fixture module's `patch` during one run; only the guarded targets are wrapped."""

    def __init__(self, run):
        self.run = run

    def object(self, target, attribute, *args, **kwargs):
        context = REAL_PATCH.object(target, attribute, *args, **kwargs)
        if any(target is module and attribute == name for module, name in GUARDED_FIXTURE_PATCHES):
            return _GuardedExit(self.run, FIXTURE_PATCH, context)
        return context


class ThreadedFixtureLifetimeTests(unittest.TestCase):
    def run_fixture(self, case, fault, fail_start_number=None):
        run = _ObservedRun(fail_start_number)

        class GuardedTemporaryDirectory(REAL_TEMPORARY_DIRECTORY):
            def cleanup(self):
                run.remediate(TEMPORARY_ROOT)
                try:
                    super().cleanup()
                finally:
                    run.events.append("exited " + TEMPORARY_ROOT)

        fixture_patch = _FixturePatch(run)
        result = unittest.TestResult()
        with REAL_PATCH.object(tempfile, "TemporaryDirectory", GuardedTemporaryDirectory), \
                REAL_PATCH.dict(globals(), {"patch": fixture_patch}), \
                REAL_PATCH.object(ownership_tests, "patch", fixture_patch), \
                REAL_PATCH.object(threading.Thread, "start", lambda thread: run.start(thread)), \
                REAL_PATCH.object(threading.Thread, "join", lambda thread, timeout=None: run.join(thread, timeout)), \
                REAL_PATCH.object(threading.Event, "wait", lambda event, timeout=None: run.wait(event, timeout)), fault:
            try:
                case.run(result)
            finally:
                run.close()  # Fallback only; the guarded boundaries already remediated.
        return result, run

    def assert_owned_to_completion(self, case, fault, reported, *, started=1, fail_start_number=None):
        result, run = self.run_fixture(case, fault, fail_start_number)
        closed = [False] * started
        # The guards observed, then remediated, before each real boundary ran, in fixture order.
        self.assertEqual(run.events, [
            "observed " + FIXTURE_PATCH, "remediated " + FIXTURE_PATCH, "exited " + FIXTURE_PATCH,
            "observed " + TEMPORARY_ROOT, "remediated " + TEMPORARY_ROOT, "exited " + TEMPORARY_ROOT,
        ])
        self.assertEqual(run.closed, {FIXTURE_PATCH: closed, TEMPORARY_ROOT: closed},
                         "remediation must close every started worker before the real patch exit and root cleanup")
        self.assertEqual(run.alive(), closed, "the run itself left a started worker alive")
        failures = [text for _test, text in reported(result)]
        self.assertEqual(len(failures), 1, "the injected failure must be the one reported failure")
        self.assertIn("injected", failures[0])
        # The fixture's own ownership, observed before any remediation.
        self.assertEqual(run.observed, {FIXTURE_PATCH: closed, TEMPORARY_ROOT: closed},
                         "a started worker was still alive when its fixture reached its own patch exit")

    def test_an_assertion_failure_after_worker_entry_still_releases_and_joins(self):
        for owner, name in [(ConcurrentReplacementTests, name) for name in REPLACEMENT_FIXTURES] + [
                (ownership_tests.DirectoryOwnershipTests, RESUMER_FIXTURE)]:
            fault = REAL_PATCH.object(owner, "assertTrue", side_effect=AssertionError("injected barrier assertion"))
            with self.subTest(fixture=name):
                self.assert_owned_to_completion(owner(name), fault, lambda result: result.failures)

    def test_a_file_operation_failure_after_worker_entry_still_releases_and_joins(self):
        def failing_replacement_write(path, *args, **kwargs):
            if path.name == "keep.txt":
                raise OSError("injected replacement write")
            return REAL_WRITE_TEXT(path, *args, **kwargs)

        for name in REPLACEMENT_FIXTURES:
            with self.subTest(fixture=name):
                self.assert_owned_to_completion(ConcurrentReplacementTests(name),
                                                REAL_PATCH.object(Path, "write_text", failing_replacement_write),
                                                lambda result: result.errors)

    def test_a_failed_second_start_still_joins_the_started_first_resumer(self):
        self.assert_owned_to_completion(ownership_tests.DirectoryOwnershipTests(RESUMER_FIXTURE), nullcontext(),
                                        lambda result: result.errors, fail_start_number=2)


if __name__ == "__main__":
    unittest.main()
