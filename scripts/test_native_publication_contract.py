"""Publication dispatch wire, hosted report and visibility rules over the canonical request and promotion owners.

Every value here is pure: no storage, network, process or clock is used.
"""

from __future__ import annotations

import copy
from dataclasses import FrozenInstanceError, replace
import hashlib
import json
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.native_release.contract import PRODUCTION_STORE, PublicationRequest, ReleaseError
from scripts.delegate_acceptance.native_release.publication_contract import (
    INPUT_NAMES, PUBLICATION_RUN_PATHS, REPORT_FIELD, PublicationDispatch, PublicationFailure, PublishedDelivery,
    ReportBinding, parse_dispatch_inputs, publication_report, publication_title, request_digest,
    require_failed, require_public_repository, require_published,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import payload_paths
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import (
    CandidateReference, encode_promotion,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.published_contract import PackageIdentity, registry_tarball_url
from scripts.test_candidate_publication_contract import BUILD, MERGED, TAG, VERSION, promotion_for, release


INVOCATION = "5" * 32
HARNESS = "9" * 40
REFERENCE = CandidateReference(candidate_id="0a" * 32, freeze_commit="3" * 40, merged_commit="4" * 40)


def request(**changes: object) -> PublicationRequest:
    return replace(PublicationRequest(store=PRODUCTION_STORE, reference=REFERENCE, readiness_run_id=901,
                                      readiness_run_attempt=2, resume_published=False), **changes)


def dispatch(**changes: object) -> PublicationDispatch:
    values: dict[str, object] = {"request": request(), "invocation_id": INVOCATION, "harness": HARNESS, **changes}
    return PublicationDispatch(**values)


def digest_of(value: PublicationRequest) -> str:
    """An independent canonical encoding: sorted keys, compact separators and one trailing newline."""
    encoded = json.dumps(value.document(), sort_keys=True, separators=(",", ":")) + "\n"
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


class DispatchWireTests(unittest.TestCase):
    def test_the_inputs_are_exactly_nine_canonical_strings_and_round_trip(self):
        for resume, text in ((False, "false"), (True, "true")):
            with self.subTest(resume=resume):
                value = dispatch(request=request(resume_published=resume))
                inputs = value.inputs()
                self.assertEqual(inputs, {
                    "candidate_id": "0a" * 32, "freeze_commit": "3" * 40, "merged_commit": "4" * 40,
                    "readiness_run_id": "901", "readiness_run_attempt": "2", "resume_published": text,
                    "invocation_id": INVOCATION, "request_sha256": digest_of(value.request), "harness": HARNESS})
                self.assertEqual(tuple(inputs), INPUT_NAMES)
                self.assertEqual(parse_dispatch_inputs(json.loads(json.dumps(inputs))), value)
        self.assertNotIn("store", INPUT_NAMES)
        self.assertEqual(PUBLICATION_RUN_PATHS, (".github/workflows/release.yml", ".github/workflows/release.yml@main"))

    def test_the_digest_covers_the_whole_canonical_request(self):
        base = request()
        self.assertEqual(request_digest(base), digest_of(base))
        others = (request(resume_published=True), request(readiness_run_id=902), request(readiness_run_attempt=1),
                  *(request(reference=replace(REFERENCE, **{field: "8" * (64 if field == "candidate_id" else 40)}))
                    for field in ("candidate_id", "freeze_commit", "merged_commit")))
        self.assertEqual(len({request_digest(base), *(request_digest(other) for other in others)}), 1 + len(others))
        with self.assertRaisesRegex(ReleaseError, "publication_request_invalid"):
            request_digest(base.document())

    def test_the_title_is_stable_for_the_same_dispatch_and_names_only_invocation_and_digest(self):
        value = dispatch()
        self.assertEqual(value.title, f"Candidate publication {INVOCATION} {digest_of(value.request)}")
        self.assertEqual(dispatch().title, value.title)
        self.assertEqual(dispatch(harness="7" * 40).title, value.title)
        self.assertNotEqual(dispatch(invocation_id="6" * 32).title, value.title)
        self.assertNotEqual(dispatch(request=request(resume_published=True)).title, value.title)
        for invocation, digest in (("5" * 31, "a" * 64), ("A" * 32, "a" * 64), ("5" * 32, "A" * 64), (None, "a" * 64)):
            with self.subTest(invocation=invocation, digest=digest), \
                    self.assertRaisesRegex(ReleaseError, "publication_title_identity_invalid"):
                publication_title(invocation, digest)

    def test_only_a_production_store_request_with_exact_identities_is_dispatched(self):
        other = replace(PRODUCTION_STORE, branch="development-proof/other")
        cases = (({"request": request(store=other)}, "publication_store_not_production"),
                 ({"request": request().document()}, "publication_request_invalid"),
                 ({"invocation_id": "5" * 31}, "publication_dispatch_invalid"),
                 ({"invocation_id": "G" * 32}, "publication_dispatch_invalid"),
                 ({"harness": "9" * 64}, "publication_dispatch_invalid"),
                 ({"harness": "9" * 39}, "publication_dispatch_invalid"),
                 ({"request": request(readiness_run_id=10 ** 18)}, "publication_dispatch_invalid"))
        for change, code in cases:
            with self.subTest(change=change), self.assertRaises(ReleaseError) as caught:
                dispatch(**change)
            self.assertEqual(caught.exception.code, code)

    def test_unknown_missing_or_non_string_inputs_refuse_as_a_shape(self):
        inputs = dispatch().inputs()
        cases = {"store": {**inputs, "store": "x"}, "token": {**inputs, "token": "x"},
                 "missing": {key: value for key, value in inputs.items() if key != "harness"},
                 "integer": {**inputs, "readiness_run_id": 901}, "boolean": {**inputs, "resume_published": False},
                 "null": {**inputs, "harness": None}}
        for label, value in cases.items():
            with self.subTest(label), self.assertRaises(ReleaseError) as caught:
                parse_dispatch_inputs(value)
            self.assertEqual(caught.exception.code, "publication_dispatch_inputs_invalid")
        with self.assertRaisesRegex(ReleaseError, "release_record_object_invalid"):
            parse_dispatch_inputs([inputs])

    def test_a_non_canonical_value_names_only_its_input(self):
        cases = {
            "readiness_run_id": ("0", "0901", "+901", "901 ", " 901", "9.01e2", "-1", "1" * 19, "٩٠١"),
            "readiness_run_attempt": ("0", "02", "two"),
            "resume_published": ("True", "TRUE", "1", "0", "yes", ""),
            "candidate_id": ("0A" * 32, "0a" * 31), "freeze_commit": ("3" * 39, "G" * 40),
            "merged_commit": ("4" * 65,), "invocation_id": ("5" * 31, "5" * 33, "A" * 32),
            "request_sha256": ("A" * 64, "a" * 63), "harness": ("9" * 39, "9" * 41, "9" * 64),
        }
        inputs = dispatch().inputs()
        for field, values in cases.items():
            for value in values:
                with self.subTest(field=field, value=value), self.assertRaises(ReleaseError) as caught:
                    parse_dispatch_inputs({**inputs, field: value})
                self.assertEqual((caught.exception.code, caught.exception.mismatched),
                                 ("publication_dispatch_input_invalid", (field,)))
        with self.assertRaises(ReleaseError) as caught:
            parse_dispatch_inputs({**inputs, "harness": "x", "candidate_id": "x"})
        self.assertEqual(caught.exception.mismatched, ("candidate_id", "harness"))

    def test_a_digest_of_any_other_request_refuses_after_reconstruction(self):
        inputs = dispatch().inputs()
        for digest in ("0" * 64, request_digest(request(resume_published=True)),
                       request_digest(request(store=replace(PRODUCTION_STORE, branch="other")))):
            with self.subTest(digest=digest), self.assertRaises(ReleaseError) as caught:
                parse_dispatch_inputs({**inputs, "request_sha256": digest})
            self.assertEqual(caught.exception.code, "publication_request_digest_mismatch")
        flipped = {**inputs, "resume_published": "true"}
        with self.assertRaisesRegex(ReleaseError, "publication_request_digest_mismatch"):
            parse_dispatch_inputs(flipped)

    def test_dispatch_values_are_immutable(self):
        value = dispatch()
        with self.assertRaises(FrozenInstanceError):
            setattr(value, "harness", "7" * 40)
        inputs = value.inputs()
        inputs["harness"] = "7" * 40
        self.assertEqual(value.inputs()["harness"], HARNESS)


class VisibilityTests(unittest.TestCase):
    def test_only_an_exactly_public_observation_passes(self):
        self.assertIsNone(require_public_repository(False))
        with self.assertRaisesRegex(ReleaseError, "publication_repository_private"):
            require_public_repository(True)
        for value in (None, 0, 1, "false", "public", []):
            with self.subTest(value=value), self.assertRaisesRegex(ReleaseError, "repository_visibility_unknown"):
                require_public_repository(value)


PROMOTION = promotion_for()
BINDING = ReportBinding.of(dispatch(), run_id=7001, run_attempt=1)
NPM = PackageIdentity(name="delegation-layer", version=VERSION, integrity="sha512-" + "A" * 86 + "==",
                      shasum="1" * 40, tarball=registry_tarball_url(VERSION), git_head=BUILD)
SUBJECTS = [{"name": subject.name, "sha256": subject.sha256} for subject in PROMOTION.subjects]


def delivery(**changes: object) -> PublishedDelivery:
    published = release(*reversed(payload_paths(VERSION)), draft=False)
    values: dict[str, object] = {"release_id": 55, "release": published, "npm": NPM, "npm_latest": VERSION,
                                 "tap_blob_sha": "2" * 40, **changes}
    return PublishedDelivery(**values)


def gate(report: object, **changes: object) -> dict[str, object]:
    """The finalized removal shape ``finish_gate`` writes, with the report nested under its field."""
    return {"gate": "candidate-publication", "verdict": "pass", "stage": "complete", "failure": None,
            "completion": {"state": "complete"}, "cleanup": {"status": "complete"}, REPORT_FIELD: report, **changes}


def success() -> dict[str, object]:
    return publication_report(BINDING, first_mutation_started=True, promotion=PROMOTION, delivery=delivery())


def failed(report: dict[str, object], *, code: str = "candidate_npm_conflict",
           stage: str = "deliver-candidate-npm") -> dict[str, object]:
    return gate(report, verdict="fail", stage=stage, failure={"code": code, "stage": stage},
                completion={"state": "prepared"}, cleanup={"status": "retained-for-investigation"})


class ReportTests(unittest.TestCase):
    def assertMismatch(self, report: dict[str, object], code: str, mismatched: tuple[str, ...] = ()) -> None:
        with self.assertRaises(ReleaseError) as caught:
            require_published(json.loads(json.dumps(gate(report))), binding=BINDING, promotion=PROMOTION)
        self.assertEqual((caught.exception.code, caught.exception.mismatched), (code, mismatched))

    def test_a_finalized_success_report_binds_every_fact_and_round_trips(self):
        report = success()
        self.assertEqual(report, {
            "schema_version": 1, "kind": "delegation-layer-publication-report",
            "binding": {"invocation_id": INVOCATION, "request_sha256": digest_of(request()), "run_id": 7001,
                        "run_attempt": 1, "harness": HARNESS},
            "first_mutation_started": True,
            "delivery": {
                "promotion": {"sha256": sha(encode_promotion(PROMOTION)), "subjects": SUBJECTS},
                "github": {"release_id": 55, "tag": TAG, "tag_commit": MERGED, "assets": SUBJECTS},
                "npm": {"name": "delegation-layer", "version": VERSION, "integrity": NPM.integrity,
                        "shasum": NPM.shasum, "tarball": NPM.tarball, "git_head": BUILD, "latest": VERSION},
                "tap": {"blob_sha": "2" * 40}}})
        self.assertEqual(len(SUBJECTS), 8)
        returned = require_published(json.loads(json.dumps(gate(report))), binding=BINDING, promotion=PROMOTION)
        self.assertEqual(returned, report)
        returned["delivery"]["tap"]["blob_sha"] = "3" * 40
        self.assertEqual(success(), report)
        without_head = publication_report(BINDING, first_mutation_started=False, promotion=PROMOTION,
                                          delivery=delivery(npm=replace(NPM, git_head=None)))
        self.assertIsNone(require_published(gate(without_head), binding=BINDING,
                                            promotion=PROMOTION)["delivery"]["npm"]["git_head"])

    def test_every_binding_field_must_be_the_expected_run(self):
        for field, value in (("invocation_id", "6" * 32), ("request_sha256", "b" * 64), ("run_id", 7002),
                             ("run_attempt", 2), ("harness", "7" * 40)):
            with self.subTest(field=field):
                report = success()
                report["binding"][field] = value
                self.assertMismatch(report, "publication_report_binding_mismatch", (field,))
        for change, code in (({"run_id": True}, "release_record_integer_invalid"),
                             ({"harness": "x"}, "publication_report_binding_invalid"),
                             ({"extra": 1}, "publication_report_binding_invalid")):
            with self.subTest(change=change):
                report = success()
                report["binding"].update(change)
                self.assertMismatch(report, code)

    def test_every_delivered_fact_must_be_the_expected_promotion(self):
        reordered = [*SUBJECTS[1:], SUBJECTS[0]]
        changed = [{**SUBJECTS[0], "sha256": "0" * 64}, *SUBJECTS[1:]]
        cases = (
            (("promotion", "sha256"), "0" * 64, "promotion_sha256"),
            (("promotion", "subjects"), SUBJECTS[1:], "subjects"),
            (("github", "release_id"), 0, "release_id"), (("github", "release_id"), True, "release_id"),
            (("github", "release_id"), "55", "release_id"), (("github", "tag"), "v0.1.36", "tag"),
            (("github", "tag_commit"), BUILD, "tag_commit"), (("github", "assets"), SUBJECTS[:-1], "assets"),
            (("github", "assets"), reordered, "assets"), (("github", "assets"), changed, "assets"),
            (("github", "assets"), [*SUBJECTS, {"name": "extra", "sha256": "0" * 64}], "assets"),
            (("npm", "name"), "other", "npm_identity"), (("npm", "version"), "0.1.36", "npm_identity"),
            (("npm", "integrity"), "sha512-short", "npm_identity"), (("npm", "shasum"), "X" * 40, "npm_identity"),
            (("npm", "tarball"), "https://example.invalid/x.tgz", "npm_identity"),
            (("npm", "git_head"), MERGED, "npm_identity"), (("npm", "latest"), "0.1.36", "npm_latest"),
            (("npm", "latest"), None, "npm_latest"), (("tap", "blob_sha"), "2" * 39, "tap_blob_sha"),
            (("tap", "blob_sha"), "G" * 40, "tap_blob_sha"),
        )
        for (section, field), value, name in cases:
            with self.subTest(section=section, field=field, value=value):
                report = success()
                report["delivery"][section][field] = value
                self.assertMismatch(report, "publication_report_delivery_mismatch", (name,))
        for section in ("promotion", "github", "npm", "tap"):
            with self.subTest(unknown=section):
                report = success()
                report["delivery"][section]["extra"] = 1
                self.assertMismatch(report, "publication_report_delivery_invalid")
        report = success()
        del report["delivery"]["tap"]
        self.assertMismatch(report, "publication_report_delivery_invalid")

    def test_the_envelope_is_strict_and_a_status_string_is_no_authority(self):
        cases = (("schema_version", True), ("schema_version", "1"), ("schema_version", 2), ("kind", "other"),
                 ("first_mutation_started", 1), ("first_mutation_started", None), ("extra", "PUBLISHED"))
        for field, value in cases:
            with self.subTest(field=field, value=value):
                self.assertMismatch({**success(), field: value}, "publication_report_invalid")
        self.assertMismatch({key: value for key, value in success().items() if key != "delivery"},
                            "publication_report_invalid")
        self.assertMismatch({**success(), "delivery": None}, "publication_report_delivery_missing")
        unreported = {key: value for key, value in gate(success()).items() if key != REPORT_FIELD}
        with self.assertRaisesRegex(ReleaseError, "release_record_object_invalid"):
            require_published({**unreported, "status": "PUBLISHED"}, binding=BINDING, promotion=PROMOTION)

    def test_only_a_finalized_successful_gate_is_published(self):
        report = success()
        changes = ({"verdict": "blocked"}, {"verdict": "fail"}, {"verdict": "in_progress"},
                   {"stage": "cleanup-prepared"}, {"failure": {"code": "x", "stage": "y"}},
                   {"completion": {"state": "prepared"}}, {"completion": None})
        for change in changes:
            with self.subTest(change=change), self.assertRaisesRegex(ReleaseError, "publication_report_not_finalized"):
                require_published(gate(report, **change), binding=BINDING, promotion=PROMOTION)
        missing = {key: value for key, value in gate(report).items() if key != "failure"}
        with self.assertRaisesRegex(ReleaseError, "publication_report_not_finalized"):
            require_published(missing, binding=BINDING, promotion=PROMOTION)
        with self.assertRaisesRegex(ReleaseError, "publication_report_promotion_invalid"):
            require_published(gate(report), binding=BINDING, promotion=None)

    def test_only_a_completed_cleanup_with_nothing_retained_is_published(self):
        report = success()
        cleanups = (None, [], "complete", {}, {"status": None}, {"status": "incomplete"}, {"status": "prepared"},
                    {"status": "retained-for-investigation"}, {"status": "retained-for-native-acceptance"},
                    {"status": "complete", "retained_paths": ["/runner/work/payload"]},
                    {"status": "complete", "retained_paths": None}, {"status": "complete", "retained_paths": 0},
                    {"status": "complete", "retained_paths": {}})
        for cleanup in cleanups:
            with self.subTest(cleanup=cleanup):
                with self.assertRaises(ReleaseError) as caught:
                    require_published(gate(report, cleanup=cleanup), binding=BINDING, promotion=PROMOTION)
                self.assertEqual(caught.exception.code, "publication_report_not_finalized")
        omitted = {key: value for key, value in gate(report).items() if key != "cleanup"}
        with self.assertRaisesRegex(ReleaseError, "publication_report_not_finalized"):
            require_published(omitted, binding=BINDING, promotion=PROMOTION)

    def test_a_completed_recovery_keeps_its_historical_cleanup_errors_as_evidence(self):
        recovered = {"status": "complete", "errors": ["release_cleanup_failed"], "removed_paths": ["/runner/work"],
                     "retained_paths": [], "transaction": {"journal_sha256": "c" * 64}}
        report = success()
        self.assertEqual(require_published(gate(report, cleanup=recovered), binding=BINDING, promotion=PROMOTION),
                         report)

    def test_the_writer_records_delivery_only_for_a_complete_published_release(self):
        with self.assertRaisesRegex(ReleaseError, "publication_report_release_incomplete"):
            publication_report(BINDING, first_mutation_started=True, promotion=PROMOTION,
                               delivery=delivery(release=release(*payload_paths(VERSION), draft=True)))
        for published, code in ((release(*payload_paths(VERSION)[1:], draft=False),
                                 "candidate_release_published_incomplete"),
                                (release(*payload_paths(VERSION), draft=False, body="other"),
                                 "candidate_release_body_mismatch")):
            with self.subTest(code=code), self.assertRaises(VerificationError) as caught:
                publication_report(BINDING, first_mutation_started=True, promotion=PROMOTION,
                                   delivery=delivery(release=published))
            self.assertEqual(caught.exception.code, code)
        with self.assertRaises(ReleaseError) as caught:
            publication_report(BINDING, first_mutation_started=True, promotion=PROMOTION,
                               delivery=delivery(npm_latest="0.1.36"))
        self.assertEqual(caught.exception.mismatched, ("npm_latest",))
        for kwargs in ({"promotion": PROMOTION}, {"delivery": delivery()}, {"first_mutation_started": 1}):
            with self.subTest(kwargs=kwargs), self.assertRaisesRegex(ReleaseError, "publication_report_invalid"):
                publication_report(BINDING, **{"first_mutation_started": True, **kwargs})
        for change in ({"release_id": 0}, {"npm": NPM.__dict__}, {"tap_blob_sha": None}):
            with self.subTest(change=change), self.assertRaises(ReleaseError):
                delivery(**change)


class FailedReportTests(unittest.TestCase):
    def test_a_failed_run_keeps_mutation_start_and_the_original_failure(self):
        for started in (False, True):
            with self.subTest(started=started):
                pending = publication_report(BINDING, first_mutation_started=started)
                self.assertIsNone(pending["delivery"])
                receipt = json.loads(json.dumps(failed(pending)))
                self.assertEqual(require_failed(receipt, binding=BINDING, promotion=PROMOTION), PublicationFailure(
                    first_mutation_started=started, delivery_confirmed=False, code="candidate_npm_conflict",
                    stage="deliver-candidate-npm"))
                with self.assertRaisesRegex(ReleaseError, "publication_report_not_finalized"):
                    require_published(receipt, binding=BINDING, promotion=PROMOTION)

    def test_a_confirmed_delivery_with_failed_finalization_is_never_a_success(self):
        receipt = failed(success(), code="release_cleanup_failed", stage="cleanup")
        result = require_failed(receipt, binding=BINDING, promotion=PROMOTION)
        self.assertEqual((result.first_mutation_started, result.delivery_confirmed, result.code),
                         (True, True, "release_cleanup_failed"))
        conflicting = copy.deepcopy(receipt)
        conflicting[REPORT_FIELD]["delivery"]["tap"]["blob_sha"] = "x"
        with self.assertRaisesRegex(ReleaseError, "publication_report_delivery_mismatch"):
            require_failed(conflicting, binding=BINDING, promotion=PROMOTION)

    def test_only_a_sanitized_finalized_failure_of_the_expected_run_is_read(self):
        pending = publication_report(BINDING, first_mutation_started=True)
        for receipt in (gate(pending), failed(pending, code="Token ghp_secret"), failed(pending, code=""),
                        failed(pending, code="x" * 129), {**failed(pending), "stage": "other"},
                        {**failed(pending), "failure": {"code": "x", "stage": "deliver-candidate-npm", "extra": 1}}):
            with self.subTest(receipt=receipt["failure"]), \
                    self.assertRaisesRegex(ReleaseError, "publication_report_not_failed"):
                require_failed(receipt, binding=BINDING, promotion=PROMOTION)
        other = ReportBinding.of(dispatch(), run_id=7001, run_attempt=2)
        with self.assertRaises(ReleaseError) as caught:
            require_failed(failed(pending), binding=other, promotion=PROMOTION)
        self.assertEqual((caught.exception.code, caught.exception.mismatched),
                         ("publication_report_binding_mismatch", ("run_attempt",)))


if __name__ == "__main__":
    unittest.main()
