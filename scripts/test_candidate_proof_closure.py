"""The semantic proof closure reader, alone and through store admission and replay.

The reader tests use the actual C2 proof's validated case 07 anchor: a failed
timeout source with no session binding whose only diagnostic task closed by
its budget. The replay tests store a
supplement for the canonical failing proof fixture and supply no trusted
closure fact, exactly as every production reader does.
"""

from __future__ import annotations

from dataclasses import replace
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.native_evidence import candidate_proof_fixture as proof_fixture
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.native_evidence.candidate_proof import validate_candidate_proof
from scripts.delegate_acceptance.native_evidence.lifetime_closure import (
    LIFETIME_CLOSURE_KIND, LIFETIME_CLOSURE_SCHEMA_VERSION,
)
from scripts.delegate_acceptance.native_evidence.public_contract import (
    CASE_IDS, CaseClosureAnchor, CaseOutcome, NativeCompletion, ValidatedCandidateProof,
)
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    CellStatus, ReviewProvenance, candidate_proof_expectation, claim_sha256,
)
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    ClosureResolutionRef, ProofSubject, ResourceDisposition, encode_closure_evidence,
)
from scripts.delegate_acceptance.release_verification.candidate_history import (
    TriageRecord, encode_triage, parse_triage,
)
from scripts.delegate_acceptance.release_verification.candidate_proof_closure import (
    CaseClosure, OrdinaryTask, ProofClosure, RetainedObservation, encode_proof_closure, parse_proof_closure,
    read_proof_closure,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    AttemptState, decide_launch, evaluate_attempt,
)
from scripts.delegate_acceptance.release_verification.candidate_record import triage
from scripts.delegate_acceptance.release_verification.candidate_store_replay import admit_append
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_store_replay import DARWIN, PLANNED, Facts, Files, ReplayCase, additions, commit, raw


# The actual retained C2 facts: the unchanged proof's public case 07 anchor, read from the original
# ZIP through its receipt's persisted expectation, and its native completion and managed root.
C2_PROOF = "bdf2eabf1785bbd015f34f29df737c52153ba81e0c37b10bc95a101cec2b3dcb"
C2_CASE = "case-07-codex-exec-workspace-write-timeout-resume"
C2_TASK = "29f63e406577d46aa20d8fb83a0d8d93"
C2_INVOCATION = "5a976262898c3f09b3c27c6015e798f4"
C2_ROOT = "cd2ddb2c07f94ded2b9d66e9198b094c"
C2_ANCHOR = CaseClosureAnchor(
    case_id=C2_CASE, source_compact_sha256="55ed424c01d88b9c4529b317c75e20c4be412eb03facc46045fc671922607b55",
    source_receipt_sha256="16c8b27f638486fcf460b995ef64fad3759c9b82c96353263a39fce288a0229d",
    cleanup_receipt_sha256="74d5a4ed98445a786340e1d802e530a4ea8163e64740cd345abc015190123fef",
    failure_diagnostics_sha256="780029bbeeaa1ec902e9f8401faecb2aa4870fc2c017f10da11022a959dc3c41",
    task_ids=(), task_session_bindings=(), diagnostic_task_ids=(C2_TASK,))
C2_COMPLETION = NativeCompletion(
    C2_INVOCATION, True, 1, "845970bab50d0dcc948815b3611fafd85f091233114bbec379c29ad29ece3030",
    "4530d36df4307fa1fec73b73479e51a55bed61ee082538fc8f886803f12a756c",
    "fd394fe85d0d4a5a1d89bb60f17ffafc42406313c31009f1d57cebe132dbe1bb")
SPEC, META = "e506039e203d033c0dd09e1cdb787fa3773c828e52a947cf587ac468c2d8a593", "d" * 64
OTHER_TASK = "0" * 31 + "1"
INSPECTION = "0" * 31 + "2"
OBSERVATION = RetainedObservation(coordinator_receipt_sha256="7" * 64, directory_identity_sha256="8" * 64,
                                  capture_records_sha256="9" * 64, capture_count=44)


def task_fact(**changes: object) -> dict[str, object]:
    """A positively closed managed task lifetime; the C2 source closed by its budget after a natural Wait."""
    return {"cause": "budget", "scope_empty": True, "wait_result": "exit-error", "recovered": False,
            "launch_count": 3, "registration_sha256": "a" * 64, "termination_sha256": "b" * 64,
            "wait_sha256": "c" * 64, "spec_sha256": SPEC, "meta_sha256": META, **changes}


def inspection_fact(**changes: object) -> dict[str, object]:
    return {"cause": "completed", "scope_empty": True, "wait_result": "success", "recovered": False,
            "launch_count": 1, "registration_sha256": "a" * 64, "termination_sha256": "b" * 64,
            "wait_sha256": "c" * 64, "request_sha256": "e" * 64, **changes}


def root(tasks: dict[str, dict[str, object]], inspections: dict[str, dict[str, object]] | None = None,
         ) -> dict[str, object]:
    if not tasks and not inspections:
        return {"root_id": None, "launch": "none", "tasks": {}, "inspections": {}}
    return {"root_id": C2_ROOT, "launch": "admitted", "tasks": tasks, "inspections": inspections or {}}


def compact(tasks: dict[str, dict[str, object]], *, discovery: object = None) -> dict[str, object]:
    """The compact closure the managed-lifetime root projection writes; no tasks is a known no-launch root."""
    return {"schema_version": LIFETIME_CLOSURE_SCHEMA_VERSION, "kind": LIFETIME_CLOSURE_KIND,
            "ordinary": root(tasks), "model_discovery": discovery}


def case(anchor: CaseClosureAnchor = C2_ANCHOR, closure: object | None = None,
         ordinary: tuple[str, ...] = (C2_TASK,), discovery: str | None = None) -> CaseClosure:
    """The witness the native owner classifies from the case's bound diagnostics and retained state."""
    return CaseClosure(
        case_id=anchor.case_id, source_compact_sha256=anchor.source_compact_sha256,
        source_receipt_sha256=str(anchor.source_receipt_sha256),
        cleanup_receipt_sha256=str(anchor.cleanup_receipt_sha256),
        failure_diagnostics_sha256=str(anchor.failure_diagnostics_sha256),
        ordinary_tasks=tuple(OrdinaryTask(task, SPEC, META) for task in sorted(ordinary)),
        discovery_inspection_id=discovery,
        lifetime_closure=compact({task: task_fact() for task in ordinary}) if closure is None else closure)


def supplement(proof: ValidatedCandidateProof, cases: tuple[CaseClosure, ...] | None = None,
               **changes: object) -> ProofClosure:
    """The supplement the native observation owner would write for every unresolved case of ``proof``."""
    completion = proof.completion
    value = ProofClosure(
        proof_sha256=proof.public_zip_sha256, invocation_id=proof.invocation_id,
        started_sha256=completion.started_sha256, invocation_sha256=completion.invocation_sha256,
        completed_sha256=completion.completed_sha256, observation=OBSERVATION,
        cases=(case(),) if cases is None else cases)
    return replace(value, **changes)


def c2_proof(anchor: CaseClosureAnchor = C2_ANCHOR) -> ValidatedCandidateProof:
    """The actual C2 FAIL proof's validated facts: case 07 is its only unresolved case."""
    return ValidatedCandidateProof(
        public_zip_sha256=C2_PROOF, aggregate_sha256="cef67627f443aaa34665b752d54f158b25e67c125e3a8d75f8040e1374cde10f",
        candidate_id="3" * 64, claim_sha256="4" * 64, invocation_id=C2_INVOCATION, platform_os="Linux",
        platform_arch="amd64", archive_sha256="5" * 64, binaries=fx.NATIVE_BINARIES, completion=C2_COMPLETION,
        status="FAIL", case_results_verdict="FAIL",
        cases=tuple(CaseOutcome(case_id, "FAIL" if case_id == anchor.case_id else "PASS", None)
                    for case_id in CASE_IDS),
        unresolved_case_ids=(anchor.case_id,), closure_anchors=(anchor,))


class ReaderTests(unittest.TestCase):
    """The reader binds the supplement to the stored proof and applies only canonical predicates."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.proof = c2_proof()

    def assertRefused(self, code: str, data: bytes, proofs: tuple[ValidatedCandidateProof, ...] | None = None) -> None:
        with self.assertRaises(VerificationError) as caught:
            read_proof_closure(data, (self.proof,) if proofs is None else proofs)
        self.assertEqual(caught.exception.code, code)

    def encoded(self, *cases: CaseClosure, proof: ValidatedCandidateProof | None = None, **changes: object) -> bytes:
        return encode_proof_closure(supplement(proof or self.proof, cases or None, **changes))

    def fact(self, **fact: object) -> bytes:
        return self.encoded(case(closure=compact({C2_TASK: task_fact(**fact)})))

    def test_the_actual_diagnostic_only_c2_case_resolves_by_its_budget_without_a_session(self):
        data = self.encoded()
        resolution = read_proof_closure(data, (self.proof,))
        self.assertEqual((resolution.subject, resolution.invocation_id, resolution.resource_ids),
                         (ProofSubject(C2_PROOF), C2_INVOCATION, (C2_CASE,)))
        self.assertEqual([(item.disposition, item.fact_sha256) for item in resolution.resources],
                         [(ResourceDisposition.WAITED, sha(data))])
        self.assertEqual(self.proof.cases[CASE_IDS.index(C2_CASE)].status, "FAIL")

    def test_an_ordinary_non_timeout_failed_case_may_close_by_its_budget(self):
        anchor = replace(C2_ANCHOR, case_id=CASE_IDS[3], task_ids=(C2_TASK,),
                         task_session_bindings=(("fresh", C2_TASK),), diagnostic_task_ids=())
        proof = c2_proof(anchor)
        resolution = read_proof_closure(self.encoded(case(anchor), proof=proof), (proof,))
        self.assertEqual(resolution.resource_ids, (CASE_IDS[3],))
        self.assertEqual(proof.cases[3].status, "FAIL")

    def test_the_proof_invocation_and_native_completion_must_be_the_stored_proofs(self):
        self.assertRefused("candidate_proof_closure_proof_unknown", self.encoded(proof_sha256="f" * 64))
        self.assertRefused("candidate_proof_closure_invocation_mismatch",
                           self.encoded(invocation_id="other-invocation"))
        for field in ("started_sha256", "invocation_sha256", "completed_sha256"):
            with self.subTest(field=field):
                self.assertRefused("candidate_proof_closure_invocation_mismatch",
                                   self.encoded(**{field: "0" * 64}))

    def test_the_cases_are_exactly_the_unresolved_cases_and_their_bound_digests(self):
        extra = replace(case(), case_id=CASE_IDS[0])
        self.assertRefused("candidate_proof_closure_cases_mismatch", self.encoded(extra, case()))
        self.assertRefused("candidate_proof_closure_cases_mismatch", self.encoded(extra))
        for field in ("source_compact_sha256", "source_receipt_sha256", "cleanup_receipt_sha256",
                      "failure_diagnostics_sha256"):
            with self.subTest(field=field):
                self.assertRefused("candidate_proof_closure_case_mismatch",
                                   self.encoded(replace(case(), **{field: "0" * 64})))
        unbound = replace(C2_ANCHOR, failure_diagnostics_sha256=None)
        with self.assertRaises(VerificationError) as caught:
            read_proof_closure(self.encoded(case(), proof=c2_proof(unbound)), (c2_proof(unbound),))
        self.assertEqual(caught.exception.code, "candidate_proof_closure_case_mismatch")
        with self.assertRaises(VerificationError):
            supplement(self.proof, ())

    def test_the_witness_classifies_exactly_the_bound_identities(self):
        for label, witness in {
                "missing": case(ordinary=(), closure=compact({})),
                "extra": case(ordinary=(C2_TASK, OTHER_TASK)),
                "foreign": case(ordinary=(OTHER_TASK,))}.items():
            with self.subTest(label):
                self.assertRefused("candidate_proof_closure_inventory_mismatch", self.encoded(witness))
        # A witness that calls the retained ordinary task an inspection contradicts the closure.
        self.assertRefused("candidate_proof_closure_lifetime_closure_incomplete", self.encoded(
            case(ordinary=(), discovery=C2_TASK, closure=compact({C2_TASK: task_fact()}))))
        session = replace(C2_ANCHOR, task_session_bindings=(("fresh", OTHER_TASK),))
        proof = c2_proof(session)
        with self.assertRaises(VerificationError) as caught:
            read_proof_closure(self.encoded(case(session, ordinary=(C2_TASK,)), proof=proof), (proof,))
        self.assertEqual(caught.exception.code, "candidate_proof_closure_inventory_mismatch")

    def test_the_closure_inventory_and_digests_are_the_witnesses(self):
        self.assertRefused("candidate_proof_closure_lifetime_closure_incomplete", self.encoded(
            case(closure=compact({C2_TASK: task_fact(), OTHER_TASK: task_fact()}))))
        self.assertRefused("candidate_proof_closure_lifetime_closure_incomplete",
                           self.encoded(case(closure=compact({}))))
        self.assertRefused("candidate_proof_closure_task_digest_mismatch", self.fact(spec_sha256="0" * 64))
        self.assertRefused("candidate_proof_closure_task_digest_mismatch", self.fact(meta_sha256="0" * 64))

    def test_scope_recovery_and_wait_must_be_positive(self):
        for fact in ({"scope_empty": False}, {"recovered": True}, {"wait_result": "recovered"},
                     {"wait_result": None}, {"launch_count": 0}, {"cause": "requested"}):
            with self.subTest(fact=fact):
                self.assertRefused("candidate_proof_closure_lifetime_closure_unproven", self.fact(**fact))

    def test_model_discovery_admission_is_the_witnessed_inspection(self):
        anchor = replace(C2_ANCHOR, diagnostic_task_ids=(C2_TASK, INSPECTION))
        proof = c2_proof(anchor)
        discovered = root({}, {INSPECTION: inspection_fact()})
        witness = case(anchor, discovery=INSPECTION, closure=compact({C2_TASK: task_fact()}, discovery=discovered))
        self.assertEqual(read_proof_closure(self.encoded(witness, proof=proof), (proof,)).resource_ids, (C2_CASE,))
        for label, closure in {"absent": compact({C2_TASK: task_fact()}),
                               "foreign": compact({C2_TASK: task_fact()}, discovery=root({}, {
                                   OTHER_TASK: inspection_fact()})),
                               "budget": compact({C2_TASK: task_fact()}, discovery=root({}, {
                                   INSPECTION: inspection_fact(cause="budget")}))}.items():
            with self.subTest(label), self.assertRaises(VerificationError) as caught:
                read_proof_closure(self.encoded(replace(witness, lifetime_closure=closure), proof=proof), (proof,))
            self.assertTrue(caught.exception.code.startswith("candidate_proof_closure_lifetime_closure_"))
        self.assertRefused("candidate_proof_closure_lifetime_closure_incomplete", self.encoded(case(
            closure=compact({C2_TASK: task_fact()}, discovery=root({}, {INSPECTION: inspection_fact()})))))

    def test_a_no_launch_root_has_no_invented_identity(self):
        anchor = replace(C2_ANCHOR, diagnostic_task_ids=())
        proof = c2_proof(anchor)
        witness = case(anchor, ordinary=(), closure=compact({}))
        self.assertEqual(read_proof_closure(self.encoded(witness, proof=proof), (proof,)).resource_ids, (C2_CASE,))
        invented = compact({})
        invented["ordinary"] = {**invented["ordinary"], "root_id": C2_ROOT}
        with self.assertRaises(VerificationError) as caught:
            read_proof_closure(self.encoded(replace(witness, lifetime_closure=invented), proof=proof), (proof,))
        self.assertEqual(caught.exception.code, "candidate_proof_closure_lifetime_closure_invalid")

    def test_codec_validity_alone_is_not_a_resolution(self):
        data = self.encoded(case(closure={"kind": "anything"}))
        self.assertEqual(parse_proof_closure(data).cases[0].lifetime_closure, {"kind": "anything"})
        self.assertRefused("candidate_proof_closure_lifetime_closure_invalid", data)
        self.assertRefused("candidate_record_not_canonical", data.replace(b'{"cases"', b'{ "cases"', 1))
        self.assertRefused("candidate_proof_closure_proof_unknown", self.encoded(), proofs=())


FIXTURE_FAILED_CASE = "case-02-claude-print-workspace-write-write"


def bind_private_records(case_record: dict[str, object]) -> None:
    """Bind the cleanup receipt and failure diagnostics digests a real failed case records."""
    case_record["cleanup"]["observation"]["cleanup_receipt_sha256"] = "1" * 64
    case_record["source_proof"]["lifecycle_proof"]["failure_diagnostics_sha256"] = "2" * 64


class ReplayTests(ReplayCase):
    """Attempt 1 has a passing Darwin proof and a failing Linux proof with one unresolved case.

    The failing case binds its cleanup receipt and failure diagnostics, so a
    native witness can classify its identities; its verdict stays FAIL.
    """

    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()
        s = cls.s
        facts = candidate_proof_expectation(s.candidate.manifest, s.candidate.locator, cls.linux)
        cls.failing_zip = proof_fixture.candidate_zip(s.failing, facts, case_changes={
            f"cases/{FIXTURE_FAILED_CASE}.json": bind_private_records})
        cls.attempt1 = [s.register(), s.intent(), s.claim_file(cls.darwin), s.claim_file(cls.linux),
                        s.proof_file(cls.darwin, cls.darwin_zip), s.proof_file(cls.linux, cls.failing_zip)]
        prior = cls().replayed(cls.attempt1)
        cls.prior = prior.history
        cls.explained = evaluate_attempt(s.candidate.manifest, prior.history, 1).findings
        validated = cls.validated_failing()
        anchor = validated.closure_anchors[0]
        cls.supplement = encode_proof_closure(supplement(validated, (case(anchor, ordinary=(), closure=compact({})),)))
        cls.resolution = read_proof_closure(cls.supplement, (validated,))

    @classmethod
    def validated_failing(cls) -> ValidatedCandidateProof:
        s = cls.s
        return validate_candidate_proof(cls.failing_zip, candidate_proof_expectation(
            s.candidate.manifest, s.candidate.locator, cls.linux))

    def triage_bytes(self, refs: tuple[ClosureResolutionRef, ...] | None = None) -> bytes:
        if refs is None:
            refs = (ClosureResolutionRef(subject=ProofSubject(sha(self.failing_zip)),
                                         invocation_id=self.linux.invocation_id,
                                         evidence_sha256=self.resolution.evidence_sha256),)
        return encode_triage(TriageRecord(
            candidate_id=self.s.id, attempt=1,
            prior_claim_sha256s=tuple(sorted((claim_sha256(self.darwin), claim_sha256(self.linux)))),
            prior_proof_sha256s=tuple(sorted((sha(self.darwin_zip), sha(self.failing_zip)))),
            explained_findings=self.explained, diagnosis_sha256s=fx.DIAGNOSIS, unresolved_product_findings=(),
            reviewer=fx.REVIEWER, closure_resolutions=refs, approval=fx.approval(attempt=2, platforms=PLANNED)))

    def provenance(self, data: bytes) -> Facts:
        return Facts(provenance=(ReviewProvenance(artifact_sha256=sha(data), reviewer=fx.REVIEWER,
                                                  transaction_commit=commit(self.s.id, len(self.attempt1))),))

    def triage_group(self, data: bytes, *supplements: bytes) -> Files:
        group = triage(data, supplements)
        self.assertEqual(group.candidate_id, self.s.id)
        return {blob.entry.name: blob.data for blob in group.additions}

    def test_the_failing_attempt_stays_failed_and_unresolved_without_a_supplement(self):
        self.assertEqual(evaluate_attempt(self.s.candidate.manifest, self.prior, 1).state, AttemptState.FAIL)
        self.assertIn(CellStatus.FAIL, {cell.status for cell in self.prior.proofs[1].cells})
        data = self.triage_bytes(refs=())
        snapshot = self.replay([*self.attempt1, self.triage_group(data)], self.provenance(data))
        intent = fx.intent(self.s.candidate, attempt=2, platforms=PLANNED, triage_sha256=sha(data))
        decision = decide_launch(candidate=self.s.candidate, history=snapshot.history, intent=intent)
        self.assertFalse(decision.allowed)
        self.assertIn("closure_resolution_missing", {item.code for item in decision.findings})

    def test_a_stored_supplement_and_triage_permit_the_replacement_with_no_trusted_fact(self):
        data = self.triage_bytes()
        groups = [*self.attempt1, self.triage_group(data, self.supplement), self.s.intent(2, triage_sha256=sha(data))]
        snapshot = self.replay(groups, self.provenance(data))
        self.assertEqual(snapshot.history.triages[0].resolutions, (self.resolution,))
        self.assertEqual([intent.attempt for intent in snapshot.history.intents], [1, 2])
        self.assertEqual(evaluate_attempt(self.s.candidate.manifest, snapshot.history, 1).state, AttemptState.FAIL)
        self.assertEqual(snapshot.history.proofs, self.prior.proofs)

    def test_admission_reads_the_supplement_with_the_same_reader(self):
        data = self.triage_bytes()
        prefix = raw(self.s.id, self.attempt1)
        self.assertIsNone(admit_append(prefix, additions(self.triage_group(data, self.supplement)),
                                       review_provenance=(), closure_resolutions=(), installer_sources=()))
        validated = self.validated_failing()
        foreign = encode_proof_closure(supplement(validated, (case(validated.closure_anchors[0], ordinary=(),
                                                                   closure=compact({})),),
                                                  invocation_id="foreign-invocation"))
        with self.assertRaises(VerificationError) as caught:
            admit_append(prefix, additions(self.triage_group(data, foreign)), review_provenance=(),
                         closure_resolutions=(), installer_sources=())
        self.assertEqual(caught.exception.code, "candidate_proof_closure_invocation_mismatch")

    def test_the_codec_encoding_of_the_derived_resolution_is_not_admitted(self):
        data = self.triage_bytes()
        groups = [*self.attempt1, self.triage_group(data, encode_closure_evidence(self.resolution))]
        with self.assertRaises(VerificationError) as caught:
            self.replayed(groups, self.provenance(data))
        self.assertEqual(caught.exception.code, "store_closure_unverified")

    def test_an_unreferenced_supplement_refuses(self):
        data = self.triage_bytes(refs=())
        with self.assertRaises(VerificationError) as caught:
            self.replayed([*self.attempt1, self.triage_group(data, self.supplement)], self.provenance(data))
        self.assertEqual(caught.exception.code, "store_closure_unreferenced")

    def test_a_claim_without_a_proof_stays_refused(self):
        attempt1 = [self.s.register(), self.s.intent(), self.s.claim_file(self.darwin), self.s.claim_file(self.linux),
                    self.s.proof_file(self.linux, self.failing_zip)]
        history = self.replayed(attempt1).history
        data = encode_triage(replace(
            parse_triage(self.triage_bytes()), prior_proof_sha256s=(sha(self.failing_zip),),
            explained_findings=evaluate_attempt(self.s.candidate.manifest, history, 1).findings))
        facts = Facts(provenance=(ReviewProvenance(artifact_sha256=sha(data), reviewer=fx.REVIEWER,
                                                   transaction_commit=commit(self.s.id, len(attempt1))),))
        snapshot = self.replay([*attempt1, self.triage_group(data, self.supplement)], facts)
        intent = fx.intent(self.s.candidate, attempt=2, platforms=PLANNED, triage_sha256=sha(data))
        decision = decide_launch(candidate=self.s.candidate, history=snapshot.history, intent=intent)
        self.assertFalse(decision.allowed)
        self.assertEqual({(item.code, item.subject) for item in decision.findings
                          if item.code.startswith("closure_")},
                         {("closure_resolution_missing", f"{DARWIN[0]}/{DARWIN[1]}")})


if __name__ == "__main__":
    unittest.main()
