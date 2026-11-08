"""Build finalization: closure-based retention, independent owner settlement and terminal receipts.

These tests run the builder in process over a dedicated fixture repository.
Closure faults are finite fake owner results raised by the bounded command owner
before the stopped command starts; no test leaves a real child running. The
build's original failure and terminal receipt reach the case and fixture owners
exactly as for the builder CLI. Every case also proves that all directory pins
were consumed by comparing open descriptor counts.
"""

from collections.abc import Generator
import contextlib
from contextlib import contextmanager
from dataclasses import replace
import errno
import io
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import test_candidate_build_support as support
from scripts.release_tests.release_fixture import FixtureTree
from scripts.acceptance.testing.resources import open_descriptors
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.processes import operations as operations
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.contracts import CommandFailure
from scripts.release_verification import candidate_build as candidate_build
from scripts.release_verification import candidate_build_finalize as candidate_build_finalize
from scripts.acceptance.evidence import source as commands
from scripts.release_verification import filesystem as filesystem
from scripts.release_verification.candidate_build import BuildPaths
from scripts.release_verification.candidate_build import build_candidate
from scripts.release_verification.candidate_build_plan import BuildRequest
from scripts.release_verification.candidate_manifest import expected_inventory
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.published_contract import UNRESOLVED_OWNERSHIP_CODES


RENDERER = "render-homebrew-formula.sh"
PAYLOAD_FILES = sorted(path for path, _kind, _os, _arch in expected_inventory(support.VERSION))
BEFORE_RENDER = sorted(name for name in PAYLOAD_FILES if name != "delegation-layer.rb")
CREATED_DIRECTORIES = {"work", "clone", "npm", "payload", "extract-Darwin-amd64", "extract-Darwin-arm64",
                       "extract-Linux-amd64", "extract-Linux-arm64"}


class InProcessCase(unittest.TestCase):
    """A dedicated fixture and case per test, so a synthetic unresolved fault never retains a shared owner.

    The fixture setup and every build launch children only through the bounded command
    owner, which a ledger wraps from the start of setUp until settlement: it refuses the
    stopped command before it starts and counts each real launch and each normal return,
    the canonical runner's exact Wait. A retained tree is removed by its created identity
    only once every launch was waited.
    """

    def setUp(self):
        descriptors = open_descriptors()
        self.addCleanup(lambda: self.assertEqual(open_descriptors(), descriptors))
        self.launched = self.waited = 0
        self.stop: tuple[str, CommandFailure] | None = None
        self.real_command = commands.run_bounded_command
        # Installed before the fixture's own Git and removed only after settlement used the counts.
        ledger = patch.object(commands, "run_bounded_command", side_effect=self.ledger)
        ledger.start()
        self.addCleanup(ledger.stop)
        self.owners: list[FixtureTree] = []
        self.addCleanup(self.settle)
        self.outer = FixtureTree.open(prefix="candidate-finalization-fixture",
                                      label="candidate finalization fixture")
        self.owners.append(self.outer)
        self.fixture = support.CandidateFixture(self.outer)
        self.case = FixtureTree.open(self.outer.root, prefix="candidate-finalization-case",
                                     label="candidate finalization case")
        self.owners.insert(0, self.case)
        self.scratch = self.case.root

    def settle(self):
        """Dispose both owners; remove a retained tree only after every real launch was waited."""
        report = io.StringIO()
        with contextlib.redirect_stderr(report):
            retained = [tree for tree in self.owners if not tree.dispose()]
        if retained and self.launched != self.waited:
            raise self.failureException(f"{self.launched - self.waited} real launch(es) without a Wait; "
                                        f"retained: {report.getvalue()}")
        for tree in retained:
            tree.remove_retained()

    def attempt(self, name: str) -> BuildPaths:
        self.fixture.require_resolved("build attempt directory")
        directory = self.scratch / name
        directory.mkdir()
        return BuildPaths(
            source_root=self.fixture.repository, work_dir=directory / "work",
            output_dir=directory / "payload", receipt_path=directory / "receipt.json",
            goreleaser=self.fixture.tools / "bin" / "goreleaser", python=sys.executable)

    def build_in_process(self, paths: BuildPaths):
        request = BuildRequest(version=support.VERSION, commit=self.fixture.head,
                               base_commit=self.fixture.base, repository="hishamkaram/delegation-layer")
        return self.fixture.run_in_process(
            self.case, lambda: build_candidate(request, paths, self.fixture.environment()), paths.receipt_path)

    def ledger(self, argv, *arguments, **keywords):
        if self.stop is not None and any(self.stop[0] in word for word in argv):
            raise BoundedCommandError(self.stop[1])
        self.launched += 1
        result = self.real_command(argv, *arguments, **keywords)
        self.waited += 1
        return result

    @contextmanager
    def stop_command(self, fragment: str, failure: CommandFailure) -> Generator[None, None, None]:
        """While active, refuse every matching command before it starts."""
        self.stop = (fragment, failure)
        try:
            yield
        finally:
            self.stop = None

    def fail_rollback(self, failing_call: int):
        """Run the real rollback, then report a cleanup fault on the numbered call."""
        real = DirectoryOwner.rollback
        calls = []

        def rollback(owner):
            calls.append(owner)
            real(owner)
            if len(calls) == failing_call:
                raise RuntimeError("fixture cleanup fault")
            return True

        return patch.object(DirectoryOwner, "rollback", autospec=True, side_effect=rollback)

    @contextmanager
    def fail_receipt_replace(self, paths: BuildPaths) -> Generator[list[str], None, None]:
        """Fail the real terminal receipt's atomic replace with ENOSPC; every other replace runs."""
        real = filesystem.os.replace
        refused: list[str] = []

        def replace_file(source, destination, *arguments, **keywords):
            if Path(destination) == paths.receipt_path:
                refused.append(Path(source).name)
                raise OSError(errno.ENOSPC, "fixture receipt device full")
            return real(source, destination, *arguments, **keywords)

        with patch.object(filesystem.os, "replace", side_effect=replace_file):
            yield refused

    def receipt_of(self, paths: BuildPaths) -> dict:
        return json.loads(paths.receipt_path.read_text())

    def names(self, directory: Path) -> list[str]:
        return sorted(path.name for path in directory.iterdir())

    def observed_codes(self) -> list[str | None]:
        """Each owner's first unresolved code: the case's, then its containing fixture's."""
        return [fact.code if isinstance(fact, VerificationError) else None
                for fact in (self.case.unresolved, self.outer.unresolved)]


class SuccessFinalizationTests(InProcessCase):
    def test_success_removes_scratch_retains_payload_and_records_every_creation(self):
        paths = self.attempt("success")
        before = open_descriptors()
        manifest = self.build_in_process(paths)
        self.assertEqual(open_descriptors(), before)
        self.assertEqual(manifest.version, support.VERSION)
        self.assertFalse(paths.work_dir.exists())
        self.assertEqual(self.names(paths.output_dir), sorted([*PAYLOAD_FILES, "candidate-manifest.json"]))
        receipt = self.receipt_of(paths)
        self.assertEqual((receipt["verdict"], receipt["stage"]), ("pass", "complete"))
        self.assertEqual(receipt["cleanup"]["dispositions"], {"scratch": "removed", "payload": "retained"})
        self.assertEqual((receipt["cleanup"]["status"], receipt["cleanup"]["errors"]), ("settled", []))
        created = {Path(path).name for path in receipt["cleanup"]["directory_identities"]}
        self.assertEqual(created, CREATED_DIRECTORIES)
        for identity in receipt["cleanup"]["directory_identities"].values():
            self.assertTrue({"device", "inode", "parent_device", "parent_inode"} <= set(identity))


class ClosureRetentionTests(InProcessCase):
    """Each unresolved code has its own test, so every code gets fresh owners from setUp."""

    RETAINS = "_retains_both_trees_with_their_evidence"

    def check_unresolved_code(self, code: str) -> None:
        paths = self.attempt(code)
        before = open_descriptors()
        with self.stop_command(RENDERER, CommandFailure(code)):
            with self.assertRaises(VerificationError) as caught:
                self.build_in_process(paths)
        self.assertEqual(caught.exception.code, code)
        first = caught.exception
        self.assertEqual((self.case.unresolved, self.outer.unresolved), (first, first))
        self.assertEqual(open_descriptors(), before)
        self.assertEqual(self.names(paths.output_dir), BEFORE_RENDER)
        self.assertTrue((paths.work_dir / "clone" / ".git").is_dir())
        receipt = self.receipt_of(paths)
        self.assertEqual((receipt["verdict"], receipt["failure"]["code"]), ("fail", code))
        self.assertEqual(receipt["cleanup"]["dispositions"],
                         {"payload": "retained", "scratch": "retained"})
        self.assertEqual((receipt["cleanup"]["status"], receipt["cleanup"]["errors"]), ("retained", []))
        self.assertIn(str(paths.output_dir.resolve()), receipt["cleanup"]["directory_identities"])

    def test_command_capture_unavailable_retains_both_trees_with_their_evidence(self):
        self.check_unresolved_code("command_capture_unavailable")

    def test_command_termination_failed_retains_both_trees_with_their_evidence(self):
        self.check_unresolved_code("command_termination_failed")

    def test_command_termination_timeout_retains_both_trees_with_their_evidence(self):
        self.check_unresolved_code("command_termination_timeout")

    def test_command_timeout_retains_both_trees_with_their_evidence(self):
        self.check_unresolved_code("command_timeout")

    def test_closed_failure_removes_both_trees_and_consumes_the_pins(self):
        paths = self.attempt("closed")
        before = open_descriptors()
        with self.stop_command(RENDERER, CommandFailure.START_FAILED):
            with self.assertRaises(VerificationError) as caught:
                self.build_in_process(paths)
        self.assertEqual(caught.exception.code, "command_start_failed")
        self.assertEqual(self.observed_codes(), [None, None])
        self.assertEqual(open_descriptors(), before)
        self.assertFalse(paths.work_dir.exists())
        self.assertFalse(paths.output_dir.exists())
        receipt = self.receipt_of(paths)
        self.assertEqual(receipt["cleanup"]["dispositions"], {"payload": "removed", "scratch": "removed"})
        self.assertEqual(receipt["cleanup"]["status"], "settled")

    def test_unexpected_exception_retains_both_trees_and_still_writes_the_receipt(self):
        paths = self.attempt("unexpected")
        before = open_descriptors()
        with patch.object(candidate_build, "render_formula", side_effect=RuntimeError("fixture defect")):
            with self.assertRaisesRegex(RuntimeError, "fixture defect"):
                self.build_in_process(paths)
        self.assertEqual(self.observed_codes(), ["command_termination_failed"] * 2)
        self.assertEqual(open_descriptors(), before)
        self.assertEqual(self.names(paths.output_dir), BEFORE_RENDER)
        self.assertTrue(paths.work_dir.is_dir())
        receipt = self.receipt_of(paths)
        self.assertEqual(receipt["failure"], {"code": "build_unexpected_error", "stage": "build-candidate",
                                              "exception": "RuntimeError"})
        self.assertEqual(receipt["cleanup"]["dispositions"], {"payload": "retained", "scratch": "retained"})


class SettlementFaultTests(InProcessCase):
    def test_scratch_cleanup_failure_after_success_fails_the_build_and_removes_the_payload(self):
        paths = self.attempt("scratch-fault")
        before = open_descriptors()
        with self.fail_rollback(1):
            with self.assertRaises(VerificationError) as caught:
                self.build_in_process(paths)
        self.assertEqual(caught.exception.code, "build_cleanup_failed")
        self.assertIn("scratch cleanup failed: fixture cleanup fault", caught.exception.__notes__)
        self.assertEqual(self.observed_codes(), ["command_termination_failed"] * 2)
        self.assertEqual(open_descriptors(), before)
        self.assertFalse(paths.output_dir.exists())
        receipt = self.receipt_of(paths)
        self.assertEqual((receipt["verdict"], receipt["failure"]["code"]), ("fail", "build_cleanup_failed"))
        self.assertEqual(receipt["cleanup"]["dispositions"], {"scratch": "failed", "payload": "removed"})
        self.assertEqual(receipt["cleanup"]["status"], "failed")

    def test_one_owner_failing_does_not_leave_the_other_unsettled(self):
        paths = self.attempt("independent")
        before = open_descriptors()
        with self.stop_command(RENDERER, CommandFailure.START_FAILED), self.fail_rollback(2):
            with self.assertRaises(VerificationError) as caught:
                self.build_in_process(paths)
        self.assertEqual(caught.exception.code, "command_start_failed")
        self.assertEqual(caught.exception.__notes__, ["scratch cleanup failed: fixture cleanup fault"])
        self.assertEqual(self.observed_codes(), ["command_termination_failed"] * 2)
        self.assertEqual(open_descriptors(), before)
        self.assertFalse(paths.output_dir.exists())
        receipt = self.receipt_of(paths)
        self.assertEqual(receipt["cleanup"]["dispositions"], {"payload": "removed", "scratch": "failed"})
        self.assertEqual(receipt["failure"]["code"], "command_start_failed")

    def test_unwritable_terminal_receipt_after_success_is_an_explicit_failure(self):
        paths = self.attempt("receipt-fault")
        unwritable = VerificationError("receipt_too_large", "write-receipt")
        with patch.object(candidate_build_finalize, "write_receipt", side_effect=unwritable):
            with self.assertRaises(VerificationError) as caught:
                self.build_in_process(paths)
        self.assertEqual(caught.exception.code, "build_receipt_unwritable")
        self.assertEqual(caught.exception.__notes__, ["build receipt was not written: receipt_too_large"])
        self.assertFalse(paths.receipt_path.exists())
        self.assertFalse(paths.work_dir.exists())

    def test_unwritable_terminal_receipt_never_replaces_the_primary_failure(self):
        paths = self.attempt("receipt-and-build-fault")
        unwritable = VerificationError("receipt_too_large", "write-receipt")
        with self.stop_command(RENDERER, CommandFailure.START_FAILED):
            with patch.object(candidate_build_finalize, "write_receipt", side_effect=unwritable):
                with self.assertRaises(VerificationError) as caught:
                    self.build_in_process(paths)
        self.assertEqual(caught.exception.code, "command_start_failed")
        self.assertEqual(caught.exception.__notes__, ["build receipt was not written: receipt_too_large"])
        self.assertFalse(paths.work_dir.exists())

    def test_receipt_os_error_after_success_is_an_explicit_receipt_failure(self):
        paths = self.attempt("receipt-os-fault")
        before = open_descriptors()
        with self.fail_receipt_replace(paths) as refused:
            with self.assertRaises((VerificationError, OSError)) as caught:
                self.build_in_process(paths)
        self.assertIsInstance(caught.exception, VerificationError)
        assert isinstance(caught.exception, VerificationError)
        self.assertEqual(caught.exception.code, "build_receipt_unwritable")
        self.assertEqual(caught.exception.__notes__,
                         ["build receipt was not written: build_receipt_write_failed"])
        self.assertEqual(len(refused), 1)
        self.assertEqual(self.observed_codes(), [None, None])
        self.assertEqual(open_descriptors(), before)
        self.assertEqual(self.names(paths.receipt_path.parent), ["payload"])

    def test_receipt_os_error_after_a_primary_failure_is_only_a_note(self):
        paths = self.attempt("receipt-os-and-build-fault")
        before = open_descriptors()
        with self.stop_command(RENDERER, CommandFailure("command_timeout")):
            with self.fail_receipt_replace(paths) as refused:
                with self.assertRaises((VerificationError, OSError)) as caught:
                    self.build_in_process(paths)
        self.assertIsInstance(caught.exception, VerificationError)
        assert isinstance(caught.exception, VerificationError)
        self.assertEqual(caught.exception.code, "command_timeout")
        self.assertEqual(caught.exception.__notes__,
                         ["build receipt was not written: build_receipt_write_failed"])
        self.assertEqual(len(refused), 1)
        first = caught.exception
        self.assertEqual((self.case.unresolved, self.outer.unresolved), (first, first))
        self.assertEqual(open_descriptors(), before)
        self.assertEqual(self.names(paths.receipt_path.parent), ["payload", "work"])
        self.assertEqual(self.names(paths.output_dir), BEFORE_RENDER)
        self.assertTrue((paths.work_dir / "clone" / ".git").is_dir())

    def test_aliasing_receipts_are_refused_before_any_directory_is_created(self):
        paths = self.attempt("alias")
        for receipt in (paths.output_dir / "candidate-manifest.json", paths.work_dir / "clone" / "r.json"):
            with self.subTest(receipt=receipt.name):
                aliased = replace(paths, receipt_path=receipt)
                with self.assertRaises(VerificationError) as caught:
                    self.build_in_process(aliased)
                self.assertEqual(caught.exception.code, "build_receipt_inside_owned_tree")
                self.assertEqual(self.names(paths.work_dir.parent), [])


class UnresolvedCodeCoverageTests(unittest.TestCase):
    def test_every_unresolved_code_has_its_own_retention_test(self):
        suffix = ClosureRetentionTests.RETAINS
        tested = {name.removeprefix("test_").removesuffix(suffix) for name in dir(ClosureRetentionTests)
                  if name.startswith("test_") and name.endswith(suffix)}
        self.assertEqual(tested, UNRESOLVED_OWNERSHIP_CODES)


class ReuseRefusalTests(InProcessCase):
    def test_attempt_beneath_an_unresolved_child_creates_nothing(self):
        failure = VerificationError("command_timeout", "fixture-synthetic")
        self.outer.observe(failure)
        with self.assertRaisesRegex(RuntimeError, "no proven closure"):
            self.attempt("refused")
        self.assertEqual(self.names(self.scratch), [])
        self.assertIs(self.outer.unresolved, failure)

    def test_attempt_on_a_resolved_fixture_creates_its_directory(self):
        paths = self.attempt("accepted")
        self.assertEqual(self.names(self.scratch), ["accepted"])
        self.assertEqual(paths.receipt_path.parent, self.scratch / "accepted")


class SetupLedgerRegressionTests(unittest.TestCase):
    """An unknown closure while the fixture is set up is a ledger launch without a Wait, so it retains.

    The delegated command owner reports the unknown closure instead of starting fixture Git, and
    Popen refuses and records launches; the retained probe tree is removed only after zero launches.
    """

    def refuse_launch(self, *arguments, **keywords):
        self.launches.append(arguments)
        raise AssertionError("a real child must not launch in a setup regression")

    def unknown_closure(self, argv, *arguments, **keywords):
        self.delegated.append(argv)
        raise BoundedCommandError(CommandFailure.TIMEOUT)

    def test_unknown_setup_git_closure_retains_the_fixture(self):
        self.launches: list[tuple] = []
        self.delegated: list[list[str]] = []

        class Probe(InProcessCase):
            def test_unreached(probe):
                probe.fail("setUp must fail before the test body")

        probe = Probe("test_unreached")
        self.addCleanup(self.remove_proven, probe)
        before = open_descriptors()
        result = unittest.TestResult()
        with patch.object(commands, "run_bounded_command", side_effect=self.unknown_closure), \
                patch.object(operations.subprocess, "Popen", side_effect=self.refuse_launch), \
                contextlib.redirect_stderr(io.StringIO()):
            probe.run(result)
        self.assertEqual((self.launches, len(self.delegated)), ([], 1))
        assert isinstance(probe.outer.unresolved, VerificationError)
        self.assertEqual(probe.outer.unresolved.code, "command_timeout")
        self.assertTrue(probe.outer.root.is_dir(), "settlement removed a fixture retained by setup")
        self.assertEqual((probe.launched, probe.waited), (1, 0))
        ((_, setup_error),) = result.errors
        self.assertIn("command_timeout", setup_error)
        ((_, settlement),) = result.failures
        self.assertIn("1 real launch(es) without a Wait", settlement)
        self.assertEqual(open_descriptors(), before)

    def remove_proven(self, probe):
        """Remove the retained probe trees only after zero launches were proven."""
        if self.launches:
            return
        for tree in probe.owners:
            if tree.root.exists() and not tree.dispose():
                tree.remove_retained()


if __name__ == "__main__":
    unittest.main()
