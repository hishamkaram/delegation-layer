"""Real directory-journal recovery and exact hosted-report consumer decisions."""

from __future__ import annotations

import io
import json
from unittest.mock import patch
import unittest

from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.native_release import cleanup, cli, coordinator, submission, watching
from scripts.delegate_acceptance.native_release.contract import ReleaseError
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.delegate_acceptance.native_release.submission import dispatch
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.native_history import ValidationObservation
from scripts.delegate_acceptance.release_verification.native_remote_history import AttemptReport
from scripts.test_native_release_support import PROOF_HASH, RELEASE, ReleaseFixture


class CliRecoveryTests(ReleaseFixture):
    def seed_upload_with_intent(self) -> None:
        self.seed_project()
        self.client.assets = [self.client.asset()]
        project = self.receipt.stage("project").result
        intent = {"name": self.receipt.invocation.proof_name, **project}
        result = {"asset_id": 71, "proof_sha256": project["proof_sha256"],
                  "size": project["size"], "name": self.receipt.invocation.proof_name}
        self.receipt.intend("upload", intent)
        self.receipt.complete("upload", result)

    def run_resume(self) -> tuple[int, dict[str, object]]:
        output = io.StringIO()
        with patch.object(cli, "require_python"), \
                patch.object(cli, "load_receipt", side_effect=lambda _path: Receipt.load(self.receipt.path)), \
                patch.object(cli, "state_base", return_value=self.base), \
                patch.object(cli, "acquire_exclusive_file_lock", return_value=41), \
                patch.object(cli, "release_exclusive_file_lock"), \
                patch.object(coordinator, "github_client", return_value=self.client), \
                patch.object(coordinator, "read_public"), \
                patch.object(submission, "read_public"), \
                patch("sys.stdout", output), patch("sys.stderr", io.StringIO()):
            exit_code = cli.main(["resume", "--receipt", str(self.receipt.path)])
        report = json.loads(output.getvalue())
        if not isinstance(report, dict):
            raise AssertionError("CLI report is not an object")
        return exit_code, report

    def test_unintended_upload_error_remains_blocked_without_effect(self) -> None:
        self.seed_project()
        with patch.object(submission, "recheck_submission",
                          side_effect=VerificationError("github_request_failed", "github-api")), \
                patch.object(submission, "command") as command, \
                patch.object(coordinator, "native") as native:
            exit_code, report = self.run_resume()
        self.assertEqual((exit_code, report["status"], report["reason"]),
                         (2, "BLOCKED", "github_request_failed"))
        self.assertEqual(self.receipt.stage("upload").state, "pending")
        self.assertEqual(self.client.posts, 0)
        command.assert_not_called()
        native.assert_not_called()

    def test_upload_reconciliation_error_resumes_without_paid_or_duplicate_effect(self) -> None:
        self.seed_project(verdict="FAIL")
        self.complete("native", {"exit_code": 1, "status": "FAIL"})
        self.receipt.intend("upload", {
            "name": self.receipt.invocation.proof_name, **self.receipt.stage("project").result,
        })
        with patch.object(submission, "recheck_submission"), \
                patch.object(submission, "_asset_matches",
                             side_effect=VerificationError("github_request_failed", "github-api")), \
                patch.object(submission, "command") as command, \
                patch.object(coordinator, "native") as native:
            for _attempt in range(2):
                exit_code, report = self.run_resume()
                self.assertEqual(exit_code, 3)
                self.assertEqual(report["status"], "INCOMPLETE")
                self.assertEqual(report["reason"],
                                 "submission_intent_unresolved_resume_required:github_request_failed")
                self.assertEqual(report["native_status"], "FAIL")
        self.assertEqual(Receipt.load(self.receipt.path).stage("upload").state, "intended")
        self.assertEqual(self.client.posts, 0)
        command.assert_not_called()
        native.assert_not_called()

    def test_initial_upload_read_failure_remains_pending_and_resume_uploads_once(self) -> None:
        self.seed_project()
        with patch.object(submission, "recheck_submission"), \
                patch.object(submission, "_asset_matches",
                             side_effect=VerificationError("github_request_failed", "github-api")), \
                patch.object(submission, "command") as command:
            exit_code, report = self.run_resume()
        self.assertEqual((exit_code, report["status"]), (2, "BLOCKED"))
        self.assertEqual(Receipt.load(self.receipt.path).stage("upload").state, "pending")
        command.assert_not_called()
        self.assertEqual(self.client.posts, 0)

        def upload_effect(*_args, **_kwargs):
            self.assertEqual(Receipt.load(self.receipt.path).stage("upload").state, "intended")
            self.client.assets = [self.client.asset()]

        with patch.object(submission, "recheck_submission"), \
                patch.object(submission, "command", side_effect=upload_effect) as command, \
                patch.object(coordinator, "watch"), patch.object(coordinator, "native") as native:
            for _attempt in range(2):
                exit_code, report = self.run_resume()
                self.assertEqual((exit_code, report["status"]), (2, "BLOCKED"))
                self.assertEqual(report["reason"], "native_validation_not_promotable")
        recovered = Receipt.load(self.receipt.path)
        self.assertEqual(recovered.stage("upload").state, "complete")
        self.assertEqual(recovered.stage("dispatch").state, "complete")
        self.assertEqual(command.call_count, 1)
        self.assertEqual(self.client.posts, 1)
        native.assert_not_called()

    def test_initial_dispatch_read_failure_remains_pending_and_resume_posts_once(self) -> None:
        self.seed_upload_with_intent()
        with patch.object(submission, "recheck_submission"), \
                patch.object(submission, "reconcile_dispatch",
                             side_effect=VerificationError("github_request_failed", "github-api")), \
                patch.object(submission, "command") as command:
            exit_code, report = self.run_resume()
        self.assertEqual((exit_code, report["status"]), (2, "BLOCKED"))
        self.assertEqual(Receipt.load(self.receipt.path).stage("dispatch").state, "pending")
        self.assertEqual(self.client.posts, 0)
        command.assert_not_called()

        with patch.object(submission, "recheck_submission"), \
                patch.object(submission, "command") as command, \
                patch.object(coordinator, "watch"), patch.object(coordinator, "native") as native:
            for _attempt in range(2):
                exit_code, report = self.run_resume()
                self.assertEqual((exit_code, report["status"]), (2, "BLOCKED"))
                self.assertEqual(report["reason"], "native_validation_not_promotable")
        self.assertEqual(Receipt.load(self.receipt.path).stage("dispatch").state, "complete")
        self.assertEqual(self.client.posts, 1)
        command.assert_not_called()
        native.assert_not_called()

    def test_dispatch_intent_reconciliation_error_never_posts_twice(self) -> None:
        self.seed_upload_with_intent()
        self.client.lose_dispatch_response = True
        with patch.object(submission, "recheck_submission"), \
                patch.object(submission, "reconcile_dispatch", side_effect=[
                    None, VerificationError("github_request_failed", "github-api"),
                ]):
            with self.assertRaisesRegex(VerificationError, "github_request_failed"):
                dispatch(self.receipt, self.client)
        self.assertEqual(self.receipt.stage("dispatch").state, "intended")
        self.assertEqual(self.client.posts, 1)

        with patch.object(submission, "recheck_submission"), \
                patch.object(submission, "reconcile_dispatch",
                             side_effect=VerificationError("github_request_failed", "github-api")), \
                patch.object(coordinator, "native") as native:
            for _attempt in range(2):
                exit_code, report = self.run_resume()
                self.assertEqual((exit_code, report["status"]), (3, "INCOMPLETE"))
                self.assertEqual(report["reason"],
                                 "submission_intent_unresolved_resume_required:github_request_failed")
        self.assertEqual(self.client.posts, 1)
        self.assertEqual(len(self.client.runs), 1)
        native.assert_not_called()

    def test_watch_intent_reconciliation_error_only_observes_existing_run(self) -> None:
        self.seed_upload_with_intent()
        with patch.object(submission, "recheck_submission"):
            dispatch(self.receipt, self.client)
        self.assertEqual(self.client.posts, 1)
        with patch.object(watching, "validate_run",
                          side_effect=VerificationError("github_request_failed", "github-api")):
            with self.assertRaisesRegex(VerificationError, "github_request_failed"):
                watching.watch(self.receipt, self.client, timeout=0)
        self.assertEqual(self.receipt.stage("watch").state, "intended")

        with patch.object(submission, "recheck_submission"), \
                patch.object(watching, "validate_run",
                             side_effect=VerificationError("github_request_failed", "github-api")), \
                patch.object(coordinator, "native") as native:
            for _attempt in range(2):
                exit_code, report = self.run_resume()
                self.assertEqual((exit_code, report["status"]), (3, "INCOMPLETE"))
                self.assertEqual(report["reason"],
                                 "submission_intent_unresolved_resume_required:github_request_failed")
        self.assertEqual(self.client.posts, 1)
        native.assert_not_called()


    def test_watch_run_binding_mismatch_reports_field_names_without_redispatch(self) -> None:
        self.seed_upload_with_intent()
        with patch.object(submission, "recheck_submission"):
            dispatch(self.receipt, self.client)
        self.client.run_path = ".github/workflows/release.yml"
        with patch.object(submission, "recheck_submission"), \
                patch.object(coordinator, "native") as native:
            exit_code, report = self.run_resume()
        self.assertEqual((exit_code, report["status"]), (3, "INCOMPLETE"))
        self.assertEqual(report["reason"], "submission_intent_unresolved_resume_required:"
                                           "native_validation_run_unbound:mismatch=path")
        self.assertNotIn("release.yml", report["reason"])
        self.assertEqual(Receipt.load(self.receipt.path).stage("watch").state, "intended")
        self.assertEqual(self.client.posts, 1)
        native.assert_not_called()


class CleanupTests(ReleaseFixture):
    def test_completed_stage_cannot_be_mutated_through_caller_or_reader(self) -> None:
        intent = {"inputs": {"model": "exact"}}
        result = {"bound": {"sha256": "a" * 64}}
        self.receipt.intend("project", intent)
        self.receipt.complete("project", result)
        intent["inputs"]["model"] = "changed"
        result["bound"]["sha256"] = "b" * 64
        snapshot = self.receipt.stage("project")
        inputs, bound = snapshot.intent["inputs"], snapshot.result["bound"]
        if not isinstance(inputs, dict) or not isinstance(bound, dict):
            raise AssertionError("nested stage facts missing")
        inputs["model"] = "changed-again"
        bound["sha256"] = "c" * 64
        self.receipt.report("INCOMPLETE", "snapshot_read_only")
        loaded = Receipt.load(self.receipt.path).stage("project")
        self.assertEqual(loaded.intent, {"inputs": {"model": "exact"}})
        self.assertEqual(loaded.result, {"bound": {"sha256": "a" * 64}})

    def ready_cleanup(self, *, verdict: str = "PASS") -> None:
        self.seed_project(verdict=verdict)
        owner = DirectoryOwner()
        token = owner.create("test runner", self.root / "runner", self.root)
        (self.root / "runner" / "scratch").write_text("owned fixture scratch")
        self.complete("download", {"runner_identity": directory_identity_record(token)})
        owner.release()
        for name in ("archive", "native", "finalize"):
            self.complete(name, {"exit_code": 0, "completion_sha256": "d" * 64,
                                 "lifetime_closed": True})
        self.complete("watch", {"promotable": True})

    def test_cleanup_replays_durable_canonical_journal_without_touching_replacement(self) -> None:
        self.ready_cleanup()
        with patch.object(cleanup, "observe_child", return_value={"exit_code": 0, "completion_sha256": "d" * 64}):
            cleanup.cleanup(self.receipt)
        self.assertFalse((self.root / "runner").exists())
        original = self.receipt.stage("cleanup").result
        replacement = self.root / "runner"
        replacement.mkdir()
        (replacement / "keep").write_text("unowned replacement")
        cleanup.cleanup(Receipt.load(self.receipt.path))
        self.assertEqual((replacement / "keep").read_text(), "unowned replacement")
        self.assertEqual(Receipt.load(self.receipt.path).stage("cleanup").result, original)

    def test_replaced_target_before_cleanup_is_retained(self) -> None:
        self.ready_cleanup()
        (self.root / "runner").rename(self.root / "original-runner")
        replacement = self.root / "runner"
        replacement.mkdir()
        (replacement / "keep").write_text("must not delete")
        with patch.object(cleanup, "observe_child", return_value={"exit_code": 0, "completion_sha256": "d" * 64}):
            with self.assertRaisesRegex(ReleaseError, "release_scratch_cleanup_unresolved"):
                cleanup.cleanup(self.receipt)
        self.assertTrue((replacement / "keep").is_file())
        self.assertTrue((self.root / "original-runner" / "scratch").is_file())

    def test_crash_after_cleanup_intent_can_only_reconcile_same_identity(self) -> None:
        self.ready_cleanup()
        with patch.object(cleanup, "observe_child", return_value={"exit_code": 0, "completion_sha256": "d" * 64}), \
                patch.object(cleanup, "prepare_directory_cleanup", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                cleanup.cleanup(self.receipt)
        recovered = Receipt.load(self.receipt.path)
        self.assertEqual(recovered.stage("cleanup").state, "intended")
        with patch.object(cleanup, "observe_child") as child:
            cleanup.cleanup(recovered)
        child.assert_not_called()
        self.assertFalse((self.root / "runner").exists())

    def test_failed_or_unresolved_proof_retains_all_private_state(self) -> None:
        self.ready_cleanup(verdict="FAIL")
        failed = Receipt.load(self.receipt.path)
        with patch.object(cleanup, "resume_directory_cleanup") as remove:
            cleanup.cleanup(failed)
        remove.assert_not_called()
        self.assertTrue((self.root / "runner" / "scratch").is_file())


class WatchingTests(ReleaseFixture):
    def seed_dispatch(self) -> None:
        self.seed_upload()
        with patch("scripts.delegate_acceptance.native_release.preflight.resolve_release_identity",
                   return_value={"commit_sha": RELEASE}):
            dispatch(self.receipt, self.client)

    def report(self, *, asset_id: int = 71, verdict: str = "pass") -> AttemptReport:
        value: dict[str, object] = {
            "proof_asset_id": asset_id, "proof_sha256": PROOF_HASH,
            "invocation_id": "1" * 32, "opencode_model": "vendor/exact-model",
            "ccr_model": "exact-alias", "sender_id": 61, "admission": "admitted",
            "stage": "complete" if verdict == "pass" else "validate-native-facts",
        }
        return AttemptReport("2026-10-17T00:00:00Z", ValidationObservation(81, 1, PROOF_HASH, "admitted", verdict),
                             value, "completed", "success" if verdict == "pass" else "failure")

    def test_watch_requires_exact_report_and_all_history(self) -> None:
        self.seed_dispatch()
        run = {**self.client.run(), "status": "completed", "conclusion": "success"}
        with patch.object(self.client, "run", return_value=run), \
                patch.object(watching, "history_reports", return_value=(self.report(),)):
            watching.watch(self.receipt, self.client, timeout=0)
        self.assertTrue(self.receipt.stage("watch").result["promotable"])

    def test_wrong_asset_report_never_becomes_promotion_authority(self) -> None:
        self.seed_dispatch()
        run = {**self.client.run(), "status": "completed", "conclusion": "success"}
        with patch.object(self.client, "run", return_value=run), \
                patch.object(watching, "history_reports", return_value=(self.report(asset_id=999),)):
            with self.assertRaisesRegex(ReleaseError, "native_validation_report_asset_mismatch"):
                watching.watch(self.receipt, self.client, timeout=0)

    def test_submitted_native_failure_is_observed_and_retained(self) -> None:
        self.seed_dispatch()
        run = {**self.client.run(), "status": "completed", "conclusion": "failure"}
        with patch.object(self.client, "run", return_value=run), \
                patch.object(watching, "history_reports", return_value=(self.report(verdict="fail"),)):
            watching.watch(self.receipt, self.client, timeout=0)
        self.assertFalse(self.receipt.stage("watch").result["promotable"])
        self.assertEqual(self.receipt.stage("watch").result["observation"]["verdict"], "fail")

    def test_finite_watch_does_not_redispatch(self) -> None:
        self.seed_dispatch()
        with self.assertRaisesRegex(ReleaseError, "native_validation_watch_expired"):
            watching.watch(self.receipt, self.client, timeout=0)
        self.assertEqual(self.client.posts, 1)
        self.assertEqual(self.receipt.stage("watch").state, "intended")

    def test_complete_projection_resume_only_observes_submission(self) -> None:
        self.seed_project()
        with patch.object(coordinator, "read_public"), patch.object(coordinator, "github_client", return_value=self.client), \
                patch.object(coordinator, "submit") as submit, patch.object(coordinator, "native") as native, \
                patch.object(coordinator, "archive") as archive, patch.object(coordinator, "download") as download:
            coordinator.resume(Receipt.load(self.receipt.path))
        submit.assert_called_once()
        native.assert_not_called()
        archive.assert_not_called()
        download.assert_not_called()

    def test_local_only_output_keeps_explicit_native_verdict(self) -> None:
        self.seed_project(verdict="BLOCKED")
        self.complete("native", {"status": "BLOCKED"})
        self.receipt.report("LOCAL_ONLY", "no_submit_native_blocked_not_promotion_authority")
        output = io.StringIO()
        with patch("sys.stdout", output):
            cli._report(self.receipt)
        report = json.loads(output.getvalue())
        self.assertEqual(report["status"], "LOCAL_ONLY")
        self.assertEqual(report["native_status"], "BLOCKED")
        self.assertIn("native_proof", report)


if __name__ == "__main__":
    unittest.main()
