"""Deterministic tests for native acceptance matrix evidence and retention."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch

import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import acceptance_supervisor_common as common
from scripts import native_acceptance_evidence as evidence
from scripts.delegate_acceptance.directories import journal as cleanup_journal
from scripts.delegate_acceptance.native_evidence import cases as case_operations
from scripts.delegate_acceptance.native_evidence import retention as retention_operations


class NativeAcceptanceEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="native-evidence-test-")
        self.root = Path(self.temporary.name).resolve(strict=True)

    def tearDown(self):
        self.temporary.cleanup()

    def test_prepared_case_directory_binds_empty_identity_and_definition(self):
        root = self.root / "prepared-case"
        (root / evidence.CASE_ROOT_NAME).mkdir(parents=True)
        definition = evidence.EXPECTED_CASES[0]
        case_directory = evidence.prepare_case_directory(root, definition.case_id)

        self.assertEqual(
            evidence.validate_prepared_case_directory(
                case_directory, definition.provider, definition.permission, definition.scenario),
            case_directory,
        )

        (case_directory / "unexpected").write_text("partial evidence", encoding="utf-8")
        with self.assertRaisesRegex(evidence.EvidenceError, "prepared_case_directory_not_empty"):
            evidence.validate_prepared_case_directory(
                case_directory, definition.provider, definition.permission, definition.scenario)

    def test_prepared_case_directory_rejects_symlinked_parent(self):
        physical_parent = self.root / "physical-parent"
        physical_parent.mkdir()
        alias = self.root / "parent-alias"
        alias.symlink_to(physical_parent, target_is_directory=True)
        root = alias / "aggregate"
        (root / evidence.CASE_ROOT_NAME).mkdir(parents=True)

        with self.assertRaisesRegex(evidence.EvidenceError, "evidence_path_symlink"):
            evidence.prepare_case_directory(root, evidence.EXPECTED_CASES[0].case_id)

    def test_prepared_case_directory_rejects_definition_or_identity_drift(self):
        root = self.root / "prepared-case-drift"
        (root / evidence.CASE_ROOT_NAME).mkdir(parents=True)
        definition = evidence.EXPECTED_CASES[0]
        case_directory = evidence.prepare_case_directory(root, definition.case_id)

        with self.assertRaisesRegex(evidence.EvidenceError, "prepared_case_definition_mismatch"):
            evidence.validate_prepared_case_directory(
                case_directory, "claude:print", definition.permission, definition.scenario)

        previous = root / "previous-case-directory"
        case_directory.rename(previous)
        case_directory.mkdir()
        with self.assertRaisesRegex(evidence.EvidenceError, "prepared_case_identity_changed"):
            evidence.validate_prepared_case_directory(
                case_directory, definition.provider, definition.permission, definition.scenario)

    def write_pass_case(self, index, case, case_directory, requested_model, requested_effort):
        aliases = evidence._expected_task_aliases(case.provider, case.scenario)
        session = "session-" + case.case_id
        tasks = {
            alias: f"{index + 1:016x}{position + 1:016x}"
            for position, alias in enumerate(aliases)
        }
        provider_runtimes = {
            task_id: {
                "executable": f"/fixture/provider-{position + 1}",
                "version": f"1.{position}",
                "sha256": ("a" if position == 0 else "b") * 64,
            }
            for position, task_id in enumerate(tasks.values())
        }
        first_task = next(iter(tasks.values()))
        first_runtime = provider_runtimes[first_task]
        (case_directory / "success.json").write_text(json.dumps({
            "gate": "acceptance-native", "status": "passed", "provider": case.provider,
            "mode": case.permission, "scenario": case.scenario,
            "requested_model": requested_model, "requested_effort": requested_effort,
            "tasks": tasks,
            "provider_sessions": {alias: session for alias in aliases},
            "provider_versions": {
                task_id: identity["version"]
                for task_id, identity in provider_runtimes.items()
            },
            "provider_runtime_identities": provider_runtimes,
            "natural_daemon_shutdown": True,
        }), encoding="utf-8")
        (case_directory / "binding.json").write_text(json.dumps({
            "provider": case.provider, "mode": case.permission,
            "scenario": case.scenario,
            "provider_identity_scope": "first-admitted-task",
            "provider_task_id": first_task,
            "requested_model": requested_model,
            "requested_effort": requested_effort,
            "provider_executable": first_runtime["executable"],
            "provider_version": first_runtime["version"],
            "provider_sha256": first_runtime["sha256"],
            "delegate_sha256": "2" * 64, "runner_sha256": "3" * 64,
            "pueue_sha256": "4" * 64, "pueued_sha256": "5" * 64,
        }), encoding="utf-8")

    def make_matrix(
            self, root: Path, *, status: str = "BLOCKED", failed_index: int | None = None,
            with_cleanup: bool = True, requested_model: str | None = None,
            requested_effort: str | None = None) -> Path:
        (root / evidence.CASE_ROOT_NAME).mkdir(parents=True, exist_ok=True)
        (root / evidence.COMPACT_RECEIPT_ROOT_NAME).mkdir(parents=True, exist_ok=True)
        lines = [evidence.MATRIX_HEADER]
        for index, case in enumerate(evidence.EXPECTED_CASES):
            case_directory = evidence.prepare_case_directory(root, case.case_id)
            row_status = status
            exit_code = 2
            if failed_index == index:
                row_status, exit_code = "FAIL", 1
                (case_directory / "failure.json").write_text(json.dumps({
                    "gate": "acceptance-native", "status": "failed", "provider": case.provider,
                    "scenario": case.scenario, "requested_model": requested_model,
                    "requested_effort": requested_effort,
                }), encoding="utf-8")
                (case_directory / "raw-provider-output").write_text(
                    "provider secret must remain in unresolved raw evidence", encoding="utf-8")
            elif status == "PASS":
                exit_code = 0
                self.write_pass_case(
                    index, case, case_directory, requested_model, requested_effort,
                )
            else:
                (case_directory / "failure.json").write_text(json.dumps({
                    "gate": "acceptance-native", "status": "BLOCKED", "provider": case.provider,
                    "scenario": case.scenario, "requested_model": requested_model,
                    "requested_effort": requested_effort, "error": "do not copy this detail",
                }), encoding="utf-8")
            if with_cleanup and row_status == "PASS":
                (case_directory / "cleanup.json").write_text(json.dumps({
                    "status": "removed", "supervisor_shutdown_observed": True,
                    "raw_process_captures": "removed",
                }), encoding="utf-8")
            lines.append("\t".join((case.case_id, case.provider, case.permission,
                                     case.scenario, str(exit_code), row_status,
                                     case.case_output)) + "\n")
        matrix = root / evidence.CASE_MATRIX_NAME
        matrix.write_text("".join(lines), encoding="ascii")
        return matrix

    def build(self, *, status="BLOCKED", require_pass=False, failed_index=None,
              root=None, with_cleanup=True, retention_count=None,
              requested_model=None, requested_effort=None):
        target = root or self.root / "native-aggregate-current"
        matrix = self.make_matrix(
            target, status=status, failed_index=failed_index, with_cleanup=with_cleanup,
            requested_model=requested_model, requested_effort=requested_effort,
        )
        return evidence.build_aggregate(
            target, matrix, target / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40, require_pass=require_pass,
            retention_count=retention_count,
        )

    def test_matrix_completeness_and_blocked_scope(self):
        incomplete_root = self.root / "native-aggregate-incomplete"
        incomplete_matrix = self.make_matrix(incomplete_root)
        incomplete = self.root / "incomplete.tsv"
        lines = incomplete_matrix.read_text(encoding="ascii").splitlines(True)
        incomplete.write_text("".join(lines[:-1]), encoding="ascii")
        with self.assertRaisesRegex(evidence.EvidenceError, "case_matrix_incomplete"):
            evidence._parse_matrix(incomplete)

        root = self.root / "native-aggregate-blocked"
        value, code = self.build(root=root, require_pass=True)
        self.assertEqual(code, 2)
        self.assertEqual(value["status"], "BLOCKED")
        self.assertTrue(value["matrix_complete"])
        self.assertEqual(value["case_count"], len(evidence.EXPECTED_CASES))
        self.assertEqual(value["blocked_scope"], "authentication-or-prerequisite-only")
        self.assertTrue(all(item["status"] == "BLOCKED" for item in value["cases"]))
        self.assertEqual(len(list((root / evidence.CASE_ROOT_NAME).iterdir())), 0)
        for case in value["cases"]:
            compact = json.loads(Path(case["receipt_path"]).read_text(encoding="utf-8"))
            self.assertEqual(compact["blocked_scope"], "authentication-or-prerequisite")
            self.assertNotIn("do not copy this detail", json.dumps(compact))

    def test_digest_tampering_is_rejected(self):
        root = self.root / "native-aggregate-pass"
        _, code = self.build(root=root, status="PASS")
        self.assertEqual(code, 0)
        aggregate = root / evidence.AGGREGATE_RECEIPT_NAME
        value, _ = evidence.validate_aggregate_receipt(aggregate)
        compact = Path(value["cases"][0]["receipt_path"])
        compact.write_text(compact.read_text(encoding="utf-8") + "tampered\n", encoding="utf-8")
        with self.assertRaisesRegex(evidence.EvidenceError, "aggregate_case_digest_mismatch"):
            evidence.validate_aggregate_receipt(aggregate)

    def test_success_cleanup_retains_compact_receipts_and_cleanup_journals(self):
        root = self.root / "native-aggregate-pass"
        value, code = self.build(root=root, status="PASS")
        self.assertEqual(code, 0)
        self.assertEqual(value["evidence"]["case_raw"]["before"]["count"], 3 * len(evidence.EXPECTED_CASES))
        self.assertEqual(value["evidence"]["case_raw"]["after"]["count"], 0)
        compact_root = root / evidence.COMPACT_RECEIPT_ROOT_NAME
        self.assertEqual(len(list(compact_root.glob("case-*.json"))), 2 * len(evidence.EXPECTED_CASES))
        self.assertEqual(len(list(compact_root.glob("*.cleanup.json"))), len(evidence.EXPECTED_CASES))
        self.assertEqual(len(list(compact_root.glob("*.cleanup.json.lock"))), len(evidence.EXPECTED_CASES))
        self.assertEqual(list((root / evidence.CASE_ROOT_NAME).iterdir()), [])
        compact = json.loads(Path(value["cases"][0]["receipt_path"]).read_text(encoding="utf-8"))
        self.assertEqual(compact["source_proof"]["requested_options_state"], "recorded")
        self.assertIsNone(compact["source_proof"]["requested_model"])
        self.assertIsNone(compact["source_proof"]["requested_effort"])

    def test_requested_model_and_effort_survive_raw_cleanup_and_replay(self):
        root = self.root / "native-aggregate-requested-options"
        value, code = self.build(
            root=root, status="PASS", requested_model="openrouter/openai/gpt-5",
            requested_effort="high",
        )
        self.assertEqual(code, 0)
        compact_path = Path(value["cases"][0]["receipt_path"])
        compact = json.loads(compact_path.read_text(encoding="utf-8"))
        proof = compact["source_proof"]
        self.assertEqual(proof["requested_options_state"], "recorded")
        self.assertEqual(proof["requested_model"], "openrouter/openai/gpt-5")
        self.assertEqual(proof["requested_effort"], "high")
        for key in ("requested_options_state", "requested_model", "requested_effort"):
            self.assertNotIn(key, compact)
        self.assertEqual(compact["cleanup"]["raw_after"]["file_count"], 0)
        replayed, replay_code = evidence.validate_aggregate_receipt(
            root / evidence.AGGREGATE_RECEIPT_NAME,
        )
        self.assertRegex(replay_code, r"^[0-9a-f]{64}$")
        replayed_compact = json.loads(
            Path(replayed["cases"][0]["receipt_path"]).read_text(encoding="utf-8"),
        )
        self.assertEqual(replayed_compact["source_proof"]["requested_model"], proof["requested_model"])
        self.assertEqual(replayed_compact["source_proof"]["requested_effort"], proof["requested_effort"])

    def test_missing_or_invalid_requested_options_refuse_cleanup(self):
        cases = (
            ("missing", lambda value: value.pop("requested_effort")),
            ("invalid", lambda value: value.__setitem__("requested_model", {"bad": True})),
        )
        for label, mutate in cases:
            with self.subTest(label=label):
                root = self.root / f"native-aggregate-requested-options-{label}"
                matrix = self.make_matrix(root, status="PASS")
                source = root / evidence.EXPECTED_CASES[0].case_output / "success.json"
                value = json.loads(source.read_text(encoding="utf-8"))
                mutate(value)
                source.write_text(json.dumps(value), encoding="utf-8")

                aggregate, code = evidence.build_aggregate(
                    root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
                    tag="v1.2.3", source_revision="a" * 40,
                )
                self.assertEqual(code, 1)
                self.assertEqual(aggregate["status"], "FAIL")
                self.assertEqual(aggregate["cases"][0]["evidence_status"], "UNRESOLVED")
                case_directory = root / evidence.EXPECTED_CASES[0].case_output
                self.assertTrue((case_directory / "success.json").is_file())
                compact = json.loads(Path(aggregate["cases"][0]["receipt_path"]).read_text())
                self.assertEqual(compact["source_proof"]["requested_options_state"], label)
                self.assertIsNone(compact["source_proof"]["requested_model"])
                self.assertIsNone(compact["source_proof"]["requested_effort"])
                self.assertEqual(compact["evidence_error"],
                                 f"source_requested_options_{label}")

    def test_requested_option_provenance_tampering_is_rejected(self):
        root = self.root / "native-aggregate-requested-options-tamper"
        value, code = self.build(root=root, status="PASS", requested_model="model-a")
        self.assertEqual(code, 0)
        aggregate_path = root / evidence.AGGREGATE_RECEIPT_NAME
        aggregate = json.loads(aggregate_path.read_text(encoding="utf-8"))
        compact_path = Path(aggregate["cases"][0]["receipt_path"])
        compact = json.loads(compact_path.read_text(encoding="utf-8"))
        compact["source_proof"]["requested_model"] = "model-b"
        compact_path.write_text(json.dumps(compact), encoding="utf-8")
        aggregate["cases"][0]["receipt_sha256"] = hashlib.sha256(
            compact_path.read_bytes(),
        ).hexdigest()
        aggregate_path.write_text(json.dumps(aggregate), encoding="utf-8")

        with self.assertRaisesRegex(evidence.EvidenceError,
                                   "aggregate_case_cleanup_journal_invalid"):
            evidence.validate_aggregate_receipt(aggregate_path)

    def test_requested_options_must_match_binding_before_cleanup(self):
        root = self.root / "native-aggregate-requested-options-binding-mismatch"
        matrix = self.make_matrix(root, status="PASS", requested_model="model-a")
        binding_path = root / evidence.EXPECTED_CASES[0].case_output / "binding.json"
        binding = json.loads(binding_path.read_text(encoding="utf-8"))
        binding["requested_model"] = "model-b"
        binding_path.write_text(json.dumps(binding), encoding="utf-8")

        aggregate, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(code, 1)
        self.assertEqual(aggregate["cases"][0]["evidence_status"], "UNRESOLVED")
        compact = json.loads(
            Path(aggregate["cases"][0]["receipt_path"]).read_text(encoding="utf-8"),
        )
        self.assertEqual(compact["source_proof"]["requested_options_state"], "invalid")
        self.assertEqual(compact["evidence_error"], "source_requested_options_invalid")
        self.assertTrue(binding_path.is_file())

    def test_interrupted_replay_binds_requested_options_before_raw_deletion(self):
        root = self.root / "native-aggregate-requested-options-pending"
        matrix = self.make_matrix(root, status="PASS", requested_model="model-a")
        definition = evidence.EXPECTED_CASES[0]
        case_directory = root / definition.case_output
        source = case_directory / "success.json"
        original_source = source.read_bytes()
        with patch.object(case_operations, "resume_directory_cleanup",
                          side_effect=RuntimeError("interrupted before cleanup")):
            with self.assertRaisesRegex(evidence.EvidenceError, "case_cleanup_journal_invalid"):
                evidence.build_aggregate(
                    root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
                    tag="v1.2.3", source_revision="a" * 40,
                )

        compact_path = root / evidence.COMPACT_RECEIPT_ROOT_NAME / f"{definition.case_id}.json"
        compact_bytes = compact_path.read_bytes()
        compact = json.loads(compact_bytes)
        self.assertNotIn("requested_model", compact)
        compact["source_proof"]["requested_model"] = "model-b"
        compact_path.write_text(json.dumps(compact), encoding="utf-8")
        row = evidence.MatrixRow(definition, 0, "PASS")
        with patch.object(case_operations, "resume_directory_cleanup") as cleanup:
            replayed = evidence._replay_prepared_case(
                compact_path, case_directory, root / evidence.CASE_ROOT_NAME,
                root / evidence.CASE_CLEANUP_ROOT_NAME, row,
            )
        self.assertIsNone(replayed)
        cleanup.assert_not_called()
        self.assertEqual(source.read_bytes(), original_source)

        compact_path.write_bytes(compact_bytes)
        replayed = evidence._replay_prepared_case(
            compact_path, case_directory, root / evidence.CASE_ROOT_NAME,
            root / evidence.CASE_CLEANUP_ROOT_NAME, row,
        )
        self.assertIsNotNone(replayed)
        self.assertFalse(case_directory.exists())
        final = json.loads(compact_path.read_text(encoding="utf-8"))
        self.assertEqual(final["source_proof"]["requested_model"], "model-a")

    def test_compact_terminal_receipt_is_published_before_cleanup(self):
        root = self.root / "native-aggregate-order"
        observed: list[str] = []
        original = evidence.prepare_directory_cleanup

        def prepare(journal, label, identity, quarantine_parent, authorization):
            compact = Path(journal).with_name(Path(journal).name.replace(".cleanup.json", ".json"))
            value = json.loads(compact.read_text(encoding="utf-8"))
            observed.append(value["terminal_state"])
            self.assertEqual(value["raw_deletion"]["authority"], "DirectoryCleanup")
            self.assertEqual(value["raw_deletion"]["authorization_sha256"], authorization)
            self.assertEqual(value["raw_deletion"]["journal_path"], str(Path(journal).resolve()))
            return original(journal, label, identity, quarantine_parent, authorization)

        with patch.object(case_operations, "prepare_directory_cleanup", side_effect=prepare):
            value, code = self.build(root=root, status="PASS")
        self.assertEqual(code, 0)
        self.assertEqual(observed, ["published-before-cleanup"] * len(evidence.EXPECTED_CASES))
        self.assertTrue(all(
            json.loads(Path(case["receipt_path"]).read_text(encoding="utf-8"))["source_proof"]
            ["binary_identities"]["state"] == "complete"
            for case in value["cases"]
        ))

    def test_compact_proof_accepts_exact_per_task_provider_runtime_drift(self):
        root = self.root / "native-aggregate-runtime-drift"
        value, code = self.build(root=root, status="PASS")
        self.assertEqual(code, 0)
        compact = json.loads(Path(value["cases"][0]["receipt_path"]).read_text(encoding="utf-8"))
        binaries = compact["source_proof"]["binary_identities"]
        runtimes = binaries["provider_runtime_identities"]
        self.assertEqual(len(runtimes), 2)
        self.assertEqual({identity["version"] for identity in runtimes.values()}, {"1.0", "1.1"})
        self.assertEqual({identity["sha256"] for identity in runtimes.values()}, {"a" * 64, "b" * 64})
        first = binaries["provider_binding"]
        self.assertEqual(runtimes[first["task_id"]], {
            "executable": first["executable"],
            "version": first["version"],
            "sha256": first["sha256"],
        })

    def test_pass_requires_exact_provider_runtime_task_coverage(self):
        root = self.root / "native-aggregate-runtime-coverage"
        matrix = self.make_matrix(root, status="PASS")
        case_directory = root / evidence.EXPECTED_CASES[0].case_output
        source = case_directory / "success.json"
        value = json.loads(source.read_text(encoding="utf-8"))
        value["provider_runtime_identities"].pop(value["tasks"]["resume"])
        source.write_text(json.dumps(value), encoding="utf-8")

        aggregate, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(code, 1)
        self.assertEqual(aggregate["cases"][0]["evidence_status"], "UNRESOLVED")
        self.assertTrue(case_directory.is_dir())

    def test_pass_rejects_first_binding_runtime_mismatch(self):
        root = self.root / "native-aggregate-runtime-mismatch"
        matrix = self.make_matrix(root, status="PASS")
        case_directory = root / evidence.EXPECTED_CASES[0].case_output
        binding = case_directory / "binding.json"
        value = json.loads(binding.read_text(encoding="utf-8"))
        value["provider_sha256"] = "f" * 64
        binding.write_text(json.dumps(value), encoding="utf-8")

        aggregate, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(code, 1)
        self.assertEqual(aggregate["cases"][0]["evidence_status"], "UNRESOLVED")
        self.assertTrue(case_directory.is_dir())

    def test_replay_uses_terminal_receipts_after_matrix_removal(self):
        root = self.root / "native-aggregate-replay"
        matrix = self.make_matrix(root, status="PASS")
        first, first_code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(first_code, 0)
        matrix.unlink()
        second, second_code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(second_code, 0)
        self.assertEqual(second, first)

    def test_replay_reapplies_the_requested_pass_policy(self):
        root = self.root / "native-aggregate-policy-replay"
        first, first_code = self.build(root=root, require_pass=False)
        self.assertEqual(first_code, 0)
        receipt = root / evidence.AGGREGATE_RECEIPT_NAME
        before = receipt.read_bytes()
        with self.assertRaisesRegex(evidence.EvidenceError, "aggregate_policy_mismatch"):
            evidence.build_aggregate(
                root, root / evidence.CASE_MATRIX_NAME, receipt,
                tag="v1.2.3", source_revision="a" * 40, require_pass=True,
            )
        self.assertEqual(receipt.read_bytes(), before)
        self.assertFalse(first["policy"]["require_at_least_one_pass"])

    def test_terminal_aggregate_cannot_be_rebound_to_another_revision(self):
        root = self.root / "native-aggregate-revision-replay"
        self.build(root=root, status="PASS")
        receipt = root / evidence.AGGREGATE_RECEIPT_NAME
        before = receipt.read_bytes()
        with self.assertRaisesRegex(evidence.EvidenceError, "aggregate_existing_receipt_mismatch"):
            evidence.build_aggregate(
                root, root / evidence.CASE_MATRIX_NAME, receipt,
                tag="v1.2.3", source_revision="b" * 40,
            )
        self.assertEqual(receipt.read_bytes(), before)

    def test_replay_finishes_after_cleanup_before_final_case_receipt(self):
        root = self.root / "native-aggregate-replay-window"
        matrix = self.make_matrix(root, status="PASS")
        original = evidence._write_atomic
        first_case = evidence.EXPECTED_CASES[0].case_id
        interrupted = False

        def publish_then_interrupt(path, value):
            nonlocal interrupted
            if (not interrupted and Path(path).name == f"{first_case}.json"
                    and isinstance(value, dict)
                    and value.get("terminal_state") == "published-after-cleanup"):
                interrupted = True
                raise RuntimeError("simulated interruption after raw cleanup")
            return original(path, value)

        with patch.object(case_operations, "_write_atomic", side_effect=publish_then_interrupt):
            with self.assertRaisesRegex(RuntimeError, "simulated interruption"):
                evidence.build_aggregate(
                    root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
                    tag="v1.2.3", source_revision="a" * 40,
                )
        self.assertFalse((root / evidence.EXPECTED_CASES[0].case_output).exists())
        value, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(code, 0)
        self.assertEqual(value["status"], "PASS")
        self.assertEqual(value["unresolved_case_count"], 0)

    def test_replay_deletes_raw_evidence_moved_before_interruption(self):
        root = self.root / "native-aggregate-replay-moved"
        matrix = self.make_matrix(root, status="PASS")
        original = common._cleanup_empty
        interrupted = False

        def interrupt_once(value):
            nonlocal interrupted
            if not interrupted:
                interrupted = True
                raise RuntimeError("simulated interruption after isolation")
            return original(value)

        with patch.object(cleanup_journal, "_cleanup_empty", side_effect=interrupt_once):
            with self.assertRaisesRegex(evidence.EvidenceError, "case_cleanup_journal_invalid"):
                evidence.build_aggregate(
                    root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
                    tag="v1.2.3", source_revision="a" * 40,
                )
        retained = list((root / evidence.CASE_CLEANUP_ROOT_NAME).rglob("success.json"))
        self.assertEqual(len(retained), 1)
        value, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual((code, value["status"], value["unresolved_case_count"]), (0, "PASS", 0))
        self.assertFalse(any((root / evidence.CASE_CLEANUP_ROOT_NAME).rglob("success.json")))

    def test_oversized_source_receipt_is_bounded_and_retained(self):
        root = self.root / "native-aggregate-oversized"
        matrix = self.make_matrix(root, status="PASS")
        first = root / evidence.EXPECTED_CASES[0].case_output / "success.json"
        first.write_bytes(b"{" + b"x" * evidence.MAX_INPUT_BYTES + b"}")
        value, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(code, 1)
        self.assertTrue((root / evidence.EXPECTED_CASES[0].case_output).exists())
        self.assertEqual(value["cases"][0]["evidence_status"], "UNRESOLVED")
        self.assertEqual(value["cases"][0]["receipt_sha256"].__class__, str)

    def test_stage_manifest_contains_exact_aggregate_and_fourteen_cases(self):
        root = self.root / "native-aggregate-stage"
        self.build(root=root, status="PASS")
        output = self.root / "native-acceptance-evidence"
        manifest = evidence.stage_evidence(
            root / evidence.AGGREGATE_RECEIPT_NAME, output,
            expected_tag="v1.2.3", expected_source_revision="a" * 40,
        )
        self.assertEqual(manifest["case_count"], len(evidence.EXPECTED_CASES))
        self.assertEqual(len(manifest["case_receipts"]), len(evidence.EXPECTED_CASES))
        self.assertEqual(len(manifest["cleanup_journals"]), len(evidence.EXPECTED_CASES))
        self.assertEqual(len(list((output / "case-receipts").glob("*.json"))), len(evidence.EXPECTED_CASES))
        self.assertEqual(len(list((output / "cleanup-journals").glob("*.json"))), len(evidence.EXPECTED_CASES))
        self.assertEqual(len(manifest["files"]), 2 * len(evidence.EXPECTED_CASES) + 2)
        self.assertTrue((output / manifest["retention"]["plan"]["path"]).is_file())
        shutil.rmtree(root)
        for item in manifest["files"]:
            self.assertTrue((output / item["path"]).is_file())
        self.assertFalse(output.name.startswith("."))

    def test_large_aggregate_replays_and_stages_with_long_native_paths(self):
        root = self.root / ("native-path-" + "x" * 150) / ("nested-" + "y" * 150)
        self.build(root=root, status="PASS")
        receipt_path = root / evidence.AGGREGATE_RECEIPT_NAME
        original = receipt_path.read_bytes()
        self.assertGreater(len(original), evidence.MAX_RECEIPT_BYTES)
        self.assertLessEqual(len(original), evidence.MAX_AGGREGATE_BYTES)

        value, code = evidence.build_aggregate(
            root, root / evidence.CASE_MATRIX_NAME, receipt_path,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual((code, value["status"]), (0, "PASS"))
        self.assertEqual(receipt_path.read_bytes(), original)
        output = self.root / "staged-long-paths"
        manifest = evidence.stage_evidence(
            receipt_path, output,
            expected_tag="v1.2.3", expected_source_revision="a" * 40,
        )
        self.assertEqual((output / evidence.AGGREGATE_RECEIPT_NAME).read_bytes(), original)
        self.assertEqual(manifest["case_count"], len(evidence.EXPECTED_CASES))

    def test_oversized_aggregate_remains_bounded(self):
        receipt_path = self.root / evidence.AGGREGATE_RECEIPT_NAME
        receipt_path.write_bytes(b" " * (evidence.MAX_AGGREGATE_BYTES + 1))
        with self.assertRaisesRegex(evidence.EvidenceError, "evidence_file_too_large"):
            evidence.validate_aggregate_receipt(receipt_path)

    def test_individual_receipt_write_bound_is_unchanged(self):
        receipt_path = self.root / "individual.json"
        with self.assertRaisesRegex(evidence.EvidenceError, "receipt_too_large"):
            evidence._write_atomic(receipt_path, {"data": "x" * evidence.MAX_RECEIPT_BYTES})
        self.assertFalse(receipt_path.exists())

    def test_unresolved_failure_preserves_raw_evidence(self):
        root = self.root / "native-aggregate-failure"
        value, code = self.build(root=root, status="PASS", failed_index=0)
        self.assertEqual(code, 1)
        self.assertEqual(value["status"], "FAIL")
        failed_case = root / evidence.EXPECTED_CASES[0].case_output
        self.assertTrue((failed_case / "raw-provider-output").is_file())
        compact = root / evidence.COMPACT_RECEIPT_ROOT_NAME / f"{evidence.EXPECTED_CASES[0].case_id}.json"
        self.assertNotIn("provider secret", compact.read_text(encoding="utf-8"))
        self.assertEqual(value["unresolved_case_count"], 1)

    def test_blocked_cleanup_retention_preserves_raw_evidence(self):
        root = self.root / "native-aggregate-blocked-raw"
        matrix = self.make_matrix(root)
        case_directory = root / evidence.EXPECTED_CASES[0].case_output
        (case_directory / "cleanup.json").write_text(json.dumps({
            "status": "removed", "supervisor_shutdown_observed": True,
            "raw_process_captures": "retained",
        }), encoding="utf-8")
        raw = case_directory / "raw-provider-output"
        raw.write_text("blocked raw evidence", encoding="utf-8")
        value, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(code, 1)
        self.assertEqual(value["status"], "FAIL")
        self.assertTrue(raw.is_file())
        self.assertEqual(value["cases"][0]["evidence_status"], "UNRESOLVED")

    def test_replaced_case_directory_is_not_removed(self):
        root = self.root / "native-aggregate-replaced"
        matrix = self.make_matrix(root, status="PASS")
        case_directory = root / evidence.EXPECTED_CASES[0].case_output
        replacement = self.root / "replacement"
        replacement.mkdir()
        case_directory.rename(self.root / "original-case")
        case_directory.symlink_to(replacement, target_is_directory=True)
        value, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(code, 1)
        self.assertEqual(value["cases"][0]["evidence_status"], "UNRESOLVED")
        self.assertTrue(case_directory.is_symlink())

    def test_regular_case_replacement_after_creation_is_not_removed(self):
        root = self.root / "native-aggregate-replaced-directory"
        matrix = self.make_matrix(root, status="PASS")
        case_directory = root / evidence.EXPECTED_CASES[0].case_output
        original = self.root / "original-owned-case"
        case_directory.rename(original)
        case_directory.mkdir()
        marker = case_directory / "replacement-marker"
        marker.write_text("retain replacement", encoding="utf-8")
        value, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(code, 1)
        self.assertEqual(value["cases"][0]["evidence_status"], "UNRESOLVED")
        self.assertTrue(marker.is_file())
        self.assertTrue(original.is_dir())

    def test_pass_requires_exact_provider_session_identities(self):
        root = self.root / "native-aggregate-missing-session"
        matrix = self.make_matrix(root, status="PASS")
        source = root / evidence.EXPECTED_CASES[0].case_output / "success.json"
        value = json.loads(source.read_text(encoding="utf-8"))
        value.pop("provider_sessions")
        source.write_text(json.dumps(value), encoding="utf-8")
        aggregate, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(code, 1)
        self.assertEqual(aggregate["cases"][0]["evidence_status"], "UNRESOLVED")
        self.assertTrue((root / evidence.EXPECTED_CASES[0].case_output).is_dir())

    def test_pass_rejects_broken_task_session_continuity_before_raw_cleanup(self):
        root = self.root / "native-aggregate-session-discontinuity"
        matrix = self.make_matrix(root, status="PASS")
        case_directory = root / evidence.EXPECTED_CASES[0].case_output
        source = case_directory / "success.json"
        value = json.loads(source.read_text(encoding="utf-8"))
        value["provider_sessions"]["resume"] = "different-session"
        source.write_text(json.dumps(value), encoding="utf-8")

        aggregate, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40,
        )
        self.assertEqual(code, 1)
        self.assertEqual(aggregate["cases"][0]["evidence_status"], "UNRESOLVED")
        self.assertTrue((case_directory / "success.json").is_file())

    def test_pending_retention_receipt_never_validates_or_replays(self):
        root = self.root / "native-aggregate-pending-retention"
        value, code = self.build(root=root, status="PASS")
        self.assertEqual(code, 0)
        receipt = root / evidence.AGGREGATE_RECEIPT_NAME
        value["retention"] = {"configured_count": 3, "status": "pending"}
        receipt.write_text(json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n",
                           encoding="utf-8")
        with self.assertRaisesRegex(evidence.EvidenceError, "aggregate_retention_incomplete"):
            evidence.validate_aggregate_receipt(receipt)
        with self.assertRaises(evidence.EvidenceError):
            evidence.build_aggregate(
                root, root / evidence.CASE_MATRIX_NAME, receipt,
                tag="v1.2.3", source_revision="a" * 40,
            )

    def test_resolved_retention_prunes_only_old_resolved_roots(self):
        parent = self.root / "aggregates"
        parent.mkdir()
        for name, status, timestamp in (
                ("native-aggregate-oldest", "PASS", 1),
                ("native-aggregate-middle", "BLOCKED", 2),
                ("native-aggregate-newest", "PASS", 3),
                ("native-aggregate-unresolved", "FAIL", 4)):
            old = parent / name
            with patch.object(evidence.time, "time_ns", return_value=timestamp):
                self.build(root=old, status="PASS" if status == "PASS" else "BLOCKED"
                           if status == "BLOCKED" else "PASS", failed_index=0
                           if status == "FAIL" else None, retention_count=32)
        current = parent / "native-aggregate-current"
        value, code = self.build(root=current, status="PASS", retention_count=2)
        self.assertEqual(code, 0)
        self.assertTrue((parent / "native-aggregate-oldest").exists() is False)
        self.assertFalse((parent / "native-aggregate-middle").exists())
        self.assertTrue((parent / "native-aggregate-newest").exists())
        self.assertTrue((parent / "native-aggregate-unresolved").exists())
        self.assertEqual(value["retention"]["before"]["count"], 4)
        self.assertEqual(value["retention"]["before"]["root_count"], 4)
        self.assertEqual(value["retention"]["after"]["count"], 2)

    def test_retention_never_prunes_the_current_root_when_the_clock_moves_back(self):
        parent = self.root / "aggregates-clock-rollback"
        parent.mkdir()
        old = parent / "native-aggregate-old"
        with patch.object(evidence.time, "time_ns", return_value=10):
            self.build(root=old, status="PASS", retention_count=32)
        current = parent / "native-aggregate-current"
        with patch.object(evidence.time, "time_ns", return_value=1):
            value, code = self.build(root=current, status="PASS", retention_count=1)
        self.assertEqual(code, 0)
        self.assertTrue(current.is_dir())
        self.assertFalse(old.exists())
        self.assertEqual(value["retention"]["after"]["root_count"], 1)

    def test_interrupted_retention_resumes_from_its_cleanup_journal(self):
        parent = self.root / "aggregates-retention-replay"
        parent.mkdir()
        old = parent / "native-aggregate-old"
        self.build(root=old, status="PASS", retention_count=32)
        current = parent / "native-aggregate-current"
        matrix = self.make_matrix(current, status="PASS")
        original = common._cleanup_empty
        interrupted = False

        def interrupt_retention(value):
            nonlocal interrupted
            if not interrupted and value.get("label") == "resolved native aggregate retention":
                interrupted = True
                raise RuntimeError("simulated retention interruption")
            return original(value)

        with patch.object(cleanup_journal, "_cleanup_empty", side_effect=interrupt_retention), \
                self.assertRaisesRegex(evidence.EvidenceError, "aggregate_retention_incomplete"):
            evidence.build_aggregate(
                current, matrix, current / evidence.AGGREGATE_RECEIPT_NAME,
                tag="v1.2.3", source_revision="a" * 40, retention_count=1,
            )
        plan = json.loads((current / evidence.RETENTION_PLAN_NAME).read_text(encoding="utf-8"))
        self.assertEqual(plan["state"], "partial")
        value, code = evidence.build_aggregate(
            current, matrix, current / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40, retention_count=1,
        )
        self.assertEqual((code, value["status"]), (0, "PASS"))
        self.assertFalse(old.exists())
        self.assertEqual(
            json.loads((current / evidence.RETENTION_PLAN_NAME).read_text(encoding="utf-8"))["state"],
            "complete",
        )

    def test_new_retention_run_reconciles_an_older_pending_plan_first(self):
        parent = self.root / "aggregates-retention-coordinator"
        parent.mkdir()
        old = parent / "native-aggregate-old"
        self.build(root=old, status="PASS", retention_count=32)
        first = parent / "native-aggregate-first"
        first_matrix = self.make_matrix(first, status="PASS")
        original_prepare = evidence.prepare_directory_cleanup
        interrupted = False

        def interrupt_first_plan(journal, label, identity, quarantine_parent, authorization):
            nonlocal interrupted
            if not interrupted and label == "resolved native aggregate retention":
                interrupted = True
                raise RuntimeError("simulated pending retention plan")
            return original_prepare(
                journal, label, identity, quarantine_parent, authorization,
            )

        with patch.object(retention_operations, "prepare_directory_cleanup", side_effect=interrupt_first_plan), \
                self.assertRaisesRegex(evidence.EvidenceError, "aggregate_retention_incomplete"):
            evidence.build_aggregate(
                first, first_matrix, first / evidence.AGGREGATE_RECEIPT_NAME,
                tag="v1.2.3", source_revision="a" * 40, retention_count=1,
            )
        self.assertTrue(old.is_dir())
        second = parent / "native-aggregate-second"
        value, code = self.build(root=second, status="PASS", retention_count=2)
        self.assertEqual((code, value["status"]), (0, "PASS"))
        self.assertFalse(old.exists())
        first_receipt = first / evidence.AGGREGATE_RECEIPT_NAME
        first_value, _ = evidence.validate_aggregate_receipt(
            first_receipt, expected_tag="v1.2.3", expected_source_revision="a" * 40,
        )
        self.assertTrue(first_value["terminal_evidence_published"])
        self.assertEqual((first_value["status"], first_value["unresolved_case_count"]),
                         ("PASS", 0))
        self.assertEqual(first_value["retention"]["after"]["root_count"], 1)
        self.assertEqual(value["retention"]["after"]["root_count"], 2)
        first_plan = json.loads((first / evidence.RETENTION_PLAN_NAME).read_text(encoding="utf-8"))
        self.assertEqual(first_plan["state"], "complete")

    def test_reconciliation_never_rewrites_a_terminal_aggregate(self):
        parent = self.root / "aggregates-terminal-immutable"
        parent.mkdir()
        first = parent / "native-aggregate-first"
        self.build(root=first, status="PASS", retention_count=1)
        receipt = first / evidence.AGGREGATE_RECEIPT_NAME
        terminal_bytes = receipt.read_bytes()
        plan_path = first / evidence.RETENTION_PLAN_NAME
        plan = json.loads(plan_path.read_text(encoding="utf-8"))
        plan["state"] = "partial"
        evidence._write_atomic(plan_path, plan)

        second = parent / "native-aggregate-second"
        with self.assertRaisesRegex(evidence.EvidenceError,
                                    "aggregate_retention_plan_invalid"):
            self.build(root=second, status="PASS", retention_count=2)

        self.assertEqual(receipt.read_bytes(), terminal_bytes)
        self.assertEqual(json.loads(receipt.read_text(encoding="utf-8"))["status"], "PASS")

    def test_reconciliation_skips_complete_terminal_receipt_from_older_schema(self):
        parent = self.root / "aggregates-terminal-older-schema"
        parent.mkdir()
        first = parent / "native-aggregate-first"
        self.build(root=first, status="PASS", retention_count=1)
        receipt = first / evidence.AGGREGATE_RECEIPT_NAME
        value = json.loads(receipt.read_text(encoding="utf-8"))
        del value["cases"][0]["binary_identities"]["task_ids"]
        evidence._write_atomic(receipt, value, evidence.MAX_AGGREGATE_BYTES)
        terminal_bytes = receipt.read_bytes()
        with self.assertRaises(evidence.EvidenceError):
            evidence.validate_aggregate_receipt(receipt)

        second = parent / "native-aggregate-second"
        current, code = self.build(root=second, status="PASS", retention_count=1)

        self.assertEqual((code, current["status"]), (0, "PASS"))
        self.assertTrue(first.is_dir())
        self.assertEqual(receipt.read_bytes(), terminal_bytes)
        self.assertEqual(current["retention"]["after"]["root_count"], 1)

    def test_retention_never_prunes_pathname_only_unowned_root(self):
        parent = self.root / "aggregates-unowned"
        parent.mkdir()
        old = parent / "native-aggregate-unowned"
        old.mkdir()
        (old / evidence.CASE_ROOT_NAME).mkdir()
        (old / evidence.AGGREGATE_RECEIPT_NAME).write_text(json.dumps({
            "kind": evidence.AGGREGATE_KIND, "status": "PASS",
            "aggregate_root": str(old), "unresolved_case_count": 0,
            "matrix_complete": True, "expected_case_count": len(evidence.EXPECTED_CASES), "case_count": len(evidence.EXPECTED_CASES),
            "cases": [{"status": "PASS", "evidence_status": "RESOLVED"}
                      for _ in evidence.EXPECTED_CASES],
            "completed_at_ns": 1, "terminal_evidence_published": True,
        }), encoding="utf-8")
        current = parent / "native-aggregate-current"
        value, code = self.build(root=current, status="PASS", retention_count=1)
        self.assertEqual(code, 0)
        self.assertTrue(old.exists())
        self.assertEqual(value["retention"]["before"]["root_count"], 1)

    def test_retention_replacement_race_retains_the_unowned_directory(self):
        parent = self.root / "aggregates-retention-race"
        parent.mkdir()
        old = parent / "native-aggregate-old"
        with patch.object(evidence.time, "time_ns", return_value=1):
            self.build(root=old, status="PASS", retention_count=32)
        current = parent / "native-aggregate-current"
        matrix = self.make_matrix(current, status="PASS")
        original_prepare = evidence.prepare_directory_cleanup
        moved = self.root / "retention-original"
        swapped = False

        def swap_before_prepare(journal, label, identity, quarantine_parent, authorization):
            nonlocal swapped
            if label == "resolved native aggregate retention" and Path(identity["path"]) == old:
                old.rename(moved)
                old.mkdir()
                (old / "replacement-marker").write_text("retain", encoding="utf-8")
                swapped = True
            return original_prepare(
                journal, label, identity, quarantine_parent, authorization,
            )

        with patch.object(retention_operations, "prepare_directory_cleanup", side_effect=swap_before_prepare):
            with self.assertRaisesRegex(evidence.EvidenceError, "aggregate_retention_incomplete"):
                evidence.build_aggregate(
                    current, matrix, current / evidence.AGGREGATE_RECEIPT_NAME,
                    tag="v1.2.3", source_revision="a" * 40, retention_count=1,
                )
        self.assertTrue(swapped)
        value = json.loads((current / evidence.AGGREGATE_RECEIPT_NAME).read_text(encoding="utf-8"))
        self.assertEqual((value["status"], value["retention"]["status"]), ("FAIL", "partial"))
        self.assertTrue((old / "replacement-marker").is_file())
        self.assertTrue(moved.is_dir())


if __name__ == "__main__":
    unittest.main()
