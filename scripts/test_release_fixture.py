"""The release fixture tree consumes its pins and disposes of or retains its tree by closure."""

from __future__ import annotations

import contextlib
import io
import unittest
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import TypedDict, Unpack

from scripts.release_tests.release_fixture import FixtureTree
from scripts.acceptance.testing.resources import open_descriptors
from scripts.acceptance.testing.invalid_calls import invoke_invalid
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.contracts import CommandFailure
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.published_contract import UNRESOLVED_OWNERSHIP_CODES

LABEL = "release fixture test"
PREFIX = "release-fixture-test"


class ObserveOptions(TypedDict, total=False):
    dependencies: tuple[FixtureTree, ...]
    closure_required: bool


@dataclass
class Outcome:
    """What one inner test case did, observed from the outer, ordinarily disposed tree."""

    result: unittest.TestResult
    tree: FixtureTree
    stderr: str
    descriptors: tuple[int, int]


class FixtureTreeTests(unittest.TestCase):
    def setUp(self):
        # Registered first, so it runs last: any pin a test forgot to consume fails the test.
        self.addCleanup(self.assert_pins_restored, open_descriptors())
        self.outer = FixtureTree.open(prefix=PREFIX, label=LABEL)
        self.addCleanup(self.outer.dispose)

    def assert_pins_restored(self, descriptors: int) -> None:
        self.assertEqual(open_descriptors(), descriptors)

    def inner(self) -> FixtureTree:
        """A tree below the outer one, disposed (if still open) before the outer is."""
        tree = FixtureTree.open(self.outer.root, prefix=PREFIX, label=LABEL)
        self.addCleanup(tree.dispose)
        return tree

    def run_inner(self, body: Callable[[FixtureTree], None], *,
                  setup_error: Exception | None = None) -> Outcome:
        trees: list[FixtureTree] = []
        parent = self.outer.root

        class Inner(unittest.TestCase):
            def setUp(self):
                tree = FixtureTree.open(parent, prefix=PREFIX, label=LABEL)
                trees.append(tree)
                self.addCleanup(tree.dispose)
                if setup_error is not None:
                    raise setup_error

            def runTest(self):
                body(trees[0])

        stderr = io.StringIO()
        before = open_descriptors()
        with contextlib.redirect_stderr(stderr):
            result = unittest.TextTestRunner(stream=io.StringIO()).run(Inner())
        return Outcome(result, trees[0], stderr.getvalue(), (before, open_descriptors()))

    def assert_pins_consumed(self, outcome: Outcome) -> None:
        self.assertEqual(outcome.descriptors[0], outcome.descriptors[1])

    def finish_outer(self) -> None:
        self.assertTrue(self.outer.dispose())
        self.assertFalse(self.outer.root.exists())

    def test_root_is_a_new_named_directory_below_the_resolved_parent(self):
        first, second = self.inner(), self.inner()
        self.assertEqual(first.root.parent, self.outer.root)
        self.assertRegex(first.root.name, rf"^{PREFIX}-[0-9a-f]{{12}}$")
        self.assertTrue(first.root.is_dir())
        self.assertNotEqual(first.root, second.root)

    def test_closed_failures_remove_the_tree_and_consume_the_pins(self):
        def fail_closed(tree: FixtureTree) -> None:
            (tree.root / "evidence").write_text("child output")
            tree.observe(VerificationError("github_request_failed", "github-api"))

        outcome = self.run_inner(fail_closed)
        self.assertTrue(outcome.result.wasSuccessful())
        self.assertIsNone(outcome.tree.unresolved)
        self.assertFalse(outcome.tree.root.exists())
        self.assert_pins_consumed(outcome)
        self.assertEqual(outcome.stderr, "")

    def test_unknown_closure_retains_the_whole_tree_and_still_consumes_the_pins(self):
        for code in sorted(UNRESOLVED_OWNERSHIP_CODES):
            with self.subTest(code=code):
                def fail_unknown(tree: FixtureTree) -> None:
                    (tree.root / "evidence").write_text("child output")
                    tree.observe(VerificationError("github_request_failed", "github-api"))
                    tree.observe(VerificationError(code, "github-api"))
                    tree.observe(VerificationError("command_timeout", "later"))

                outcome = self.run_inner(fail_unknown)
                self.assertTrue(outcome.result.wasSuccessful())
                assert isinstance(outcome.tree.unresolved, VerificationError)
                self.assertEqual(outcome.tree.unresolved.code, code)
                self.assertEqual((outcome.tree.root / "evidence").read_text(), "child output")
                self.assertEqual(outcome.stderr, f"{LABEL} retained ({code}): {outcome.tree.root}\n")
                self.assertFalse(outcome.tree.dispose())
                self.assert_pins_consumed(outcome)
        self.finish_outer()

    def test_the_first_original_unresolved_error_is_kept_by_identity(self):
        first = VerificationError("command_timeout", "first")
        verifier = VerificationError(sorted(UNRESOLVED_OWNERSHIP_CODES)[0], "github-api")
        second = VerificationError("command_timeout", "second")

        def observe_many(tree: FixtureTree) -> None:
            tree.observe(VerificationError("github_request_failed", "closed"))
            tree.observe(VerificationError("github_request_failed", "github-api"))
            self.assertIsNone(tree.unresolved)
            tree.observe(first)
            tree.observe(verifier)
            tree.observe(second)
            self.assertIs(tree.unresolved, first)

        outcome = self.run_inner(observe_many)
        self.assertTrue(outcome.result.wasSuccessful())
        self.assertIs(outcome.tree.unresolved, first)
        self.assert_pins_consumed(outcome)

        def verifier_first(tree: FixtureTree) -> None:
            tree.observe(verifier)
            tree.observe(first)

        outcome = self.run_inner(verifier_first)
        self.assertIs(outcome.tree.unresolved, verifier)
        self.assert_pins_consumed(outcome)
        self.finish_outer()

    def test_verifier_policy_uses_canonical_ownership_codes(self):
        outcome = self.run_inner(lambda tree: tree.observe(
            VerificationError("github_request_failed", "github-api")))
        self.assertIsNone(outcome.tree.unresolved)
        for code in sorted(UNRESOLVED_OWNERSHIP_CODES):
            error = VerificationError(code, "github-api")
            outcome = self.run_inner(lambda tree, error=error: tree.observe(error))
            self.assertIs(outcome.tree.unresolved, error)
            self.assert_pins_consumed(outcome)
        self.finish_outer()

    def test_failing_assertion_and_setup_failure_are_reported_and_remove_the_tree(self):
        cases = {
            "assertion": (lambda tree: self.fail("inner assertion"), None, "failures"),
            "setup": (lambda tree: None, RuntimeError("setup failed"), "errors"),
        }
        for name, (body, setup_error, kind) in cases.items():
            with self.subTest(name=name):
                outcome = self.run_inner(body, setup_error=setup_error)
                ((_, trace),) = getattr(outcome.result, kind)
                self.assertIn("inner assertion" if kind == "failures" else "setup failed", trace)
                self.assertFalse(outcome.tree.root.exists())
                self.assert_pins_consumed(outcome)

    def test_cleanup_failure_is_reported_retains_the_tree_and_consumes_the_pins(self):
        def displace(tree: FixtureTree) -> None:
            tree.root.rename(tree.root.with_name("displaced"))

        outcome = self.run_inner(displace)
        ((_, trace),) = outcome.result.errors
        self.assertIn("directory rollback failed", trace)
        self.assertTrue((self.outer.root / "displaced").is_dir())
        self.assert_pins_consumed(outcome)
        self.finish_outer()

    def test_retained_tree_is_removed_by_identity_without_clearing_the_decision(self):
        descriptors = open_descriptors()
        tree = self.inner()
        (tree.root / "evidence").write_text("child output")
        tree.observe(VerificationError("command_termination_failed", "github-api"))
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertFalse(tree.dispose())
        tree.remove_retained()
        self.assertFalse(tree.root.exists())
        assert isinstance(tree.unresolved, VerificationError)
        self.assertEqual(tree.unresolved.code, "command_termination_failed")
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            self.assertFalse(tree.dispose())
        self.assertEqual(stderr.getvalue(), "")
        with self.assertRaises(FileNotFoundError):
            tree.remove_retained()
        self.assertEqual(open_descriptors(), descriptors)

    def test_only_an_untouched_retained_tree_can_be_removed(self):
        descriptors = open_descriptors()
        open_tree, closed, replaced = self.inner(), self.inner(), self.inner()
        closed.dispose()
        replaced.observe(VerificationError("command_timeout", "github-api"))
        with contextlib.redirect_stderr(io.StringIO()):
            replaced.dispose()
        replaced.root.rename(replaced.root.with_name("moved"))
        replaced.root.mkdir()
        (replaced.root / "impostor").write_text("keep")
        for tree in (open_tree, closed):
            with self.subTest(tree=tree.root.name), self.assertRaisesRegex(RuntimeError, "not retained"):
                tree.remove_retained()
        with self.assertRaisesRegex(RuntimeError, "replaced"):
            replaced.remove_retained()
        self.assertEqual((replaced.root / "impostor").read_text(), "keep")
        self.assertTrue(open_tree.root.is_dir())
        self.assertTrue(open_tree.dispose())
        self.assertEqual(open_descriptors(), descriptors)
        self.finish_outer()

    def test_disposition_is_decided_once(self):
        removed, retained = self.inner(), self.inner()
        retained.observe(VerificationError("command_termination_failed", "github-api"))
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            decisions = [removed.dispose(), removed.dispose(), retained.dispose(), retained.dispose()]
        self.assertEqual(decisions, [True, True, False, False])
        self.assertEqual(stderr.getvalue().count("retained"), 1)
        self.assertFalse(removed.root.exists())
        self.assertTrue(Path(retained.root).is_dir())
        self.finish_outer()

    def operate(self, tree: FixtureTree, error: BaseException,
                **options: Unpack[ObserveOptions]) -> list[int]:
        """Run one failing operation through the owner; the caller sees the very same error.

        The operation starts no child, so a retained tree it leaves is settled with zero starts proven.
        """
        calls: list[int] = []

        def operation() -> bytes:
            calls.append(1)
            raise error

        with self.assertRaises(type(error)) as caught:
            tree.observe_operation(operation, **options)
        self.assertIs(caught.exception, error)
        return calls

    def settle_zero_start_trees(self, *trees: FixtureTree) -> None:
        """Each tree is retained, then removed by identity: its operations started no child."""
        for tree in trees:
            with contextlib.redirect_stderr(io.StringIO()) as stderr:
                self.assertFalse(tree.dispose())
            self.assertEqual(stderr.getvalue(), f"{LABEL} retained ({tree.unresolved}): {tree.root}\n")
            tree.remove_retained()
            self.assertFalse(tree.root.exists())

    def test_one_run_delivers_the_identical_original_error_to_every_owner(self):
        case, shared, enclosing = self.inner(), self.inner(), self.inner()
        error = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)
        self.assertEqual(self.operate(case, error, dependencies=(shared, enclosing)), [1])
        self.assertEqual([tree.unresolved for tree in (case, shared, enclosing)], [error] * 3)
        self.assertTrue(all(tree.unresolved is error for tree in (case, shared, enclosing)))
        self.settle_zero_start_trees(case, shared, enclosing)
        self.finish_outer()

    def test_each_owner_keeps_its_own_first_unresolved_fact(self):
        case, shared, fresh = self.inner(), self.inner(), self.inner()
        earlier = VerificationError("command_timeout", "earlier child")
        shared.observe(earlier)
        later = BoundedCommandError(CommandFailure.CAPTURE_UNAVAILABLE)
        last = VerificationError("command_timeout", "child")
        self.operate(case, later, dependencies=(shared,))
        self.operate(case, last, dependencies=(fresh,))
        self.assertIs(case.unresolved, later)
        self.assertIs(shared.unresolved, earlier)
        self.assertIs(fresh.unresolved, last)
        self.settle_zero_start_trees(case, shared, fresh)
        self.finish_outer()

    def test_closed_errors_and_success_leave_every_dependency_clean(self):
        case, shared = self.inner(), self.inner()
        for error in (BoundedCommandError(CommandFailure.START_FAILED, None),
                      BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED, -9),
                      VerificationError("command_start_failed", "child"), RuntimeError("command_timeout"),
                      OSError("not a child failure"), KeyboardInterrupt()):
            with self.subTest(error=type(error).__name__, code=str(error)):
                self.assertEqual(self.operate(case, error, dependencies=(shared,)), [1])
        value, calls = {"exit_code": 0}, []
        self.assertIs(case.observe_operation(lambda: calls.append(1) or value, dependencies=(shared,)), value)
        self.assertEqual(calls, [1])
        self.assertEqual((case.unresolved, shared.unresolved), (None, None))
        self.assertEqual((case.dispose(), shared.dispose()), (True, True))
        self.finish_outer()

    def test_original_bounded_error_reaches_every_owner_before_caller_conversion(self):
        case, shared = self.inner(), self.inner()
        original = BoundedCommandError(CommandFailure.TERMINATION_FAILED)
        calls: list[int] = []

        def launch() -> bytes:
            calls.append(1)
            raise original

        def converting_consumer() -> bytes:
            try:
                return case.observe_operation(launch, dependencies=(shared,))
            except BoundedCommandError as error:
                self.assertEqual((case.unresolved, shared.unresolved), (error, error))
                raise VerificationError(error.reason.value, "fixture-cli") from error

        with self.assertRaises(VerificationError) as caught:
            converting_consumer()
        self.assertIs(caught.exception.__cause__, original)
        for tree in (case, shared):
            tree.observe(caught.exception)
            self.assertIs(tree.unresolved, original)
        self.assertEqual(calls, [1])
        self.settle_zero_start_trees(case, shared)
        self.finish_outer()

    def test_invalid_dependencies_are_refused_before_the_operation_runs(self):
        case, other, calls = self.inner(), self.inner(), []
        for dependencies in ([other], (case.owner,), (case, "tree")):
            with self.subTest(dependencies=dependencies), self.assertRaises(TypeError):
                invoke_invalid(case.observe_operation, lambda: calls.append(1), dependencies=dependencies)
        self.assertEqual(calls, [])
        self.assertEqual((case.unresolved, other.unresolved), (None, None))
        self.assertEqual((case.dispose(), other.dispose()), (True, True))
        self.finish_outer()

    def test_a_successful_operation_runs_once_and_returns_its_value(self):
        tree, value, calls = self.inner(), {"exit_code": 0}, []

        def operation() -> dict[str, int]:
            calls.append(1)
            return value

        self.assertIs(tree.observe_operation(operation), value)
        self.assertEqual(calls, [1])
        self.assertIsNone(tree.unresolved)
        self.assertTrue(tree.dispose())
        self.finish_outer()

    def test_each_family_retains_only_an_unproven_original_error(self):
        families: list[tuple[BaseException, bool]] = [
            *((BoundedCommandError(reason, 3), reason.value in UNRESOLVED_OWNERSHIP_CODES)
              for reason in CommandFailure),
            *((VerificationError(code, "child"), True) for code in sorted(UNRESOLVED_OWNERSHIP_CODES)),
            (VerificationError("command_start_failed", "child"), False),
        ]
        self.assertIn(True, [unproven for error, unproven in families if isinstance(error, BoundedCommandError)])
        self.assertIn(False, [unproven for error, unproven in families if isinstance(error, BoundedCommandError)])
        for error, unproven in families:
            with self.subTest(error=type(error).__name__, code=str(error)):
                tree = self.inner()
                self.assertEqual(self.operate(tree, error), [1])
                if not unproven:
                    self.assertIsNone(tree.unresolved)
                    self.assertTrue(tree.dispose())
                    continue
                self.assertIs(tree.unresolved, error)
                with contextlib.redirect_stderr(io.StringIO()) as stderr:
                    self.assertFalse(tree.dispose())
                self.assertEqual(stderr.getvalue(), f"{LABEL} retained ({error}): {tree.root}\n")
                tree.remove_retained()
        self.finish_outer()

    def test_the_first_unresolved_raw_command_error_is_kept_over_its_later_conversion(self):
        tree = self.inner()
        original = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)
        converted = VerificationError(original.reason.value, "child")
        self.operate(tree, BoundedCommandError(CommandFailure.START_FAILED))
        self.operate(tree, original)
        self.operate(tree, converted)
        self.assertIs(tree.unresolved, original)
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertFalse(tree.dispose())
        tree.remove_retained()
        self.finish_outer()

    def test_any_other_exception_passes_through_unobserved(self):
        tree = self.inner()
        for error in (OSError("not a child failure"), AssertionError("test refusal"), KeyboardInterrupt()):
            with self.subTest(error=type(error).__name__):
                self.assertEqual(self.operate(tree, error), [1])
                self.assertIsNone(tree.unresolved)
        self.assertTrue(tree.dispose())
        self.finish_outer()

    def settle_required_trees(self, code: str, *trees: FixtureTree) -> None:
        """Each tree is retained under ``code`` only, then removed by identity: no child was started."""
        for tree in trees:
            with contextlib.redirect_stderr(io.StringIO()) as stderr:
                self.assertFalse(tree.dispose())
            self.assertEqual(stderr.getvalue(), f"{LABEL} retained ({code}): {tree.root}\n")
            tree.remove_retained()
            self.assertFalse(tree.root.exists())

    def test_a_required_closure_keeps_any_original_exception_on_every_owner(self):
        failures: list[tuple[BaseException, str]] = [
            (VerificationError("release_receipt_invalid", "receipt"), "release_receipt_invalid"),
            (VerificationError("command_start_failed", "child"), "command_start_failed"),
            (BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED, 3), "command_output_exceeded"),
            (OSError("receipt unreadable"), "OSError"),
            (ValueError("receipt malformed"), "ValueError"),
            (AssertionError("test refusal"), "AssertionError"),
            (KeyboardInterrupt(), "KeyboardInterrupt"),
            (SystemExit(3), "SystemExit"),
            (GeneratorExit(), "GeneratorExit"),
            (BaseException("bare"), "BaseException"),
        ]
        for error, code in failures:
            with self.subTest(error=type(error).__name__):
                case, shared, args = self.inner(), self.inner(), error.args
                self.assertEqual(self.operate(case, error, dependencies=(shared,), closure_required=True), [1])
                self.assertIs(case.unresolved, error)
                self.assertIs(shared.unresolved, error)
                self.assertEqual((error.args, error.__cause__), (args, None))
                self.settle_required_trees(code, case, shared)
        self.finish_outer()

    def test_a_required_closure_never_replaces_an_earlier_fact_and_success_never_clears_one(self):
        case, shared, fresh = self.inner(), self.inner(), self.inner()
        closed = VerificationError("command_start_failed", "earlier closed child")
        self.operate(case, closed, dependencies=(shared,))
        self.assertEqual((case.unresolved, shared.unresolved), (None, None))
        earlier = VerificationError("release_receipt_invalid", "receipt")
        self.operate(case, earlier, dependencies=(shared,), closure_required=True)
        self.assertIs(case.unresolved, earlier)
        later = OSError("later required failure")
        self.operate(case, later, dependencies=(shared, fresh), closure_required=True)
        self.operate(case, BoundedCommandError(CommandFailure.TIMEOUT), dependencies=(fresh,))
        self.assertEqual(case.observe_operation(lambda: 7, dependencies=(shared, fresh), closure_required=True), 7)
        self.assertEqual(case.observe_operation(lambda: 8, dependencies=(shared, fresh)), 8)
        self.assertEqual([tree.unresolved for tree in (case, shared, fresh)], [earlier, earlier, later])
        self.assertIs(fresh.unresolved, later)
        self.settle_required_trees("release_receipt_invalid", case, shared)
        self.settle_required_trees("OSError", fresh)
        self.finish_outer()

    def test_the_default_context_ignores_every_non_child_exception_directly_and_through_an_operation(self):
        case, shared = self.inner(), self.inner()
        for error in (OSError("not a child failure"), ValueError("malformed"), AssertionError("refusal"),
                      KeyboardInterrupt(), SystemExit(3), GeneratorExit(), BaseException("bare"),
                      RuntimeError("receipt invalid")):
            with self.subTest(error=type(error).__name__):
                case.observe(error)
                shared.observe(error, closure_required=False)
                self.assertEqual(self.operate(case, error, dependencies=(shared,)), [1])
                self.assertEqual((case.unresolved, shared.unresolved), (None, None))
        self.assertEqual((case.dispose(), shared.dispose()), (True, True))
        self.finish_outer()

    def test_an_invalid_closure_flag_is_refused_before_the_operation_or_observation(self):
        case, other, calls = self.inner(), self.inner(), []
        error = VerificationError("command_timeout", "child")
        for flag in (1, 0, "true", None, 1.0):
            with self.subTest(flag=flag):
                with self.assertRaises(TypeError):
                    invoke_invalid(case.observe_operation, lambda: calls.append(1), dependencies=(other,), closure_required=flag)
                with self.assertRaises(TypeError):
                    invoke_invalid(case.observe, error, closure_required=flag)
        for value in ("child_natural_completion_unproven", None):
            with self.subTest(error=value), self.assertRaises(TypeError):
                invoke_invalid(case.observe, value, closure_required=True)
        self.assertEqual(calls, [])
        self.assertEqual((case.unresolved, other.unresolved), (None, None))
        case.observe(error)
        with self.assertRaises(TypeError):
            invoke_invalid(case.observe, error, closure_required=1)
        self.assertIs(case.unresolved, error)
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertFalse(case.dispose())
        case.remove_retained()
        self.assertTrue(other.dispose())
        self.finish_outer()

    def test_the_retention_diagnostic_names_only_the_type_of_an_untyped_original(self):
        tree = self.inner()
        secret = "token=ghp_fixture-secret /private/fixture/path"
        error = OSError(13, secret, "/private/fixture/receipt.json")
        self.operate(tree, error, closure_required=True)
        self.assertIs(tree.unresolved, error)
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            self.assertFalse(tree.dispose())
        self.assertEqual(stderr.getvalue(), f"{LABEL} retained (PermissionError): {tree.root}\n")
        self.assertNotIn("secret", stderr.getvalue())
        self.assertNotIn("/private/fixture", stderr.getvalue())
        tree.remove_retained()
        self.finish_outer()


if __name__ == "__main__":
    unittest.main()
