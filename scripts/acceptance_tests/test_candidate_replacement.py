"""One reviewed triage artifact explains a failed attempt and approves its replacement."""

from __future__ import annotations

import unittest

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    AttemptIntent, CellStatus, Closure, MatrixScope, ReviewProvenance,
)
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    ClosureResolutionRef, ProofSubject, ResourceDisposition,
)
from scripts.delegate_acceptance.release_verification.candidate_history import (
    FindingRef, ReviewedTriage, TriageRecord,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import PLATFORMS
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    AttemptState, authorize_publication, decide_launch, evaluate_attempt, evaluate_readiness,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError


LINUX_AMD64 = ("Linux", "amd64")
LINUX_ARM64 = ("Linux", "arm64")
DARWIN_ARM64 = ("Darwin", "arm64")
CODEX_WRITE = fx.case_id("codex:exec", "write")
APPROVED = fx.approval(attempt=2)


def codes(result) -> set[str]:
    return {finding.code for finding in result.findings}


class ReplacementPolicyTests(unittest.TestCase):
    """Attempt 1 has an artifact mismatch, an unresolved closure and a failed cell."""

    def setUp(self):
        self.candidate = fx.bound()
        self.first = fx.intent(self.candidate)
        failed = fx.cells(overrides={CODEX_WRITE: (CellStatus.FAIL, None)})
        self.prior = tuple(
            fx.proof(self.candidate, attempt=1, platform=platform,
                     archive_sha256="9" * 64 if platform == LINUX_AMD64 else None,
                     closure=Closure.UNRESOLVED if platform == DARWIN_ARM64 else Closure.COMPLETE,
                     results=failed if platform == LINUX_ARM64 else None)
            for platform in PLATFORMS)
        self.base = fx.history(self.candidate, intents=(self.first,), proof_records=self.prior)
        self.prior_findings = evaluate_attempt(self.candidate.manifest, self.base, 1).findings
        self.unresolved = next(proof for proof in self.prior if proof.closure is Closure.UNRESOLVED)
        self.resolution = self.resolved()
        self.reference = fx.reference(self.resolution)

    def resolved(self, *, invocation_id=None, resources=None, fact=fx.FACT):
        ids = self.unresolved.unresolved_resources if resources is None else resources
        dispositions = (ResourceDisposition.WAITED, ResourceDisposition.NOT_STARTED)
        return fx.resolution(ProofSubject(proof_sha256=self.unresolved.proof_sha256),
                             invocation=invocation_id or self.unresolved.invocation_id, fact=fact,
                             resources=tuple((resource, dispositions[index % 2])
                                             for index, resource in enumerate(ids)))

    def triage(self, *, explained=None, approved=APPROVED, closure_refs=None, resolutions=None,
               prior_proofs=None, unresolved=()):
        return fx.triage(
            self.candidate, attempt=1, prior_proofs=self.prior if prior_proofs is None else prior_proofs,
            explained=self.prior_findings if explained is None else explained, approved=approved,
            unresolved=unresolved, prior_claims=fx.claims_for(self.candidate, self.prior),
            closure_refs=(self.reference,) if closure_refs is None else closure_refs,
            resolutions=(self.resolution,) if resolutions is None else resolutions)

    def replaced(self, triage, *, intent_triage=None):
        second = fx.intent(self.candidate, attempt=2, triage_sha256=intent_triage or triage.sha256)
        history = fx.history(self.candidate, intents=(self.first, second),
                             proof_records=self.prior + fx.proofs(self.candidate, attempt=2),
                             triages=(triage,))
        before = fx.history(self.candidate, intents=(self.first,), proof_records=self.prior,
                            triages=(triage,))
        return history, before, second

    def readiness(self, history, change=None):
        return evaluate_readiness(candidate=self.candidate, observed=fx.observed(),
                                  scope=change or fx.scope(), history=history,
                                  review=fx.review(self.candidate))

    def both_refuse(self, triage, expected, *, intent_triage=None):
        history, before, second = self.replaced(triage, intent_triage=intent_triage)
        launch = decide_launch(candidate=self.candidate, history=before, intent=second)
        self.assertFalse(launch.allowed)
        self.assertEqual(codes(launch), expected)
        self.assertEqual(codes(self.readiness(history)), expected)

    def test_prior_attempt_reports_artifact_closure_and_cell_findings(self):
        self.assertEqual({finding.code for finding in self.prior_findings},
                         {"artifact_mismatch", "closure_unresolved", "cell_failed"})

    def test_valid_approved_replacement_is_ready_and_prior_failure_stays_fail(self):
        triage = self.triage()
        history, before, second = self.replaced(triage)
        self.assertTrue(decide_launch(candidate=self.candidate, history=before, intent=second).allowed)
        result = self.readiness(history)
        self.assertTrue(result.ready, result.findings)
        self.assertEqual([verdict.state for verdict in result.attempt_verdicts],
                         [AttemptState.FAIL, AttemptState.PASS])
        self.assertEqual(result.attempt_verdicts[0].findings, self.prior_findings)
        self.assertEqual(result.replacements, (triage,))
        self.assertEqual(result.replacements[0].provenance.transaction_commit, fx.TRANSACTION)
        self.assertEqual(result.replacements[0].resolutions, (self.resolution,))

    def test_missing_approval_cannot_authorize_paid_replacement(self):
        self.both_refuse(self.triage(approved=None), {"replacement_unapproved"})

    def test_fake_triage_digest_is_refused(self):
        self.both_refuse(self.triage(), {"triage_mismatch"}, intent_triage="7" * 64)

    def test_wrong_approval_target_is_refused(self):
        linux_only = tuple(platform for platform in PLATFORMS if platform[0] == "Linux")
        for approved in (fx.approval(attempt=2, change=fx.scope(MatrixScope.FINAL_VERSION)),
                         fx.approval(attempt=2, platforms=linux_only),
                         fx.approval(attempt=2, source="f" * 40)):
            with self.subTest(approved=approved):
                self.both_refuse(self.triage(approved=approved), {"replacement_target_mismatch"})

    def test_wrong_prior_proof_digests_are_refused(self):
        foreign = fx.proof(self.candidate, attempt=9, platform=LINUX_AMD64)
        self.both_refuse(self.triage(prior_proofs=self.prior[1:] + (foreign,)), {"triage_proof_mismatch"})

    def test_unexplained_artifact_and_closure_findings_are_refused(self):
        cells_only = tuple(finding for finding in self.prior_findings if finding.code == "cell_failed")
        self.both_refuse(self.triage(explained=cells_only), {"triage_incomplete"})

    def test_extra_foreign_triage_finding_is_refused(self):
        extra = FindingRef(code="cell_failed", subject=f"Darwin/amd64/{CODEX_WRITE}")
        self.both_refuse(self.triage(explained=self.prior_findings + (extra,)), {"triage_foreign_finding"})

    def test_human_closure_claim_without_owner_resolution_is_refused(self):
        self.both_refuse(self.triage(resolutions=()), {"closure_resolution_unverified"})
        self.both_refuse(self.triage(closure_refs=(), resolutions=()), {"closure_resolution_missing"})

    def test_wrong_invocation_resource_or_evidence_resolution_is_refused(self):
        # The triage references the fact it was given, but that fact describes
        # another invocation or an incomplete resource inventory.
        for fact in (self.resolved(invocation_id="invocation-other"), self.resolved(resources=("pueue-task-7",))):
            with self.subTest(fact=fact):
                self.both_refuse(self.triage(closure_refs=(fx.reference(fact),), resolutions=(fact,)),
                                 {"closure_resolution_mismatch"})
        # The validated fact differs from the evidence the triage reviewed.
        self.both_refuse(self.triage(resolutions=(self.resolved(fact="5" * 64),)),
                         {"closure_resolution_unverified"})
        wrong_reference = fx.reference(self.resolution, invocation="invocation-other")
        self.both_refuse(self.triage(closure_refs=(wrong_reference,)), {"closure_resolution_mismatch"})
        complete = self.prior[0]
        foreign = ClosureResolutionRef(subject=ProofSubject(proof_sha256=complete.proof_sha256),
                                       invocation_id=complete.invocation_id, evidence_sha256="6" * 64)
        self.both_refuse(self.triage(closure_refs=(self.reference, foreign)), {"closure_resolution_foreign"})

    def test_unresolved_product_finding_refuses_replacement(self):
        self.both_refuse(self.triage(unresolved=("codex-write",)), {"product_finding_unresolved"})

    def test_reviewed_triage_requires_matching_provenance_fact(self):
        triage = self.triage()
        for provenance in (
                ReviewProvenance(artifact_sha256="8" * 64, reviewer=fx.REVIEWER, transaction_commit=fx.TRANSACTION),
                ReviewProvenance(artifact_sha256=triage.sha256, reviewer="someone-else",
                                 transaction_commit=fx.TRANSACTION)):
            with self.assertRaises(VerificationError) as caught:
                ReviewedTriage(record=triage.record, provenance=provenance, resolutions=())
            self.assertEqual(caught.exception.code, "candidate_triage_provenance_mismatch")

    def test_triage_record_guards_its_own_shape(self):
        record = self.triage().record
        invalid = (
            dict(diagnosis_sha256s=()),
            dict(prior_claim_sha256s=()),
            dict(prior_proof_sha256s=tuple(reversed(record.prior_proof_sha256s))),
            dict(explained_findings=tuple(reversed(record.explained_findings))),
            dict(approval=fx.approval(attempt=3)),
            dict(closure_resolutions=(self.reference, self.reference)),
        )
        for change in invalid:
            values = {field: getattr(record, field) for field in record.__dataclass_fields__}
            values.update(change)
            with self.subTest(change=sorted(change)):
                with self.assertRaises(VerificationError) as caught:
                    TriageRecord(**values)
                self.assertEqual(caught.exception.code, "candidate_triage_invalid")
        claim_only = {field: getattr(record, field) for field in record.__dataclass_fields__}
        claim_only.update(prior_proof_sha256s=())
        self.assertEqual(TriageRecord(**claim_only).prior_proof_sha256s, ())


class ReplacementChainTests(unittest.TestCase):
    """Attempt 1 leaves one closure unresolved; attempts 2 and 3 replace it in turn."""

    def setUp(self):
        self.candidate = fx.bound()
        self.first = fx.intent(self.candidate)
        self.prior = tuple(
            fx.proof(self.candidate, attempt=1, platform=platform,
                     closure=Closure.UNRESOLVED if platform == DARWIN_ARM64 else Closure.COMPLETE)
            for platform in PLATFORMS)
        recorded = fx.history(self.candidate, intents=(self.first,), proof_records=self.prior)
        self.prior_findings = evaluate_attempt(self.candidate.manifest, recorded, 1).findings
        unresolved = next(proof for proof in self.prior if proof.closure is Closure.UNRESOLVED)
        self.resolution = fx.resolution(ProofSubject(proof_sha256=unresolved.proof_sha256),
                                        invocation=unresolved.invocation_id,
                                        resources=tuple((resource, ResourceDisposition.WAITED)
                                                        for resource in unresolved.unresolved_resources))
        self.reference = fx.reference(self.resolution)
        self.second_proofs = fx.proofs(self.candidate, attempt=2)
        self.second_triage = fx.triage(self.candidate, attempt=2, prior_proofs=self.second_proofs, explained=(),
                                       approved=fx.approval(attempt=3))

    def first_triage(self, *, approved=APPROVED, resolutions=None):
        return fx.triage(self.candidate, attempt=1, prior_proofs=self.prior, explained=self.prior_findings,
                         approved=approved, closure_refs=(self.reference,),
                         resolutions=(self.resolution,) if resolutions is None else resolutions)

    def chain(self, first_triage):
        second = fx.intent(self.candidate, attempt=2, triage_sha256=first_triage.sha256)
        third = fx.intent(self.candidate, attempt=3, triage_sha256=self.second_triage.sha256)
        triages = (first_triage, self.second_triage)
        before = fx.history(self.candidate, intents=(self.first, second),
                            proof_records=self.prior + self.second_proofs, triages=triages)
        after = fx.history(self.candidate, intents=(self.first, second, third),
                           proof_records=self.prior + self.second_proofs + fx.proofs(self.candidate, attempt=3),
                           triages=triages)
        return before, third, after

    def launch(self, history, intent):
        return decide_launch(candidate=self.candidate, history=history, intent=intent)

    def readiness(self, history):
        return evaluate_readiness(candidate=self.candidate, observed=fx.observed(), scope=fx.scope(),
                                  history=history, review=fx.review(self.candidate))

    def test_fully_valid_three_attempt_chain_launches_and_is_ready(self):
        first_triage = self.first_triage()
        before, third, after = self.chain(first_triage)
        self.assertTrue(self.launch(before, third).allowed)
        result = self.readiness(after)
        self.assertTrue(result.ready, result.findings)
        self.assertEqual([verdict.state for verdict in result.attempt_verdicts],
                         [AttemptState.FAIL, AttemptState.PASS, AttemptState.PASS])
        self.assertEqual(result.replacements, (first_triage, self.second_triage))

    def test_earlier_unapproved_edge_blocks_a_later_launch(self):
        before, third, after = self.chain(self.first_triage(approved=None))
        expected = (FindingRef(code="replacement_unapproved", subject="2"),)
        self.assertEqual(self.launch(before, third).findings, expected)
        self.assertEqual(self.readiness(after).findings, expected)

    def test_earlier_unresolved_closure_blocks_a_later_launch(self):
        before, third, after = self.chain(self.first_triage(resolutions=()))
        expected = (FindingRef(code="closure_resolution_unverified", subject="Darwin/arm64"),)
        self.assertEqual(self.launch(before, third).findings, expected)
        self.assertEqual(self.readiness(after).findings, expected)

    def test_later_approved_triage_supersedes_by_digest_without_rewriting_history(self):
        initial = self.first_triage(approved=None)
        corrected = self.first_triage()
        self.assertNotEqual(initial.sha256, corrected.sha256)
        recorded = fx.history(self.candidate, intents=(self.first,), proof_records=self.prior,
                              triages=(initial, corrected))
        second = fx.intent(self.candidate, attempt=2, triage_sha256=corrected.sha256)
        self.assertTrue(self.launch(recorded, second).allowed)
        replaced = fx.history(self.candidate, intents=(self.first, second),
                              proof_records=self.prior + self.second_proofs, triages=(initial, corrected))
        result = self.readiness(replaced)
        self.assertTrue(result.ready, result.findings)
        self.assertEqual(replaced.triages, (initial, corrected))
        self.assertEqual(result.replacements, (corrected,))
        self.assertEqual(result.attempt_verdicts[0].state, AttemptState.FAIL)
        stale = fx.intent(self.candidate, attempt=2, triage_sha256=initial.sha256)
        self.assertEqual(codes(self.launch(recorded, stale)), {"replacement_unapproved"})

    def test_selection_is_scoped_to_the_prior_attempt_and_exact_digest(self):
        first_triage = self.first_triage()
        before, _, _ = self.chain(first_triage)
        for digest in ("7" * 64, first_triage.sha256):
            with self.subTest(digest=digest):
                intent = fx.intent(self.candidate, attempt=3, triage_sha256=digest)
                self.assertEqual(self.launch(before, intent).findings,
                                 (FindingRef(code="triage_mismatch", subject="3"),))
        with self.assertRaises(VerificationError) as caught:
            fx.history(self.candidate, intents=(self.first,), proof_records=self.prior,
                       triages=(first_triage, first_triage))
        self.assertEqual(caught.exception.code, "candidate_history_invalid")


class LaunchAndHistoryTests(unittest.TestCase):
    def setUp(self):
        self.candidate = fx.bound()
        self.first = fx.intent(self.candidate)
        self.passing = fx.proofs(self.candidate, attempt=1)

    def decide(self, history, intent):
        return decide_launch(candidate=self.candidate, history=history, intent=intent)

    def readiness(self, history):
        return evaluate_readiness(candidate=self.candidate, observed=fx.observed(), scope=fx.scope(),
                                  history=history, review=fx.review(self.candidate))

    def test_first_attempt_is_allowed_once_and_its_pass_is_ready(self):
        empty = fx.history(self.candidate, intents=())
        self.assertTrue(self.decide(empty, self.first).allowed)
        recorded = fx.history(self.candidate, intents=(self.first,))
        self.assertEqual(codes(self.decide(recorded, self.first)), {"attempt_number_invalid"})
        passed = fx.history(self.candidate, intents=(self.first,), proof_records=self.passing)
        self.assertTrue(self.readiness(passed).ready)

    def test_unresolved_claims_of_incomplete_prior_attempt_only_observe(self):
        claims = tuple(fx.claim(self.candidate, attempt=1, platform=platform) for platform in PLATFORMS)
        recorded = fx.history(self.candidate, intents=(self.first,), proof_records=self.passing[:2],
                              claims=claims)
        findings = evaluate_attempt(self.candidate.manifest, recorded, 1).findings
        triage = fx.triage(self.candidate, attempt=1, prior_proofs=self.passing[:2], prior_claims=claims,
                           explained=findings, approved=APPROVED)
        partial = fx.history(self.candidate, intents=(self.first,), proof_records=self.passing[:2],
                             claims=claims, triages=(triage,))
        second = fx.intent(self.candidate, attempt=2, triage_sha256=triage.sha256)
        unproven = {f"{os_name}/{arch}" for os_name, arch in PLATFORMS[2:]}
        result = self.decide(partial, second)
        self.assertEqual({finding.code for finding in result.findings}, {"closure_resolution_missing"})
        self.assertEqual({finding.subject for finding in result.findings}, unproven)

    def test_every_historical_intent_source_must_match_the_manifest(self):
        triage = fx.triage(self.candidate, attempt=1, prior_proofs=self.passing, explained=(),
                           approved=APPROVED)
        old = AttemptIntent(candidate_id=self.candidate.candidate_id, attempt=1, source_commit="f" * 40,
                            scope=fx.scope(), platforms=PLATFORMS, replaces=None)
        second = fx.intent(self.candidate, attempt=2, triage_sha256=triage.sha256)
        history = fx.history(self.candidate, intents=(old, second),
                             proof_records=self.passing + fx.proofs(self.candidate, attempt=2),
                             triages=(triage,))
        result = self.readiness(history)
        self.assertIn(FindingRef(code="attempt_source_mismatch", subject="1"), result.findings)
        before = fx.history(self.candidate, intents=(old,), proof_records=self.passing, triages=(triage,))
        self.assertIn(FindingRef(code="attempt_source_mismatch", subject="1"),
                      self.decide(before, second).findings)

    def test_earlier_attempt_may_have_a_different_scope(self):
        narrow = fx.scope(MatrixScope.AFFECTED_PROVIDERS, ("claude",))
        first = fx.intent(self.candidate, change=narrow)
        triage = fx.triage(self.candidate, attempt=1, prior_proofs=self.passing, explained=(),
                           approved=APPROVED)
        second = fx.intent(self.candidate, attempt=2, triage_sha256=triage.sha256)
        history = fx.history(self.candidate, intents=(first, second),
                             proof_records=self.passing + fx.proofs(self.candidate, attempt=2),
                             triages=(triage,))
        self.assertTrue(self.readiness(history).ready)

    def test_publication_freeze_binds_the_exact_latest_attempt(self):
        before = fx.history(self.candidate, intents=(self.first,), proof_records=self.passing)
        freeze = authorize_publication(candidate=self.candidate, observed=fx.observed(), scope=fx.scope(),
                                       history=before, review=fx.review(self.candidate),
                                       evidence_parent=fx.TRANSACTION)
        current = fx.history(self.candidate, intents=(self.first,), proof_records=self.passing,
                             publication=freeze)
        self.assertTrue(self.readiness(current).ready)
        triage = fx.triage(self.candidate, attempt=1, prior_proofs=self.passing, explained=(),
                           approved=APPROVED)
        second = fx.intent(self.candidate, attempt=2, triage_sha256=triage.sha256)
        frozen = fx.history(self.candidate, intents=(self.first,), proof_records=self.passing,
                            triages=(triage,), publication=freeze)
        self.assertEqual(codes(self.decide(frozen, second)), {"candidate_frozen"})
        stale = fx.history(self.candidate, intents=(self.first, second),
                           proof_records=self.passing + fx.proofs(self.candidate, attempt=2),
                           triages=(triage,), publication=freeze)
        self.assertEqual(codes(self.readiness(stale)), {"publication_stale"})

    def test_launch_requires_both_operating_systems(self):
        linux_only = tuple(platform for platform in PLATFORMS if platform[0] == "Linux")
        intent = fx.intent(self.candidate, platforms=linux_only)
        result = self.decide(fx.history(self.candidate, intents=()), intent)
        self.assertEqual(result.findings, (FindingRef(code="required_platform_missing", subject="Darwin"),))


if __name__ == "__main__":
    unittest.main()
