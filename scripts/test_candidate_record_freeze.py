"""The freeze command over the production store and replay, observing a real Git checkout.

The ready history is committed to the in-memory ``FakeGitHub`` store as one
transaction per canonical group; every freeze record and finding expected
here comes from ``candidate_readiness`` over the facts the store reads back.
Races are modelled by another candidate's commit moving the branch at a
chosen point; the store itself is never asked to retry.
"""

from __future__ import annotations

import contextlib
import io
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification import candidate_store_replay, commands
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    PROVIDER_FAMILIES, ChangeScope, MatrixScope, encode_review,
)
from scripts.delegate_acceptance.release_verification.candidate_history import encode_publication
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    SourceObservation, authorize_publication, evaluate_readiness,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import TransitionKind
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_record_support import RecordCase
from scripts.test_candidate_source_support import ChildFault, settle_retained
from scripts.test_candidate_store_support import BRANCH


AFFECTED = ChangeScope(matrix=MatrixScope.AFFECTED_PROVIDERS, changed_providers=(PROVIDER_FAMILIES[0],),
                       provider_imports=())


def codes(readiness) -> set[str]:
    return {item.code for item in readiness.findings}


class FreezeTests(RecordCase):
    """A ready candidate: registration, attempt 1 with both proofs, every install and one review."""

    def setUp(self):
        super().setUp()
        self.ready = self.seed(*self.records.ready())
        self.review_sha256 = sha(encode_review(self.records.review()))

    def test_freeze_appends_the_canonical_authorization_and_names_only_the_confirmed_commit(self):
        before = self.readback()
        source = self.records.source
        self.github.calls.clear()
        reported = self.assertConfirmed(self.freeze(self.review_sha256), TransitionKind.FREEZE, self.ready)
        # Replay's own recomputation: the latest intent's scope and the recorded source, on the read head.
        expected = authorize_publication(
            candidate=before.candidate, observed=SourceObservation(head_commit=source.commit, tree=source.tree),
            scope=before.history.intents[-1].scope, history=before.history, review=before.reviews[0],
            evidence_parent=self.ready)
        data = encode_publication(expected)
        self.assertEqual(reported["record_sha256s"], [sha(data)])
        self.assertEqual(self.stored(reported["commit"], "publication.json"), data)
        self.assertEqual(self.document()["inputs"], {
            "candidate_id": self.records.id, "review_sha256": self.review_sha256,
            "observed_head_commit": source.commit, "observed_tree": source.tree})
        self.assertEqual(self.git_commands(), ["git-head", "git-tree"])
        snapshot = self.readback()
        self.assertEqual((snapshot.head, snapshot.history.publication), (reported["commit"], expected))
        # A rerun on the frozen candidate is refused by the canonical policy; nothing is recomputed.
        self.github.calls.clear()
        self.work = self.owned("work-again")
        self.assertRefusedBeforeWrite(self.freeze(self.review_sha256), "candidate_frozen", reported["commit"])

    def test_a_checkout_at_another_commit_refuses_before_any_write(self):
        # The moved checkout is the same history one commit further, not an unrelated repository.
        self.assertEqual(self.moved.git("rev-parse", f"{self.later}^"), self.records.source.commit)
        snapshot = self.readback()
        reported = self.assertRefusedBeforeWrite(self.freeze(self.review_sha256, repository=self.moved.path),
                                                 "publication_not_ready", self.ready)
        self.assertEqual(reported["stage"], "decide-candidate-readiness")
        observed = SourceObservation(head_commit=self.later, tree=self.moved.tree_of(self.later))
        self.assertEqual(self.document()["inputs"]["observed_head_commit"], self.later)
        review = snapshot.reviews[0]
        moved = evaluate_readiness(candidate=snapshot.candidate, observed=observed,
                                   scope=review.record.reviewed_scope.scope, history=snapshot.history, review=review)
        self.assertEqual(codes(moved), {"source_changed"})

    def test_an_unknown_review_digest_refuses_before_observing_the_checkout(self):
        self.assertRefusedBeforeWrite(self.freeze("0" * 64), "freeze_review_missing", self.ready)
        self.assertEqual(self.git_commands(), [])

    def test_an_unregistered_candidate_refuses_before_observing_the_checkout(self):
        self.assertRefusedBeforeWrite(self.freeze(self.review_sha256, candidate_id="8" * 64),
                                      "candidate_not_registered", self.ready)
        self.assertEqual(self.git_commands(), [])

    def test_a_head_that_moves_before_admission_refuses_canonically_with_no_ref_update(self):
        moved: list[str] = []
        self.after_observation = lambda: moved.append(self.advance())
        reported = self.assertRefusedBeforeWrite(self.freeze(self.review_sha256), "store_publication_mismatch",
                                                 moved[0])
        self.assertEqual((reported["stage"], self.patches()), (candidate_store_replay.STAGE, 0))

    def test_a_head_that_moves_before_the_ref_update_is_ambiguous_after_exactly_one_update(self):
        moved: list[str] = []
        self.on_patch = lambda: moved.append(self.advance())
        code, reported, stderr = self.freeze(self.review_sha256)
        self.assertEqual((self.patches(), self.calls_after_patch()), (1, []))
        self.assertEqual((code, reported["disposition"], reported["failure"]), (3, "ambiguous", "github_request_failed"))
        # The proposal is not F: the branch holds the other commit, and nothing reads it back or retries.
        self.assertNotIn("commit", reported)
        self.assertEqual(self.github.refs[BRANCH], moved[0])
        self.assertNotEqual(reported["proposed"], moved[0])
        self.assertEqual(self.document()["outcome"]["proposed"], reported["proposed"])
        self.assertIn("store append unresolved: github_request_failed", stderr)
        self.assertTrue(any(path.name.endswith("-ref.json") for path in self.work.iterdir()))


class FreezeScopeTests(RecordCase):
    """The freeze passes the selected review's scope; canonical readiness compares it with the latest intent."""

    def test_no_intent_refuses_as_not_ready_with_the_canonical_attempt_missing_finding(self):
        review = self.records.review()
        head = self.seed(self.records.register(), self.records.review_file(review))
        self.assertRefusedBeforeWrite(self.freeze(sha(encode_review(review))), "publication_not_ready", head)
        snapshot = self.readback()
        readiness = evaluate_readiness(candidate=snapshot.candidate, observed=SourceObservation(
            head_commit=self.records.source.commit, tree=self.records.source.tree),
            scope=review.reviewed_scope.scope, history=snapshot.history, review=snapshot.reviews[0])
        self.assertIn("attempt_missing", codes(readiness))

    def test_a_review_scope_that_differs_from_the_intent_refuses_as_a_stale_attempt(self):
        affected = self.records.review(AFFECTED)
        head = self.seed(*self.records.ready(), self.records.review_file(affected))
        self.assertRefusedBeforeWrite(self.freeze(sha(encode_review(affected))), "publication_not_ready", head)
        snapshot = self.readback()
        selected = next(item for item in snapshot.reviews if item.record == affected)
        source = self.records.source
        observed = SourceObservation(head_commit=source.commit, tree=source.tree)

        def readiness(scope: ChangeScope) -> set[str]:
            return codes(evaluate_readiness(candidate=snapshot.candidate, observed=observed, scope=scope,
                                            history=snapshot.history, review=selected))

        self.assertEqual(readiness(AFFECTED), {"attempt_scope_stale"})
        # Passing the intent's scope instead would have reported the review, not the attempt, as stale.
        self.assertEqual(readiness(snapshot.history.intents[-1].scope), {"review_scope_mismatch"})


class FreezeLifetimeTests(RecordCase):
    def test_a_converted_checkout_timeout_still_retains_the_tree(self):
        tree = FixtureTree.open(prefix="candidate-record-freeze-fault", label="candidate record freeze fault")
        self.addCleanup(tree.dispose)
        # This test's own owner: its work directory, input and receipt, and its children's faults.
        self.tree = tree
        self.work, self.scratch = self.owned("work"), self.owned("scratch")
        review = self.records.review()
        head = self.seed(self.records.register(), self.records.review_file(review))
        fault = ChildFault(lambda argv: argv[1:3] == ["rev-parse", "HEAD"])
        with patch.object(commands, "run_bounded_command", fault):
            code, reported, _ = self.freeze(sha(encode_review(review)))
        self.assertEqual((code, reported["disposition"], reported["code"], reported["stage"]),
                         (1, "refused", "command_timeout", "git-head"))
        self.assertIsInstance(tree.unresolved, VerificationError)
        self.assertEqual((tree.unresolved.code, tree.unresolved.stage), ("command_timeout", "git-head"))
        self.assertEqual(self.document()["outcome"]["code"], "command_timeout")
        self.assertEqual((self.writes(), self.github.refs[BRANCH], fault.launched), ([], head, 0))
        settle_retained(self, tree, fault, (self.work, self.scratch, self.receipt))


class SetupLifetimeTests(unittest.TestCase):
    def test_a_setup_child_timeout_retains_the_class_tree_with_the_original_error(self):
        class Faulted(RecordCase):
            """Runs the real class setup; unittest never collects this local class."""

        self.addCleanup(Faulted.doClassCleanups)
        fault = ChildFault(lambda argv: "commit" in argv)
        with patch.object(commands, "run_bounded_command", fault), self.assertRaises(VerificationError) as caught:
            Faulted.setUpClass()
        tree = Faulted.tree
        self.assertEqual((caught.exception.code, caught.exception.stage), ("command_timeout", "fixture-git"))
        self.assertIs(tree.unresolved, caught.exception)
        with contextlib.redirect_stderr(io.StringIO()):
            Faulted.doClassCleanups()
        self.assertEqual(Faulted.tearDown_exceptions, [])
        self.assertGreater(fault.launched, 0)
        settle_retained(self, tree, fault, (Faulted.source.path,))


if __name__ == "__main__":
    unittest.main()
