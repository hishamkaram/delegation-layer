"""The archive fixture tree owns every verifier child's input and output until closure is proven.

The verifier converts a child failure and ``finish_gate`` consumes it into a receipt, so the
fixture tree and each containing tree must observe the original failure before that happens.
"""

from __future__ import annotations

from collections.abc import Callable, Generator, Mapping
import contextlib
import errno
import io
import os
from pathlib import Path
import subprocess
from typing import IO, TypedDict, Unpack
import sys
import threading
import traceback
import unittest
from unittest.mock import Mock, patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.release_tests.release_fixture import FixtureTree
from scripts.acceptance.testing.resources import open_descriptors
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.contracts import CommandFailure
# A module import, so discovery here does not collect the archive tests a second time.
from scripts import test_verify_release_archive as archive


class PopenOptions(TypedDict, total=False):
    cwd: str | Path | None
    env: Mapping[str, str] | None
    stdin: int | IO[bytes] | None
    stdout: int | IO[bytes] | None
    stderr: int | IO[bytes] | None
    shell: bool
    close_fds: bool
    start_new_session: bool


def delegate_version(argv: list[str]) -> bool:
    return Path(argv[0]).name == "delegate" and argv[1:2] == ["version"]


class StartFault:
    """Refuses each selected child start with ``failure``; every other child launches for real.

    The canonical bounded runner waits for every child it started, so a known ``returncode`` on
    each launched child plus zero selected starts is the test's own proof that none can still write.
    """

    def __init__(self, selected: Callable[[list[str]], bool], failure: Exception):
        self.selected = selected
        self.failure = failure
        self.popen = subprocess.Popen
        self.refused: list[list[str]] = []
        self.launched: list[subprocess.Popen[bytes]] = []

    def __call__(self, argv: list[str], **options: Unpack[PopenOptions]) -> subprocess.Popen[bytes]:
        if self.selected(argv):
            self.refused.append(argv)
            raise self.failure
        process = self.popen(argv, **options)
        self.launched.append(process)
        return process

    def closed(self) -> bool:
        return len(self.refused) == 1 and all(child.returncode is not None for child in self.launched)


class ReleaseArchiveOwnershipTests(unittest.TestCase):
    def setUp(self):
        self.baseline = open_descriptors()

    def verify_with(self, fixture: archive.ReleaseArchiveVerificationTests, fault: StartFault, code: CommandFailure) -> None:
        """Run the real verifier CLI under ``fault``, then the fixture's teardown and registered cleanups."""
        with contextlib.redirect_stderr(io.StringIO()), fixture.prepared():
            with patch.object(subprocess, "Popen", side_effect=fault):
                self.assertEqual(fixture.verify(), 1)
            failure = fixture.load_receipt()["failure"]
        self.assertEqual((failure["code"], failure["stage"]), (code.value, "delegate-version"))

    def test_unknown_child_wait_retains_the_fixture_until_closure_is_proven(self):
        fixture = archive.ReleaseArchiveVerificationTests()
        fault = StartFault(delegate_version, subprocess.TimeoutExpired(["delegate"], 0))
        self.verify_with(fixture, fault, CommandFailure.TIMEOUT)
        self.assertTrue(fixture.root.is_dir(), "an unknown child Wait must retain the archive fixture")
        error = fixture.tree.unresolved
        self.assertIsInstance(error, BoundedCommandError)
        assert isinstance(error, BoundedCommandError)
        self.assertIs(error.reason, CommandFailure.TIMEOUT)
        self.assertFalse(fixture.tree.dispose())
        self.assertEqual(open_descriptors(), self.baseline)
        self.assertTrue(fault.closed())
        fixture.tree.remove_retained()
        self.assertFalse(fixture.root.exists())

    def test_containing_owner_observes_the_placed_fixture_child(self):
        outer = FixtureTree.open(prefix="release-archive-container", label="release archive container")
        self.addCleanup(outer.dispose)
        fixture = archive.ReleaseArchiveVerificationTests(parent=outer.root, containing=(outer,))
        fault = StartFault(delegate_version, subprocess.TimeoutExpired(["delegate"], 0))
        self.verify_with(fixture, fault, CommandFailure.TIMEOUT)
        self.assertEqual(fixture.root.parent, outer.root)
        self.assertIsNotNone(outer.unresolved, "a containing owner must observe the placed fixture child")
        self.assertIs(outer.unresolved, fixture.tree.unresolved)
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertFalse(outer.dispose())
        self.assertTrue(fixture.root.is_dir())
        self.assertEqual(open_descriptors(), self.baseline)
        self.assertTrue(fault.closed())
        fixture.tree.remove_retained()
        outer.remove_retained()
        self.assertFalse(outer.root.exists())

    def test_closed_child_failure_removes_the_fixture(self):
        fixture = archive.ReleaseArchiveVerificationTests()
        fault = StartFault(delegate_version, FileNotFoundError("delegate"))
        self.verify_with(fixture, fault, CommandFailure.START_FAILED)
        self.assertIsNone(fixture.tree.unresolved)
        self.assertFalse(fixture.root.exists())
        self.assertTrue(fixture.tree.dispose())
        self.assertEqual(open_descriptors(), self.baseline)
        self.assertTrue(fault.closed())



class PreparedCleanupFailureTests(unittest.TestCase):
    """``prepared()`` keeps a cleanup fault's identity and never lets it replace an earlier failure.

    No child launches here; the refusing launch recorder proves it. The fixture tree is disposed for
    real before an injected fault is raised, so every control also proves the remaining callbacks
    ran and every pin was consumed.
    """

    def setUp(self):
        self.baseline = open_descriptors()
        self.environment = dict(os.environ)
        self.runner = archive.release_commands.run_bounded_command
        self.fixture = archive.ReleaseArchiveVerificationTests()
        self.cleanup_fault = OSError(errno.EIO, "release archive fixture cleanup fault")
        self.faulted: list[FixtureTree] = []
        self.launches: list[object] = []
        launches = patch.object(subprocess, "Popen", side_effect=self.refuse_launch)
        launches.start()
        self.addCleanup(launches.stop)

    def refuse_launch(self, argv: object, **options: object) -> None:
        self.launches.append(argv)
        raise AssertionError("a cleanup control must not launch a child")

    @contextlib.contextmanager
    def failing_dispose(self) -> Generator[None]:
        """Each archive fixture tree is disposed for real, then reports ``cleanup_fault``."""
        dispose = FixtureTree.dispose

        def dispose_then_fail(tree: FixtureTree) -> bool:
            removed = dispose(tree)
            if tree.label == "release archive fixture":
                self.faulted.append(tree)
                raise self.cleanup_fault
            return removed

        with patch.object(FixtureTree, "dispose", dispose_then_fail):
            yield

    def failure_of(self, body: Callable[[], None]) -> BaseException:
        with self.failing_dispose():
            try:
                with self.fixture.prepared():
                    body()
            except BaseException as error:
                return error
        self.fail("prepared() hid the cleanup fault")

    def assertEveryCallbackRan(self) -> None:
        self.assertEqual(self.faulted, [self.fixture.tree])
        self.assertFixtureCleanedOnce()

    def assertFixtureCleanedOnce(self) -> None:
        """Every fixture cleanup ran exactly once, and a later unittest backstop repeats none of them."""
        self.assertEqual(self.launches, [])
        self.assertFalse(self.fixture.root.exists())
        self.assertEqual(dict(os.environ), self.environment)
        self.assertIs(archive.release_commands.run_bounded_command, self.runner)
        self.assertEqual(open_descriptors(), self.baseline)
        faulted = list(self.faulted)
        with self.failing_dispose():
            self.assertTrue(self.fixture.doCleanups())
        self.assertEqual(self.faulted, faulted)

    def assertCleanupFault(self, error: BaseException | None, frame: str = "dispose_then_fail") -> None:
        self.assertIs(error, self.cleanup_fault)
        self.assertEqual(self.cleanup_fault.errno, errno.EIO)
        frames = [entry.name for entry in traceback.extract_tb(self.cleanup_fault.__traceback__)]
        self.assertIn(frame, frames)

    def test_cleanup_only_failure_propagates_the_actual_error(self):
        error = self.failure_of(lambda: None)
        self.assertIs(error, self.cleanup_fault, "a cleanup-only failure must propagate the actual error")
        self.assertCleanupFault(error)
        self.assertEveryCallbackRan()

    def test_setup_failure_stays_primary_and_chains_the_cleanup_fault(self):
        setup_fault = RuntimeError("release archive fixture setUp fault")
        with patch.object(archive.verifier, "expected_archive", side_effect=setup_fault):
            error = self.failure_of(lambda: None)
        self.assertIs(error, setup_fault, "a setUp failure must stay the primary error")
        self.assertCleanupFault(error.__cause__)
        self.assertEveryCallbackRan()

    def test_body_failure_stays_primary_and_keeps_its_own_cause(self):
        cause = KeyError("body cause")
        body_fault = AssertionError("fixture body fault")

        def body() -> None:
            raise body_fault from cause

        error = self.failure_of(body)
        self.assertIs(error, body_fault, "a body failure must stay the primary error")
        group = error.__cause__
        self.assertIsInstance(group, BaseExceptionGroup)
        assert isinstance(group, BaseExceptionGroup)
        self.assertEqual(len(group.exceptions), 2)
        self.assertIs(group.exceptions[0], cause)
        self.assertCleanupFault(group.exceptions[1])
        self.assertEveryCallbackRan()

    def test_a_failing_callback_still_lets_every_later_callback_run(self):
        later: list[str] = []
        attempts: list[str] = []

        def fail_first() -> None:
            attempts.append("fail_first")
            raise self.cleanup_fault

        try:
            with self.fixture.prepared():
                # Last in, first out: the failing callback runs before the sentinel and the fixture's own.
                self.fixture.cleanup.callback(later.append, "sentinel")
                self.fixture.cleanup.callback(fail_first)
        except BaseException as error:
            raised = error
        else:
            self.fail("prepared() hid the cleanup fault")
        self.assertEqual(later, ["sentinel"], "a callback after a failing one must still run")
        self.assertIs(raised, self.cleanup_fault)
        self.assertCleanupFault(raised, frame="fail_first")
        self.assertEqual(self.faulted, [])
        self.assertFixtureCleanedOnce()
        self.assertEqual((attempts, later), (["fail_first"], ["sentinel"]))


class SerializationThreadClosureTests(unittest.TestCase):
    """The serialization test closes every writer thread it started before its receipt patch ends.

    Each control runs the real test method in a private fixture. Every child start is refused, so
    no real child exists; each writer finishes the verifier, then waits on ``hold``. Recorded
    threads are released and joined independently before the fixture is torn down, and a retained
    tree is removed only after none is alive and nothing launched. A raising join is a controlled
    interrupt from the recorded thread class, which leaves its writer held for the oracle.
    """

    METHOD = "test_release_receipt_serializes_replay_through_terminal_publication"
    STAGE = "release-fixture-thread-join"

    def setUp(self):
        self.baseline = open_descriptors()
        self.hold = threading.Event()
        self.threads: list[threading.Thread] = []
        self.joins: list[tuple[str, float | None]] = []
        self.patched_when_finished: dict[str, bool] = {}
        self.expire_joins = False
        self.join_faults: dict[str, BaseException] = {}
        self.observed: tuple[BaseException | None, ...] = ()

    def recording_thread(self) -> type[threading.Thread]:
        """A real thread class that records each start and join; a faulted or expired join returns at once."""
        control, real = self, threading.Thread

        class Recorded(real):
            def start(self) -> None:
                super().start()
                control.threads.append(self)

            def join(self, timeout: float | None = None) -> None:
                control.joins.append((self.name, timeout))
                if self.name in control.join_faults:
                    raise control.join_faults[self.name]
                if not control.expire_joins:
                    super().join(timeout)

        return Recorded

    def held_verify(self, fixture: archive.ReleaseArchiveVerificationTests) -> Callable[[], int]:
        verify = fixture.verify

        def held() -> int:
            result = verify()
            self.patched_when_finished[threading.current_thread().name] = isinstance(
                archive.receipt_files.write_receipt, Mock)
            self.hold.wait(30)
            return result

        return held

    def run_method(self, fixture: archive.ReleaseArchiveVerificationTests, *,
                   early_failure: AssertionError | None = None) -> tuple[BaseException | None, list[str]]:
        """Run the real method; return what it raised and which recorded threads outlived it.

        ``observed`` keeps each owner's unresolved fact as the method left it, before the oracle.
        """
        patcher = patch.object(fixture, "verify", self.held_verify(fixture))
        patcher.start()
        self.addCleanup(patcher.stop)
        if early_failure is not None:
            fixture.assertTrue = Mock(side_effect=early_failure)
        fault = StartFault(lambda _argv: True, FileNotFoundError("refused fixture child"))
        raised: BaseException | None = None
        with contextlib.redirect_stderr(io.StringIO()), fixture.prepared():
            try:
                with patch.object(subprocess, "Popen", side_effect=fault), \
                        patch.object(archive.threading, "Thread", self.recording_thread()):
                    try:
                        getattr(fixture, self.METHOD)()
                    except BaseException as error:
                        raised = error
                    self.observed = tuple(tree.unresolved for tree in (fixture.tree, *fixture.containing))
                    alive = [thread.name for thread in self.threads if thread.is_alive()]
            finally:
                self.hold.set()
                for thread in self.threads:
                    threading.Thread.join(thread, 30)
        self.assertEqual([thread.name for thread in self.threads if thread.is_alive()], [])
        self.assertEqual(fault.launched, [])
        return raised, alive

    def remove_after_closure(self, *trees: FixtureTree) -> None:
        """Every recorded thread was joined and no child launched; only then go the retained trees."""
        self.assertFalse(any(thread.is_alive() for thread in self.threads))
        for tree in trees:
            self.assertTrue(tree.root.is_dir(), f"{tree.label} must be retained until closure is proven")
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertFalse(tree.dispose())
            tree.remove_retained()
        self.assertEqual(open_descriptors(), self.baseline)

    def test_an_early_assertion_still_closes_every_started_thread_inside_the_patch(self):
        fixture = archive.ReleaseArchiveVerificationTests()
        early = AssertionError("forced early assertion")
        self.hold.set()
        raised, alive = self.run_method(fixture, early_failure=early)
        self.assertIs(raised, early)
        self.assertEqual(alive, [], "an early assertion must not leave a started writer running")
        self.assertEqual([thread.name for thread in self.threads], ["release-first"])
        self.assertEqual([name for name, _timeout in self.joins], ["release-first"])
        self.assertEqual(self.patched_when_finished, {"release-first": True})
        self.assertIsNone(fixture.tree.unresolved)
        self.assertFalse(fixture.root.exists())
        self.assertEqual(open_descriptors(), self.baseline)

    def test_an_unproven_join_is_observed_on_both_owners_before_teardown(self):
        outer = FixtureTree.open(prefix="release-archive-thread-container", label="release archive thread container")
        self.addCleanup(outer.dispose)
        fixture = archive.ReleaseArchiveVerificationTests(parent=outer.root, containing=(outer,))
        self.expire_joins = True
        raised, _alive = self.run_method(fixture)
        self.assertIsInstance(raised, archive.verifier.VerificationError)
        assert isinstance(raised, archive.verifier.VerificationError)
        self.assertEqual((raised.code, raised.stage), (CommandFailure.TIMEOUT.value, self.STAGE))
        self.assertIs(fixture.tree.unresolved, raised)
        self.assertIs(outer.unresolved, raised)
        self.assertTrue(all(timeout is not None for _name, timeout in self.joins), self.joins)
        self.remove_after_closure(fixture.tree, outer)

    def test_an_early_assertion_stays_primary_over_an_unproven_join(self):
        fixture = archive.ReleaseArchiveVerificationTests()
        early = AssertionError("forced early assertion")
        self.expire_joins = True
        raised, _alive = self.run_method(fixture, early_failure=early)
        self.assertIs(raised, early)
        self.assertIsInstance(early.__cause__, archive.verifier.VerificationError)
        self.assertIs(fixture.tree.unresolved, early.__cause__)
        self.remove_after_closure(fixture.tree)

    def placed_fixture(self) -> tuple[archive.ReleaseArchiveVerificationTests, FixtureTree]:
        outer = FixtureTree.open(prefix="release-archive-join-container", label="release archive join container")
        self.addCleanup(outer.dispose)
        return archive.ReleaseArchiveVerificationTests(parent=outer.root, containing=(outer,)), outer

    def test_one_join_fault_is_the_unresolved_fact_and_every_writer_is_joined(self):
        fixture, outer = self.placed_fixture()
        fault = KeyboardInterrupt("controlled join fault")
        self.join_faults["release-first"] = fault
        raised, _alive = self.run_method(fixture)
        self.assertIs(raised, fault)
        self.assertEqual(self.observed, (fault, fault), "both owners must observe the original join fault")
        self.assertEqual([name for name, _timeout in self.joins], ["release-first", "release-second"])
        self.remove_after_closure(fixture.tree, outer)

    def test_two_join_faults_are_one_group_on_both_owners_in_join_order(self):
        fixture, outer = self.placed_fixture()
        first, second = KeyboardInterrupt("first join fault"), KeyboardInterrupt("second join fault")
        self.join_faults.update({"release-first": first, "release-second": second})
        raised, _alive = self.run_method(fixture)
        self.assertIsInstance(raised, BaseExceptionGroup)
        assert isinstance(raised, BaseExceptionGroup)
        self.assertEqual(raised.exceptions, (first, second))
        self.assertEqual(self.observed, (raised, raised))
        self.remove_after_closure(fixture.tree, outer)

    def test_an_early_assertion_stays_primary_and_is_caused_by_the_join_fault(self):
        fixture, outer = self.placed_fixture()
        early, fault = AssertionError("forced early assertion"), KeyboardInterrupt("controlled join fault")
        self.join_faults["release-first"] = fault
        raised, _alive = self.run_method(fixture, early_failure=early)
        self.assertIs(raised, early)
        self.assertIs(early.__cause__, fault)
        self.assertEqual(self.observed, (fault, fault))
        self.remove_after_closure(fixture.tree, outer)


if __name__ == "__main__":
    unittest.main()
