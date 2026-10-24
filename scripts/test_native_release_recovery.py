"""Real directory-journal recovery and exact hosted-report consumer decisions."""

from __future__ import annotations

import errno
import io
import json
from pathlib import Path
import sys
from unittest.mock import patch
import unittest

from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.native_release import (
    cleanup, cli, coordinator, execution, preflight, submission, watching,
)
from scripts.delegate_acceptance.native_release.contract import (
    STAGES, ReleaseError, ReleaseRequest, invocation_name,
)
from scripts.delegate_acceptance.native_release.receipt import Receipt, lock_name
from scripts.delegate_acceptance.native_release.submission import dispatch
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.release_verification import commands
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.native_history import ValidationObservation
from scripts.delegate_acceptance.release_verification.native_remote_history import AttemptReport
from scripts.test_native_release_support import HEAD, PROOF_HASH, RELEASE, REPO, SOURCE, TAG, ReleaseFixture


class ResumeFixture(ReleaseFixture):
    """The composed `resume` command over a fixture receipt; defines no tests of its own."""

    def run_resume(self, read_error: ReleaseError | None = None) -> tuple[int, dict[str, object]]:
        return self.run_cli(["resume", "--receipt", str(self.receipt.path)], read_error)

    def run_cli(self, argv: list[str], read_error: ReleaseError | None = None) -> tuple[int, dict[str, object]]:
        """``cli.main`` with this fixture's receipt as the loaded or created one."""
        output = io.StringIO()
        with patch.object(cli, "require_python"), \
                patch.object(cli, "load_receipt", side_effect=lambda _path: Receipt.load(self.receipt.path)), \
                patch.object(cli, "create_receipt", side_effect=lambda *_arguments: self.receipt), \
                patch.object(cli, "state_base", return_value=self.base), \
                patch.object(cli, "acquire_exclusive_file_lock", return_value=41), \
                patch.object(cli, "release_exclusive_file_lock"), \
                patch.object(coordinator, "github_client", return_value=self.client), \
                patch.object(coordinator, "read_public", side_effect=read_error), \
                patch.object(submission, "read_public"), \
                patch("sys.stdout", output), patch("sys.stderr", io.StringIO()):
            exit_code = cli.main(argv)
        report = json.loads(output.getvalue())
        if not isinstance(report, dict):
            raise AssertionError("CLI report is not an object")
        return exit_code, report


class CliRecoveryTests(ResumeFixture):
    def seed_upload_with_intent(self) -> None:
        self.seed_project()
        self.client.assets = [self.client.asset()]
        project = self.receipt.stage("project").result
        intent = {"name": self.receipt.invocation.proof_name, **project}
        result = {"asset_id": 71, "proof_sha256": project["proof_sha256"],
                  "size": project["size"], "name": self.receipt.invocation.proof_name}
        self.receipt.intend("upload", intent)
        self.receipt.complete("upload", result)

    def test_unbound_agy_model_on_completed_projection_blocks_before_submission(self) -> None:
        self.seed_project()
        with patch.object(coordinator, "native") as native:
            exit_code, report = self.run_resume(ReleaseError("agy_requested_model_unbound"))
        self.assertEqual((exit_code, report["status"], report["reason"]),
                         (2, "BLOCKED", "agy_requested_model_unbound"))
        loaded = Receipt.load(self.receipt.path)
        for name in ("upload", "dispatch", "watch"):
            self.assertEqual(loaded.stage(name).state, "pending")
        self.assertEqual(self.client.calls, [])
        native.assert_not_called()

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

    def run_resume_unsaved(self) -> tuple[int, dict[str, object]]:
        """Resume while every receipt save fails; the file must keep its seeded bytes."""
        durable = self.receipt.path.read_bytes()
        with patch.object(Receipt, "save", side_effect=OSError(errno.ENOSPC, "fixture receipt volume is full")):
            outcome = self.run_resume()
        self.assertEqual(self.receipt.path.read_bytes(), durable)
        return outcome

    def unsaved_report(self, status: str, fault: str) -> dict[str, object]:
        return {"status": status, "reason": "release_receipt_unavailable", "fault": fault,
                "receipt": str(self.receipt.path), "receipt_persisted": False, "invocation_id": "1" * 32}

    def test_an_unsaved_upload_reconciliation_fault_stays_incomplete_with_its_reason(self) -> None:
        self.seed_project()
        self.receipt.intend("upload", {
            "name": self.receipt.invocation.proof_name, **self.receipt.stage("project").result,
        })
        with patch.object(submission, "recheck_submission"), \
                patch.object(submission, "_asset_matches",
                             side_effect=VerificationError("github_request_failed", "github-api")), \
                patch.object(submission, "command") as command, \
                patch.object(coordinator, "native") as native:
            exit_code, report = self.run_resume_unsaved()
        self.assertEqual((exit_code, report), (3, self.unsaved_report(
            "INCOMPLETE", "submission_intent_unresolved_resume_required:github_request_failed")))
        self.assertEqual(Receipt.load(self.receipt.path).stage("upload").state, "intended")
        self.assertEqual(self.client.posts, 0)
        command.assert_not_called()
        native.assert_not_called()

    def test_an_unsaved_unintended_upload_error_stays_blocked_with_its_reason(self) -> None:
        self.seed_project()
        with patch.object(submission, "recheck_submission",
                          side_effect=VerificationError("github_request_failed", "github-api")), \
                patch.object(submission, "command") as command, \
                patch.object(coordinator, "native") as native:
            exit_code, report = self.run_resume_unsaved()
        self.assertEqual((exit_code, report), (2, self.unsaved_report("BLOCKED", "github_request_failed")))
        self.assertEqual(Receipt.load(self.receipt.path).stage("upload").state, "pending")
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


class UnknownControlTests(ResumeFixture):
    """A control child whose closure the canonical runner could not prove is never a definite refusal.

    Before native work the first harness Git command fails inside a fake bounded-command
    owner while the canonical Popen entry refuses and records every launch, so no real
    child exists. Only the canonical unproven-ownership codes are INCOMPLETE; a closed
    start, a GitHub refusal and a known failure after the projection keep their outcomes.
    """

    RUN = ["run", "--tag", TAG, "--opencode-model", "vendor/exact-model", "--ccr-model", "exact-alias"]

    def setUp(self) -> None:
        super().setUp()
        self.launches: list[tuple[object, ...]] = []

    def refuse_launch(self, *arguments: object, **_options: object) -> None:
        self.launches.append(arguments)
        raise AssertionError("a real child must not launch in a control-closure test")

    def run_with_first_command_failing(self, failure: CommandFailure, argv: list[str]) -> tuple[int, dict[str, object]]:
        with patch.object(commands, "run_bounded_command", side_effect=BoundedCommandError(failure)), \
                patch.object(operations.subprocess, "Popen", side_effect=self.refuse_launch), \
                patch.object(coordinator, "native") as native:
            outcome = self.run_cli(argv)
        native.assert_not_called()
        self.assertEqual((self.launches, self.client.calls), ([], []))
        return outcome

    def assertBeforeNative(self, outcome: tuple[int, dict[str, object]], status: str, reason: str) -> None:
        exit_code, report = outcome
        self.assertEqual((exit_code, report["status"], report["reason"]),
                         ({"BLOCKED": 2, "INCOMPLETE": 3}[status], status, reason))
        stored = Receipt.load(self.receipt.path)
        self.assertEqual((stored.status, stored.reason), (status, reason))
        self.assertEqual((stored.stage("preflight").state, stored.stage("native").state), ("intended", "pending"))

    def assert_unknown_control_is_incomplete(self, failure: CommandFailure) -> None:
        self.assertBeforeNative(self.run_with_first_command_failing(failure, self.RUN), "INCOMPLETE", failure.value)
        resumed = self.run_with_first_command_failing(failure, ["resume", "--receipt", str(self.receipt.path)])
        self.assertEqual((resumed[0], resumed[1]["status"], resumed[1]["reason"]),
                         (3, "INCOMPLETE", "pre_native_invocation_incomplete_no_rerun"))

    def test_a_timed_out_harness_command_is_incomplete_and_never_reruns(self) -> None:
        self.assert_unknown_control_is_incomplete(CommandFailure.TIMEOUT)

    def test_a_harness_command_whose_termination_timed_out_is_incomplete(self) -> None:
        self.assert_unknown_control_is_incomplete(CommandFailure.TERMINATION_TIMEOUT)

    def test_a_harness_command_whose_termination_failed_is_incomplete(self) -> None:
        self.assert_unknown_control_is_incomplete(CommandFailure.TERMINATION_FAILED)

    def test_a_harness_command_without_its_capture_is_incomplete(self) -> None:
        self.assert_unknown_control_is_incomplete(CommandFailure.CAPTURE_UNAVAILABLE)

    def test_a_harness_command_that_never_started_stays_blocked(self) -> None:
        self.assertBeforeNative(self.run_with_first_command_failing(CommandFailure.START_FAILED, self.RUN),
                                "BLOCKED", "command_start_failed")

    def test_a_github_404_before_native_stays_blocked(self) -> None:
        self.client.repo_inaccessible = True
        with patch.object(preflight, "observe_harness", return_value=(HEAD, SOURCE, True)), \
                patch.object(coordinator, "native") as native:
            outcome = self.run_cli(self.RUN)
        self.assertBeforeNative(outcome, "BLOCKED", "github_request_failed")
        native.assert_not_called()

    def test_an_unsaved_unknown_control_outcome_is_incomplete_and_keeps_the_durable_intent(self) -> None:
        save = Receipt.save
        durable: list[bytes] = []

        def full_after_the_preflight_intent(receipt: Receipt, *, create: bool = False) -> None:
            if durable:
                raise OSError(errno.ENOSPC, "fixture receipt volume is full")
            save(receipt, create=create)
            if receipt.stage("preflight").state == "intended":
                durable.append(receipt.path.read_bytes())

        with patch.object(Receipt, "save", full_after_the_preflight_intent):
            exit_code, report = self.run_with_first_command_failing(CommandFailure.TIMEOUT, self.RUN)
        self.assertEqual((exit_code, report), (3, {
            "status": "INCOMPLETE", "reason": "release_receipt_unavailable", "fault": "command_timeout",
            "receipt": str(self.receipt.path), "receipt_persisted": False, "invocation_id": "1" * 32}))
        self.assertEqual([self.receipt.path.read_bytes()], durable)
        self.assertEqual(Receipt.load(self.receipt.path).stage("preflight").state, "intended")

    def test_an_unknown_control_after_the_projection_is_incomplete_without_any_effect(self) -> None:
        self.seed_project()
        with patch.object(submission, "recheck_submission",
                          side_effect=VerificationError("command_timeout", "github-api")), \
                patch.object(submission, "command") as command, \
                patch.object(coordinator, "native") as native:
            exit_code, report = self.run_resume()
        self.assertEqual((exit_code, report["status"], report["reason"]), (3, "INCOMPLETE", "command_timeout"))
        self.assertEqual(Receipt.load(self.receipt.path).stage("upload").state, "pending")
        self.assertEqual(self.client.posts, 0)
        command.assert_not_called()
        native.assert_not_called()


class ReleaseRequestEncodingTests(ReleaseFixture):
    LEGACY_KEYS = {"repo", "tag", "opencode_model", "ccr_model", "no_submit"}

    def test_legacy_request_parses_to_native_default_and_round_trips(self) -> None:
        legacy = self.receipt.invocation.request.document()
        self.assertEqual(set(legacy), self.LEGACY_KEYS)
        parsed = ReleaseRequest.parse(legacy)
        self.assertIsNone(parsed.agy_model)
        self.assertEqual(parsed, self.receipt.invocation.request)
        self.assertEqual(parsed.document(), legacy)
        metadata = json.loads(self.receipt.path.read_bytes())["metadata"]
        self.assertEqual(metadata["request"], legacy)
        loaded = Receipt.load(self.receipt.path)
        self.assertIsNone(loaded.invocation.request.agy_model)
        loaded.report("INCOMPLETE", "legacy_request_reloaded")
        self.assertEqual(json.loads(self.receipt.path.read_bytes())["metadata"], metadata)

    def test_agy_model_has_one_non_null_encoding(self) -> None:
        legacy = self.receipt.invocation.request.document()
        requested = ReleaseRequest.parse({**legacy, "agy_model": "vendor/agy-exact"})
        self.assertEqual(requested.agy_model, "vendor/agy-exact")
        self.assertEqual(requested.document(), {**legacy, "agy_model": "vendor/agy-exact"})
        for invalid in (None, "", "bad model", "-leading", "x" * 129, 7):
            with self.subTest(agy_model=invalid), self.assertRaises(ReleaseError):
                ReleaseRequest.parse({**legacy, "agy_model": invalid})
        with self.assertRaisesRegex(ReleaseError, "release_request_invalid"):
            ReleaseRequest.parse({**legacy, "agy_model": "vendor/agy-exact", "unknown": True})
        with self.assertRaisesRegex(ReleaseError, "release_record_text_invalid"):
            ReleaseRequest(repo=REPO, tag=TAG, opencode_model="vendor/exact-model",
                           ccr_model=None, no_submit=False, agy_model="bad model")

    def test_the_required_opencode_model_refuses_at_construction_as_its_parser_does(self) -> None:
        legacy = self.receipt.invocation.request.document()
        with self.assertRaisesRegex(ReleaseError, "release_record_text_invalid"):
            ReleaseRequest.parse({**legacy, "opencode_model": None})
        with self.assertRaisesRegex(ReleaseError, "release_record_text_invalid"):
            ReleaseRequest(repo=REPO, tag=TAG, opencode_model=None, ccr_model=None, no_submit=True)
        for ccr, agy in ((None, None), ("exact-alias", None), (None, "vendor/agy-exact"),
                         ("exact-alias", "vendor/agy-exact")):
            with self.subTest(ccr=ccr, agy=agy):
                request = ReleaseRequest(repo=REPO, tag=TAG, opencode_model="vendor/exact-model",
                                         ccr_model=ccr, no_submit=True, agy_model=agy)
                self.assertEqual(ReleaseRequest.parse(request.document()), request)

    def test_release_kind_stages_placement_and_lock_bytes_are_unchanged(self) -> None:
        stored = json.loads(self.receipt.path.read_bytes())
        legacy_stages = ["preflight", "download", "archive", "native", "finalize", "project",
                         "upload", "dispatch", "watch", "cleanup"]
        self.assertEqual((stored["kind"], set(stored["stages"]), list(STAGES)),
                         ("local-native-release", set(legacy_stages), legacy_stages))
        request = self.receipt.invocation.request
        self.assertEqual(invocation_name(request, "1" * 32), f"{TAG}-{'1' * 32}")
        self.assertEqual(lock_name(request), ".8f4a340fb6f75423d11c7cad2f1a5fc24dc231339f0bf32edd32edcdf1a8869f.lock")

    def test_cli_passes_agy_model_into_the_created_request(self) -> None:
        created: list[ReleaseRequest] = []

        def capture(_checkout: object, request: ReleaseRequest, _argv: list[str]) -> Receipt:
            created.append(request)
            return self.receipt

        argv = ["run", "--tag", TAG, "--opencode-model", "vendor/exact-model"]
        with patch.object(cli, "state_base", return_value=self.base), \
                patch.object(cli, "acquire_exclusive_file_lock", return_value=41), \
                patch.object(cli, "release_exclusive_file_lock"), \
                patch.object(cli, "create_receipt", side_effect=capture), \
                patch.object(cli, "_execute", return_value=None), patch.object(cli, "_report", return_value=0), \
                patch("sys.stderr", io.StringIO()):
            for options in ((), ("--agy-model", "vendor/agy-exact")):
                cli._locked(cli.parser().parse_args([*argv, *options]), [*argv, *options])
        self.assertEqual([request.agy_model for request in created], [None, "vendor/agy-exact"])


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


class ChildCompletionDispositionTests(ResumeFixture):
    """The resume command over real finite children: unproven completion is never a plain block."""

    CHILDREN = ("archive", "native", "finalize")

    def setUp(self) -> None:
        super().setUp()
        self.script = self.checkout / "finite.py"
        self.script.write_text("raise SystemExit(0)\n", encoding="utf-8")

    def run_children(self, names: tuple[str, ...]) -> None:
        self.create_runner()
        for name in names:
            result = self.run_finite_child(name, [str(Path(sys.executable).resolve()), str(self.script)])
            self.receipt.complete(name, {**result, "lifetime_closed": True} if name == "native" else result)

    def seed_after_project(self) -> None:
        self.seed_project()
        self.run_children(self.CHILDREN)
        self.client.assets = [self.client.asset()]
        for name, result in (("upload", {"asset_id": 71}), ("dispatch", {"run_id": 81}),
                             ("watch", {"promotable": True})):
            self.complete(name, result)

    def resume(self) -> tuple[int, dict[str, object]]:
        with patch.object(execution, "Processes", side_effect=AssertionError("resume launched a child")), \
                patch.object(coordinator, "upload"):
            return self.run_resume()

    def cleanup_state(self) -> str:
        return Receipt.load(self.receipt.path).stage("cleanup").state

    def test_after_projection_valid_children_complete_cleanup(self) -> None:
        self.seed_after_project()
        exit_code, report = self.resume()
        self.assertEqual((exit_code, report["status"]), (0, "PROMOTABLE"))
        self.assertEqual(self.cleanup_state(), "complete")
        self.assertFalse((self.root / "runner").exists())

    def assert_after_projection_incomplete(self, record: str, change, reason: str) -> None:
        self.seed_after_project()
        change(self.root / "runner" / "native-processes" / "001-native" / f"{record}.json")
        exit_code, report = self.resume()
        self.assertEqual((exit_code, report["status"], report["reason"]), (3, "INCOMPLETE", reason))
        self.assertEqual(self.cleanup_state(), "pending")
        self.assertTrue((self.root / "runner" / "native-processes" / "001-native").is_dir())

    def test_after_projection_null_completed_record_is_incomplete_and_retains_the_runner(self) -> None:
        self.assert_after_projection_incomplete(
            "completed", lambda path: path.write_text("null", encoding="utf-8"),
            "release_record_object_invalid")

    def test_after_projection_boolean_pid_is_incomplete_and_retains_the_runner(self) -> None:
        def boolean_pid(path: Path) -> None:
            value = json.loads(path.read_text(encoding="utf-8"))
            value["pid"] = True
            path.write_text(json.dumps(value), encoding="utf-8")

        self.assert_after_projection_incomplete("completed", boolean_pid, "release_record_integer_invalid")

    def test_after_projection_missing_started_record_is_incomplete_and_retains_the_runner(self) -> None:
        self.assert_after_projection_incomplete(
            "started", lambda path: path.unlink(), "child_natural_completion_unproven")

    def test_before_projection_unproven_native_completion_stays_incomplete(self) -> None:
        self.complete("preflight", self.preflight_facts())
        self.run_children(("archive",))
        self.run_finite_child("native", [str(Path(sys.executable).resolve()), str(self.script)])
        record = self.root / "runner" / "native-processes" / "001-native" / "completed.json"
        record.write_text("null", encoding="utf-8")
        exit_code, report = self.resume()
        self.assertEqual((exit_code, report["status"]), (3, "INCOMPLETE"))
        self.assertEqual(Receipt.load(self.receipt.path).stage("project").state, "pending")
        self.assertEqual(self.receipt.stage("native").state, "intended")
        self.assertTrue(record.is_file())


if __name__ == "__main__":
    unittest.main()
