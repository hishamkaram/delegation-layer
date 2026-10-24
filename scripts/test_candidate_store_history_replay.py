"""Canonical replay reads a history at a head exactly as the boundary-observed snapshot, and admits neither alone.

Every existing replay case below runs unchanged, except that each replay
reads the same verified transactions through both values and requires one
identical outcome: an equal snapshot, or the same refusal with the same
findings and cause. Admission keeps reading only the strict snapshot. Both
values name the same freeze commit: the transaction that introduced the
freeze, whichever head the history is read at.
"""

from __future__ import annotations

from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import test_candidate_store_replay as cases
from scripts.delegate_acceptance.release_verification import candidate_store_replay
from scripts.delegate_acceptance.release_verification.candidate_history import encode_publication
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    CandidateHistoryAtHead, EntryFamily, RawCandidateSnapshot,
)
from scripts.delegate_acceptance.release_verification.candidate_store_replay import (
    CandidateSnapshot, admit_append, replay_candidate,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_store_history_contract import at_head


Outcome = tuple[CandidateSnapshot | None, VerificationError | None]


def outcome(value: RawCandidateSnapshot | CandidateHistoryAtHead, facts: cases.Facts) -> Outcome:
    try:
        return replay_candidate(value, review_provenance=facts.provenance, closure_resolutions=facts.closures,
                                installer_sources=facts.sources), None
    except VerificationError as error:
        return None, error


def identity(error: VerificationError) -> tuple[object, ...]:
    cause = error.__cause__
    return (type(error), *cases.refusal(error), type(cause), getattr(cause, "args", None))


class HistoryReplayCase(cases.ReplayCase):
    """Replays each fixture history through both read values and returns the history value's result."""

    def replayed(self, groups: list[cases.Files], facts: cases.Facts = cases.Facts(), *,
                 candidate_id: str | None = None, parents: dict[int, str] | None = None) -> CandidateSnapshot | None:
        snapshot = cases.raw(candidate_id or self.s.id, groups, parents=parents)
        strict, strict_error = outcome(snapshot, facts)
        history, history_error = outcome(at_head(snapshot), facts)
        if strict_error is not None or history_error is not None:
            self.assertIsNotNone(strict_error)
            self.assertIsNotNone(history_error)
            self.assertEqual(identity(history_error), identity(strict_error))
            raise history_error
        self.assertEqual(history, strict)
        return history


class HistoryPrefixTests(HistoryReplayCase, cases.PrefixTests):
    pass


class HistoryRegistrationTests(HistoryReplayCase, cases.RegistrationTests):
    pass


class HistoryIntentAndClaimTests(HistoryReplayCase, cases.IntentAndClaimTests):
    pass


class HistoryProofTests(HistoryReplayCase, cases.ProofTests):
    pass


class HistoryInstallTests(HistoryReplayCase, cases.InstallTests):
    pass


class HistoryProvenanceTests(HistoryReplayCase, cases.ProvenanceTests):
    pass


class HistoryTriageTests(HistoryReplayCase, cases.TriageTests):
    pass


class HistoryFreezeTests(HistoryReplayCase, cases.FreezeTests):
    pass


class HistoryAdmissionTests(cases.ReplayCase):
    def test_admission_refuses_a_history_value_that_the_snapshot_admits(self):
        prefix = cases.raw(self.s.id, self.ready)
        freeze = cases.additions({cases.name(EntryFamily.PUBLICATION): encode_publication(
            self.publication(evidence_parent=prefix.head))})
        facts = self.ready_facts()
        self.assertIsNone(admit_append(prefix, freeze, review_provenance=facts.provenance,
                                       closure_resolutions=facts.closures, installer_sources=facts.sources))
        with self.assertRaises(VerificationError) as caught:
            admit_append(at_head(prefix), freeze, review_provenance=facts.provenance,
                         closure_resolutions=facts.closures, installer_sources=facts.sources)
        self.assertEqual(cases.refusal(caught.exception),
                         ("store_snapshot_invalid", candidate_store_replay.STAGE, None))

    def test_replay_reads_only_the_two_verified_history_values(self):
        snapshot = replay_candidate(at_head(cases.raw(self.s.id, self.ready)), installer_sources=(self.s.shell_source,),
                                    review_provenance=(self.review_provenance(),), closure_resolutions=())
        for value in (snapshot, object(), None):
            with self.subTest(value=type(value).__name__), self.assertRaises(VerificationError) as caught:
                replay_candidate(value, review_provenance=(), closure_resolutions=(), installer_sources=())
            self.assertEqual(cases.refusal(caught.exception),
                             ("store_snapshot_invalid", candidate_store_replay.STAGE, None))

class FreezeCommitTests(HistoryReplayCase):
    def test_the_freeze_commit_is_the_introducing_transaction_at_every_head(self):
        self.assertIsNone(self.replayed(self.ready, self.ready_facts()).freeze_commit)
        introducing = cases.commit(self.s.id, len(self.ready))
        frozen = self.replayed(self.frozen(), self.ready_facts())
        self.assertEqual((frozen.head, frozen.freeze_commit), (introducing, introducing))
        self.assertEqual(frozen.history.publication.evidence_parent, cases.commit(self.s.id, self.review_index))
        advanced = cases.raw(self.s.id, self.frozen(), head=cases.FOREIGN_COMMIT)
        for value in (advanced, at_head(advanced)):
            with self.subTest(value=type(value).__name__):
                snapshot, error = outcome(value, self.ready_facts())
                self.assertIsNone(error)
                self.assertEqual((snapshot.head, snapshot.freeze_commit), (cases.FOREIGN_COMMIT, introducing))
        record = self.publication(evidence_parent=cases.FOREIGN_COMMIT)
        interleaved = self.replayed(self.frozen(record), self.ready_facts(),
                                    parents={len(self.ready): cases.FOREIGN_COMMIT})
        self.assertEqual((interleaved.freeze_commit, interleaved.history.publication.evidence_parent),
                         (introducing, cases.FOREIGN_COMMIT))

    def test_no_freeze_commit_is_named_unless_the_whole_replay_holds(self):
        later = self.s.digest_file(EntryFamily.REVIEW, self.s.review(unresolved_findings=("finding-1",)))
        foreign = self.review_provenance(0, data=b"foreign review")
        for groups, facts, code in (([*self.frozen(), later], self.ready_facts(), "store_candidate_frozen"),
                                    (self.frozen(), self.ready_facts(foreign), "store_review_provenance_unused")):
            with self.subTest(code=code), self.assertRaises(VerificationError) as caught:
                self.replayed(groups, facts)
            self.assertEqual(caught.exception.code, code)

    def test_the_snapshot_guards_its_freeze_commit(self):
        unfrozen = self.replayed(self.ready, self.ready_facts())
        frozen = self.replayed(self.frozen(), self.ready_facts())

        def build(snapshot: CandidateSnapshot, freeze_commit: object) -> CandidateSnapshot:
            return CandidateSnapshot(head=snapshot.head, candidate=snapshot.candidate, history=snapshot.history,
                                     reviews=snapshot.reviews, freeze_commit=freeze_commit)

        self.assertEqual(build(frozen, frozen.freeze_commit), frozen)
        self.assertEqual(build(unfrozen, None), unfrozen)
        invalid = [(unfrozen, frozen.freeze_commit), (frozen, None),
                   *((frozen, value) for value in ("", "F" * 40, "0" * 39, "0" * 41, 7))]
        for snapshot, value in invalid:
            with self.subTest(frozen=snapshot is frozen, value=value), self.assertRaises(VerificationError) as caught:
                build(snapshot, value)
            self.assertEqual(cases.refusal(caught.exception),
                             ("store_snapshot_invalid", candidate_store_replay.STAGE, None))
        with self.assertRaises(TypeError):
            CandidateSnapshot(head=frozen.head, candidate=frozen.candidate, history=frozen.history,
                              reviews=frozen.reviews)


if __name__ == "__main__":
    unittest.main()
