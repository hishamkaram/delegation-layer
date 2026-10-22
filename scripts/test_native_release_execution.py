"""Preflight failures, exact input forwarding, and no paid relaunch on recovery."""

from __future__ import annotations

from dataclasses import replace
import hashlib
import os
from pathlib import Path
import sys
from unittest.mock import patch
import unittest

from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.native_release import cli, coordinator, execution, preflight
from scripts.delegate_acceptance.native_release.contract import ReleaseError
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.delegate_acceptance.native.acceptance_discovery import require_discovery
from scripts.delegate_acceptance.native_evidence.aggregate import build_aggregate
from scripts.delegate_acceptance.native_evidence.contract import MATRIX_HEADER, expected_cases
from scripts.delegate_acceptance.native_evidence.directories import prepare_case_directory
from scripts.test_native_release_support import HEAD, RELEASE, SOURCE, ReleaseFixture


class PreflightTests(ReleaseFixture):
    def test_github_404_stops_before_archive_or_paid_work(self) -> None:
        self.client.repo_inaccessible = True
        with patch.object(coordinator, "github_client", return_value=self.client), \
                patch.object(preflight, "git_bytes", return_value=HEAD.encode()), \
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
        self.client.branch_revision = "f" * 40
        with patch.object(preflight, "git_bytes", side_effect=[HEAD.encode(), b""]), \
                patch.object(preflight, "_source", return_value=(SOURCE, True)), \
                patch.object(preflight, "resolve_release_identity", return_value={"commit_sha": RELEASE}):
            with self.assertRaisesRegex(ReleaseError, "clean_current_main_harness"):
                preflight.preflight(self.receipt.invocation, self.client)
        local = replace(self.receipt.invocation,
                        request=replace(self.receipt.invocation.request, no_submit=True))
        with patch.object(preflight, "git_bytes", side_effect=[HEAD.encode(), b""]), \
                patch.object(preflight, "_source", return_value=(SOURCE, True)), \
                patch.object(preflight, "resolve_release_identity", return_value={"commit_sha": RELEASE}):
            facts = preflight.preflight(local, self.client)
        self.assertTrue(facts["candidate_harness"])
        self.assertEqual(facts["harness_revision"], HEAD)
        self.assertEqual(facts["actor_id"], 61)

    def test_untracked_candidate_source_is_hashed_without_failed_git_show(self) -> None:
        source_path = self.checkout / "new.py"
        source_path.write_bytes(b"print('candidate')\n")
        source_hash = hashlib.sha256(source_path.read_bytes()).hexdigest()
        with patch.object(preflight, "source_fingerprint", return_value={
                "sha256": SOURCE, "files": [{"path": "new.py", "sha256": source_hash}],
        }), patch.object(preflight, "git_bytes", return_value=b"") as git:
            digest, committed = preflight._source(self.checkout, {}, HEAD)
        self.assertEqual(digest, SOURCE)
        self.assertFalse(committed)
        self.assertEqual(git.call_count, 1)

    def test_github_client_uses_explicit_stored_configuration_mode(self) -> None:
        with patch.object(coordinator, "gh_environment", return_value={"HOME": str(Path.home())}) as environment:
            coordinator.github_client()
        environment.assert_called_once_with(auth_mode="local-config")

    def test_published_release_is_read_only_candidate_input_only(self) -> None:
        self.client.release_draft = False
        local = replace(self.receipt.invocation,
                        request=replace(self.receipt.invocation.request, no_submit=True))
        with (patch.object(preflight, "git_bytes", side_effect=[HEAD.encode(), b""]),
              patch.object(preflight, "_source", return_value=(SOURCE, True)),
              patch.object(preflight, "resolve_release_identity", return_value={"commit_sha": RELEASE})):
            facts = preflight.preflight(local, self.client)
        self.assertEqual(facts["release_revision"], RELEASE)
        with patch.object(preflight, "git_bytes", return_value=HEAD.encode()):
            with self.assertRaisesRegex(ReleaseError, "exact_draft_release_required"):
                preflight.preflight(self.receipt.invocation, self.client)

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

    def create_runner(self) -> None:
        owner = DirectoryOwner()
        token = owner.create("test runner", self.root / "runner", self.root)
        self.complete("download", {"runner_identity": directory_identity_record(token)})
        owner.release()

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
        result = execution.run_child(self.receipt, "native", [
            str(Path(sys.executable).resolve()), str(script), str(self.receipt.path), str(note),
        ], execution.child_environment(self.receipt.invocation), timeout=15)
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


if __name__ == "__main__":
    unittest.main()
