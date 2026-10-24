"""The candidate fixture's observed completion runs every registered cleanup and reports its failure.

Fault tests patch the canonical child-observation owner so that no child runs;
their cleanups are real ``addCleanup`` registrations run by unittest's own
``doCleanups``. They prove the helper's control flow, never a native completion.
"""

from __future__ import annotations

import unittest
from unittest.mock import patch

from scripts.acceptance_tests.native_evidence import candidate_proof_fixture as fixture
from scripts.delegate_acceptance.native_evidence.public_contract import NativeCompletion
from scripts.delegate_acceptance.native_release.contract import ReleaseError


CLEANUP_NOTE = "candidate fixture cleanup also failed"
OWNER = fixture.ChildObservationFixture


class SetupFault(Exception):
    """An injected setup failure."""


class CleanupFault(Exception):
    """An injected cleanup failure."""


class ObservedCompletionFaultTests(unittest.TestCase):
    def setUp(self) -> None:
        self.cleanups: list[str] = []

    def cleanup(self, *, fails: bool):
        def run() -> None:
            self.cleanups.append("ran")
            if fails:
                raise CleanupFault("injected cleanup fault")
        return run

    def setup(self, *, cleanup_fails: bool, error: BaseException | None):
        """A setUp that registers one real cleanup, then optionally raises ``error``.

        It starts no child, so its receipt is a placeholder the patched observer ignores.
        """
        test = self

        def set_up(owner: OWNER) -> None:
            owner.receipt = None
            owner.addCleanup(test.cleanup(fails=cleanup_fails))
            if error is not None:
                raise error
        return set_up

    def run_helper(self, *, set_up, child=None, observe=None):
        with patch.object(OWNER, "setUp", set_up), \
                patch.object(OWNER, "child", child or (lambda owner, code: {"exit_code": code})), \
                patch.object(fixture.execution, "observe_child_completion",
                             observe or (lambda receipt, name: self.fail("observation must not run"))):
            return fixture.observed_completion(0)

    def failure(self, cause: BaseException) -> SetupFault:
        try:
            raise SetupFault("injected setup fault") from cause
        except SetupFault as error:
            return error

    def test_a_failed_setup_runs_its_registered_cleanup_and_keeps_its_error(self):
        cause = OSError("setup cause")
        error = self.failure(cause)
        with self.assertRaises(SetupFault) as caught:
            self.run_helper(set_up=self.setup(cleanup_fails=False, error=error))
        self.assertIs(caught.exception, error)
        self.assertIs(caught.exception.__cause__, cause)
        self.assertEqual(self.cleanups, ["ran"])
        self.assertNotIn(CLEANUP_NOTE, getattr(caught.exception, "__notes__", []))

    def test_a_failed_setup_whose_cleanup_fails_reports_both(self):
        cause = OSError("setup cause")
        error = self.failure(cause)
        with self.assertRaises(SetupFault) as caught:
            self.run_helper(set_up=self.setup(cleanup_fails=True, error=error))
        self.assertIs(caught.exception, error)
        self.assertIs(caught.exception.__cause__, cause)
        self.assertEqual(self.cleanups, ["ran"])
        self.assertIn(CLEANUP_NOTE, getattr(caught.exception, "__notes__", []))

    def test_a_failed_observation_keeps_its_error_after_cleanup(self):
        for cleanup_fails in (False, True):
            with self.subTest(cleanup_fails=cleanup_fails):
                self.cleanups.clear()
                unproven = ReleaseError("child_natural_completion_unproven", incomplete=True)

                def observe(receipt, name):
                    raise unproven
                with self.assertRaises(ReleaseError) as caught:
                    self.run_helper(set_up=self.setup(cleanup_fails=cleanup_fails, error=None), observe=observe)
                self.assertIs(caught.exception, unproven)
                self.assertEqual(self.cleanups, ["ran"])
                self.assertEqual(CLEANUP_NOTE in getattr(caught.exception, "__notes__", []), cleanup_fails)

    def test_a_failed_child_keeps_its_error_after_cleanup(self):
        interrupted = KeyboardInterrupt()

        def child(owner, code):
            raise interrupted
        with self.assertRaises(KeyboardInterrupt) as caught:
            self.run_helper(set_up=self.setup(cleanup_fails=True, error=None), child=child)
        self.assertIs(caught.exception, interrupted)
        self.assertEqual(self.cleanups, ["ran"])
        self.assertIn(CLEANUP_NOTE, getattr(caught.exception, "__notes__", []))

    def test_a_completion_is_never_returned_after_a_failed_cleanup(self):
        observed = NativeCompletion("1" * 32, True, 0, "1" * 64, "2" * 64, "3" * 64)
        with self.assertRaises(AssertionError) as caught:
            self.run_helper(set_up=self.setup(cleanup_fails=True, error=None),
                            observe=lambda receipt, name: observed)
        self.assertEqual(str(caught.exception),
                         "candidate fixture cleanup failed; its observed completion is not returned")
        self.assertEqual(self.cleanups, ["ran"])
        self.cleanups.clear()
        self.assertIs(self.run_helper(set_up=self.setup(cleanup_fails=False, error=None),
                                      observe=lambda receipt, name: observed), observed)
        self.assertEqual(self.cleanups, ["ran"])


class ObservedCompletionTests(unittest.TestCase):
    def test_a_real_finite_child_is_observed_and_its_tree_removed(self):
        owners: list[OWNER] = []
        real_set_up = OWNER.setUp

        def record(owner: OWNER) -> None:
            owners.append(owner)
            real_set_up(owner)
        with patch.object(OWNER, "setUp", record):
            completion = fixture.observed_completion(3)
        self.assertEqual((completion.invocation_id, completion.natural_wait, completion.exit_code),
                         ("1" * 32, True, 3))
        self.assertEqual(len(owners), 1)
        self.assertFalse(owners[0].base.exists())


if __name__ == "__main__":
    unittest.main()
