"""Preflight failures, exact input forwarding, and no paid relaunch on recovery."""

from __future__ import annotations

import contextlib
from dataclasses import FrozenInstanceError, asdict, replace
import hashlib
import io
import json
import os
from pathlib import Path
import sys
from unittest.mock import Mock, patch
import unittest

from scripts.acceptance_tests.release_fixture import open_descriptors
from scripts.delegate_acceptance.evidence.bounded_files import BoundedReadError, ReadFailure
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.native_release import cli, coordinator, execution, preflight
from scripts.delegate_acceptance.native_release.contract import Invocation, ReleaseError
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.delegate_acceptance.native.acceptance_discovery import require_discovery
from scripts.delegate_acceptance.native_evidence.aggregate import build_aggregate
from scripts.delegate_acceptance.native_evidence.contract import MATRIX_HEADER, expected_cases
from scripts.delegate_acceptance.native_evidence.directories import prepare_case_directory
from scripts.delegate_acceptance.native_evidence.public_contract import NativeCompletion, PublicProofError
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.release_verification import commands
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_native_candidate_support import git, harness_checkout, observed_commands
from scripts.test_native_release_support import HEAD, RELEASE, SOURCE, ReleaseFixture


class PreflightTests(ReleaseFixture):
    def harness(self) -> tuple[Path, str]:
        """This test's real harness checkout and its HEAD, which GitHub reports as protected main."""
        checkout = harness_checkout(self.tree)
        head = git(self.tree, checkout, "rev-parse", "HEAD").decode().strip()
        self.client.branch_revision = head
        return checkout, head

    def invocation(self, checkout: Path, *, no_submit: bool) -> Invocation:
        return replace(self.receipt.invocation, checkout=checkout,
                       request=replace(self.receipt.invocation.request, no_submit=no_submit))

    def preflight(self, checkout: Path, *, no_submit: bool) -> dict[str, object]:
        """Run the real preflight; each harness Git child reports its original failure to the tree."""
        with patch.object(preflight, "resolve_release_identity", return_value={"commit_sha": RELEASE}), \
                observed_commands(self.tree):
            return preflight.preflight(self.invocation(checkout, no_submit=no_submit), self.client)

    def test_github_404_stops_before_archive_or_paid_work(self) -> None:
        self.client.repo_inaccessible = True
        with patch.object(coordinator, "github_client", return_value=self.client), \
                patch.object(preflight, "observe_harness", return_value=(HEAD, SOURCE, True)), \
                patch.object(coordinator, "download") as download, patch.object(coordinator, "native") as native:
            cli._execute(self.receipt, recovery=False)
        self.assertEqual(Receipt.load(self.receipt.path).status, "BLOCKED")
        self.assertEqual(self.receipt.reason, "github_request_failed")
        download.assert_not_called()
        native.assert_not_called()
        self.assertEqual(self.receipt.stage("native").state, "pending")

    def test_python_floor_precedes_github_or_paid_work(self) -> None:
        with patch.object(preflight.sys, "version_info", (3, 11, 9)):
            with self.assertRaisesRegex(ReleaseError, "python_3_12_required"):
                preflight.preflight(self.receipt.invocation, self.client)
        self.assertEqual(self.client.calls, [])

    def test_candidate_is_local_only_and_cannot_be_submitted(self) -> None:
        checkout, head = self.harness()
        self.assertFalse(self.preflight(checkout, no_submit=False)["candidate_harness"])
        self.client.branch_revision = "f" * 40
        with self.assertRaisesRegex(ReleaseError, "clean_current_main_harness"):
            self.preflight(checkout, no_submit=False)
        facts = self.preflight(checkout, no_submit=True)
        self.assertTrue(facts["candidate_harness"])
        self.assertEqual(facts["harness_revision"], head)
        self.assertEqual(facts["actor_id"], 61)

    def test_a_replaced_head_commit_cannot_hide_uncommitted_harness_bytes_from_submission(self) -> None:
        checkout, executing_head = self.harness()
        relative = "scripts/native_acceptance_evidence.py"
        target = checkout / relative
        executing = target.read_bytes()
        target.write_bytes(executing + b"\n# committed fixture-only change\n")
        git(self.tree, checkout, "commit", "--quiet", "--no-verify", "--all", "--message=other bytes")
        head = git(self.tree, checkout, "rev-parse", "HEAD").decode().strip()
        target.write_bytes(executing)
        git(self.tree, checkout, "add", "--", relative)
        # HEAD now commits other bytes; the replace ref presents the executing commit in its place.
        git(self.tree, checkout, "replace", head, executing_head)
        self.client.branch_revision = head
        facts = self.preflight(checkout, no_submit=True)
        self.assertEqual((facts["candidate_harness"], facts["harness_revision"]), (True, head))
        with self.assertRaisesRegex(ReleaseError, "submission_requires_clean_current_main_harness"):
            self.preflight(checkout, no_submit=False)
        self.assertEqual([call[1] for call in self.client.calls if call[0] == "POST"], [])

    def test_a_failed_harness_observation_is_never_a_local_only_candidate(self) -> None:
        with self.assertRaises(VerificationError) as caught:
            self.preflight(self.checkout, no_submit=True)
        self.assertEqual((caught.exception.code, caught.exception.stage), ("command_failed", "git-harness-head"))
        self.assertEqual(self.client.calls, [])

    def test_an_unproven_harness_git_closure_reaches_the_tree_before_conversion(self) -> None:
        checkout, _ = self.harness()
        failure = BoundedCommandError(CommandFailure.TIMEOUT)
        self.launches: list[tuple[object, ...]] = []
        self.zero_start = False
        self.addCleanup(self.remove_after_zero_starts)
        with patch.object(commands, "run_bounded_command", side_effect=failure) as owner, \
                patch.object(operations.subprocess, "Popen", side_effect=self.refuse_launch):
            try:
                with self.assertRaises(VerificationError) as caught:
                    self.preflight(checkout, no_submit=True)
            finally:
                self.zero_start = owner.call_count == 1 and not self.launches
        self.assertEqual((caught.exception.code, caught.exception.stage), ("command_timeout", "git-harness-head"))
        self.assertIs(self.tree.unresolved, failure)
        self.assertEqual((self.launches, self.client.calls), ([], []))

    def refuse_launch(self, *arguments: object, **_options: object) -> None:
        self.launches.append(arguments)
        raise AssertionError("a real child must not launch in a closure control")

    def remove_after_zero_starts(self) -> None:
        """Remove a retained tree only after the fake owner raised before any real launch."""
        report = io.StringIO()
        with contextlib.redirect_stderr(report):
            retained = not self.tree.dispose()
        if retained and not self.zero_start:
            raise self.failureException(f"a child may have started: {report.getvalue()}")
        if retained:
            self.tree.remove_retained()

    def test_github_client_uses_explicit_stored_configuration_mode(self) -> None:
        with patch.object(coordinator, "gh_environment", return_value={"HOME": str(Path.home())}) as environment:
            coordinator.github_client()
        environment.assert_called_once_with(auth_mode="local-config")

    def test_published_release_is_read_only_candidate_input_only(self) -> None:
        checkout, _ = self.harness()
        self.client.release_draft = False
        self.assertEqual(self.preflight(checkout, no_submit=True)["release_revision"], RELEASE)
        with self.assertRaisesRegex(ReleaseError, "exact_draft_release_required"):
            self.preflight(checkout, no_submit=False)

    def test_missing_and_supplied_models_override_inherited_alias_exactly(self) -> None:
        original_home = os.environ.get("HOME")
        with patch.dict(os.environ, {"CCR_LIVE_MODEL": "wrong-inherited-default", "RUNNER_TEMP": "/tmp/wrong"}):
            absent = replace(self.receipt.invocation,
                             request=replace(self.receipt.invocation.request, ccr_model=None))
            env = execution.child_environment(absent)
            self.assertEqual(env["CCR_LIVE_MODEL"], "")
            self.assertEqual(env["OPENCODE_LIVE_MODEL"], "vendor/exact-model")
            self.assertEqual(env.get("HOME"), original_home)
            self.assertEqual(env["RUNNER_TEMP"], str(self.root / "runner"))
            self.assertEqual(os.environ["RUNNER_TEMP"], "/tmp/wrong")
            supplied = execution.child_environment(self.receipt.invocation)
            self.assertEqual(supplied["CCR_LIVE_MODEL"], "exact-alias")

    def test_agy_model_overrides_inherited_alias_without_parent_mutation(self) -> None:
        inherited = {"AGY_LIVE_MODEL": "wrong-inherited-agy", "PATH": "/usr/bin:/bin"}
        with patch.dict(os.environ, inherited):
            omitted = execution.child_environment(self.receipt.invocation)
            self.assertIsNone(self.receipt.invocation.request.agy_model)
            self.assertEqual(omitted["AGY_LIVE_MODEL"], "")
            requested = replace(self.receipt.invocation,
                                request=replace(self.receipt.invocation.request,
                                                agy_model="vendor/agy-exact"))
            supplied = execution.child_environment(requested)
            self.assertEqual(supplied["AGY_LIVE_MODEL"], "vendor/agy-exact")
            self.assertEqual({key: value for key, value in supplied.items() if key != "AGY_LIVE_MODEL"},
                             {key: value for key, value in omitted.items() if key != "AGY_LIVE_MODEL"})
            self.assertEqual(supplied["PATH"], str(self.receipt.invocation.assets / "extracted")
                             + os.pathsep + "/usr/bin:/bin")
            self.assertEqual(os.environ["AGY_LIVE_MODEL"], "wrong-inherited-agy")
            self.assertEqual(os.environ["PATH"], "/usr/bin:/bin")


class ExecutionTests(ReleaseFixture):
    def test_child_path_discovers_the_verified_supervisor_pair(self) -> None:
        selected = self.receipt.invocation.assets / "extracted"
        selected.mkdir(parents=True)
        for name in ("pueue", "pueued"):
            executable = selected / name
            executable.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            executable.chmod(0o755)
        original = os.environ.get("PATH", os.defpath)
        environment = execution.child_environment(self.receipt.invocation)
        self.assertEqual(environment["PATH"], str(selected) + os.pathsep + original)
        self.assertEqual(os.environ.get("PATH", os.defpath), original)
        for name in ("pueue", "pueued"):
            require_discovery(name, (selected / name).resolve(strict=True), environment)

    def test_download_records_private_stage_identity(self) -> None:
        self.complete("preflight", self.preflight_facts())
        with patch.object(execution, "command", return_value=b"") as command:
            execution.download(self.receipt, self.client)
        stage = self.root / "private-stage"
        stage_info, parent_info = stage.lstat(), self.root.lstat()
        self.assertEqual(self.receipt.stage("download").result["stage_identity"], {
            "path": str(stage), "parent": str(self.root),
            "device": stage_info.st_dev, "inode": stage_info.st_ino,
            "parent_device": parent_info.st_dev, "parent_inode": parent_info.st_ino,
        })
        self.assertTrue((self.root / "runner" / "delegation-layer-release").is_dir())
        command.assert_called_once()

    def test_existing_private_stage_is_preserved_and_fresh_download_stops(self) -> None:
        self.complete("preflight", self.preflight_facts())
        stage = self.root / "private-stage"
        stage.mkdir()
        (stage / "keep").write_text("owned recovery evidence")
        with patch.object(execution, "command") as command:
            with self.assertRaisesRegex(ReleaseError, "private_stage_identity_unavailable") as error:
                execution.download(self.receipt, self.client)
        self.assertTrue(error.exception.incomplete)
        command.assert_not_called()
        self.assertEqual((stage / "keep").read_text(), "owned recovery evidence")
        self.assertEqual(self.receipt.stage("download").state, "intended")

    def test_download_identity_publication_crash_blocks_native_resume(self) -> None:
        self.complete("preflight", self.preflight_facts())
        with patch.object(execution, "command", return_value=b"") as command, \
                patch.object(self.receipt, "complete", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                execution.download(self.receipt, self.client)
        command.assert_called_once()
        self.assertTrue((self.root / "private-stage").is_dir())
        self.assertEqual(self.receipt.stage("download").state, "intended")
        recovered = Receipt.load(self.receipt.path)
        with patch.object(coordinator, "download") as download, \
                patch.object(coordinator, "archive") as archive, \
                patch.object(coordinator, "native") as native:
            cli._execute(recovered, recovery=True)
        download.assert_not_called()
        archive.assert_not_called()
        native.assert_not_called()
        self.assertEqual(recovered.status, "INCOMPLETE")
        self.assertEqual(recovered.reason, "pre_native_invocation_incomplete_no_rerun")

    def native_result(self) -> dict[str, object]:
        return {"exit_code": 0, "aggregate_sha256": "d" * 64,
                "status": "PASS", "lifetime_closed": True}

    def finish_native_finalizer(self, receipt: Receipt, name: str, argv: list[str],
                                env: dict[str, str], *, timeout: int) -> dict[str, object]:
        self.assertEqual(name, "finalize")
        receipt.intend(name, execution.child_intent(receipt, name, argv))
        aggregate_sha = receipt.stage("native").result["aggregate_sha256"]
        write_json(receipt.invocation.native_receipt, {
            "native_acceptance": {"aggregate_receipt_sha256": aggregate_sha},
            "verdict": "pass",
        })
        return {"exit_code": 0, "completion_sha256": "e" * 64}

    def test_exit_publication_failure_resumes_finalization_without_native_launch(self) -> None:
        self.complete("preflight", self.preflight_facts())
        self.complete("download", {"stage_identity": {}})
        self.complete("archive", {})
        native_result = self.native_result()
        self.complete("native", native_result)
        exit_file = self.root / "native-exit-code.txt"
        with patch.object(execution, "write_bytes", side_effect=OSError("before publish")), \
                patch.object(execution, "run_child") as child:
            with self.assertRaisesRegex(OSError, "before publish"):
                execution.finalize(self.receipt, {})
        child.assert_not_called()
        self.assertFalse(exit_file.exists())

        def complete_project(receipt: Receipt) -> None:
            receipt.intend("project", {"fixture": "complete"})
            receipt.complete("project", {"case_results_verdict": "PASS"})

        recovered = Receipt.load(self.receipt.path)
        with patch.object(coordinator, "observe_native", return_value=native_result) as observe, \
                patch.object(execution, "run_child", side_effect=self.finish_native_finalizer) as child, \
                patch.object(coordinator, "project", side_effect=complete_project), \
                patch.object(coordinator, "submit"), \
                patch.object(coordinator, "native") as native, \
                patch.object(coordinator, "archive") as archive, \
                patch.object(coordinator, "download") as download, \
                patch.object(coordinator, "github_client", return_value=self.client):
            coordinator.resume(recovered)
        observe.assert_called_once()
        child.assert_called_once()
        self.assertEqual(child.call_args.args[1], "finalize")
        native.assert_not_called()
        archive.assert_not_called()
        download.assert_not_called()
        self.assertEqual(exit_file.read_bytes(), b"0\n")
        self.assertEqual(recovered.stage("finalize").state, "complete")

    def test_finalize_accepts_matching_exit_file_without_overwrite(self) -> None:
        self.complete("preflight", self.preflight_facts())
        self.complete("native", self.native_result())
        exit_file = self.root / "native-exit-code.txt"
        exit_file.write_bytes(b"0\n")
        original = exit_file.stat()
        with patch.object(execution, "run_child", side_effect=self.finish_native_finalizer):
            execution.finalize(self.receipt, {})
        current = exit_file.stat()
        self.assertEqual(exit_file.read_bytes(), b"0\n")
        self.assertEqual((current.st_ino, current.st_mtime_ns),
                         (original.st_ino, original.st_mtime_ns))

    def test_finalize_rejects_different_exit_file_bytes(self) -> None:
        self.complete("preflight", self.preflight_facts())
        self.complete("native", self.native_result())
        exit_file = self.root / "native-exit-code.txt"
        exit_file.write_bytes(b"1\n")
        with patch.object(execution, "run_child") as child:
            with self.assertRaisesRegex(ReleaseError, "native_exit_readback_conflict"):
                execution.finalize(self.receipt, {})
        child.assert_not_called()
        self.assertEqual(exit_file.read_bytes(), b"1\n")

    def test_real_control_child_observes_durable_intent_and_natural_wait(self) -> None:
        self.create_runner()
        note = self.root / "observed.json"
        script = self.checkout / "finite.py"
        script.write_text(
            "import json, pathlib, sys\n"
            "receipt = json.loads(pathlib.Path(sys.argv[1]).read_bytes())\n"
            "pathlib.Path(sys.argv[2]).write_text(receipt['stages']['native']['state'])\n"
            "raise SystemExit(2)\n", encoding="utf-8")
        result = self.run_finite_child("native", [
            str(Path(sys.executable).resolve()), str(script), str(self.receipt.path), str(note),
        ])
        self.assertEqual(note.read_text(), "intended")
        self.assertEqual(result["exit_code"], 2)
        self.assertEqual(result, execution.observe_child(Receipt.load(self.receipt.path), "native"))
        invocation = execution.read_object(self.root / "runner/native-processes/001-native/invocation.json")
        self.assertFalse(invocation["environment_values_in_record"])

    def test_interrupt_after_native_intent_is_incomplete_and_resume_never_launches(self) -> None:
        self.create_runner()
        self.complete("preflight", self.preflight_facts())
        self.complete("archive", {})
        with patch.object(execution, "Processes", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                execution.run_child(self.receipt, "native", [str(Path(sys.executable).resolve()), "unused"], {}, timeout=1)
        recovered = Receipt.load(self.receipt.path)
        with patch.object(coordinator, "native") as native, patch.object(coordinator, "archive") as archive, \
                patch.object(coordinator, "download") as download, patch.object(coordinator, "github_client") as github:
            cli._execute(recovered, recovery=True)
        self.assertEqual(recovered.status, "INCOMPLETE")
        native.assert_not_called()
        archive.assert_not_called()
        download.assert_not_called()
        github.assert_not_called()
        self.assertEqual(recovered.stage("native").state, "intended")

    def test_keyboard_interrupt_keeps_incomplete_disposition(self) -> None:
        with patch.object(coordinator, "execute", side_effect=KeyboardInterrupt), \
                patch.object(cli, "execute", side_effect=KeyboardInterrupt):
            cli._execute(self.receipt, recovery=False)
        self.assertEqual(Receipt.load(self.receipt.path).status, "INCOMPLETE")

    def test_failed_native_can_complete_readback_but_unproven_lifetime_cannot(self) -> None:
        self.complete("preflight", self.preflight_facts())
        compact = self.root / "case.json"
        write_json(compact, {"cleanup": {"observation": {"status": "unverified", "raw_process_captures": "retained"}},
                             "source_proof": {"lifecycle_proof": {"state": "unresolved", "supervisor_shutdown_observed": True}}})
        aggregate = {"exit_code": 1, "status": "FAIL", "cases": [{"receipt_path": str(compact)}]}
        with patch.object(execution, "observe_child", return_value={"exit_code": 1}), \
                patch.object(execution, "validate_aggregate_receipt", return_value=(aggregate, "d" * 64)):
            result = execution.observe_native(self.receipt)
            self.assertEqual(result["status"], "FAIL")
            self.assertTrue(result["lifetime_closed"])
            write_json(compact, {"cleanup": {"observation": {"status": "verified"}},
                                 "source_proof": {"lifecycle_proof": {"state": "unresolved", "supervisor_shutdown_observed": False}}}, replace=True)
            with self.assertRaisesRegex(ReleaseError, "native_ownership_unproven"):
                execution.observe_native(self.receipt)

    def test_pass_requires_the_checked_complete_lifecycle_not_daemon_shutdown(self) -> None:
        self.complete("preflight", self.preflight_facts())
        compact = self.root / "pass-case.json"
        aggregate = {"exit_code": 0, "status": "PASS",
                     "cases": [{"status": "PASS", "receipt_path": str(compact)}]}
        with patch.object(execution, "observe_child", return_value={"exit_code": 0}), \
                patch.object(execution, "validate_aggregate_receipt", return_value=(aggregate, "d" * 64)):
            for state in ("unresolved", None):
                with self.subTest(state=state):
                    write_json(compact, {"cleanup": {"observation": {"status": "verified"}},
                                         "source_proof": {"lifecycle_proof": {
                                             "state": state, "supervisor_shutdown_observed": True}}},
                               replace=True)
                    with self.assertRaisesRegex(ReleaseError, "native_ownership_unproven"):
                        execution.observe_native(self.receipt)
            write_json(compact, {"cleanup": {"observation": {"status": "verified"}},
                                 "source_proof": {"lifecycle_proof": {
                                     "state": "complete", "supervisor_shutdown_observed": True}}},
                       replace=True)
            result = execution.observe_native(self.receipt)
        self.assertEqual((result["status"], result["lifetime_closed"]), ("PASS", True))

    def test_canonical_failed_aggregate_retains_raw_after_lifetime_closure(self) -> None:
        self.complete("preflight", self.preflight_facts())
        root = self.receipt.invocation.aggregate.parent
        root.mkdir()
        (root / "cases").mkdir()
        lines = [MATRIX_HEADER]
        failed_directory = root
        for index, case in enumerate(expected_cases()):
            directory = prepare_case_directory(root, case.case_id)
            failed = index == 0
            write_json(directory / "failure.json", {
                "gate": "acceptance-native", "status": "failed" if failed else "BLOCKED",
                "provider": case.provider, "mode": case.permission, "scenario": case.scenario,
                "requested_model": None, "requested_effort": None,
            })
            if failed:
                failed_directory = directory
                (directory / "private-output").write_text("retained fixture evidence")
                write_json(directory / "cleanup.json", {
                    "status": "partial", "supervisor_shutdown_observed": True,
                    "raw_process_captures": "retained",
                })
            lines.append("\t".join((case.case_id, case.provider, case.permission, case.scenario,
                                     "1" if failed else "2", "FAIL" if failed else "BLOCKED",
                                     case.case_output)) + "\n")
        matrix = root / "case-matrix.tsv"
        matrix.write_text("".join(lines), encoding="ascii")
        aggregate, code = build_aggregate(root, matrix, self.receipt.invocation.aggregate,
                                         tag="v1.2.3", source_revision=HEAD, require_pass=False)
        self.assertEqual((aggregate["status"], code), ("FAIL", 1))
        with patch.object(execution, "observe_child", return_value={"exit_code": 1}):
            result = execution.observe_native(self.receipt)
        self.assertTrue(result["lifetime_closed"])
        self.assertEqual(result["status"], "FAIL")
        self.assertTrue((failed_directory / "private-output").is_file())


class ChildObservationFixture(ReleaseFixture):
    """Real finite children run by the canonical owner, then observed from their records."""

    def setUp(self) -> None:
        super().setUp()
        self.create_runner()
        self.script = self.checkout / "finite.py"
        self.script.write_text("import sys\nraise SystemExit(int(sys.argv[1]))\n", encoding="utf-8")

    def child(self, name: str = "native", code: int = 2) -> dict[str, object]:
        return self.run_finite_child(
            name, [str(Path(sys.executable).resolve()), str(self.script), str(code)])

    def records(self, name: str = "native") -> dict[str, Path]:
        directory = self.root / "runner" / f"{name}-processes" / f"001-{name}"
        return {record: directory / f"{record}.json"
                for record in ("completed", "started", "invocation")}

    def refused(self, name: str = "native") -> ReleaseError:
        with patch.object(execution, "Processes", side_effect=AssertionError("observation launched")):
            with self.assertRaises(ReleaseError) as caught:
                execution.observe_child_completion(self.receipt, name)
        self.assertTrue(caught.exception.incomplete)
        return caught.exception


class ChildCompletionTests(ChildObservationFixture):
    def test_finite_child_yields_an_immutable_completion_and_the_exact_legacy_projection(self) -> None:
        legacy = self.child(code=2)
        raw = {record: path.read_bytes() for record, path in self.records().items()}
        with patch.object(execution, "Processes", side_effect=AssertionError("observation launched")):
            completion = execution.observe_child_completion(self.receipt, "native")
            again = execution.observe_child(self.receipt, "native")
        def sha(data: bytes) -> str:
            return hashlib.sha256(data).hexdigest()
        self.assertEqual(completion, NativeCompletion(
            invocation_id="1" * 32, natural_wait=True, exit_code=2,
            started_sha256=sha(raw["started"]), invocation_sha256=sha(raw["invocation"]),
            completed_sha256=sha(raw["completed"])))
        self.assertEqual(legacy, {"exit_code": 2, "completion_sha256": sha(raw["completed"])})
        self.assertEqual(again, legacy)
        self.assertEqual(list(legacy), ["exit_code", "completion_sha256"])
        with self.assertRaises(FrozenInstanceError):
            completion.exit_code = 0
        self.assertEqual(set(asdict(completion)), {
            "invocation_id", "natural_wait", "exit_code",
            "started_sha256", "invocation_sha256", "completed_sha256"})
        self.assertNotIn(str(self.checkout), json.dumps(asdict(completion)))

    def test_each_record_is_read_once_and_the_digest_binds_the_validated_bytes(self) -> None:
        self.child(code=2)
        real = execution.read_bounded_regular_file
        reads: list[str] = []
        validated: list[bytes] = []

        def spy(path: Path, limit: int) -> bytes:
            data = real(path, limit)
            reads.append(path.name)
            if path.name == "completed.json":
                validated.append(data)
                path.write_bytes(data.replace(b'"exit_code": 2', b'"exit_code": 3'))
            return data

        with patch.object(execution, "read_bounded_regular_file", spy):
            completion = execution.observe_child_completion(self.receipt, "native")
        self.assertEqual(reads, ["completed.json", "started.json", "invocation.json"])
        self.assertEqual(completion.exit_code, 2)
        self.assertEqual(completion.completed_sha256, hashlib.sha256(validated[0]).hexdigest())
        self.assertNotEqual(self.records()["completed"].read_bytes(), validated[0])

    def mutated(self, record: str, change) -> ReleaseError:
        """Apply one change to a finished child's records, observe, then restore the records."""
        saved = {key: path.read_bytes() for key, path in self.records().items()}
        try:
            change(self.records()[record])
            return self.refused()
        finally:
            for key, path in self.records().items():
                path.unlink(missing_ok=True)
                path.write_bytes(saved[key])

    def edited(self, change):
        def apply(path: Path) -> None:
            value = json.loads(path.read_text(encoding="utf-8"))
            change(value)
            path.write_text(json.dumps(value), encoding="utf-8")
        return apply

    def test_unproven_natural_completion_is_refused_as_incomplete(self) -> None:
        def pid(value): value["pid"] += 1
        def natural(flag): return lambda value: value.update(natural_wait=flag)
        def exit_code(candidate): return lambda value: value.update(exit_code=candidate)
        def late(value): value["started_ns"] = value["started_ns"] + (1 << 62)
        def argv(value): value["argv"] = [*value["argv"], "extra"]
        def cwd(value): value["cwd"] = str(self.root)
        cases = (
            ("natural false", "completed", natural(False)),
            ("natural missing", "completed", lambda value: value.pop("natural_wait")),
            ("natural text", "completed", natural("true")),
            ("exit bool", "completed", exit_code(True)), ("exit text", "completed", exit_code("2")),
            ("exit negative", "completed", exit_code(-1)), ("exit high", "completed", exit_code(256)),
            ("exit null", "completed", exit_code(None)), ("completed pid", "completed", pid),
            ("started pid", "started", pid), ("started late", "started", late),
            ("argv", "invocation", argv), ("cwd", "invocation", cwd),
        )
        self.child(code=2)
        for label, record, change in cases:
            with self.subTest(label):
                error = self.mutated(record, self.edited(change))
                self.assertEqual(error.code, "child_natural_completion_unproven")
                self.assertIsNone(error.__cause__)
        self.assertEqual(execution.observe_child_completion(self.receipt, "native").exit_code, 2)

    def test_scalar_faults_keep_their_code_and_cause_but_are_incomplete(self) -> None:
        def literal(text):
            return lambda path: path.write_text(text, encoding="utf-8")
        def field(name, value):
            return self.edited(lambda body: body.update({name: value}))
        integer_code, object_code = "release_record_integer_invalid", "release_record_object_invalid"
        cases = (
            ("completed null", "completed", literal("null"), object_code),
            ("started null", "started", literal("null"), object_code),
            ("invocation null", "invocation", literal("null"), object_code),
            ("completed pid bool", "completed", field("pid", True), integer_code),
            ("completed pid null", "completed", field("pid", None), integer_code),
            ("started pid bool", "started", field("pid", True), integer_code),
            ("ended text", "completed", field("ended_ns", "late"), integer_code),
            ("started time null", "started", field("started_ns", None), integer_code),
        )
        self.child(code=2)
        for label, record, change, code in cases:
            with self.subTest(label):
                error = self.mutated(record, change)
                self.assertEqual(error.code, code)
                self.assertIsInstance(error.__cause__, ReleaseError)
                self.assertEqual((error.__cause__.code, error.__cause__.incomplete), (code, False))

    def test_reader_faults_become_the_canonical_incomplete_reason_with_the_reader_cause(self) -> None:
        def missing(path: Path) -> None:
            path.unlink()

        def link(path: Path) -> None:
            target = path.with_name("other.json")
            target.write_text("{}", encoding="utf-8")
            path.unlink()
            path.symlink_to(target)

        def oversized(path: Path) -> None:
            path.write_bytes(b" " * ((1 << 20) + 1))

        self.child(code=2)
        for label, change, reason in (("missing", missing, ReadFailure.UNREADABLE),
                                      ("symlink", link, ReadFailure.INVALID),
                                      ("oversized", oversized, ReadFailure.TOO_LARGE)):
            for record in ("completed", "started", "invocation"):
                with self.subTest(label=label, record=record):
                    error = self.mutated(record, change)
                    self.assertEqual(error.code, "child_natural_completion_unproven")
                    self.assertIsInstance(error.__cause__, BoundedReadError)
                    self.assertIs(error.__cause__.reason, reason)

    def test_malformed_json_is_unproven_with_the_parser_cause(self) -> None:
        self.child()
        self.records()["completed"].write_text("{", encoding="utf-8")
        error = self.refused()
        self.assertEqual(error.code, "child_natural_completion_unproven")
        self.assertIsInstance(error.__cause__, PublicProofError)

    def test_known_incomplete_errors_pass_through_and_programming_errors_are_not_caught(self) -> None:
        known = ReleaseError("child_completion_location_invalid", incomplete=True)
        with patch.object(execution, "_validated_completion", side_effect=known):
            with self.assertRaises(ReleaseError) as caught:
                execution.observe_child_completion(self.receipt, "native")
        self.assertIs(caught.exception, known)
        defect = RuntimeError("programming defect")
        with patch.object(execution, "_validated_completion", side_effect=defect):
            with self.assertRaises(RuntimeError) as raised:
                execution.observe_child_completion(self.receipt, "native")
        self.assertIs(raised.exception, defect)

    def test_a_foreign_completion_location_is_refused_without_a_cause(self) -> None:
        self.receipt.intend("archive", {"completion": str(self.root / "elsewhere" / "completed.json"),
                                        "argv": [], "cwd": str(self.checkout)})
        error = self.refused("archive")
        self.assertEqual(error.code, "child_completion_location_invalid")
        self.assertIsNone(error.__cause__)


class FiniteChildHelperTests(ChildObservationFixture):
    def retained_stderr(self) -> tuple[bool, str]:
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            return self.tree.dispose(), stderr.getvalue()

    def test_a_finite_child_succeeds_and_the_tree_is_removed(self) -> None:
        result = self.child(code=0)
        self.assertEqual(result["exit_code"], 0)
        self.assertIsNone(self.tree.unresolved)
        self.assertEqual(self.retained_stderr(), (True, ""))
        self.assertFalse(self.tree.root.exists())

    def test_an_interrupt_after_natural_completion_is_proven_closed_and_not_retained(self) -> None:
        interrupt = KeyboardInterrupt("injected after natural completion")
        with patch.object(execution, "observe_child", side_effect=interrupt):
            with self.assertRaises(KeyboardInterrupt) as caught:
                self.child(code=2)
        self.assertIs(caught.exception, interrupt)
        self.assertEqual(execution.observe_child_completion(self.receipt, "native").exit_code, 2)
        self.assertIsNone(self.tree.unresolved)
        self.assertEqual(self.retained_stderr(), (True, ""))
        self.assertFalse(self.tree.root.exists())

    def remove_after_canonical_completion(self) -> None:
        """Cleanup callback: the whole base goes only after the canonical observer proves closure.

        Registered before any fault is injected, so it runs even when an assertion fails. It
        never asserts: a regression may inject an assertion failure, and an unproven closure
        must surface as the observer's own error with the tree left retained.
        """
        if not self.tree.dispose():
            execution.observe_child_completion(self.receipt, "native")
            self.tree.remove_retained()

    def remove_without_a_start(self, processes: Mock) -> None:
        """Cleanup callback: a retained base is removed only with the explicit zero-start oracle."""
        if not self.tree.dispose():
            runner = self.root / "runner"
            if processes.call_count != 1 or any(runner.iterdir()):
                raise self.failureException(f"zero-start oracle failed under {runner}")
            self.tree.remove_retained()

    def test_known_incomplete_error_is_retained_until_the_canonical_completion_is_proven(self) -> None:
        self.addCleanup(self.remove_after_canonical_completion)
        injected = ReleaseError("injected_observation_incomplete", incomplete=True)
        with patch.object(execution, "observe_child", side_effect=injected):
            with self.assertRaises(ReleaseError) as caught:
                self.child(code=2)
        self.assertIs(caught.exception, injected)
        self.assertIs(self.tree.unresolved, injected)
        retained, stderr = self.retained_stderr()
        self.assertFalse(retained)
        self.assertIn("injected_observation_incomplete", stderr)
        self.assertTrue(self.records()["completed"].is_file())
        # Only after the canonical observer proves the child finished is deletion allowed.
        self.assertEqual(execution.observe_child_completion(self.receipt, "native").exit_code, 2)
        self.assertTrue(self.doCleanups())
        self.assertFalse(self.tree.root.exists())
        self.assertIs(self.tree.unresolved, injected)

    def test_a_launch_that_never_started_is_retained_and_removed_only_with_the_zero_start_oracle(self) -> None:
        interrupt = KeyboardInterrupt("injected before any child started")
        processes = Mock(side_effect=interrupt)
        self.addCleanup(self.remove_without_a_start, processes)
        with patch.object(execution, "Processes", processes):
            with self.assertRaises(KeyboardInterrupt) as caught:
                self.child(code=2)
        self.assertIs(caught.exception, interrupt)
        self.assertEqual(self.tree.unresolved.code, "child_natural_completion_unproven")
        self.assertTrue(self.tree.unresolved.incomplete)
        retained, _stderr = self.retained_stderr()
        self.assertFalse(retained)
        self.assertEqual(processes.call_count, 1)
        self.assertEqual(sorted(path.name for path in (self.root / "runner").iterdir()), [])
        self.assertTrue(self.doCleanups())
        self.assertFalse(self.tree.root.exists())
        self.assertEqual(self.tree.unresolved.code, "child_natural_completion_unproven")


class AssertionSafeCleanupTests(unittest.TestCase):
    """A failing assertion in a fault test still removes a base whose closure is proven.

    Each case runs an actual FiniteChildHelperTests method as an inner test whose first
    post-fault assertIs fails. Every outer safety cleanup is registered before the inner run
    and removes a leaked base only after an actual canonical completion or an explicit
    zero-start proof; a path that merely exists never authorizes removal.
    """

    KNOWN = "test_known_incomplete_error_is_retained_until_the_canonical_completion_is_proven"
    ZERO_START = "test_a_launch_that_never_started_is_retained_and_removed_only_with_the_zero_start_oracle"

    def run_inner(self, method: str, prove, *, guard: Mock | None = None, before=None,
                  fail_before_fault: bool = False):
        inner = FiniteChildHelperTests(method)

        def forced(*_arguments, **_keywords) -> None:
            if before is not None:
                before(inner)
            raise AssertionError("forced")

        def forced_before_intent(*_arguments, **_keywords) -> None:
            raise AssertionError("forced")

        setattr(inner, "child" if fail_before_fault else "assertIs",
                forced_before_intent if fail_before_fault else forced)
        self.addCleanup(self.assert_removed, inner)
        self.addCleanup(self.remove_by_proof, inner, prove)
        result, stderr = unittest.TestResult(), io.StringIO()
        descriptors = open_descriptors()
        with contextlib.redirect_stderr(stderr):
            if guard is None:
                inner.run(result)
            else:
                with patch.object(execution, "Processes", guard):
                    inner.run(result)
        return inner, result, stderr.getvalue(), (descriptors, open_descriptors())

    def remove_by_proof(self, inner: FiniteChildHelperTests, prove) -> None:
        if inner.tree.root.exists():
            prove(inner)
            inner.tree.remove_retained()

    def assert_removed(self, inner: FiniteChildHelperTests) -> None:
        """Runs after the proof-gated removal: no regression may leave its base behind."""
        self.assertFalse(inner.tree.root.exists())

    @staticmethod
    def prove_completed(inner: FiniteChildHelperTests) -> None:
        execution.observe_child_completion(inner.receipt, "native")

    def prove_not_started(self, guard: Mock):
        def prove(inner: FiniteChildHelperTests) -> None:
            native = inner.root / "runner" / "native-processes"
            if guard.call_count != 0 or (native.exists() and any(native.iterdir())):
                raise self.failureException(f"a child may have started under {inner.root}")
        return prove

    def assert_only_the_forced_failure(self, result: unittest.TestResult) -> None:
        self.assertEqual((len(result.failures), len(result.errors)), (1, 0))
        self.assertIn("forced", result.failures[0][1])

    def test_a_forced_failure_after_a_real_incomplete_observation_still_removes_the_tree(self) -> None:
        inner, result, stderr, descriptors = self.run_inner(self.KNOWN, self.prove_completed)
        self.assert_only_the_forced_failure(result)
        self.assertEqual((inner.tree.unresolved.code, inner.tree.unresolved.incomplete),
                         ("injected_observation_incomplete", True))
        self.assertIn(f"retained (injected_observation_incomplete): {inner.tree.root}", stderr)
        self.assertFalse(inner.tree.root.exists())
        self.assertEqual(descriptors[0], descriptors[1])

    def test_a_forced_failure_after_a_launch_that_never_started_still_removes_the_tree(self) -> None:
        guard = Mock(side_effect=AssertionError("Processes constructed outside the inner patch"))
        inner, result, stderr, descriptors = self.run_inner(
            self.ZERO_START, self.prove_not_started(guard), guard=guard)
        self.assert_only_the_forced_failure(result)
        self.assertEqual(inner.tree.unresolved.code, "child_natural_completion_unproven")
        self.assertIn(f"retained (child_natural_completion_unproven): {inner.tree.root}", stderr)
        self.assertEqual(guard.call_count, 0)
        self.assertFalse(inner.tree.root.exists())
        self.assertEqual(descriptors[0], descriptors[1])

    def test_an_unproven_completion_keeps_the_tree_and_reports_the_observer_error(self) -> None:
        aside_name = "completed.json.aside"

        def rename_record_aside(inner: FiniteChildHelperTests) -> None:
            record = inner.records()["completed"]
            record.rename(record.with_name(aside_name))

        def restore_then_prove(inner: FiniteChildHelperTests) -> None:
            record = inner.records()["completed"]
            aside = record.with_name(aside_name)
            if aside.exists() and not record.exists():
                aside.rename(record)
            self.prove_completed(inner)

        inner, result, stderr, descriptors = self.run_inner(
            self.KNOWN, restore_then_prove, before=rename_record_aside)
        self.assertEqual((len(result.failures), len(result.errors)), (1, 1))
        self.assertIn("forced", result.failures[0][1])
        self.assertIn("child_natural_completion_unproven", result.errors[0][1])
        self.assertEqual(descriptors[0], descriptors[1])
        self.assertTrue(inner.tree.root.is_dir())
        self.assertIn("retained (injected_observation_incomplete)", stderr)

    def test_a_planted_runner_entry_defeats_the_zero_start_oracle_and_keeps_the_tree(self) -> None:
        guard = Mock(side_effect=AssertionError("Processes constructed outside the inner patch"))
        planted = []

        def plant_native_capture(inner: FiniteChildHelperTests) -> None:
            native = inner.root / "runner" / "native-processes"
            native.mkdir()
            planted.append(native)

        inner, result, stderr, descriptors = self.run_inner(
            self.ZERO_START, self.prove_not_started(guard), guard=guard, before=plant_native_capture)
        self.assertEqual((len(result.failures), len(result.errors)), (2, 0))
        self.assertIn("forced", result.failures[0][1])
        self.assertIn("zero-start oracle failed", result.failures[1][1])
        self.assertEqual(descriptors[0], descriptors[1])
        self.assertEqual(guard.call_count, 0)
        self.assertTrue(planted[0].is_dir())
        self.assertTrue(inner.tree.root.is_dir())

    def test_a_failure_before_any_intent_disposes_normally_without_querying_a_missing_stage(self) -> None:
        guard = Mock(side_effect=AssertionError("a child was launched"))
        inner, result, stderr, descriptors = self.run_inner(
            self.KNOWN, self.prove_completed, guard=guard, fail_before_fault=True)
        self.assert_only_the_forced_failure(result)
        self.assertIsNone(inner.tree.unresolved)
        self.assertEqual((guard.call_count, stderr), (0, ""))
        self.assertEqual(inner.receipt.stage("native").state, "pending")
        self.assertFalse(inner.tree.root.exists())
        self.assertEqual(descriptors[0], descriptors[1])


if __name__ == "__main__":
    unittest.main()
