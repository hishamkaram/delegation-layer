"""Hosted report publication preserves admission and native-result authority."""

import argparse
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.native_evidence.public_contract import PublicProofError
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.delegate_acceptance.release_verification.native_history import ValidationObservation
from scripts.delegate_acceptance.release_verification.native_remote_history import AttemptReport
from scripts.delegate_acceptance.release_verification.native_hosted import _enforce_history, validate_submission
from scripts.delegate_acceptance.release_verification.native_submission import SubmissionIdentity


MODULE = "scripts.delegate_acceptance.release_verification.native_hosted."


class HostedHistoryOrderTests(unittest.TestCase):
    def invoke(self, run_id: int, rows: tuple[AttemptReport, ...]):
        report = {"repository_id": 7, "tag": "v1.2.3", "harness_revision": "c" * 40,
                  "run_id": run_id, "run_attempt": 1, "proof_sha256": "a" * 64}
        with patch(MODULE + "history_reports", return_value=rows):
            _enforce_history(GitHubClient({"commands": []}, {}), "owner/repo", report)
        return report

    def observation(self, run_id: int, admission="unknown", verdict="incomplete"):
        return AttemptReport(
            f"2026-10-17T12:00:0{run_id}Z",
            ValidationObservation(run_id, 1, "a" * 64, admission, verdict),
            None, "in_progress", None,
        )

    def test_older_current_pass_cannot_resolve_a_later_missing_report(self):
        with self.assertRaisesRegex(VerificationError, "native_history_blocked"):
            self.invoke(1, (self.observation(1), self.observation(2)))

    def test_later_current_pass_resolves_an_earlier_missing_same_proof(self):
        report = self.invoke(2, (self.observation(1), self.observation(2)))
        self.assertEqual(report["history"], {
            "allowed": True, "reason": "native_history_pass", "observed_attempts": 2,
        })

    def test_current_pass_cannot_erase_an_admitted_failure(self):
        with self.assertRaisesRegex(VerificationError, "native_history_blocked"):
            self.invoke(2, (self.observation(1, "admitted", "fail"), self.observation(2)))

    def test_unobserved_current_attempt_cannot_be_appended_as_later_evidence(self):
        with self.assertRaisesRegex(VerificationError, "native_history_current_attempt_unobserved"):
            self.invoke(2, (self.observation(1),))


class HostedNativeValidationTests(unittest.TestCase):
    def invoke(self, directory: Path, *, proof_error=None, identity_error=None,
               envelope_error=None, history_error=None):
        directory = directory.resolve(strict=True)
        event = directory / "event.json"
        event.write_text(json.dumps({"repository": {"id": 7}, "sender": {"id": 9}}))
        report = directory / "report.json"
        report.write_text(json.dumps({"schema_version": 1, "admission": "unknown",
                                      "verdict": "incomplete", "stage": "setup"}))
        args = argparse.Namespace(tag="v1.2.3", asset_id=42, proof_sha256="a" * 64,
                                  invocation_id="b" * 32, opencode_model="provider/model",
                                  ccr_model="", report=str(report))
        identity = SubmissionIdentity(
            repository="owner/repo", repository_id=7, release_revision="d" * 40,
            harness_revision="c" * 40, sender_id=9, asset_id=42, proof_sha256="a" * 64,
            asset_name="native-proof-Linux-amd64-" + "b" * 32 + ".zip",
        )
        env = {"GITHUB_EVENT_PATH": str(event), "GITHUB_SHA": "c" * 40,
               "GITHUB_RUN_ID": "2", "GITHUB_RUN_ATTEMPT": "1",
               "GITHUB_REPOSITORY": "owner/repo", "GITHUB_REF": "refs/heads/main"}
        with (patch.dict(os.environ, env), patch(MODULE + "gh_environment", return_value={}),
              patch(MODULE + "authenticate_submission", return_value=(identity, b"proof")),
              patch(MODULE + "inspect_public_proof", side_effect=envelope_error,
                    return_value={"platform": {"os": "Linux", "arch": "amd64"}}),
              patch(MODULE + "archive_expectation", return_value=None),
              patch(MODULE + "validate_public_identity", side_effect=identity_error),
              patch(MODULE + "validate_public_proof", side_effect=proof_error,
                    return_value={"status": "PASS", "case_results_verdict": "PASS"}),
              patch(MODULE + "_enforce_history", side_effect=history_error)):
            code = validate_submission(args)
        return code, json.loads(report.read_bytes())

    def test_valid_native_proof_publishes_admitted_pass(self):
        with tempfile.TemporaryDirectory() as temporary:
            code, report = self.invoke(Path(temporary))
        self.assertEqual(code, 0)
        self.assertEqual((report["admission"], report["verdict"]), ("admitted", "pass"))
        self.assertEqual(report["stage"], "complete")

    def test_case_schema_failure_after_admission_is_blocking_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            code, report = self.invoke(Path(temporary), proof_error=PublicProofError("bad-case"))
        self.assertEqual(code, 1)
        self.assertEqual((report["admission"], report["verdict"]), ("admitted", "fail"))

    def test_malformed_authenticated_envelope_keeps_uncertainty_blocking(self):
        with tempfile.TemporaryDirectory() as temporary:
            code, report = self.invoke(Path(temporary), envelope_error=PublicProofError("bad-manifest"))
        self.assertEqual(code, 1)
        self.assertEqual((report["admission"], report["verdict"]), ("unknown", "incomplete"))

    def test_explicit_binding_mismatch_remains_correctable(self):
        with tempfile.TemporaryDirectory() as temporary:
            code, report = self.invoke(Path(temporary), identity_error=PublicProofError("wrong-source"))
        self.assertEqual(code, 1)
        self.assertEqual((report["admission"], report["verdict"]), ("not_admitted", "rejected"))

    def test_history_failure_does_not_invent_a_provider_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            code, report = self.invoke(
                Path(temporary), history_error=VerificationError("native_history_blocked", "history"),
            )
        self.assertEqual(code, 1)
        self.assertEqual((report["admission"], report["verdict"]), ("admitted", "pass"))
        self.assertEqual(report["stage"], "validate-history")


if __name__ == "__main__":
    unittest.main()
