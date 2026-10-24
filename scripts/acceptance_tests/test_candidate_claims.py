"""Platform claims guard paid launches, and claim-only closure permits only a reviewed successor."""

from __future__ import annotations

from dataclasses import replace
import json
import unittest

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.native_evidence.public_contract import (
    CASE_IDS, CaseOutcome, NativeCompletion, ValidatedCandidateProof,
)
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    BlockReason, CellStatus, Closure, candidate_proof_expectation, claim_sha256, encode_claim, parse_claim,
    platform_proof,
)
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    NATIVE_CHILD_RESOURCE, ClaimSubject, ClosureResolution, ProofSubject, ResourceDisposition,
    ResourceResolution, encode_closure_evidence,
)
from scripts.delegate_acceptance.release_verification.candidate_codec import encode_record
from scripts.delegate_acceptance.release_verification.candidate_history import (
    FindingRef, PublicationAuthorization,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import PLATFORMS
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    AttemptState, decide_launch, decide_platform_claim, evaluate_attempt, evaluate_readiness,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError


DARWIN_ARM64 = ("Darwin", "arm64")
LINUX_AMD64 = ("Linux", "amd64")
WAITED = ResourceDisposition.WAITED
NOT_STARTED = ResourceDisposition.NOT_STARTED
CASE = fx.case_id("codex:exec", "write")
CASE_RESOURCES = (f"{CASE}:models-state", f"{CASE}:state", f"{CASE}:supervisor")


def codes(result) -> set[str]:
    return {finding.code for finding in result.findings}


def mutated(data: bytes, change) -> bytes:
    record = json.loads(data)
    change(record)
    return encode_record(record, "test")


class ClaimCodecTests(unittest.TestCase):
    def setUp(self):
        self.claim = fx.claim(fx.bound(), attempt=1, platform=DARWIN_ARM64)
        self.data = encode_claim(self.claim)

    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)

    def test_claim_round_trips_and_is_identified_by_its_exact_bytes(self):
        self.assertEqual(parse_claim(self.data), self.claim)
        self.assertEqual(claim_sha256(self.claim), sha(self.data))
        record = json.loads(self.data)
        self.assertEqual((record["ccr_model"], record["agy_model"]), (fx.CCR_MODEL, None))
        self.assertEqual(record["binaries"], {"delegate": "4" * 64, "delegate-run": "5" * 64,
                                              "pueue": "6" * 64, "pueued": "7" * 64})

    def test_claim_records_every_execution_expectation_within_its_bound(self):
        longest = "m" * 128
        full = replace(self.claim, opencode_model=longest, ccr_model=longest, agy_model=longest,
                       harness_revision="f" * 64, invocation_id="i" * 128)
        data = encode_claim(full)
        self.assertLessEqual(len(data), 4 * 1024)
        self.assertEqual(parse_claim(data), full)
        defaults = replace(self.claim, ccr_model=None, agy_model=None)
        self.assertEqual(parse_claim(encode_claim(defaults)), defaults)
        for change in (dict(harness_revision="d" * 39), dict(harness_sha256="D" * 64),
                       dict(binaries=self.claim.binaries.as_mapping()), dict(opencode_model=None),
                       dict(ccr_model=""), dict(agy_model="/abs/model")):
            with self.subTest(change=change):
                self.assertCode("candidate_field_invalid", replace, self.claim, **change)

    def test_malformed_claims_are_typed_refusals(self):
        cases = (
            ("candidate_field_invalid", lambda r: r.update(attempt=True)),
            ("candidate_field_invalid", lambda r: r.update(attempt=0)),
            ("candidate_field_invalid", lambda r: r.update(invocation_id="/proc/1")),
            ("candidate_field_invalid", lambda r: r.update(candidate_id="A" * 64)),
            ("candidate_platform_invalid", lambda r: r.update(os="Windows")),
            ("candidate_platform_invalid", lambda r: r.update(arch=["arm64"])),
            ("candidate_record_fields_invalid", lambda r: r.update(proof_sha256="4" * 64)),
            ("candidate_record_fields_invalid", lambda r: r.pop("invocation_id")),
            ("candidate_record_fields_invalid", lambda r: r.pop("agy_model")),
            ("candidate_record_fields_invalid", lambda r: r.pop("harness_sha256")),
            ("candidate_field_invalid", lambda r: r.update(opencode_model=7)),
            ("candidate_field_invalid", lambda r: r.update(harness_revision="HEAD")),
            ("binary_digests_invalid", lambda r: r["binaries"].pop("pueued")),
            ("binary_digests_invalid", lambda r: r["binaries"].update(runner="4" * 64)),
            ("candidate_record_kind_invalid", lambda r: r.update(schema_version=True)),
            ("candidate_record_kind_invalid", lambda r: r.update(kind="delegation-layer-candidate-attempt-intent")),
        )
        for code, change in cases:
            with self.subTest(code=code):
                self.assertCode(code, parse_claim, mutated(self.data, change))
        self.assertCode("candidate_record_not_canonical", parse_claim, self.data.rstrip(b"\n"))
        self.assertCode("candidate_record_invalid", parse_claim,
                        self.data.replace(b'"arch":', b'"arch":"amd64","arch":', 1))
        self.assertCode("candidate_record_too_large", parse_claim, b" " * (4 * 1024 + 1))


class ProofMappingTests(unittest.TestCase):
    def setUp(self):
        self.candidate = fx.bound()
        self.claim = fx.claim(self.candidate, attempt=2, platform=LINUX_AMD64)

    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)

    def expectation(self, claim=None):
        return candidate_proof_expectation(self.candidate.manifest, self.candidate.locator, claim or self.claim)

    def validated(self, *, unresolved=(), blocked=CASE_IDS[0], reason=BlockReason.UNCLASSIFIED):
        facts = self.expectation()
        return ValidatedCandidateProof(
            public_zip_sha256="1" * 64, aggregate_sha256="2" * 64, candidate_id=facts.candidate_id,
            claim_sha256=facts.claim_sha256, invocation_id=facts.invocation_id,
            platform_os=facts.platform_os, platform_arch=facts.platform_arch,
            archive_sha256=facts.archive_sha256, binaries=facts.binaries,
            completion=NativeCompletion(facts.invocation_id, True, 0, "3" * 64, "4" * 64, "5" * 64),
            status="PASS", case_results_verdict="PASS",
            cases=tuple(CaseOutcome(case_id, "BLOCKED", reason) if case_id == blocked
                        else CaseOutcome(case_id, "PASS", None) for case_id in CASE_IDS),
            unresolved_case_ids=unresolved)

    def test_expectation_comes_from_the_manifest_locator_and_claim(self):
        facts = self.expectation()
        manifest, locator = self.candidate.manifest, self.candidate.locator
        archive = manifest.archive(*LINUX_AMD64)
        self.assertEqual(
            (facts.repository, facts.candidate_id, facts.run_id, facts.run_attempt, facts.artifact_id,
             facts.artifact_archive_sha256, facts.source_commit, facts.source_tree, facts.archive_name,
             facts.archive_sha256, facts.claim_sha256, facts.invocation_id),
            (locator.repository, locator.candidate_id, locator.run_id, locator.run_attempt, locator.artifact_id,
             locator.artifact_archive_sha256, manifest.source.commit, manifest.source.tree, archive.path,
             archive.sha256, claim_sha256(self.claim), self.claim.invocation_id))
        self.assertEqual(
            (facts.harness_revision, facts.harness_sha256, facts.binaries, facts.opencode_model,
             facts.ccr_model, facts.agy_model),
            (fx.NATIVE_HARNESS_REVISION, fx.NATIVE_HARNESS_SHA256, fx.NATIVE_BINARIES, fx.OPENCODE_MODEL,
             fx.CCR_MODEL, None))
        self.assertCode("candidate_claim_mismatch", self.expectation, replace(self.claim, candidate_id="9" * 64))
        self.assertCode("candidate_claim_mismatch", candidate_proof_expectation, fx.manifest("0.1.38"),
                        self.candidate.locator, self.claim)

    def test_platform_proof_maps_the_validated_proof_of_its_claim(self):
        proof = platform_proof(self.validated(), self.claim)
        self.assertEqual((proof.attempt, proof.os, proof.arch, proof.invocation_id, proof.proof_sha256,
                          proof.aggregate_sha256, proof.closure, proof.unresolved_resources),
                         (2, "Linux", "amd64", self.claim.invocation_id, "1" * 64, "2" * 64, Closure.COMPLETE, ()))
        blocked = [cell for cell in proof.cells if cell.status is CellStatus.BLOCKED]
        self.assertEqual([(cell.case_id, cell.block_reason) for cell in blocked],
                         [(CASE_IDS[0], BlockReason.UNCLASSIFIED)])
        unresolved = platform_proof(self.validated(unresolved=(CASE_IDS[3],)), self.claim)
        self.assertEqual((unresolved.closure, unresolved.unresolved_resources),
                         (Closure.UNRESOLVED, (CASE_IDS[3],)))

    def test_platform_cells_keep_the_validated_block_reason(self):
        for reason in BlockReason:
            with self.subTest(reason=reason):
                cells = platform_proof(self.validated(reason=reason), self.claim).cells
                self.assertEqual([(cell.case_id, cell.status, cell.block_reason) for cell in cells
                                  if cell.block_reason is not None],
                                 [(CASE_IDS[0], CellStatus.BLOCKED, reason)])

    def test_a_proof_never_maps_onto_another_claim(self):
        validated = self.validated()
        other = "invocation-other"
        for foreign in (replace(validated, candidate_id="9" * 64),
                        replace(validated, invocation_id=other, completion=replace(validated.completion,
                                                                                   invocation_id=other)),
                        replace(validated, platform_os="Darwin", platform_arch="arm64"),
                        replace(validated, binaries=replace(fx.NATIVE_BINARIES, pueue="9" * 64))):
            with self.subTest(foreign=foreign):
                self.assertCode("candidate_proof_claim_mismatch", platform_proof, foreign, self.claim)
        for change in (dict(invocation_id="invocation-other"), dict(attempt=3), dict(agy_model="other-model"),
                       dict(binaries=replace(fx.NATIVE_BINARIES, pueue="9" * 64)), dict(os="Darwin", arch="arm64")):
            with self.subTest(change=change):
                self.assertCode("candidate_proof_claim_mismatch", platform_proof, validated,
                                replace(self.claim, **change))


class ClosureValueTests(unittest.TestCase):
    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)

    def resources(self, *items):
        return tuple(ResourceResolution(resource_id=resource, disposition=disposition, fact_sha256=fx.FACT)
                     for resource, disposition in items)

    def test_claim_closure_requires_a_positively_resolved_native_child(self):
        subject = ClaimSubject(claim_sha256="1" * 64)
        for resources in (
                self.resources((f"{CASE}:state", WAITED)),
                self.resources((f"{CASE}:state", WAITED), (NATIVE_CHILD_RESOURCE, NOT_STARTED)),
                self.resources((NATIVE_CHILD_RESOURCE, WAITED), (NATIVE_CHILD_RESOURCE, WAITED)),
                ()):
            with self.subTest(resources=resources):
                self.assertCode("candidate_resolution_invalid", ClosureResolution, subject=subject,
                                invocation_id="invocation-1", resources=resources)
        never_started = ClosureResolution(subject=subject, invocation_id="invocation-1",
                                          resources=self.resources((NATIVE_CHILD_RESOURCE, NOT_STARTED)))
        self.assertEqual(never_started.resource_ids, (NATIVE_CHILD_RESOURCE,))

    def test_subjects_are_discriminated_and_evidence_digest_binds_every_fact(self):
        self.assertCode("candidate_closure_subject_invalid", ClosureResolution, subject="1" * 64,
                        invocation_id="invocation-1", resources=self.resources((NATIVE_CHILD_RESOURCE, WAITED)))
        self.assertCode("candidate_field_invalid", ProofSubject, proof_sha256="")
        proof = ClosureResolution(subject=ProofSubject(proof_sha256="1" * 64), invocation_id="invocation-1",
                                  resources=self.resources((f"{CASE}:state", WAITED)))
        claim = ClosureResolution(subject=ClaimSubject(claim_sha256="1" * 64), invocation_id="invocation-1",
                                  resources=self.resources((f"{CASE}:state", WAITED), (NATIVE_CHILD_RESOURCE, WAITED)))
        self.assertNotEqual(proof.evidence_sha256, claim.evidence_sha256)
        self.assertEqual(proof.evidence_sha256, sha(encode_closure_evidence(proof)))
        changed = ResourceResolution(resource_id=f"{CASE}:state", disposition=WAITED, fact_sha256="9" * 64)
        refact = ClosureResolution(subject=proof.subject, invocation_id="invocation-1", resources=(changed,))
        self.assertNotEqual(proof.evidence_sha256, refact.evidence_sha256)


class PlatformClaimDecisionTests(unittest.TestCase):
    def setUp(self):
        self.candidate = fx.bound()
        self.first = fx.intent(self.candidate)

    def decide(self, history, claim):
        return decide_platform_claim(candidate=self.candidate, history=history, claim=claim)

    def claim(self, platform, *, attempt=1, invocation=None):
        return fx.claim(self.candidate, attempt=attempt, platform=platform, invocation=invocation)

    def test_unclaimed_planned_platform_of_latest_attempt_may_be_claimed(self):
        empty = fx.history(self.candidate, intents=(self.first,))
        self.assertTrue(self.decide(empty, self.claim(DARWIN_ARM64)).allowed)
        partial = fx.history(self.candidate, intents=(self.first,), claims=(self.claim(LINUX_AMD64),))
        self.assertTrue(self.decide(partial, self.claim(DARWIN_ARM64)).allowed)

    def test_existing_claim_is_observe_only_even_for_the_same_invocation(self):
        claimed = fx.history(self.candidate, intents=(self.first,), claims=(self.claim(DARWIN_ARM64),))
        self.assertEqual(codes(self.decide(claimed, self.claim(DARWIN_ARM64))),
                         {"claim_exists", "claim_invocation_reused"})
        self.assertEqual(codes(self.decide(claimed, self.claim(DARWIN_ARM64, invocation="invocation-new"))),
                         {"claim_exists"})
        self.assertEqual(codes(self.decide(claimed, self.claim(LINUX_AMD64, invocation=self.claim(
            DARWIN_ARM64).invocation_id))), {"claim_invocation_reused"})

    def test_unplanned_foreign_and_missing_attempt_claims_are_refused(self):
        linux_only = tuple(platform for platform in PLATFORMS if platform[0] == "Linux")
        narrow = fx.history(self.candidate, intents=(fx.intent(self.candidate, platforms=linux_only),))
        self.assertEqual(codes(self.decide(narrow, self.claim(DARWIN_ARM64))), {"claim_platform_unplanned"})
        foreign = replace(self.claim(DARWIN_ARM64, invocation="invocation-foreign"), candidate_id="9" * 64)
        self.assertEqual(codes(self.decide(fx.history(self.candidate, intents=(self.first,)), foreign)),
                         {"candidate_mismatch"})
        self.assertEqual(codes(self.decide(fx.history(self.candidate, intents=()), self.claim(DARWIN_ARM64))),
                         {"attempt_missing"})

    def test_advanced_attempt_refuses_late_claims_of_an_older_attempt(self):
        triage = fx.triage(self.candidate, attempt=1, prior_proofs=fx.proofs(self.candidate, attempt=1),
                           explained=(), approved=fx.approval(attempt=2))
        second = fx.intent(self.candidate, attempt=2, triage_sha256=triage.sha256)
        history = fx.history(self.candidate, intents=(self.first, second),
                             proof_records=fx.proofs(self.candidate, attempt=1), triages=(triage,))
        self.assertEqual(self.decide(history, self.claim(DARWIN_ARM64, invocation="invocation-late")).findings,
                         (FindingRef(code="claim_attempt_stale", subject="1"),))
        self.assertTrue(self.decide(history, self.claim(DARWIN_ARM64, attempt=2)).allowed)

    def test_freeze_and_invalid_history_refuse_every_claim(self):
        frozen = fx.history(self.candidate, intents=(self.first,), publication=PublicationAuthorization(
            candidate_id=self.candidate.candidate_id, attempt=1, evidence_parent=fx.TRANSACTION,
            source_commit=fx.COMMIT, source_tree=fx.TREE, proof_sha256s=("8" * 64,),
            install_sha256s=("9" * 64,), triage_sha256s=(), review_sha256="7" * 64))
        self.assertEqual(codes(self.decide(frozen, self.claim(DARWIN_ARM64))), {"candidate_frozen"})
        unapproved = fx.triage(self.candidate, attempt=1, prior_proofs=fx.proofs(self.candidate, attempt=1),
                               explained=())
        second = fx.intent(self.candidate, attempt=2, triage_sha256=unapproved.sha256)
        invalid = fx.history(self.candidate, intents=(self.first, second),
                             proof_records=fx.proofs(self.candidate, attempt=1), triages=(unapproved,))
        self.assertEqual(codes(self.decide(invalid, self.claim(DARWIN_ARM64, attempt=2))),
                         {"replacement_unapproved"})


class ClaimOnlyRecoveryTests(unittest.TestCase):
    """Attempt 1 proves three platforms; the Darwin/arm64 claim produced no proof."""

    def setUp(self):
        self.candidate = fx.bound()
        self.first = fx.intent(self.candidate)
        self.claims = tuple(fx.claim(self.candidate, attempt=1, platform=platform) for platform in PLATFORMS)
        self.proofs = tuple(fx.proof(self.candidate, attempt=1, platform=platform)
                            for platform in PLATFORMS if platform != DARWIN_ARM64)
        self.unproven = next(claim for claim in self.claims if claim.platform == DARWIN_ARM64)
        self.subject = ClaimSubject(claim_sha256=claim_sha256(self.unproven))
        recorded = self.recorded()
        self.prior_findings = evaluate_attempt(self.candidate.manifest, recorded, 1).findings

    def recorded(self, triages=()):
        return fx.history(self.candidate, intents=(self.first,), claims=self.claims,
                          proof_records=self.proofs, triages=triages)

    def closure(self, resources, *, invocation=None):
        return fx.resolution(self.subject, invocation=invocation or self.unproven.invocation_id,
                             resources=resources)

    def triage(self, fact, *, explained=None, prior_claims=None, closure_refs=None):
        return fx.triage(self.candidate, attempt=1, prior_proofs=self.proofs,
                         prior_claims=self.claims if prior_claims is None else prior_claims,
                         explained=self.prior_findings if explained is None else explained,
                         approved=fx.approval(attempt=2),
                         closure_refs=(fx.reference(fact),) if closure_refs is None else closure_refs,
                         resolutions=(fact,))

    def decisions(self, triage):
        second = fx.intent(self.candidate, attempt=2, triage_sha256=triage.sha256)
        launch = decide_launch(candidate=self.candidate, history=self.recorded((triage,)), intent=second)
        replaced = fx.history(self.candidate, intents=(self.first, second),
                              claims=self.claims + fx.claims_for(self.candidate, fx.proofs(self.candidate, attempt=2)),
                              proof_records=self.proofs + fx.proofs(self.candidate, attempt=2), triages=(triage,))
        readiness = evaluate_readiness(candidate=self.candidate, observed=fx.observed(), scope=fx.scope(),
                                       history=replaced, review=fx.review(self.candidate))
        return launch, readiness

    def assertReplacedWithIncompletePrior(self, fact):
        launch, readiness = self.decisions(self.triage(fact))
        self.assertTrue(launch.allowed, launch.findings)
        self.assertTrue(readiness.ready, readiness.findings)
        prior = readiness.attempt_verdicts[0]
        self.assertEqual(prior.state, AttemptState.INCOMPLETE)
        self.assertIn(FindingRef(code="proof_missing", subject="Darwin/arm64"), prior.findings)
        self.assertEqual(prior.findings, self.prior_findings)
        self.assertEqual(readiness.replacements[0].resolutions, (fact,))

    def assertRefused(self, triage, expected):
        launch, readiness = self.decisions(triage)
        self.assertFalse(launch.allowed)
        self.assertEqual(codes(launch), expected)
        self.assertEqual(codes(readiness), expected)

    def test_crash_before_native_intend_permits_reviewed_successor(self):
        self.assertReplacedWithIncompletePrior(self.closure(((NATIVE_CHILD_RESOURCE, NOT_STARTED),)))

    def test_started_child_with_positively_closed_cases_and_no_aggregate_permits_successor(self):
        started = ((NATIVE_CHILD_RESOURCE, WAITED),) + tuple((resource, WAITED) for resource in CASE_RESOURCES)
        self.assertReplacedWithIncompletePrior(self.closure(started))

    def test_unresolved_claim_without_validated_closure_blocks_successor(self):
        fact = self.closure(((NATIVE_CHILD_RESOURCE, NOT_STARTED),))
        self.assertRefused(self.triage(fact, closure_refs=()), {"closure_resolution_missing"})
        reviewed = self.closure(((NATIVE_CHILD_RESOURCE, WAITED),) + tuple((r, WAITED) for r in CASE_RESOURCES))
        incomplete = self.closure(((NATIVE_CHILD_RESOURCE, WAITED), (CASE_RESOURCES[0], WAITED)))
        triage = fx.triage(self.candidate, attempt=1, prior_proofs=self.proofs, prior_claims=self.claims,
                           explained=self.prior_findings, approved=fx.approval(attempt=2),
                           closure_refs=(fx.reference(reviewed),), resolutions=(incomplete,))
        self.assertRefused(triage, {"closure_resolution_unverified"})

    def test_foreign_or_mismatched_claim_closure_blocks_successor(self):
        other_invocation = self.closure(((NATIVE_CHILD_RESOURCE, NOT_STARTED),), invocation="invocation-other")
        self.assertRefused(self.triage(other_invocation), {"closure_resolution_mismatch"})
        wrong_subject = fx.resolution(ProofSubject(proof_sha256="1" * 64), invocation=self.unproven.invocation_id,
                                      resources=((NATIVE_CHILD_RESOURCE, WAITED),))
        self.assertRefused(self.triage(wrong_subject), {"closure_resolution_missing", "closure_resolution_foreign"})

    def test_triage_must_name_every_claim_and_explain_proof_missing(self):
        fact = self.closure(((NATIVE_CHILD_RESOURCE, NOT_STARTED),))
        self.assertRefused(self.triage(fact, prior_claims=self.claims[:-1]), {"triage_claim_mismatch"})
        explained = tuple(finding for finding in self.prior_findings if finding.code != "proof_missing")
        self.assertRefused(self.triage(fact, explained=explained), {"triage_incomplete"})
        extra = self.prior_findings + (FindingRef(code="proof_missing", subject="Linux/amd64"),)
        self.assertRefused(self.triage(fact, explained=extra), {"triage_foreign_finding"})


if __name__ == "__main__":
    unittest.main()
