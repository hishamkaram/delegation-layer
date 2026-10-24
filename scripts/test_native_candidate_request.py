"""Candidate native requests, receipts and placement over the canonical owners, and their public resume.

Receipts are created and loaded through the real storage functions and
``DirectoryOwner`` inside a test-owned tree; only the state base is pointed at
that tree. No child, provider or remote command runs.
"""

from __future__ import annotations

from dataclasses import replace
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
    CANDIDATE_STAGES, STAGES, CandidateRequest, ReleaseError, ReleaseRequest, invocation_name, receipt_kind,
    receipt_stages,
)
from scripts.delegate_acceptance.native_release.receipt import Receipt, lock_name
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.release_verification.assets import platform_identity
from scripts.delegate_acceptance.release_verification.candidate_manifest import encode_locator
from scripts.delegate_acceptance.release_verification.candidate_store_contract import StoreLocation
from scripts.delegate_acceptance.release_verification.contract import VerificationError


LOCATOR = fx.locator_for(fx.manifest())
STORE = StoreLocation(repository=fx.REPOSITORY, branch="development-evidence",
                      genesis_commit="1" * 40, marker_blob="2" * 40)
MODEL_OPTIONS = ((None, None), ("ccr/exact", None), (None, "vendor/agy:1.2+b"), ("ccr/exact", "vendor/agy:1.2+b"))


def candidate(**changes: object) -> CandidateRequest:
    request = CandidateRequest(locator=LOCATOR, store=STORE, attempt=1, opencode_model="Vendor/OpenCode-1.0",
                               ccr_model=None)
    return replace(request, **changes)


class CandidateRequestTests(unittest.TestCase):
    def test_the_document_round_trips_the_complete_pins_and_every_model_option(self):
        for ccr, agy in MODEL_OPTIONS:
            with self.subTest(ccr=ccr, agy=agy):
                request = candidate(attempt=3, ccr_model=ccr, agy_model=agy)
                document = json.loads(json.dumps(request.document()))
                self.assertEqual(document, {
                    "locator": json.loads(encode_locator(LOCATOR)),
                    "store": {"repository": fx.REPOSITORY, "branch": "development-evidence",
                              "genesis_commit": "1" * 40, "marker_blob": "2" * 40},
                    "attempt": 3, "opencode_model": "Vendor/OpenCode-1.0", "ccr_model": ccr, "agy_model": agy})
                self.assertEqual(CandidateRequest.parse(document), request)
                self.assertEqual((request.repository, request.candidate_id),
                                 (LOCATOR.repository, LOCATOR.manifest_sha256))

    def test_unknown_missing_or_mixed_fields_refuse(self):
        document = candidate().document()
        locator, store = dict(document["locator"]), dict(document["store"])
        cases = {
            "outer unknown": {**document, "tag": "v1.2.3"},
            "outer missing": {key: value for key, value in document.items() if key != "agy_model"},
            "release document": ReleaseRequest(repo=fx.REPOSITORY, tag="v1.2.3", opencode_model="vendor/model",
                                               ccr_model=None, no_submit=True).document(),
            "non-text model": {**document, "ccr_model": 7},
            "store unknown": {**document, "store": {**store, "head": "3" * 40}},
            "store missing": {**document, "store": {key: store[key] for key in store if key != "marker_blob"}},
            "locator unknown": {**document, "locator": {**locator, "branch": "main"}},
            "locator kind": {**document, "locator": {**locator, "kind": "delegation-layer-candidate-manifest"}},
            "store branch": {**document, "store": {**store, "branch": "bad..lock"}},
        }
        for label, value in cases.items():
            with self.subTest(label), self.assertRaises(ReleaseError) as caught:
                CandidateRequest.parse(value)
            self.assertEqual(caught.exception.code, "candidate_request_invalid")
            if label.startswith(("locator", "store branch")):
                self.assertIsInstance(caught.exception.__cause__, VerificationError)
        with self.assertRaisesRegex(ReleaseError, "release_request_invalid"):
            ReleaseRequest.parse(document)

    def test_invalid_attempts_foreign_stores_and_models_refuse(self):
        for attempt in (0, -1, True, 1.0, "1"):
            with self.subTest(attempt=attempt), self.assertRaisesRegex(ReleaseError, "release_record_integer_invalid"):
                candidate(attempt=attempt)
            with self.subTest(parsed=attempt), self.assertRaisesRegex(ReleaseError, "release_record_integer_invalid"):
                CandidateRequest.parse({**candidate().document(), "attempt": attempt})
        foreign = replace(STORE, repository="someone/else")
        with self.assertRaisesRegex(ReleaseError, "candidate_store_repository_mismatch"):
            candidate(store=foreign)
        with self.assertRaisesRegex(ReleaseError, "candidate_store_repository_mismatch"):
            CandidateRequest.parse({**candidate().document(), "store": {**candidate().document()["store"],
                                                                        "repository": "someone/else"}})
        for field, value in (("opencode_model", None), ("opencode_model", "bad model"), ("ccr_model", "-x"),
                             ("agy_model", "")):
            with self.subTest(field=field, value=value), self.assertRaisesRegex(ReleaseError,
                                                                                "release_record_text_invalid"):
                candidate(**{field: value})
        with self.assertRaisesRegex(ReleaseError, "release_record_text_invalid"):
            CandidateRequest.parse({**candidate().document(), "opencode_model": None})
        for field, value in (("locator", fx.locator_for(fx.manifest()).__dict__), ("store", STORE.__dict__)):
            with self.subTest(field=field), self.assertRaisesRegex(ReleaseError, "candidate_request_invalid"):
                candidate(**{field: value})

    def test_the_candidate_target_has_its_own_kind_stages_name_and_lock_domain(self):
        request = candidate(attempt=2)
        self.assertEqual((receipt_kind(request), receipt_stages(request)), ("local-native-candidate", (
            "preflight", "download", "archive", "claim", "native", "project", "record", "cleanup")))
        self.assertEqual(invocation_name(request, "1" * 32), f"candidate-{LOCATOR.manifest_sha256}-{'1' * 32}")
        identity = f"local-native-candidate\n{fx.REPOSITORY}\n{LOCATOR.manifest_sha256}\n2".encode()
        self.assertEqual(lock_name(request), "." + hashlib.sha256(identity).hexdigest() + ".lock")
        self.assertNotEqual(lock_name(request), lock_name(candidate(attempt=3)))
        release = ReleaseRequest(repo=fx.REPOSITORY, tag="v1.2.3", opencode_model="vendor/model", ccr_model=None,
                                 no_submit=True)
        self.assertEqual((receipt_kind(release), receipt_stages(release)), ("local-native-release", STAGES))


class StorageCase(unittest.TestCase):
    """A state base inside one test-owned tree; the canonical owners do every placement."""

    def setUp(self):
        self.tree = FixtureTree.open(prefix="native-candidate-test", label="native candidate fixture")
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

    def create(self, request: CandidateRequest | ReleaseRequest) -> Receipt:
        return storage.create_receipt(self.checkout, request, ["internal-test"])


class StorageTests(StorageCase):
    def test_a_candidate_receipt_is_created_and_loaded_with_its_exact_target(self):
        request = candidate(ccr_model="ccr/exact", agy_model="vendor/agy:1.2+b")
        receipt = self.create(request)
        invocation = receipt.invocation
        self.assertEqual(invocation.root, self.state / f"candidate-{request.candidate_id}-{invocation.invocation_id}")
        platform = platform_identity()
        self.assertEqual((invocation.platform_os, invocation.platform_arch), (platform["os"], platform["arch"]))
        stored = json.loads(receipt.path.read_bytes())
        self.assertEqual((stored["kind"], stored["status"], stored["metadata"]["request"]),
                         ("local-native-candidate", "INCOMPLETE", request.document()))
        self.assertEqual(stored["stages"], {name: {"state": "pending", "intent": {}, "result": {}}
                                            for name in CANDIDATE_STAGES})
        loaded = storage.load_receipt(receipt.path)
        self.assertEqual(loaded.invocation, invocation)
        self.assertIsInstance(loaded.invocation.request, CandidateRequest)

    def test_a_release_receipt_keeps_its_tag_placement_kind_and_stages(self):
        request = ReleaseRequest(repo=fx.REPOSITORY, tag="v1.2.3", opencode_model="vendor/model", ccr_model=None,
                                 no_submit=True)
        receipt = self.create(request)
        self.assertEqual(receipt.invocation.root.name, f"v1.2.3-{receipt.invocation.invocation_id}")
        stored = json.loads(receipt.path.read_bytes())
        self.assertEqual((stored["kind"], set(stored["stages"])), ("local-native-release", set(STAGES)))
        self.assertEqual(storage.load_receipt(receipt.path).invocation.request, request)

    def test_stage_facts_are_immutable_through_callers_and_reload(self):
        receipt = self.create(candidate())
        intent = {"claim": {"platform": ["Linux", "amd64"]}}
        result = {"ack": {"commit": "4" * 40}}
        receipt.intend("claim", intent)
        intent["claim"]["platform"] = ["Darwin", "arm64"]
        receipt.complete("claim", result)
        result["ack"]["commit"] = "5" * 40
        snapshot = receipt.stage("claim")
        snapshot.intent["claim"]["platform"] = ["changed"]
        expected = ({"claim": {"platform": ["Linux", "amd64"]}}, {"ack": {"commit": "4" * 40}})
        for reader in (receipt, storage.load_receipt(receipt.path)):
            with self.subTest(reader=reader is receipt):
                stage = reader.stage("claim")
                self.assertEqual((stage.state, stage.intent, stage.result), ("complete", *expected))
        with self.assertRaisesRegex(ReleaseError, "release_intent_conflict"):
            receipt.intend("claim", {"claim": {"platform": ["Darwin", "arm64"]}})
        with self.assertRaisesRegex(ReleaseError, "release_completion_conflict"):
            receipt.complete("claim", {"ack": {"commit": "5" * 40}})

    def test_the_schema_version_is_exactly_the_integer_one_for_both_targets(self):
        release = ReleaseRequest(repo=fx.REPOSITORY, tag="v1.2.3", opencode_model="vendor/model", ccr_model=None,
                                 no_submit=True)
        for request in (candidate(), release):
            receipt = self.create(request)
            original = receipt.path.read_bytes()
            stored = json.loads(original)
            for version in (1, True, 1.0, False, "1", None):
                with self.subTest(kind=stored["kind"], version=version):
                    data = (json.dumps({**stored, "schema_version": version}, sort_keys=True, indent=2) + "\n").encode()
                    receipt.path.write_bytes(data)
                    if type(version) is not int:
                        with self.assertRaises(ReleaseError) as caught:
                            storage.load_receipt(receipt.path)
                        self.assertEqual(caught.exception.code, "release_receipt_invalid")
                        continue
                    self.assertEqual(data, original)
                    loaded = storage.load_receipt(receipt.path)
                    self.assertEqual(loaded.invocation, receipt.invocation)
                    loaded.report(loaded.status, loaded.reason)
                    self.assertEqual(receipt.path.read_bytes(), original)

    def test_mixed_unknown_or_mismatched_receipts_refuse_on_load(self):
        receipt = self.create(candidate())
        original = json.loads(receipt.path.read_bytes())
        request = original["metadata"]["request"]
        other = {**request, "locator": {**request["locator"], "manifest_sha256": "9" * 64}}
        cases = {
            "release_receipt_invalid": {**original, "kind": "local-native-installer"},
            "release_request_invalid": {**original, "kind": "local-native-release"},
            "release_receipt_stages_invalid": {**original, "stages": {
                name: {"state": "pending", "intent": {}, "result": {}} for name in STAGES}},
            "release_receipt_location_mismatch": {**original, "metadata": {**original["metadata"], "request": other}},
            "candidate_request_invalid": {**original, "metadata": {**original["metadata"], "request": {
                **request, "no_submit": True}}},
        }
        for code, value in cases.items():
            with self.subTest(code=code):
                receipt.path.write_text(json.dumps(value), encoding="utf-8")
                with self.assertRaises(ReleaseError) as caught:
                    storage.load_receipt(receipt.path)
                self.assertEqual(caught.exception.code, code)


class PublicResumeTests(StorageCase):
    """The public resume command over actual stored receipts; every effect boundary is a sentinel."""

    def resume(self, receipt: Receipt) -> tuple[int, dict[str, object], dict[str, Mock]]:
        sentinels = {"state_base": Mock(return_value=self.state), "lock": Mock(return_value=41),
                     "unlock": Mock(), "create": Mock(), "execute": Mock(), "resume": Mock()}
        refusal = AssertionError("no child may launch while a receipt is resumed in this test")
        output = io.StringIO()
        with patch.object(cli, "state_base", sentinels["state_base"]), \
                patch.object(cli, "acquire_exclusive_file_lock", sentinels["lock"]), \
                patch.object(cli, "release_exclusive_file_lock", sentinels["unlock"]), \
                patch.object(cli, "create_receipt", sentinels["create"]), \
                patch.object(cli, "execute", sentinels["execute"]), patch.object(cli, "resume", sentinels["resume"]), \
                patch.object(operations.subprocess, "Popen", Mock(side_effect=refusal)) as launches, \
                patch("sys.stdout", output), patch("sys.stderr", io.StringIO()):
            code = cli.main(["resume", "--receipt", str(receipt.path)])
        self.assertEqual(launches.call_count, 0)
        return code, json.loads(output.getvalue()), sentinels

    def test_a_candidate_receipt_resumes_under_its_own_lock_with_a_candidate_report(self):
        request = candidate()
        receipt = self.create(request)
        listing = sorted(self.tree.root.rglob("*"))
        code, report, sentinels = self.resume(receipt)
        self.assertEqual((code, report["status"], report["reason"]), (3, "INCOMPLETE", "invocation_initialized"))
        sentinels["lock"].assert_called_once_with(self.state / lock_name(request))
        self.assertEqual((sentinels["resume"].call_count, sentinels["unlock"].call_count), (1, 1))
        self.assertEqual((sentinels["create"].call_count, sentinels["execute"].call_count), (0, 0))
        self.assertEqual((report["candidate_id"], report["attempt"]), (request.candidate_id, 1))
        self.assertFalse({"asset", "validation", "native_status"} & set(report))
        self.assertEqual(report["claim"], {"state": "pending", "result": {}})
        self.assertEqual(sorted(self.tree.root.rglob("*")), listing)

    def test_a_release_receipt_still_resumes_under_its_lock(self):
        request = ReleaseRequest(repo=fx.REPOSITORY, tag="v1.2.3", opencode_model="vendor/model", ccr_model=None,
                                 no_submit=True)
        receipt = self.create(request)
        code, report, sentinels = self.resume(receipt)
        self.assertEqual((code, report["status"], report["reason"]), (3, "INCOMPLETE", "invocation_initialized"))
        sentinels["lock"].assert_called_once_with(self.state / lock_name(request))
        self.assertEqual((sentinels["resume"].call_count, sentinels["unlock"].call_count), (1, 1))
        self.assertEqual((sentinels["create"].call_count, sentinels["execute"].call_count), (0, 0))


class PublicRunCandidateTests(StorageCase):
    """The public run-candidate command builds the exact request and runs it under its candidate lock."""

    def run_candidate(self, locator: bytes) -> tuple[int, dict[str, object], dict[str, Mock]]:
        path = self.tree.root / "locator.json"
        path.write_bytes(locator)
        created: list[Receipt] = []

        def create(_checkout: Path, request: CandidateRequest | ReleaseRequest, argv: list[str]) -> Receipt:
            created.append(storage.create_receipt(self.checkout, request, argv))
            return created[-1]

        sentinels = {"state_base": Mock(return_value=self.state), "lock": Mock(return_value=41),
                     "unlock": Mock(), "create": Mock(side_effect=create), "execute": Mock(), "resume": Mock()}
        output = io.StringIO()
        with patch.object(cli, "state_base", sentinels["state_base"]), \
                patch.object(cli, "acquire_exclusive_file_lock", sentinels["lock"]), \
                patch.object(cli, "release_exclusive_file_lock", sentinels["unlock"]), \
                patch.object(cli, "create_receipt", sentinels["create"]), \
                patch.object(cli, "execute", sentinels["execute"]), patch.object(cli, "resume", sentinels["resume"]), \
                patch("sys.stdout", output), patch("sys.stderr", io.StringIO()):
            code = cli.main(["run-candidate", "--locator", str(path), "--store-branch", STORE.branch,
                             "--store-genesis-commit", STORE.genesis_commit, "--store-marker-blob", STORE.marker_blob,
                             "--attempt", "1", "--opencode-model", "Vendor/OpenCode-1.0"])
        if created:
            sentinels["execute"].assert_called_once_with(created[0])
        return code, json.loads(output.getvalue()), sentinels

    def test_the_exact_request_runs_under_the_candidate_lock(self):
        code, report, sentinels = self.run_candidate(encode_locator(LOCATOR))
        request = candidate()
        self.assertEqual(sentinels["create"].call_args.args[1], request)
        sentinels["lock"].assert_called_once_with(self.state / lock_name(request))
        self.assertEqual((sentinels["unlock"].call_count, sentinels["resume"].call_count), (1, 0))
        self.assertEqual((code, report["status"], report["candidate_id"]), (3, "INCOMPLETE", request.candidate_id))

    def test_a_malformed_locator_is_blocked_before_any_lock(self):
        code, report, sentinels = self.run_candidate(encode_locator(LOCATOR) + b" ")
        self.assertEqual((code, report["status"]), (2, "BLOCKED"))
        self.assertEqual((sentinels["lock"].call_count, sentinels["create"].call_count), (0, 0))


if __name__ == "__main__":
    unittest.main()
