"""Candidate manifest, locator and attempt records accept only exact canonical facts."""

from __future__ import annotations

from dataclasses import replace
from enum import Enum
import json
import unittest

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    MAX_RATIONALE_CHARS, AttemptIntent, BlockReason, CellResult, CellStatus, ChangeScope, MatrixScope,
    ProviderImport, ReviewedChangeScope, ReviewedSource, ReviewProvenance, SourceReview, encode_intent,
    encode_review, parse_intent, parse_review,
)
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    ClaimSubject, ClosureResolutionRef, ProofSubject,
)
from scripts.delegate_acceptance.release_verification.candidate_history import (
    STAGE as HISTORY_STAGE, CandidateHistory, FindingRef, encode_triage, parse_triage, require_triage_inventory,
)
from scripts.delegate_acceptance.release_verification.candidate_codec import encode_record
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    PLATFORMS, CandidateLocator, bind_manifest, encode_locator, encode_manifest, parse_locator,
    parse_manifest,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness import BoundCandidate
from scripts.delegate_acceptance.release_verification.contract import VerificationError


def mutated(data: bytes, change) -> bytes:
    record = json.loads(data)
    change(record)
    return encode_record(record, "test")


class ManifestTests(unittest.TestCase):
    def setUp(self):
        self.manifest = fx.manifest()
        self.data = encode_manifest(self.manifest)

    def assertCode(self, code, function, *args):
        with self.assertRaises(VerificationError) as caught:
            function(*args)
        self.assertEqual(caught.exception.code, code)

    def test_round_trip_owns_exact_inventory(self):
        self.assertEqual(parse_manifest(self.data), self.manifest)
        paths = [entry.path for entry in self.manifest.artifacts]
        self.assertEqual(paths, sorted([
            "checksums.txt", "delegation-layer-0.1.37.tgz", "delegation-layer.rb",
            "delegation-layer_0.1.37_Darwin_amd64.tar.gz", "delegation-layer_0.1.37_Darwin_arm64.tar.gz",
            "delegation-layer_0.1.37_Linux_amd64.tar.gz", "delegation-layer_0.1.37_Linux_arm64.tar.gz",
        ]))
        self.assertNotIn(sha(self.data), self.data.decode())

    def test_non_canonical_and_duplicate_key_bytes_are_rejected(self):
        pretty = json.dumps(json.loads(self.data), indent=2, sort_keys=True).encode()
        self.assertCode("candidate_record_not_canonical", parse_manifest, pretty)
        duplicate = self.data.replace(b'{"artifacts"', b'{"artifacts":[],"artifacts"', 1)
        self.assertCode("candidate_record_invalid", parse_manifest, duplicate)
        for constant in (b"NaN", b"Infinity", b"-Infinity"):
            self.assertCode("candidate_record_invalid", parse_manifest, b'{"version":' + constant + b"}\n")
        self.assertCode("candidate_record_invalid", parse_manifest, b"\xff")
        self.assertCode("candidate_record_too_large", parse_manifest, b" " * (64 * 1024 + 1))

    def test_unsafe_artifact_paths_are_rejected(self):
        for path in ("../checksums.txt", "/checksums.txt", "dist\\checksums.txt", "./checksums.txt",
                     "dist//checksums.txt", "", "check\nsums.txt"):
            data = mutated(self.data, lambda record, path=path: record["artifacts"][0].update(path=path))
            self.assertCode("candidate_artifact_path_invalid", parse_manifest, data)

    def test_duplicate_missing_and_mislabeled_artifacts_are_rejected(self):
        data = mutated(self.data, lambda record: record["artifacts"].insert(1, record["artifacts"][0]))
        self.assertCode("candidate_artifact_duplicate", parse_manifest, data)
        data = mutated(self.data, lambda record: record["artifacts"].pop())
        self.assertCode("candidate_artifact_inventory_mismatch", parse_manifest, data)
        data = mutated(self.data, lambda record: record["artifacts"][0].update(arch="arm64"))
        self.assertCode("candidate_artifact_inventory_mismatch", parse_manifest, data)
        data = mutated(self.data, lambda record: record["artifacts"][0].update(path="delegation-layer-9.9.9.tgz"))
        self.assertCode("candidate_artifact_inventory_mismatch", parse_manifest, data)
        data = mutated(self.data, lambda record: record["artifacts"].reverse())
        self.assertCode("candidate_artifact_inventory_mismatch", parse_manifest, data)

    def test_artifact_digest_and_size_errors_are_rejected(self):
        for field, value in (("sha256", "A" * 64), ("sha256", "a" * 63), ("size", 0), ("size", True),
                             ("kind", "binary")):
            data = mutated(self.data, lambda record, f=field, v=value: record["artifacts"][0].update({f: v}))
            self.assertCode("candidate_artifact_invalid", parse_manifest, data)

    def test_identity_errors_are_rejected(self):
        cases = (
            ("candidate_build_tag_mismatch", lambda r: r.update(build_tag="v0.1.38")),
            ("candidate_field_invalid", lambda r: r.update(version="v0.1.37")),
            ("candidate_field_invalid", lambda r: r["source"].update(commit="A" * 40)),
            ("candidate_field_invalid", lambda r: r["source"].update(tree="short")),
            ("candidate_field_invalid", lambda r: r["build"].update(go_sum_sha256="0")),
            ("candidate_field_invalid", lambda r: r["build"].update(harness_sha256="A" * 64)),
            ("candidate_field_invalid", lambda r: r["build"].update(harness_sha256=None)),
            ("candidate_record_fields_invalid", lambda r: r["build"].pop("harness_sha256")),
            ("candidate_field_invalid", lambda r: r["build"]["toolchains"].update(go="go1.27.3")),
            ("candidate_record_fields_invalid", lambda r: r["build"]["toolchains"].pop("node")),
            ("candidate_record_fields_invalid", lambda r: r.update(self_sha256="0" * 64)),
            ("candidate_manifest_kind_invalid", lambda r: r.update(schema_version=2)),
        )
        for code, change in cases:
            self.assertCode(code, parse_manifest, mutated(self.data, change))

    def test_locator_binds_manifest_bytes_externally(self):
        locator = fx.locator_for(self.manifest)
        self.assertEqual(parse_locator(encode_locator(locator)), locator)
        self.assertEqual(bind_manifest(locator, self.data), self.manifest)
        self.assertEqual(locator.candidate_id, sha(self.data))
        changed = mutated(self.data, lambda r: r["build"].update(go_sum_sha256="3" * 64))
        self.assertCode("candidate_manifest_digest_mismatch", bind_manifest, locator, changed)
        other = fx.manifest("0.1.38")
        with self.assertRaises(VerificationError) as caught:
            BoundCandidate(locator=locator, manifest=other)
        self.assertEqual(caught.exception.code, "candidate_manifest_digest_mismatch")

    def test_build_harness_fingerprint_is_part_of_candidate_identity(self):
        rebuilt = fx.manifest(harness_sha256="b" * 64)
        self.assertEqual(parse_manifest(encode_manifest(rebuilt)).build.harness_sha256, "b" * 64)
        self.assertNotEqual(fx.locator_for(rebuilt).candidate_id, fx.locator_for(self.manifest).candidate_id)
        changed = mutated(self.data, lambda r: r["build"].update(harness_sha256="b" * 64))
        self.assertCode("candidate_manifest_digest_mismatch", bind_manifest, fx.locator_for(self.manifest), changed)

    def test_locator_identity_errors_are_rejected(self):
        data = encode_locator(fx.locator_for(self.manifest))
        for field, value in (("run_id", 0), ("run_attempt", True), ("artifact_id", "77"),
                             ("artifact_archive_sha256", "sha256:" + "f" * 64),
                             ("repository", "no-slash")):
            self.assertCode("candidate_field_invalid", parse_locator,
                            mutated(data, lambda r, f=field, v=value: r.update({f: v})))
        self.assertCode("candidate_locator_kind_invalid", parse_locator,
                        mutated(data, lambda r: r.update(kind="other")))
        self.assertIsInstance(fx.locator_for(self.manifest), CandidateLocator)


class LookalikeReason(Enum):
    """An unrelated enum whose members share the canonical reason values."""

    AUTH_UNAVAILABLE = "auth-unavailable"
    UNCLASSIFIED = "unclassified"


class AttemptRecordTests(unittest.TestCase):
    def setUp(self):
        self.candidate = fx.bound()

    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)

    def test_intent_round_trip_and_replacement_contract(self):
        first = fx.intent(self.candidate, change=fx.scope(MatrixScope.AFFECTED_PROVIDERS, ("claude",)))
        self.assertEqual(parse_intent(encode_intent(first)), first)
        second = fx.intent(self.candidate, attempt=2, triage_sha256="4" * 64)
        self.assertEqual(parse_intent(encode_intent(second)), second)
        data = mutated(encode_intent(second), lambda r: r.update(replaces=None))
        self.assertCode("candidate_intent_invalid", parse_intent, data)
        data = mutated(encode_intent(second), lambda r: r["replaces"].update(prior_attempt=3))
        self.assertCode("candidate_intent_invalid", parse_intent, data)
        data = mutated(encode_intent(second), lambda r: r["replaces"].update(authorization_sha256="4" * 64))
        self.assertCode("candidate_record_fields_invalid", parse_intent, data)
        data = mutated(encode_intent(first), lambda r: r["platforms"].__setitem__(0, {"os": "Windows", "arch": "amd64"}))
        self.assertCode("candidate_platform_invalid", parse_intent, data)
        data = mutated(encode_intent(first), lambda r: r["platforms"].reverse())
        self.assertCode("candidate_intent_invalid", parse_intent, data)

    def test_scope_inputs_are_explicit_known_providers(self):
        invalid = (
            (MatrixScope.AFFECTED_PROVIDERS, (), ()),
            (MatrixScope.SHARED, ("gemini",), ()),
            (MatrixScope.SHARED, ("codex", "claude"), ()),
        )
        for matrix, changed, imports in invalid:
            self.assertCode("candidate_scope_invalid", ChangeScope, matrix=matrix,
                            changed_providers=changed, provider_imports=imports)
        self.assertCode("candidate_scope_invalid", ProviderImport, importer="claude", imported="claude")
        self.assertCode("candidate_scope_invalid", ProviderImport, importer="ccr", imported=["claude"])
        data = mutated(encode_intent(fx.intent(self.candidate)), lambda r: r["scope"].update(matrix="partial"))
        self.assertCode("candidate_scope_invalid", parse_intent, data)
        chained = ChangeScope(matrix=MatrixScope.AFFECTED_PROVIDERS, changed_providers=("claude",),
                              provider_imports=(ProviderImport(importer="ccr", imported="claude"),
                                                ProviderImport(importer="pi", imported="ccr")))
        self.assertEqual(chained.affected_providers(), frozenset({"claude", "ccr", "pi"}))

    def test_triage_parses_to_a_raw_record_bound_by_its_bytes(self):
        prior = fx.proofs(self.candidate, attempt=1)
        finding = FindingRef(code="cell_failed", subject=f"Linux/amd64/{fx.case_id('codex:exec', 'write')}")
        reference = ClosureResolutionRef(subject=ProofSubject(proof_sha256=prior[0].proof_sha256),
                                         invocation_id=prior[0].invocation_id, evidence_sha256="4" * 64)
        claimed = ClosureResolutionRef(subject=ClaimSubject(claim_sha256="5" * 64),
                                       invocation_id="invocation-claim-only", evidence_sha256="6" * 64)
        triage = fx.triage(self.candidate, attempt=1, prior_proofs=prior, explained=(finding,),
                           approved=fx.approval(attempt=2), unresolved=("codex-write",),
                           closure_refs=(reference, claimed))
        data = encode_triage(triage.record)
        self.assertEqual(parse_triage(data), triage.record)
        self.assertEqual(triage.sha256, sha(data))
        self.assertEqual([ref.key[0] for ref in parse_triage(data).closure_resolutions], ["claim", "proof"])
        invalid_subject = "candidate_closure_subject_invalid"
        subject_cases = (({}, invalid_subject), ({"claim_sha256": "5" * 64, "proof_sha256": "4" * 64}, invalid_subject),
                         ({"proof": "4" * 64}, invalid_subject), (None, invalid_subject), ("4" * 64, invalid_subject),
                         ({"proof_sha256": None}, "candidate_field_invalid"),
                         ({"claim_sha256": "A" * 64}, "candidate_field_invalid"))
        for subject, code in subject_cases:
            with self.subTest(subject=subject):
                changed = mutated(data, lambda r, v=subject: r["closure_resolutions"][1].update(subject=v))
                self.assertCode(code, parse_triage, changed)
        cases = (
            ("candidate_triage_invalid", lambda r: r.update(prior_claim_sha256s=[])),
            ("candidate_triage_invalid", lambda r: r["closure_resolutions"].reverse()),
            ("candidate_record_fields_invalid", lambda r: r.pop("prior_claim_sha256s")),
            ("candidate_field_invalid", lambda r: r["explained_findings"][0].update(code="Cell-Failed")),
            ("candidate_field_invalid", lambda r: r.update(unresolved_product_findings=["z", "a"])),
            ("candidate_field_invalid", lambda r: r.update(reviewer="")),
            ("candidate_field_invalid", lambda r: r["closure_resolutions"][0].update(invocation_id="/abs")),
            ("candidate_triage_invalid", lambda r: r["approval"].update(next_attempt=3)),
            ("candidate_triage_invalid", lambda r: r.update(diagnosis_sha256s=[])),
            ("candidate_record_fields_invalid", lambda r: r.update(approved=True)),
        )
        for code, change in cases:
            with self.subTest(code=code):
                self.assertCode(code, parse_triage, mutated(data, change))
        self.assertCode("candidate_record_kind_invalid", parse_review, data)

    def test_review_parses_to_a_raw_record_without_authority(self):
        record = fx.review_record(self.candidate, unresolved=("p1-finding",))
        data = encode_review(record)
        parsed = parse_review(data)
        self.assertEqual(parsed, record)
        self.assertIsInstance(parsed, SourceReview)
        self.assertNotIsInstance(parsed, ReviewedSource)
        self.assertEqual(fx.review(self.candidate, unresolved=("p1-finding",)).sha256, sha(data))
        self.assertCode("candidate_record_fields_invalid", parse_review,
                        mutated(data, lambda r: r.pop("reviewer")))
        self.assertCode("candidate_field_invalid", parse_review,
                        mutated(data, lambda r: r.update(reviewer="bad/reviewer")))

    def test_review_scope_round_trips_its_exact_canonical_bytes(self):
        change = fx.scope(MatrixScope.AFFECTED_PROVIDERS, ("claude",))
        record = fx.review_record(self.candidate, change=change)
        data = encode_review(record)
        parsed = parse_review(data)
        self.assertEqual(parsed, record)
        self.assertEqual(encode_review(parsed), data)
        self.assertEqual(parsed.reviewed_scope, ReviewedChangeScope(
            scope=change, base_commit=fx.BASE, base_tree=fx.BASE_TREE,
            changed_path_count=fx.CHANGED_PATH_COUNT, changed_paths_sha256=fx.CHANGED_PATHS_SHA256,
            subsystem_registry_sha256=fx.SUBSYSTEM_REGISTRY_SHA256, rationale=fx.RATIONALE))
        self.assertEqual(set(json.loads(data)["reviewed_scope"]), {
            "scope", "base_commit", "base_tree", "changed_path_count", "changed_paths_sha256",
            "subsystem_registry_sha256", "rationale"})

    def test_review_without_an_exact_scope_never_parses(self):
        data = encode_review(fx.review_record(self.candidate))
        cases = (
            ("candidate_record_fields_invalid", lambda r: r.pop("reviewed_scope")),
            ("candidate_record_fields_invalid", lambda r: r.update(reviewed_scope=None)),
            ("candidate_record_fields_invalid", lambda r: r.update(scope=r["reviewed_scope"]["scope"])),
            ("candidate_record_fields_invalid", lambda r: r["reviewed_scope"].pop("base_commit")),
            ("candidate_record_fields_invalid", lambda r: r["reviewed_scope"].update(head_commit=fx.COMMIT)),
            ("candidate_record_fields_invalid", lambda r: r["reviewed_scope"].update(scope=None)),
            ("candidate_record_fields_invalid", lambda r: r["reviewed_scope"]["scope"].pop("matrix")),
            ("candidate_scope_invalid", lambda r: r["reviewed_scope"]["scope"].update(matrix="none")),
            ("candidate_scope_invalid", lambda r: r["reviewed_scope"]["scope"].update(
                matrix="affected-providers", changed_providers=[])),
            ("candidate_scope_invalid", lambda r: r["reviewed_scope"]["scope"].update(
                changed_providers=["unknown"])),
            ("candidate_field_invalid", lambda r: r["reviewed_scope"].update(base_commit="c" * 39)),
            ("candidate_field_invalid", lambda r: r["reviewed_scope"].update(base_tree=None)),
            ("candidate_field_invalid", lambda r: r["reviewed_scope"].update(changed_path_count=-1)),
            ("candidate_field_invalid", lambda r: r["reviewed_scope"].update(changed_path_count=True)),
            ("candidate_field_invalid", lambda r: r["reviewed_scope"].update(changed_path_count=1.5)),
            ("candidate_field_invalid", lambda r: r["reviewed_scope"].update(changed_path_count="3")),
            ("candidate_field_invalid", lambda r: r["reviewed_scope"].update(changed_paths_sha256="A" * 64)),
            ("candidate_field_invalid", lambda r: r["reviewed_scope"].update(subsystem_registry_sha256="1" * 63)),
            ("candidate_review_invalid", lambda r: r["reviewed_scope"].update(rationale="")),
            ("candidate_review_invalid", lambda r: r["reviewed_scope"].update(rationale=" padded")),
            ("candidate_review_invalid", lambda r: r["reviewed_scope"].update(rationale="tab\there")),
            ("candidate_review_invalid", lambda r: r["reviewed_scope"].update(rationale="caf\u00e9")),
            ("candidate_review_invalid", lambda r: r["reviewed_scope"].update(
                rationale="x" * (MAX_RATIONALE_CHARS + 1))),
            ("candidate_review_invalid", lambda r: r["reviewed_scope"].update(rationale=["list"])),
        )
        for index, (code, change) in enumerate(cases):
            with self.subTest(index=index, code=code):
                self.assertCode(code, parse_review, mutated(data, change))
        multiline = mutated(data, lambda r: r["reviewed_scope"].update(rationale="First line.\nSecond line."))
        self.assertEqual(parse_review(multiline).reviewed_scope.rationale, "First line.\nSecond line.")
        longest = mutated(data, lambda r: r["reviewed_scope"].update(rationale="x" * MAX_RATIONALE_CHARS))
        self.assertEqual(len(parse_review(longest).reviewed_scope.rationale), MAX_RATIONALE_CHARS)

    def test_review_record_requires_a_typed_reviewed_scope(self):
        record = fx.review_record(self.candidate)
        for value in (None, record.reviewed_scope.scope, {"scope": "shared"}):
            with self.subTest(value=value):
                self.assertCode("candidate_review_invalid", replace, record, reviewed_scope=value)
        self.assertCode("candidate_review_invalid", replace, record.reviewed_scope, scope=None)

    def test_changed_review_scope_bytes_invalidate_provenance(self):
        record = fx.review_record(self.candidate)
        provenance = ReviewProvenance(artifact_sha256=sha(encode_review(record)), reviewer=fx.REVIEWER,
                                      transaction_commit=fx.TRANSACTION)
        self.assertEqual(ReviewedSource(record=record, provenance=provenance).sha256, provenance.artifact_sha256)
        reviewed = record.reviewed_scope
        for field, value in (("scope", fx.scope(MatrixScope.SHARED, ("opencode",))),
                             ("base_commit", "9" * 40), ("base_tree", "9" * 40),
                             ("changed_path_count", reviewed.changed_path_count + 1),
                             ("changed_paths_sha256", "9" * 64), ("subsystem_registry_sha256", "9" * 64),
                             ("rationale", reviewed.rationale + " Edited.")):
            with self.subTest(field=field):
                changed = replace(record, reviewed_scope=replace(reviewed, **{field: value}))
                self.assertNotEqual(encode_review(changed), encode_review(record))
                self.assertCode("candidate_review_provenance_mismatch", ReviewedSource, record=changed,
                                provenance=provenance)

    def test_reviewed_source_requires_matching_provenance(self):
        record = fx.review_record(self.candidate)
        digest = sha(encode_review(record))
        for provenance in (
                ReviewProvenance(artifact_sha256="8" * 64, reviewer=fx.REVIEWER, transaction_commit=fx.TRANSACTION),
                ReviewProvenance(artifact_sha256=digest, reviewer="someone-else", transaction_commit=fx.TRANSACTION),
                None, digest):
            with self.subTest(provenance=provenance):
                self.assertCode("candidate_review_provenance_mismatch", ReviewedSource, record=record,
                                provenance=provenance)
        valid = ReviewProvenance(artifact_sha256=digest, reviewer=fx.REVIEWER, transaction_commit=fx.TRANSACTION)
        self.assertEqual(ReviewedSource(record=record, provenance=valid).sha256, digest)

    def test_cell_status_requires_typed_block_reason(self):
        case = fx.case_id("pi:json", "write")
        self.assertCode("candidate_cell_invalid", CellResult, case_id=case, status=CellStatus.BLOCKED,
                        block_reason=None)
        self.assertCode("candidate_cell_invalid", CellResult, case_id=case, status=CellStatus.PASS,
                        block_reason=BlockReason.AUTH_UNAVAILABLE)
        self.assertCode("candidate_cell_invalid", CellResult, case_id=["unhashable"], status=CellStatus.PASS,
                        block_reason=None)

    def test_cell_reason_is_none_or_a_canonical_member(self):
        case = fx.case_id("pi:json", "write")
        for reason in BlockReason:
            self.assertIs(CellResult(case_id=case, status=CellStatus.BLOCKED, block_reason=reason).block_reason,
                          reason)
        for status in (CellStatus.PASS, CellStatus.FAIL):
            self.assertIsNone(CellResult(case_id=case, status=status, block_reason=None).block_reason)
        cases = [(status, value) for status in (CellStatus.PASS, CellStatus.FAIL)
                 for value in (LookalikeReason.AUTH_UNAVAILABLE, "auth-unavailable", "", 1, 0, False, object())]
        cases += [(CellStatus.BLOCKED, member) for member in LookalikeReason]
        for status, reason in cases:
            with self.subTest(status=status, reason=reason):
                self.assertCode("candidate_cell_invalid", CellResult, case_id=case, status=status,
                                block_reason=reason)

    def test_history_rejects_inconsistent_append_records(self):
        first = fx.intent(self.candidate)
        second = fx.intent(self.candidate, attempt=2, triage_sha256="4" * 64)
        linux_only = tuple(platform for platform in PLATFORMS if platform[0] == "Linux")
        narrow = AttemptIntent(candidate_id=first.candidate_id, attempt=1, source_commit=first.source_commit,
                               scope=first.scope, platforms=linux_only, replaces=None)
        darwin = fx.proof(self.candidate, attempt=1, platform=("Darwin", "arm64"))
        invalid = (
            dict(intents=(second,)),
            dict(intents=(first, first)),
            dict(intents=(narrow,), proof_records=(darwin,)),
            dict(intents=(first,), proof_records=(darwin, darwin)),
            dict(intents=(first,), proof_records=(fx.proof(self.candidate, attempt=2, platform=PLATFORMS[0]),)),
            dict(intents=(first,), triages=(fx.triage(self.candidate, attempt=2, prior_proofs=(darwin,),
                                                      explained=()),)),
        )
        claim = fx.claim(self.candidate, attempt=1, platform=("Darwin", "arm64"))
        other = fx.claim(self.candidate, attempt=1, platform=("Linux", "amd64"))
        def variant(base, **change):
            values = {field: getattr(base, field) for field in base.__dataclass_fields__}
            values.update(change)
            return type(base)(**values)
        invalid += (
            dict(intents=(first,), claims=(claim, variant(claim, invocation_id="invocation-again"))),
            dict(intents=(first,), claims=(claim, variant(other, invocation_id=claim.invocation_id))),
            dict(intents=(first,), claims=(variant(claim, candidate_id="9" * 64),)),
            dict(intents=(narrow,), claims=(claim,)),
            dict(intents=(first,), claims=(variant(claim, attempt=2),)),
            dict(intents=(first,), claims=(), proof_records=(darwin,)),
            dict(intents=(first,), claims=(variant(claim, invocation_id="invocation-other"),),
                 proof_records=(darwin,)),
        )
        for arguments in invalid:
            self.assertCode("candidate_history_invalid", fx.history, self.candidate, **arguments)
        foreign = fx.intent(self.candidate)
        self.assertCode("candidate_history_invalid", CandidateHistory, candidate_id="9" * 64,
                        intents=(foreign,), claims=(), proofs=(), installs=(), triages=(),
                        publication=None)
        self.assertCode("candidate_history_invalid", CandidateHistory, candidate_id=foreign.candidate_id,
                        intents=(foreign,), claims=(encode_intent(foreign),), proofs=(), installs=(),
                        triages=(), publication=None)

    def test_triage_inventory_binds_candidate_attempt_range_and_exact_digest(self):
        darwin = fx.proof(self.candidate, attempt=1, platform=("Darwin", "arm64"))
        first = fx.triage(self.candidate, attempt=1, prior_proofs=(darwin,), explained=()).record
        second = fx.triage(self.candidate, attempt=2, prior_proofs=(darwin,), explained=()).record
        candidate_id = self.candidate.candidate_id
        for attempts, records in ((0, ()), (1, (first,)), (2, (first, second))):
            with self.subTest(attempts=attempts, records=len(records)):
                self.assertIsNone(require_triage_inventory(candidate_id, attempts, records))
        invalid = (
            (1, (second,)), (0, (first,)), (1, (first, first)),
            (1, (replace(first, candidate_id="9" * 64),)),
            (1, [first]), (1, (encode_triage(first),)),
        )
        for attempts, records in invalid:
            with self.subTest(attempts=attempts, records=records):
                with self.assertRaises(VerificationError) as caught:
                    require_triage_inventory(candidate_id, attempts, records)
                self.assertEqual((caught.exception.code, caught.exception.stage),
                                 ("candidate_history_invalid", HISTORY_STAGE))

class DecoderBoundaryTests(unittest.TestCase):
    """Every candidate decoder shares one strict reader and header refusal."""

    def setUp(self):
        candidate = fx.bound()
        triage = fx.triage(candidate, attempt=1, prior_proofs=fx.proofs(candidate, attempt=1), explained=())
        self.records = (
            (parse_manifest, encode_manifest(candidate.manifest), "candidate_manifest_kind_invalid"),
            (parse_locator, encode_locator(candidate.locator), "candidate_locator_kind_invalid"),
            (parse_intent, encode_intent(fx.intent(candidate)), "candidate_record_kind_invalid"),
            (parse_triage, encode_triage(triage.record), "candidate_record_kind_invalid"),
            (parse_review, encode_review(fx.review(candidate).record), "candidate_record_kind_invalid"),
        )

    def test_schema_version_must_be_exact_integer_one(self):
        for parse, data, code in self.records:
            self.assertEqual(json.loads(data)["schema_version"], 1)
            parse(data)
            for version in (True, 1.0, [1], {"version": 1}, "1", None, 0, 2):
                changed = mutated(data, lambda record, v=version: record.update(schema_version=v))
                with self.subTest(parse=parse.__name__, version=version):
                    with self.assertRaises(VerificationError) as caught:
                        parse(changed)
                    self.assertEqual(caught.exception.code, code)
            for kind in (["wrong"], {"kind": 1}, None):
                changed = mutated(data, lambda record, k=kind: record.update(kind=k))
                with self.subTest(parse=parse.__name__, kind=kind):
                    with self.assertRaises(VerificationError) as caught:
                        parse(changed)
                    self.assertEqual(caught.exception.code, code)

    def test_hostile_nesting_is_a_typed_refusal(self):
        # Deeper than the interpreter's JSON decoder recursion limit; each
        # decoder refuses with its size bound or the shared strict reader.
        depth = 10500
        for parse, _, _ in self.records:
            expected = "candidate_record_too_large" if parse is parse_locator else "candidate_record_invalid"
            for nested in (b"[" * depth + b"]" * depth, b'{"a":' * depth + b"1" + b"}" * depth):
                with self.subTest(parse=parse.__name__, shape=nested[:1]):
                    with self.assertRaises(VerificationError) as caught:
                        parse(nested)
                    self.assertEqual(caught.exception.code, expected)

    def test_nested_unknown_field_is_refused_by_the_schema(self):
        for parse, data, _ in self.records:
            nested = mutated(data, lambda record: record.update(padding=[[[[1]]]]))
            with self.subTest(parse=parse.__name__):
                with self.assertRaises(VerificationError) as caught:
                    parse(nested)
                self.assertEqual(caught.exception.code, "candidate_record_fields_invalid")

    def test_brackets_inside_strings_are_ordinary_text(self):
        data = encode_manifest(fx.manifest())
        quoted = mutated(data, lambda record: record["artifacts"][0].update(path='[[[[[[[[[\\"{{{{{{{{{'))
        with self.assertRaises(VerificationError) as caught:
            parse_manifest(quoted)
        self.assertEqual(caught.exception.code, "candidate_artifact_path_invalid")

    def test_unhashable_enum_values_are_typed_refusals(self):
        data = encode_manifest(fx.manifest())
        with self.assertRaises(VerificationError) as caught:
            parse_manifest(mutated(data, lambda record: record["artifacts"][0].update(kind=["archive"])))
        self.assertEqual(caught.exception.code, "candidate_artifact_invalid")
        intent = encode_intent(fx.intent(fx.bound()))
        with self.assertRaises(VerificationError) as caught:
            parse_intent(mutated(intent, lambda record: record["scope"].update(matrix={"shared": 1})))
        self.assertEqual(caught.exception.code, "candidate_scope_invalid")


if __name__ == "__main__":
    unittest.main()
