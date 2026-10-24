"""The publication freeze binds exactly what a recomputed ready verdict relied on."""

from __future__ import annotations

from dataclasses import replace
import json
import unittest

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    MAX_LIST_ITEMS, CellStatus,
)
from scripts.delegate_acceptance.release_verification.candidate_codec import encode_record
from scripts.delegate_acceptance.release_verification.candidate_history import (
    MAX_PUBLICATION_BYTES, FindingRef, PublicationAuthorization, encode_publication, parse_publication,
)
from scripts.delegate_acceptance.release_verification.candidate_install_contract import InstallChannel
from scripts.delegate_acceptance.release_verification.candidate_manifest import PLATFORMS
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    AttemptState, authorize_publication, decide_launch, evaluate_attempt, evaluate_readiness,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError


DARWIN_ARM = ("Darwin", "arm64")
LINUX_AMD = ("Linux", "amd64")
PLANNED = (DARWIN_ARM, LINUX_AMD)
CODEX_WRITE = fx.case_id("codex:exec", "write")
PARENT = "e" * 40
FIELDS = frozenset({"schema_version", "kind", "candidate_id", "attempt", "evidence_parent", "source_commit",
                    "source_tree", "proof_sha256s", "install_sha256s", "triage_sha256s", "review_sha256"})


def mutated(data: bytes, change) -> bytes:
    record = json.loads(data)
    change(record)
    return encode_record(record, "test")


def digests(count: int, *, prefix: str) -> tuple[str, ...]:
    return tuple(sorted(sha(f"{prefix}-{index}".encode()) for index in range(count)))


class PublicationCase(unittest.TestCase):
    def setUp(self):
        self.candidate = fx.bound()

    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)

    def authorize(self, history, *, observed=None, review=None, reviewed=True, parent=PARENT):
        return authorize_publication(
            candidate=self.candidate, observed=observed or fx.observed(), scope=fx.scope(), history=history,
            review=(review or fx.review(self.candidate)) if reviewed else None, evidence_parent=parent)

    def single(self, *, proof_records=None, installs=None):
        intent = fx.intent(self.candidate)
        proofs = fx.proofs(self.candidate, attempt=1) if proof_records is None else proof_records
        return fx.history(self.candidate, intents=(intent,), proof_records=proofs, installs=installs)

    def replaced(self):
        """Attempt 1 failed everywhere; one of two reviewed triages approves attempt 2 on ``PLANNED``.

        Installs exist for every platform, including those only attempt 1 planned.
        """
        first = fx.intent(self.candidate)
        failed = fx.cells(overrides={CODEX_WRITE: (CellStatus.FAIL, None)})
        prior = tuple(fx.proof(self.candidate, attempt=1, platform=platform, results=failed)
                      for platform in PLATFORMS)
        installs = fx.installs_for(self.candidate, PLATFORMS)
        base = fx.history(self.candidate, intents=(first,), proof_records=prior, installs=installs)
        explained = evaluate_attempt(self.candidate.manifest, base, 1).findings
        selected = fx.triage(self.candidate, attempt=1, prior_proofs=prior, explained=explained,
                             approved=fx.approval(attempt=2, platforms=PLANNED))
        unrelated = fx.triage(self.candidate, attempt=1, prior_proofs=prior, explained=explained)
        second = fx.intent(self.candidate, attempt=2, platforms=PLANNED, triage_sha256=selected.sha256)
        latest = tuple(fx.proof(self.candidate, attempt=2, platform=platform) for platform in PLANNED)
        history = fx.history(self.candidate, intents=(first, second), proof_records=prior + latest,
                             triages=(unrelated, selected), installs=installs)
        return history, selected, unrelated


class CodecTests(PublicationCase):
    def setUp(self):
        super().setUp()
        self.record = self.authorize(self.replaced()[0])
        self.data = encode_publication(self.record)

    def test_records_round_trip_and_are_identified_by_their_exact_bytes(self):
        for record in (self.record, self.authorize(self.single())):
            with self.subTest(attempt=record.attempt):
                data = encode_publication(record)
                self.assertEqual(parse_publication(data), record)
                self.assertEqual(encode_publication(parse_publication(data)), data)
                self.assertEqual(record.sha256, sha(data))
        value = json.loads(self.data)
        self.assertEqual(set(value), FIELDS)
        self.assertEqual((value["schema_version"], value["kind"]), (1, "delegation-layer-candidate-publication"))

    def test_header_fields_and_scalar_types_are_exact(self):
        cases = (
            ("candidate_record_fields_invalid", lambda r: r.update(ready=True)),
            ("candidate_record_fields_invalid", lambda r: r.pop("evidence_parent")),
            ("candidate_record_fields_invalid", lambda r: r.pop("triage_sha256s")),
            ("candidate_record_kind_invalid", lambda r: r.update(schema_version=True)),
            ("candidate_record_kind_invalid", lambda r: r.update(kind="delegation-layer-candidate-attempt-triage")),
            ("candidate_field_invalid", lambda r: r.update(attempt=True)),
            ("candidate_field_invalid", lambda r: r.update(attempt=0)),
            ("candidate_field_invalid", lambda r: r.update(attempt="2")),
            ("candidate_field_invalid", lambda r: r.update(candidate_id="A" * 64)),
            ("candidate_field_invalid", lambda r: r.update(evidence_parent="E" * 40)),
            ("candidate_field_invalid", lambda r: r.update(evidence_parent="e" * 39)),
            ("candidate_field_invalid", lambda r: r.update(source_commit=None)),
            ("candidate_field_invalid", lambda r: r.update(source_tree=["b" * 40])),
            ("candidate_field_invalid", lambda r: r.update(review_sha256="7" * 63)),
        )
        for code, change in cases:
            with self.subTest(code=code, change=change):
                self.assertCode(code, parse_publication, mutated(self.data, change))

    def test_inventories_are_bounded_sorted_unique_digest_lists(self):
        proofs = list(self.record.proof_sha256s)
        cases = (
            ("candidate_publication_invalid", lambda r: r.update(proof_sha256s=[])),
            ("candidate_publication_invalid", lambda r: r.update(install_sha256s=[])),
            ("candidate_publication_invalid", lambda r: r.update(proof_sha256s=proofs[::-1])),
            ("candidate_publication_invalid", lambda r: r.update(proof_sha256s=proofs + proofs[-1:])),
            ("candidate_publication_invalid", lambda r: r.update(triage_sha256s=[])),
            ("candidate_publication_invalid", lambda r: r.update(triage_sha256s=sorted(
                r["triage_sha256s"] + ["0" * 64]))),
            ("candidate_field_invalid", lambda r: r.update(install_sha256s="9" * 64)),
            ("candidate_field_invalid", lambda r: r.update(install_sha256s={"sha256": "9" * 64})),
            ("candidate_field_invalid", lambda r: r.update(proof_sha256s=["F" * 64])),
            ("candidate_field_invalid", lambda r: r.update(proof_sha256s=list(digests(MAX_LIST_ITEMS + 1,
                                                                                       prefix="proof")))),
        )
        for code, change in cases:
            with self.subTest(code=code, change=change):
                self.assertCode(code, parse_publication, mutated(self.data, change))

    def test_noncanonical_duplicate_and_oversized_bytes_are_refused(self):
        self.assertCode("candidate_record_not_canonical", parse_publication, self.data.rstrip(b"\n"))
        self.assertCode("candidate_record_not_canonical", parse_publication,
                        json.dumps(json.loads(self.data), sort_keys=True, indent=1).encode() + b"\n")
        self.assertCode("candidate_record_invalid", parse_publication,
                        self.data.replace(b'{"attempt"', b'{"attempt":1,"attempt"', 1))
        self.assertCode("candidate_record_too_large", parse_publication, b" " * (MAX_PUBLICATION_BYTES + 1))

    def test_constructor_refuses_list_aliases_and_bounds_every_inventory(self):
        for field in ("proof_sha256s", "install_sha256s", "triage_sha256s"):
            with self.subTest(field=field):
                self.assertCode("candidate_publication_invalid", replace, self.record,
                                **{field: list(getattr(self.record, field))})
                self.assertCode("candidate_publication_invalid", replace, self.record,
                                **{field: digests(MAX_LIST_ITEMS + 1, prefix=field)})

    def test_largest_valid_record_fits_the_exported_byte_bound(self):
        largest = PublicationAuthorization(
            candidate_id="1" * 64, attempt=MAX_LIST_ITEMS + 1, evidence_parent="2" * 64,
            source_commit="3" * 64, source_tree="4" * 64, proof_sha256s=digests(MAX_LIST_ITEMS, prefix="proof"),
            install_sha256s=digests(MAX_LIST_ITEMS, prefix="install"),
            triage_sha256s=digests(MAX_LIST_ITEMS, prefix="triage"), review_sha256="5" * 64)
        data = encode_publication(largest)
        self.assertLessEqual(len(data), MAX_PUBLICATION_BYTES)
        self.assertEqual(parse_publication(data), largest)


class AuthorizationTests(PublicationCase):
    def test_freeze_binds_latest_attempt_selected_facts_source_review_and_parent(self):
        history, selected, unrelated = self.replaced()
        readiness = evaluate_readiness(candidate=self.candidate, observed=fx.observed(), scope=fx.scope(),
                                       history=history, review=fx.review(self.candidate))
        self.assertEqual([verdict.state for verdict in readiness.attempt_verdicts],
                         [AttemptState.FAIL, AttemptState.PASS])
        record = self.authorize(history)
        planned_installs = tuple(sorted(install.sha256 for install in history.installs
                                        if install.platform in PLANNED))
        self.assertEqual(record, PublicationAuthorization(
            candidate_id=self.candidate.candidate_id, attempt=2, evidence_parent=PARENT,
            source_commit=fx.COMMIT, source_tree=fx.TREE,
            proof_sha256s=tuple(sorted(proof.proof_sha256 for proof in history.proofs_for(2))),
            install_sha256s=planned_installs, triage_sha256s=(selected.sha256,),
            review_sha256=fx.review(self.candidate).sha256))
        self.assertEqual(len(record.install_sha256s), len(PLANNED) * len(InstallChannel))
        self.assertNotIn(unrelated.sha256, record.triage_sha256s)
        self.assertEqual((record.proof_sha256s, record.install_sha256s),
                         (readiness.proof_sha256s, readiness.install_sha256s))

    def test_non_ready_history_is_never_frozen(self):
        failed = fx.cells(overrides={CODEX_WRITE: (CellStatus.FAIL, None)})
        complete = fx.installs_for(self.candidate, PLATFORMS)
        cases = {
            "failed cell": (self.single(proof_records=fx.proofs(self.candidate, attempt=1, results=failed)), {}),
            "no proofs": (self.single(proof_records=()), {}),
            "missing install": (self.single(installs=complete[1:]), {}),
            "no attempt": (fx.history(self.candidate, intents=()), {}),
            "source changed": (self.single(), {"observed": fx.observed(tree="f" * 40)}),
            "review missing": (self.single(), {"reviewed": False}),
            "review stale": (self.single(), {"review": fx.review(self.candidate, head="f" * 40)}),
            "review unverified": (self.single(), {"review": fx.review_record(self.candidate)}),
        }
        for name, (history, overrides) in cases.items():
            with self.subTest(case=name):
                self.assertCode("publication_not_ready", self.authorize, history, **overrides)

    def test_frozen_history_cannot_be_frozen_again_and_refuses_launch(self):
        before = self.single()
        frozen = fx.history(self.candidate, intents=before.intents, proof_records=before.proofs,
                            publication=self.authorize(before))
        self.assertCode("candidate_frozen", self.authorize, frozen)
        launch = decide_launch(candidate=self.candidate, history=frozen,
                               intent=fx.intent(self.candidate, attempt=2))
        self.assertFalse(launch.allowed)
        self.assertIn(FindingRef(code="candidate_frozen", subject=frozen.publication.sha256), launch.findings)

    def test_each_freeze_binding_change_is_refused(self):
        history, selected, unrelated = self.replaced()
        record = self.authorize(history)
        unplanned = next(install.sha256 for install in history.installs if install.platform not in PLANNED)
        changed = {
            "candidate_id": "9" * 64,
            "evidence_parent": "f" * 40,
            "source_commit": "f" * 40,
            "source_tree": "f" * 40,
            "proof_sha256s": tuple(sorted(record.proof_sha256s[1:] + ("0" * 64,))),
            "install_sha256s": tuple(sorted(record.install_sha256s + (unplanned,))),
            "triage_sha256s": (unrelated.sha256,),
            "review_sha256": "7" * 64,
        }
        for field, value in changed.items():
            with self.subTest(field=field):
                other = replace(record, **{field: value})
                self.assertNotEqual(other, record)
                self.assertNotEqual(other.sha256, record.sha256)
                self.assertEqual(parse_publication(encode_publication(other)), other)
        self.assertNotEqual(self.authorize(history, parent="f" * 40), record)
        for attempt in (1, 3):
            with self.subTest(attempt=attempt):
                self.assertCode("candidate_publication_invalid", replace, record, attempt=attempt)
        self.assertEqual(self.authorize(history), record)


if __name__ == "__main__":
    unittest.main()
