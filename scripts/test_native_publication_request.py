"""Publication requests and receipts over the canonical reference, store, receipt and placement owners.

Receipts are created and loaded through the real storage functions and
``DirectoryOwner`` inside a test-owned tree; only the state base is pointed at
that tree. No child, provider or remote command runs.
"""

from __future__ import annotations

from dataclasses import FrozenInstanceError, replace
import hashlib
import io
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock, patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.native_release import cli, storage
from scripts.delegate_acceptance.native_release.contract import (
    CANDIDATE_STAGES, NATIVE_STATUSES, PUBLICATION_STAGES, PUBLICATION_STATUSES, STAGES, CandidateRequest,
    Invocation, PublicationRequest, ReleaseError, ReleaseRequest, candidate_request, invocation_name,
    publication_request, receipt_kind, receipt_stages, receipt_statuses, release_request, status_value,
)
from scripts.delegate_acceptance.native_release.receipt import Receipt, lock_name
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import (
    CandidateReference, encode_reference,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import StoreLocation
from scripts.delegate_acceptance.release_verification.contract import VerificationError


STORE = StoreLocation(repository=fx.REPOSITORY, branch="development-evidence",
                      genesis_commit="1" * 40, marker_blob="2" * 40)
REFERENCE = CandidateReference(candidate_id="0a" * 32, freeze_commit="3" * 40, merged_commit="4" * 40)
INVOCATION_ID = "5" * 32


def publication(**changes: object) -> PublicationRequest:
    request = PublicationRequest(store=STORE, reference=REFERENCE, readiness_run_id=901, readiness_run_attempt=2,
                                 resume_published=False)
    return replace(request, **changes)


def native_candidate() -> CandidateRequest:
    return CandidateRequest(locator=fx.locator_for(fx.manifest()), store=STORE, attempt=1,
                            opencode_model="Vendor/OpenCode-1.0", ccr_model=None)


def native_release() -> ReleaseRequest:
    return ReleaseRequest(repo=fx.REPOSITORY, tag="v1.2.3", opencode_model="vendor/model", ccr_model=None,
                          no_submit=True)


def lock_of(identity: str) -> str:
    return "." + hashlib.sha256(identity.encode("utf-8")).hexdigest() + ".lock"


class PublicationRequestTests(unittest.TestCase):
    def test_the_document_is_the_exact_pinned_grammar_and_round_trips(self):
        for resume in (False, True):
            with self.subTest(resume=resume):
                request = publication(resume_published=resume)
                document = json.loads(json.dumps(request.document()))
                self.assertEqual(document, {
                    "store": {"repository": fx.REPOSITORY, "branch": "development-evidence",
                              "genesis_commit": "1" * 40, "marker_blob": "2" * 40},
                    "reference": json.loads(encode_reference(REFERENCE)),
                    "readiness_run_id": 901, "readiness_run_attempt": 2, "resume_published": resume})
                self.assertEqual(PublicationRequest.parse(document), request)
        request = publication()
        self.assertEqual((request.repository, request.candidate_id), (STORE.repository, REFERENCE.candidate_id))

    def test_unknown_missing_or_foreign_fields_refuse(self):
        document = publication().document()
        store, reference = dict(document["store"]), dict(document["reference"])
        cases = {
            "outer unknown": {**document, "repository": fx.REPOSITORY},
            "outer missing": {key: value for key, value in document.items() if key != "resume_published"},
            "prepared release": {**document, "release_id": 7},
            "token": {**document, "token": "x"},
            "candidate document": native_candidate().document(),
            "release document": native_release().document(),
            "resume integer": {**document, "resume_published": 1},
            "resume text": {**document, "resume_published": "true"},
            "resume null": {**document, "resume_published": None},
            "store unknown": {**document, "store": {**store, "head": "6" * 40}},
            "store missing": {**document, "store": {key: store[key] for key in store if key != "marker_blob"}},
            "store branch": {**document, "store": {**store, "branch": "bad..lock"}},
            "reference unknown": {**document, "reference": {**reference, "release_id": "7"}},
            "reference missing": {**document, "reference": {key: reference[key] for key in reference
                                                            if key != "merged_commit"}},
            "reference candidate": {**document, "reference": {**reference, "candidate_id": "A" * 64}},
            "reference freeze": {**document, "reference": {**reference, "freeze_commit": "3" * 39}},
        }
        for label, value in cases.items():
            with self.subTest(label), self.assertRaises(ReleaseError) as caught:
                PublicationRequest.parse(value)
            self.assertEqual(caught.exception.code, "publication_request_invalid")
            if label.startswith(("store branch", "reference")):
                self.assertIsInstance(caught.exception.__cause__, VerificationError)
        with self.assertRaisesRegex(ReleaseError, "candidate_request_invalid"):
            CandidateRequest.parse(document)
        with self.assertRaisesRegex(ReleaseError, "release_request_invalid"):
            ReleaseRequest.parse(document)
        with self.assertRaisesRegex(ReleaseError, "release_record_object_invalid"):
            PublicationRequest.parse([document])

    def test_run_identities_must_be_real_positive_integers(self):
        for field in ("readiness_run_id", "readiness_run_attempt"):
            for value in (0, -1, True, False, 1.0, "1", None):
                with self.subTest(field=field, value=value), \
                        self.assertRaisesRegex(ReleaseError, "release_record_integer_invalid"):
                    publication(**{field: value})
                with self.subTest(parsed=field, value=value), \
                        self.assertRaisesRegex(ReleaseError, "release_record_integer_invalid"):
                    PublicationRequest.parse({**publication().document(), field: value})

    def test_construction_requires_the_canonical_owner_types_and_an_explicit_choice(self):
        for field, value in (("store", STORE.__dict__), ("reference", REFERENCE.__dict__),
                             ("reference", encode_reference(REFERENCE)), ("store", None)):
            with self.subTest(field=field, value=type(value).__name__), \
                    self.assertRaisesRegex(ReleaseError, "publication_request_invalid"):
                publication(**{field: value})
        for value in (0, 1, "false", None):
            with self.subTest(resume=value), self.assertRaisesRegex(ReleaseError, "publication_resume_option_invalid"):
                publication(resume_published=value)

    def test_the_request_and_its_document_are_immutable(self):
        request = publication()
        with self.assertRaises(FrozenInstanceError):
            setattr(request, "readiness_run_id", 902)
        document = request.document()
        document["store"]["branch"] = "other"
        document["reference"]["candidate_id"] = "1b" * 32
        self.assertEqual(request.document(), publication().document())


class TargetNamespaceTests(unittest.TestCase):
    def test_the_publication_target_has_its_own_kind_stages_and_directory(self):
        request = publication()
        self.assertEqual((receipt_kind(request), receipt_stages(request)), (
            "local-candidate-publication", ("preflight", "prepare", "dispatch", "observe", "verify", "cleanup")))
        self.assertEqual(PUBLICATION_STAGES, receipt_stages(request))
        self.assertEqual(invocation_name(request, INVOCATION_ID), f"publication-{'0a' * 32}-{INVOCATION_ID}")
        self.assertEqual(invocation_name(publication(readiness_run_id=7, resume_published=True), INVOCATION_ID),
                         invocation_name(request, INVOCATION_ID))

    def test_release_and_candidate_namespaces_are_byte_compatible(self):
        release, candidate = native_release(), native_candidate()
        self.assertEqual((receipt_kind(release), receipt_stages(release)), ("local-native-release", STAGES))
        self.assertEqual((receipt_kind(candidate), receipt_stages(candidate)),
                         ("local-native-candidate", CANDIDATE_STAGES))
        self.assertEqual(invocation_name(release, INVOCATION_ID), f"v1.2.3-{INVOCATION_ID}")
        self.assertEqual(invocation_name(candidate, INVOCATION_ID),
                         f"candidate-{candidate.candidate_id}-{INVOCATION_ID}")
        self.assertEqual(lock_name(release), lock_of(f"{fx.REPOSITORY}\nv1.2.3"))
        self.assertEqual(lock_name(candidate), lock_of(
            f"local-native-candidate\n{fx.REPOSITORY}\n{candidate.candidate_id}\n1"))

    def test_the_publication_lock_binds_the_repository_and_exact_reference_only(self):
        request = publication()
        self.assertEqual(lock_name(request), lock_of(
            f"local-candidate-publication\n{fx.REPOSITORY}\n{'0a' * 32}\n{'3' * 40}\n{'4' * 40}"))
        for choice in ({"readiness_run_id": 902}, {"readiness_run_attempt": 1}, {"resume_published": True},
                       {"store": replace(STORE, branch="other-evidence", genesis_commit="7" * 40)}):
            with self.subTest(choice=choice):
                self.assertEqual(lock_name(publication(**choice)), lock_name(request))
        for field in ("candidate_id", "freeze_commit", "merged_commit"):
            with self.subTest(field=field):
                other = replace(REFERENCE, **{field: "8" * 64 if field == "candidate_id" else "8" * 40})
                self.assertNotEqual(lock_name(publication(reference=other)), lock_name(request))
        self.assertNotEqual(lock_name(publication(store=replace(STORE, repository="someone/else"))),
                            lock_name(request))
        self.assertNotIn(lock_name(request), {lock_name(native_release()), lock_name(native_candidate())})


class TargetAccessorTests(unittest.TestCase):
    def invocation(self, request: ReleaseRequest | CandidateRequest | PublicationRequest) -> Invocation:
        return Invocation(invocation_id=INVOCATION_ID, request=request, checkout=Path("/checkout"),
                          root=Path("/state") / invocation_name(request, INVOCATION_ID), platform_os="Linux",
                          platform_arch="amd64", directory_identity={})

    def test_each_accessor_reads_only_its_own_target(self):
        request = publication()
        self.assertIs(publication_request(self.invocation(request)), request)
        for accessor, code in ((release_request, "release_request_required"),
                               (candidate_request, "candidate_request_required")):
            with self.subTest(accessor=accessor.__name__), self.assertRaises(ReleaseError) as caught:
                accessor(self.invocation(request))
            self.assertEqual(caught.exception.code, code)
        for native in (native_release(), native_candidate()):
            with self.subTest(native=type(native).__name__), self.assertRaises(ReleaseError) as caught:
                publication_request(self.invocation(native))
            self.assertEqual(caught.exception.code, "publication_request_required")


class PublicationStorageCase(unittest.TestCase):
    """A state base inside one test-owned tree; the canonical owners do every placement."""

    def setUp(self):
        self.tree = FixtureTree.open(prefix="native-publication-test", label="native publication fixture")
        self.addCleanup(self.tree.dispose)
        self.state = self.tree.root / "state"
        self.checkout = self.tree.root / "checkout"
        self.checkout.mkdir()
        placement = patch.object(storage, "state_base", side_effect=self.state_base)
        placement.start()
        self.addCleanup(placement.stop)

    def state_base(self, checkout: Path) -> Path:
        self.assertEqual(checkout, self.checkout)
        return self.state

    def create(self, request: ReleaseRequest | CandidateRequest | PublicationRequest) -> Receipt:
        return storage.create_receipt(self.checkout, request, ["internal-test"])


class PublicationStorageTests(PublicationStorageCase):
    def test_every_target_kind_round_trips_through_create_and_load(self):
        for request, kind, stages, prefix in (
                (native_release(), "local-native-release", STAGES, "v1.2.3"),
                (native_candidate(), "local-native-candidate", CANDIDATE_STAGES,
                 f"candidate-{native_candidate().candidate_id}"),
                (publication(resume_published=True), "local-candidate-publication", PUBLICATION_STAGES,
                 f"publication-{'0a' * 32}")):
            with self.subTest(kind=kind):
                receipt = self.create(request)
                invocation = receipt.invocation
                self.assertEqual(invocation.root, self.state / f"{prefix}-{invocation.invocation_id}")
                stored = json.loads(receipt.path.read_bytes())
                self.assertEqual((stored["kind"], stored["status"], stored["metadata"]["request"]),
                                 (kind, "INCOMPLETE", request.document()))
                self.assertEqual(stored["stages"], {name: {"state": "pending", "intent": {}, "result": {}}
                                                    for name in stages})
                loaded = storage.load_receipt(receipt.path)
                self.assertEqual(loaded.invocation, invocation)
                self.assertIs(type(loaded.invocation.request), type(request))

    def test_stage_progress_keeps_the_request_and_completed_results_immutable(self):
        request = publication()
        receipt = self.create(request)
        intent = {"dispatch": {"workflow": "release.yml", "ref": "main"}}
        result = {"run": {"id": 77, "attempt": 1}}
        self.assertTrue(receipt.intend("dispatch", intent))
        self.assertEqual(storage.load_receipt(receipt.path).stage("dispatch").state, "intended")
        intent["dispatch"]["ref"] = "other"
        self.assertFalse(receipt.intend("dispatch", {"dispatch": {"workflow": "release.yml", "ref": "main"}}))
        receipt.complete("dispatch", result)
        result["run"]["id"] = 78
        receipt.complete("dispatch", {"run": {"id": 77, "attempt": 1}})
        expected = ({"dispatch": {"workflow": "release.yml", "ref": "main"}}, {"run": {"id": 77, "attempt": 1}})
        for reader in (receipt, storage.load_receipt(receipt.path)):
            with self.subTest(reloaded=reader is not receipt):
                stage = reader.stage("dispatch")
                self.assertEqual((stage.state, stage.intent, stage.result), ("complete", *expected))
                self.assertEqual(reader.invocation.request, request)
                self.assertEqual({name: reader.stage(name).state for name in PUBLICATION_STAGES if name != "dispatch"},
                                 {name: "pending" for name in PUBLICATION_STAGES if name != "dispatch"})
        self.assertEqual(json.loads(receipt.path.read_bytes())["metadata"]["request"], request.document())
        with self.assertRaisesRegex(ReleaseError, "release_intent_conflict"):
            receipt.intend("dispatch", {"dispatch": {"workflow": "release.yml", "ref": "other"}})
        with self.assertRaisesRegex(ReleaseError, "release_completion_conflict"):
            receipt.complete("dispatch", {"run": {"id": 78, "attempt": 1}})
        with self.assertRaisesRegex(ReleaseError, "release_completion_conflict"):
            receipt.complete("verify", {"verified": True})
        with self.assertRaises(KeyError):
            receipt.stage("native")

    def test_mixed_unknown_or_mismatched_publication_receipts_refuse_on_load(self):
        receipt = self.create(publication())
        original = json.loads(receipt.path.read_bytes())
        metadata, request = original["metadata"], original["metadata"]["request"]
        other = {**request, "reference": {**request["reference"], "candidate_id": "9" * 64}}
        cases = {
            "unknown kind": ("release_receipt_invalid", {**original, "kind": "local-native-publication"}),
            "as release": ("release_request_invalid", {**original, "kind": "local-native-release"}),
            "as candidate": ("candidate_request_invalid", {**original, "kind": "local-native-candidate"}),
            "native stages": ("release_receipt_stages_invalid", {**original, "stages": {
                name: {"state": "pending", "intent": {}, "result": {}} for name in CANDIDATE_STAGES}}),
            "missing stage": ("release_receipt_stages_invalid", {**original, "stages": {
                name: {"state": "pending", "intent": {}, "result": {}} for name in PUBLICATION_STAGES[:-1]}}),
            "other reference": ("release_receipt_location_mismatch", {**original, "metadata": {
                **metadata, "request": other}}),
            "extra request field": ("publication_request_invalid", {**original, "metadata": {
                **metadata, "request": {**request, "release_id": 7}}}),
            "store fault": ("publication_request_invalid", {**original, "metadata": {
                **metadata, "request": {**request, "store": {**request["store"], "genesis_commit": "x"}}}}),
        }
        for label, (code, value) in cases.items():
            with self.subTest(label):
                receipt.path.write_text(json.dumps(value), encoding="utf-8")
                with self.assertRaises(ReleaseError) as caught:
                    storage.load_receipt(receipt.path)
                self.assertEqual(caught.exception.code, code)

    def test_native_receipts_relabelled_as_publication_refuse(self):
        for request in (native_release(), native_candidate()):
            receipt = self.create(request)
            original = json.loads(receipt.path.read_bytes())
            with self.subTest(kind=original["kind"]):
                receipt.path.write_text(json.dumps({**original, "kind": "local-candidate-publication"}),
                                        encoding="utf-8")
                with self.assertRaises(ReleaseError) as caught:
                    storage.load_receipt(receipt.path)
                self.assertEqual(caught.exception.code, "publication_request_invalid")


class ReceiptStatusTests(PublicationStorageCase):
    """One status vocabulary per target, shared by ``Receipt.load`` and ``Receipt.report``."""

    def targets(self) -> tuple[tuple[ReleaseRequest | CandidateRequest | PublicationRequest, tuple[str, ...]], ...]:
        return ((native_release(), NATIVE_STATUSES), (native_candidate(), NATIVE_STATUSES),
                (publication(), PUBLICATION_STATUSES))

    def test_the_vocabularies_are_exact_and_only_a_publication_can_be_published(self):
        self.assertEqual(NATIVE_STATUSES, ("INCOMPLETE", "BLOCKED", "FAIL", "LOCAL_ONLY", "PROMOTABLE"))
        self.assertEqual(PUBLICATION_STATUSES, ("INCOMPLETE", "BLOCKED", "PUBLISHED"))
        for request, statuses in self.targets():
            with self.subTest(kind=receipt_kind(request)):
                self.assertEqual(receipt_statuses(request), statuses)
                self.assertEqual([status_value(request, status) for status in statuses], list(statuses))

    def test_every_own_status_round_trips_through_report_and_load(self):
        for request, statuses in self.targets():
            receipt = self.create(request)
            for status in statuses:
                with self.subTest(kind=receipt_kind(request), status=status):
                    receipt.report(status, f"reason_{status.lower()}")
                    loaded = storage.load_receipt(receipt.path)
                    self.assertEqual((loaded.status, loaded.reason), (status, f"reason_{status.lower()}"))
                    stored = json.loads(receipt.path.read_bytes())
                    self.assertEqual((stored["status"], stored["reason"]), (status, f"reason_{status.lower()}"))

    def test_a_refused_report_changes_neither_the_receipt_object_nor_its_file(self):
        for request, statuses in self.targets():
            foreign = tuple(status for status in (*NATIVE_STATUSES, *PUBLICATION_STATUSES) if status not in statuses)
            receipt = self.create(request)
            receipt.report("BLOCKED", "before_refusal")
            original = receipt.path.read_bytes()
            outcomes = [(status, "reason") for status in (*foreign, "published", "", "INCOMPLETE ", None, 1)]
            outcomes += [("INCOMPLETE", ""), ("INCOMPLETE", None), ("INCOMPLETE", "nul\x00")]
            for status, reason in outcomes:
                with self.subTest(kind=receipt_kind(request), status=status, reason=reason), \
                        self.assertRaises(ReleaseError) as caught:
                    receipt.report(status, reason)
                self.assertEqual(caught.exception.code, "release_record_text_invalid")
                self.assertEqual((receipt.status, receipt.reason), ("BLOCKED", "before_refusal"))
                self.assertEqual(receipt.path.read_bytes(), original)

    def test_load_refuses_another_targets_status_with_the_legacy_text_identity(self):
        for request, statuses in self.targets():
            receipt = self.create(request)
            original = json.loads(receipt.path.read_bytes())
            for status in (*NATIVE_STATUSES, *PUBLICATION_STATUSES, "published", None):
                if status in statuses:
                    continue
                with self.subTest(kind=receipt_kind(request), status=status):
                    receipt.path.write_text(json.dumps({**original, "status": status}), encoding="utf-8")
                    with self.assertRaises(ReleaseError) as caught:
                        storage.load_receipt(receipt.path)
                    self.assertEqual(caught.exception.code, "release_record_text_invalid")


class LegacyResumeAdmissionTests(PublicationStorageCase):
    """The native acceptance CLI refuses a publication receipt before any placement, lock or effect."""

    def test_public_resume_refuses_a_publication_receipt_before_any_lock_or_effect(self):
        receipt = self.create(publication())
        original = receipt.path.read_bytes()
        listing = sorted(self.tree.root.rglob("*"))
        sentinels = {name: Mock(side_effect=AssertionError(f"native resume admitted a publication: {name}"))
                     for name in ("state_base", "lock", "unlock", "create", "execute", "resume")}
        loads = Mock(side_effect=storage.load_receipt)
        output = io.StringIO()
        with patch.object(cli, "state_base", sentinels["state_base"]), \
                patch.object(cli, "acquire_exclusive_file_lock", sentinels["lock"]), \
                patch.object(cli, "release_exclusive_file_lock", sentinels["unlock"]), \
                patch.object(cli, "create_receipt", sentinels["create"]), \
                patch.object(cli, "execute", sentinels["execute"]), patch.object(cli, "resume", sentinels["resume"]), \
                patch.object(cli, "load_receipt", loads), \
                patch.object(Receipt, "report", Mock(side_effect=AssertionError("receipt reported"))) as report, \
                patch.object(operations.subprocess, "Popen", Mock(side_effect=AssertionError("child"))) as launches, \
                patch("sys.stdout", output), patch("sys.stderr", io.StringIO()) as errors:
            code = cli.main(["resume", "--receipt", str(receipt.path)])
        self.assertEqual((code, output.getvalue()),
                         (2, json.dumps({"status": "BLOCKED", "reason": "native_acceptance_request_required"},
                                        sort_keys=True) + "\n"))
        self.assertEqual(errors.getvalue(), "")
        loads.assert_called_once_with(receipt.path)
        self.assertEqual({name: mock.call_count for name, mock in sentinels.items()},
                         dict.fromkeys(sentinels, 0))
        self.assertEqual((report.call_count, launches.call_count), (0, 0))
        self.assertEqual(hashlib.sha256(receipt.path.read_bytes()).hexdigest(), hashlib.sha256(original).hexdigest())
        self.assertEqual(sorted(self.tree.root.rglob("*")), listing)


if __name__ == "__main__":
    unittest.main()
