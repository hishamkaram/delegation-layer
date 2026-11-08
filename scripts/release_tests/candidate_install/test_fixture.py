"""Controls for the candidate install fixture's ownership: it must dispose of pinned trees correctly."""

from __future__ import annotations

from pathlib import Path
import contextlib
import io
import shutil
import sys
import unittest
from unittest import mock

repository_root = str(Path(__file__).resolve().parents[3])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.release_tests.candidate_install import fixtures as support
from scripts.release_tests.release_fixture import FixtureTree
from scripts.release_tests.release_fixture import open_descriptors
from scripts.acceptance.processes import operations as operations
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.contracts import CommandFailure
from scripts.acceptance.evidence import source as commands
from scripts.release_verification.candidate_install_contract import CHANNEL_NPM
from scripts.release_verification.candidate_install_contract import CHANNEL_SHELL
from scripts.acceptance.evidence.source import VerificationError
from scripts.test_candidate_source_support import ChildFault, settle_retained


SHELL = (CHANNEL_SHELL,)


def refuse_bad_checksum(case: support.CandidateCase) -> None:
    with case.assertRaises(VerificationError) as caught:
        case.verify(case.request(checksum="0" * 64), channels=SHELL)
    case.assertEqual(caught.exception.code, "command_failed")


def unresolved_termination(case: support.CandidateCase) -> None:
    def execute(receipt, name, argv, env, *, capture, timeout, cwd):
        raise VerificationError("command_termination_failed", name)

    case.recording.execute = execute
    with case.assertRaises(VerificationError) as caught:
        case.verify(case.request(), channels=SHELL)
    case.assertEqual(caught.exception.code, "command_termination_failed")


def unproven_default_path(fault: ChildFault):
    """Time out the first by-name default-path child, which runs outside the install ports."""
    def body(case: support.CandidateCase) -> None:
        with mock.patch.object(commands, "run_bounded_command", fault), \
                case.assertRaises(VerificationError) as caught:
            case.verify(case.request(), channels=SHELL)
        case.assertEqual((caught.exception.code, caught.exception.stage), ("command_timeout", "delegate-version"))
    return body


def unproven_then_retry(fault: ChildFault):
    """Time out the install child, then try to rewrite the inputs and to verify again."""
    def body(case: support.CandidateCase) -> None:
        with mock.patch.object(commands, "run_bounded_command", fault):
            request = case.request()
            checksums_path = request.files.checksums
            assert checksums_path is not None
            checksums = checksums_path.read_bytes()
            with case.assertRaises(VerificationError) as caught:
                case.verify(request, channels=SHELL)
            case.assertEqual((caught.exception.code, caught.exception.stage),
                             ("command_timeout", "candidate-shell-install"))
            with case.assertRaises(AssertionError):
                case.request(checksum="0" * 64)
            with case.assertRaises(AssertionError):
                case.verify(request, channels=SHELL)
        case.assertEqual(checksums_path.read_bytes(), checksums)
        case.assertEqual(len(list(case.parent.iterdir())), 1)
    return body


def unproven_with_hook(fault: ChildFault, hooks: list[str]):
    """Time out the install child of a case whose mutation hook would rewrite the checksums."""
    def body(case: support.CandidateCase) -> None:
        request = case.request()

        checksums = request.files.checksums
        assert checksums is not None

        def tamper(name: str) -> None:
            hooks.append(name)
            checksums.write_text("tampered\n")

        case.recording.after = tamper
        with mock.patch.object(commands, "run_bounded_command", fault), \
                case.assertRaises(VerificationError) as caught:
            case.verify(request, channels=SHELL)
        case.assertEqual((caught.exception.code, caught.exception.stage),
                         ("command_timeout", "candidate-shell-install"))
    return body


def displace_workspace(case: support.CandidateCase) -> None:
    case.verify(case.request(), channels=SHELL)
    case.layout.root.rename(case.layout.root.with_name("displaced"))


class Outcome:
    """What one inner test case did, observed from the outer, ordinarily disposed fixture."""

    def __init__(self, result: unittest.TestResult, case: support.CandidateCase | None,
                 stderr: str, descriptors: tuple[int, int]):
        self.result, self.case, self.stderr, self.descriptors = result, case, stderr, descriptors

    @property
    def opened_case(self) -> support.CandidateCase:
        assert self.case is not None, "case setup did not open its fixture"
        return self.case

    @property
    def root(self) -> Path:
        return self.opened_case.root


class FixtureDispositionTests(support.CandidateCase):
    def run_inner(self, body, *, setup_error: Exception | None = None, pack: FixtureTree | None = None,
                  contained: bool = True) -> Outcome:
        """Run one inner case, in this case's tree unless ``contained`` is False, with ``pack`` as its input."""
        seen: list[support.CandidateCase] = []
        outer = self.tree if contained else None

        class Inner(support.CandidateCase):
            containing_tree = outer
            pack_tree = pack

            def setUp(self):
                super().setUp()
                seen.append(self)
                if setup_error is not None:
                    raise setup_error

            def runTest(self):
                body(self)

        stderr = io.StringIO()
        before = open_descriptors()
        with contextlib.redirect_stderr(stderr):
            result = unittest.TextTestRunner(stream=io.StringIO()).run(Inner())
        return Outcome(result, seen[0] if seen else None, stderr.getvalue(), (before, open_descriptors()))

    def assertPinsConsumed(self, outcome: Outcome) -> None:
        self.assertEqual(outcome.descriptors[0], outcome.descriptors[1])

    def test_closed_refusal_consumes_pins_and_removes_the_containing_tree(self):
        outcome = self.run_inner(refuse_bad_checksum)
        self.assertTrue(outcome.result.wasSuccessful())
        self.assertFalse(outcome.root.exists())
        self.assertPinsConsumed(outcome)
        self.assertEqual(outcome.stderr, "")

    def test_failing_assertion_is_reported_unchanged_and_the_tree_is_removed(self):
        outcome = self.run_inner(lambda case: case.fail("inner assertion"))
        ((_, trace),) = outcome.result.failures
        self.assertIn("inner assertion", trace)
        self.assertEqual(outcome.result.errors, [])
        self.assertFalse(outcome.root.exists())
        self.assertPinsConsumed(outcome)

    def test_setup_failure_after_the_tree_opened_still_removes_it(self):
        outcome = self.run_inner(refuse_bad_checksum, setup_error=RuntimeError("setup failed"))
        ((_, trace),) = outcome.result.errors
        self.assertIn("setup failed", trace)
        self.assertFalse(outcome.root.exists())
        self.assertPinsConsumed(outcome)

    def test_unresolved_child_closure_retains_the_whole_tree_as_evidence(self):
        refusal = AssertionError("no child may launch in a synthetic closure fault")
        with mock.patch.object(operations.subprocess, "Popen", mock.Mock(side_effect=refusal)) as launches:
            outcome = self.run_inner(unresolved_termination)
        self.assertTrue(outcome.result.wasSuccessful())
        self.assertIn("command_termination_failed", outcome.stderr)
        self.assertIn(str(outcome.root), outcome.stderr)
        (workspace,) = outcome.opened_case.parent.iterdir()
        self.assertTrue((workspace / "shell" / "shim-requests.log").is_file())
        self.assertTrue((outcome.root / "candidate" / "checksums.txt").is_file())
        self.assertPinsConsumed(outcome)
        # The containing tree keeps the same fact, so its own cleanup cannot remove the retained child.
        self.assertIs(self.tree.unresolved, outcome.opened_case.tree.unresolved)
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertFalse(self.tree.dispose())
        self.assertTrue(outcome.root.is_dir())
        # The proof: the synthetic fault launched nothing.
        self.assertEqual(launches.call_count, 0)
        self.tree.remove_retained()
        self.assertFalse(outcome.root.exists())

    def test_an_unproven_default_path_child_retains_the_case_and_containing_trees(self):
        fault = ChildFault(lambda argv: argv[:1] == ["delegate"])
        outcome = self.run_inner(unproven_default_path(fault))
        self.assertTrue(outcome.result.wasSuccessful())
        unresolved = outcome.opened_case.tree.unresolved
        self.assertIs(type(unresolved), BoundedCommandError)
        assert isinstance(unresolved, BoundedCommandError)
        self.assertIs(unresolved.reason, CommandFailure.TIMEOUT)
        self.assertIs(self.tree.unresolved, unresolved)
        self.assertIn(f"(command_timeout): {outcome.root}", outcome.stderr)
        (workspace,) = outcome.opened_case.parent.iterdir()
        self.assertTrue((workspace / "shell" / "bin" / "delegate").is_file())
        self.assertPinsConsumed(outcome)
        self.assertGreater(fault.launched, 0)
        settle_retained(self, self.tree, fault, (workspace, outcome.root))

    def test_an_unresolved_shared_input_refuses_a_retry_and_the_next_case_before_any_write(self):
        pack = FixtureTree.open(prefix=support.TREE_PREFIX, label=support.TREE_LABEL)
        self.addCleanup(pack.dispose)
        package = pack.root / "private.tgz"
        package.write_bytes(b"private pack input\n")
        fault = ChildFault(lambda argv: True)
        first = self.run_inner(unproven_then_retry(fault), pack=pack, contained=False)
        fact = pack.unresolved
        second = self.run_inner(unproven_then_retry(fault), pack=pack, contained=False)
        pack_files = {path.name: path.read_bytes() for path in pack.root.iterdir()}
        # The proof: no child ever launched, so every retained tree may go.
        self.assertEqual((fault.launched, fault.unclosed), (0, 0))
        for tree in [outcome.opened_case.tree for outcome in (first, second) if outcome.case is not None] + [pack]:
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertFalse(tree.dispose())
            tree.remove_retained()
        self.assertEqual([trace for _, trace in first.result.failures + first.result.errors], [])
        self.assertIs(type(fact), BoundedCommandError)
        self.assertIs(first.opened_case.tree.unresolved, fact)
        ((_, trace),) = second.result.failures
        self.assertEqual(second.result.errors, [])
        self.assertIn(f"{support.TREE_LABEL} holds an unresolved child: {pack.root}", trace)
        self.assertIsNone(second.case)
        self.assertEqual(len(fault.faulted), 1)
        self.assertIs(pack.unresolved, fact)
        self.assertEqual(pack_files, {"private.tgz": b"private pack input\n"})
        self.assertPinsConsumed(first)
        self.assertPinsConsumed(second)

    def test_a_mutation_hook_never_runs_after_an_unresolved_child(self):
        fault, hooks = ChildFault(lambda argv: True), []
        outcome = self.run_inner(unproven_with_hook(fault, hooks))
        checksums = (outcome.root / "candidate" / "checksums.txt").read_bytes()
        settle_retained(self, self.tree, fault, (outcome.root,))
        self.assertTrue(outcome.result.wasSuccessful())
        self.assertEqual(hooks, [])
        self.assertNotEqual(checksums, b"tampered\n")
        self.assertIs(type(outcome.opened_case.tree.unresolved), BoundedCommandError)
        self.assertIs(self.tree.unresolved, outcome.opened_case.tree.unresolved)
        self.assertPinsConsumed(outcome)

    def test_cleanup_failure_is_reported_and_retains_the_tree(self):
        outcome = self.run_inner(displace_workspace)
        ((_, trace),) = outcome.result.errors
        self.assertIn("directory rollback failed", trace)
        self.assertTrue((outcome.opened_case.parent / "displaced" / "shell").is_dir())
        self.assertTrue(outcome.root.exists())
        self.assertPinsConsumed(outcome)
        self.finish_workspace()


@unittest.skipUnless(shutil.which("npm") and shutil.which("node"), "npm and node are required for the npm candidate channel")
class SharedPackTests(support.PackedCandidateCase):
    """The class's packed TGZ is every npm child's input, so its tree keeps their unresolved facts.

    The one test here proves its fault launched nothing, then removes the class's retained pack tree.
    """

    def test_an_unproven_npm_child_retains_the_case_and_the_shared_pack_trees(self):
        fault = ChildFault(lambda argv: "install-cli" in argv)
        with mock.patch.object(commands, "run_bounded_command", fault), \
                self.assertRaises(VerificationError) as caught:
            self.verify(self.request(), channels=(CHANNEL_NPM,))
        self.assertEqual((caught.exception.code, caught.exception.stage),
                         ("command_timeout", "candidate-npm-install-cli"))
        settle_retained(self, self.tree, fault, (self.layout.root,))
        self.assertIs(self.pack_tree.unresolved, self.tree.unresolved)
        self.assertIs(type(self.tree.unresolved), BoundedCommandError)
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertFalse(self.pack_tree.dispose())
        self.assertTrue(self.package.is_file())
        self.pack_tree.remove_retained()
        self.assertFalse(self.pack_tree.root.exists())


if __name__ == "__main__":
    unittest.main()
