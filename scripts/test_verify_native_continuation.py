"""The supplemental verifier composes the candidate coordinator's readers with the supplemental validator.

The candidate receipt and proof are the real ones the composed candidate
fixture produces (see ``test_native_candidate_support``). The supplemental
validator is the canonical owner of its own evidence and has its own tests, so
here it is replaced by a recorder; what is under test is the association the
edge requires before and after it. Every verification, refused or repeated,
must leave the invocation's bytes and inventory unchanged.
"""

from __future__ import annotations

import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import shutil
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import verify_native_continuation as verify
from scripts.delegate_acceptance.native_evidence.contract import AGGREGATE_RECEIPT_NAME
from scripts.delegate_acceptance.native_evidence.public_stage import NATIVE_COMPLETION_NAME
from scripts.delegate_acceptance.native_release.contract import receipt_stages
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.test_native_candidate_support import CandidateCase


REVISION = "a" * 40
OTHER_DIGEST = "d" * 64


def inventory(root: Path) -> dict[str, str]:
    """Every entry under ``root``: directories by kind, files by content digest, links by target."""
    entries: dict[str, str] = {}
    for current, directories, files in os.walk(root):
        for name in directories:
            entries[os.path.relpath(Path(current) / name, root)] = "directory"
        for name in files:
            path = Path(current) / name
            relative = os.path.relpath(path, root)
            entries[relative] = ("link:" + os.readlink(path) if path.is_symlink()
                                 else hashlib.sha256(path.read_bytes()).hexdigest())
    return entries


class VerifyCase(CandidateCase):
    def recorded_host(self) -> contextlib.AbstractContextManager[object]:
        """The host identity the producing run observes; the validating host is always the real one."""
        return contextlib.nullcontext()

    def setUp(self) -> None:
        with self.recorded_host():
            super().setUp()
            self.receipt = self.execute()
        self.assertEqual(self.receipt.status, "LOCAL_ONLY")
        self.aggregate_sha256 = self.receipt.stage("project").intent["source_digests"][AGGREGATE_RECEIPT_NAME]

    def edit(self, change) -> None:
        """Mutate the stored receipt document, as a damaged or foreign receipt would be."""
        path = self.receipt.path
        value = json.loads(path.read_text(encoding="utf-8"))
        change(value)
        path.write_text(json.dumps(value), encoding="utf-8")

    def invoke(self, *, candidate_id: str | None = None, receipt_path: Path | None = None):
        """One verifier call; the caller owns how ``validate_supplemental`` is replaced."""
        return verify.verify_native_continuation(
            candidate_receipt=receipt_path or self.receipt.path,
            candidate_id=candidate_id or self.locator.candidate_id,
            supplemental_root=Path("supplemental"), canonical_aggregate=Path("aggregate-receipt.json"),
            source_revision=REVISION, candidate_delegate=Path("delegate"), pi_control=Path("pi-control"),
            environment={}, checkout=Path("checkout"))

    def verify(self, *, candidate_id: str | None = None, aggregate_sha256: str | None = None,
               receipt_path: Path | None = None):
        """Run the verifier over the stored receipt; return its summary and the recorder."""
        summary = {"kind": "native-acceptance-supplemental-summary",
                   "canonical_aggregate_sha256": aggregate_sha256 or self.aggregate_sha256,
                   "source_revision": REVISION, "credentials_in_receipt": False}
        with patch.object(verify, "validate_supplemental", return_value=summary) as recorder:
            result = self.invoke(candidate_id=candidate_id, receipt_path=receipt_path)
        return result, recorder

    def assertRefused(self, code: str, **options) -> None:
        """The refusal names its cause, never reaches the supplemental validator, and changes nothing."""
        before = inventory(self.case.root)
        with patch.object(verify, "validate_supplemental") as recorder:
            with self.assertRaisesRegex(Exception, code):
                self.invoke(**options)
        recorder.assert_not_called()
        self.assertEqual(inventory(self.case.root), before)


class SuccessTests(VerifyCase):
    def test_a_completed_local_only_proof_extends_the_supplemental_summary(self) -> None:
        before = inventory(self.case.root)
        first, recorder = self.verify()
        self.assertEqual(inventory(self.case.root), before)
        second, _ = self.verify()
        self.assertEqual(inventory(self.case.root), before)
        self.assertEqual(first, second)
        claim_sha256 = self.receipt.stage("claim").intent["claim_sha256"]
        self.assertEqual((first["candidate_id"], first["attempt"], first["claim_sha256"]),
                         (self.locator.candidate_id, 1, claim_sha256))
        self.assertEqual(first["canonical_aggregate_sha256"], self.aggregate_sha256)
        self.assertEqual(first["kind"], "native-acceptance-supplemental-summary")
        recorder.assert_called_once_with(
            supplemental_root=Path("supplemental"), canonical_aggregate=Path("aggregate-receipt.json"),
            source_revision=REVISION, candidate_delegate=Path("delegate"), pi_control=Path("pi-control"),
            environment={}, checkout=Path("checkout"))

    def test_the_cli_prints_only_the_summary_and_never_the_receipt_path(self) -> None:
        summary = {"kind": "native-acceptance-supplemental-summary",
                   "canonical_aggregate_sha256": self.aggregate_sha256}
        out, err = io.StringIO(), io.StringIO()
        argv = ["--supplemental-root", "s", "--canonical-aggregate", "a", "--source-revision", REVISION,
                "--candidate-delegate", "d", "--pi-control", "p",
                "--candidate-receipt", str(self.receipt.path), "--candidate-id", self.locator.candidate_id]
        with patch.object(verify, "validate_supplemental", return_value=summary):
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                self.assertEqual(verify.main(argv), 0)
        printed = json.loads(out.getvalue())
        self.assertEqual((printed["candidate_id"], printed["attempt"]), (self.locator.candidate_id, 1))
        self.assertEqual(err.getvalue(), "")
        self.assertNotIn(str(self.receipt.path.parent), out.getvalue())


@unittest.skipIf(platform.system() == "Darwin", "needs a validating host other than the recorded one")
class DarwinReceiptTests(VerifyCase):
    """A receipt recorded on Darwin verifies from a non-Darwin host: the validating host is not consulted."""

    def recorded_host(self) -> contextlib.AbstractContextManager[object]:
        return patch.multiple(platform, system=lambda: "Darwin", machine=lambda: "arm64")

    def test_a_darwin_receipt_is_verified_from_a_non_darwin_host(self) -> None:
        invocation = self.receipt.invocation
        self.assertEqual((invocation.platform_os, invocation.platform_arch), ("Darwin", "arm64"))
        self.assertNotEqual(platform.system(), "Darwin")
        before = inventory(self.case.root)
        result, _ = self.verify()
        self.assertEqual(inventory(self.case.root), before)
        self.assertEqual(result["candidate_id"], self.locator.candidate_id)


class RefusalTests(VerifyCase):
    def test_a_different_candidate_is_refused(self) -> None:
        self.assertRefused("candidate_id_mismatch", candidate_id="9" * 64)

    def test_a_receipt_copy_away_from_its_original_path_is_refused(self) -> None:
        copy = self.case.root / "copies" / self.receipt.invocation.root.name
        shutil.copytree(self.receipt.invocation.root, copy)
        self.assertRefused("release_receipt_location_mismatch", receipt_path=copy / "receipt.json")

    def test_a_receipt_that_is_not_local_only_is_refused(self) -> None:
        self.edit(lambda value: value.update(status="INCOMPLETE", reason="candidate_cleanup_pending"))
        self.assertRefused("candidate_receipt_not_local_only")

    def test_a_pending_or_intended_stage_is_refused(self) -> None:
        original = json.loads(self.receipt.path.read_text(encoding="utf-8"))
        for name in receipt_stages(self.receipt.invocation.request):
            with self.subTest(stage=name, state="pending"):
                self.edit(lambda value: value["stages"].update(
                    {name: {"state": "pending", "intent": {}, "result": {}}}))
                self.assertRefused(f"candidate_stage_not_complete:{name}")
                self.receipt.path.write_text(json.dumps(original), encoding="utf-8")
            with self.subTest(stage=name, state="intended"):
                self.edit(lambda value: value["stages"][name].update(state="intended", result={}))
                self.assertRefused(f"candidate_stage_not_complete:{name}")
                self.receipt.path.write_text(json.dumps(original), encoding="utf-8")

    def test_a_refused_or_readback_disposition_is_refused(self) -> None:
        for name, disposition in (("claim", "refused"), ("record", "refused"), ("record", "readback")):
            with self.subTest(stage=name, disposition=disposition):
                original = self.receipt.path.read_text(encoding="utf-8")
                self.edit(lambda value: value["stages"][name]["result"].update(disposition=disposition))
                self.assertRefused(f"candidate_{name}_not_confirmed")
                self.receipt.path.write_text(original, encoding="utf-8")

    def test_a_native_stage_that_did_not_exit_zero_is_refused(self) -> None:
        for exit_code in (1, True, "0"):
            with self.subTest(exit_code=exit_code):
                original = self.receipt.path.read_text(encoding="utf-8")
                self.edit(lambda value: value["stages"]["native"]["result"].update(exit_code=exit_code))
                self.assertRefused("candidate_native_exit_nonzero")
                self.receipt.path.write_text(original, encoding="utf-8")

    def test_a_native_completion_other_than_the_proofs_is_refused(self) -> None:
        self.edit(lambda value: value["stages"]["native"]["result"].update(completion_sha256=OTHER_DIGEST))
        self.assertRefused("candidate_native_completion_mismatch")

    def test_project_intent_facts_must_name_the_validated_proof(self) -> None:
        cases = (
            ("candidate_project_claim_mismatch", lambda intent: intent.update(claim_sha256=OTHER_DIGEST)),
            ("candidate_project_aggregate_mismatch",
             lambda intent: intent["source_digests"].update({AGGREGATE_RECEIPT_NAME: OTHER_DIGEST})),
            ("candidate_project_completion_mismatch",
             lambda intent: intent["source_digests"].update({NATIVE_COMPLETION_NAME: OTHER_DIGEST})),
        )
        for code, change in cases:
            with self.subTest(code=code):
                original = self.receipt.path.read_text(encoding="utf-8")
                self.edit(lambda value: change(value["stages"]["project"]["intent"]))
                self.assertRefused(code)
                self.receipt.path.write_text(original, encoding="utf-8")

    def test_a_record_proof_other_than_the_validated_proof_is_refused(self) -> None:
        self.edit(lambda value: value["stages"]["record"]["intent"].update(proof_sha256=OTHER_DIGEST))
        self.assertRefused("candidate_record_proof_mismatch")

    def test_changed_project_facts_are_refused_by_the_existing_reader(self) -> None:
        self.edit(lambda value: value["stages"]["project"]["result"].update(pass_count=0))
        self.assertRefused("public_proof_readback_changed")

    def test_a_request_attempt_other_than_the_claims_is_refused(self) -> None:
        self.edit(lambda value: value["metadata"]["request"].update(attempt=2))
        self.assertEqual(Receipt.load(self.receipt.path).invocation.request.attempt, 2)
        self.assertRefused("candidate_claim_attempt_mismatch")

    def test_a_claim_of_another_invocation_is_refused(self) -> None:
        self.edit(lambda value: value["metadata"].update(platform_arch=(
            "arm64" if value["metadata"]["platform_arch"] == "amd64" else "amd64")))
        self.assertRefused("candidate_claim_invocation_mismatch")

    def test_a_canonical_aggregate_that_is_not_the_proofs_is_refused_after_supplemental(self) -> None:
        before = inventory(self.case.root)
        with self.assertRaisesRegex(Exception, "canonical_aggregate_not_candidate_aggregate"):
            self.verify(aggregate_sha256=OTHER_DIGEST)
        self.assertEqual(inventory(self.case.root), before)

    def test_the_cli_reports_a_refusal_on_stderr_with_a_nonzero_exit(self) -> None:
        out, err = io.StringIO(), io.StringIO()
        argv = ["--supplemental-root", "s", "--canonical-aggregate", "a", "--source-revision", REVISION,
                "--candidate-delegate", "d", "--pi-control", "p",
                "--candidate-receipt", str(self.receipt.path), "--candidate-id", "9" * 64]
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            self.assertEqual(verify.main(argv), 1)
        self.assertEqual(out.getvalue(), "")
        self.assertIn("candidate_id_mismatch", err.getvalue())

    def test_a_receipt_that_does_not_load_is_a_reported_refusal(self) -> None:
        err = io.StringIO()
        missing = self.receipt.path.with_name("absent") / "receipt.json"
        argv = ["--supplemental-root", "s", "--canonical-aggregate", "a", "--source-revision", REVISION,
                "--candidate-delegate", "d", "--pi-control", "p",
                "--candidate-receipt", str(missing), "--candidate-id", self.locator.candidate_id]
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err):
            self.assertEqual(verify.main(argv), 1)
        self.assertIn("native continuation verification failed", err.getvalue())
        self.assertIsInstance(Receipt.load(self.receipt.path), Receipt)


if __name__ == "__main__":
    unittest.main()
