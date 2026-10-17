"""Hermetic oracles for a native acceptance behavior cohort."""
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from scripts.delegate_acceptance.native import (
    acceptance as gate,
    acceptance_collection as collection_impl,
)


class NativeAcceptanceOracleTests(unittest.TestCase):
    def test_read_only_brief_and_answer_require_the_random_marker(self):
        with patch.object(gate.secrets, "token_hex", side_effect=["a" * 48, "b" * 48]):
            first_marker = gate.random_marker()
            second_marker = gate.random_marker()
        self.assertNotEqual(first_marker, second_marker)
        marker_text = first_marker.decode().removesuffix("\n")
        expected = gate.marker_answer(marker_text, continuation=False)
        continued = gate.marker_answer(marker_text, continuation=True)
        gate.verify_read_only_answer(expected.encode(), expected, "fresh")
        gate.verify_read_only_answer((" \n" + expected + "\n ").encode(), expected, "fresh")
        gate.verify_read_only_answer((continued + "\n").encode(), continued, "resume")
        for answer in (b"arbitrary success", (expected + " extra").encode()):
            with self.assertRaises(gate.AcceptanceFailure):
                gate.verify_read_only_answer(answer, expected, "fresh")

        with tempfile.TemporaryDirectory(prefix="native-brief-unit-") as directory:
            operation = object.__new__(gate.NativeAcceptance)
            operation.mode = "read-only"
            operation.marker_text = marker_text
            operation.workspace_marker = Path(directory) / "workspace" / "acceptance-marker.txt"
            operation.briefs = Path(directory) / "briefs"
            operation.briefs.mkdir()
            fresh = operation.brief("fresh")
            resume = operation.brief("resume", continuation=True)
            fresh_text = fresh.read_text()
            resume_text = resume.read_text()
            self.assertIn("DELEGATION_LIVE_OK:", fresh_text)
            self.assertIn("DELEGATION_LIVE_CONTINUED:", resume_text)
            self.assertNotIn(marker_text, fresh_text)
            self.assertNotIn(marker_text, resume_text)
            for text in (fresh_text, resume_text):
                self.assertIn(str(operation.workspace_marker), text)
                self.assertIn("file-read tool or a read-only shell command", text)

    def test_antigravity_read_only_brief_requires_file_read_tool(self):
        with tempfile.TemporaryDirectory(prefix="native-read-brief-unit-") as directory:
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "antigravity:print"
            operation.mode = "read-only"
            operation.briefs = Path(directory)
            operation.workspace_marker = Path(directory) / "acceptance-marker.txt"
            path = operation.brief("fresh")
            text = path.read_text()
            self.assertIn("provider's file-read tool only", text)
            self.assertIn("Do not use a command or shell tool", text)

    def test_pi_runtime_entries_are_limited_to_workspace_write(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.provider = "pi:json"
        operation.mode = "workspace-write"
        self.assertTrue(operation._is_pi_runtime_entry(".pi"))
        self.assertTrue(operation._is_pi_runtime_entry(".pi/tasks/session/file"))
        operation.mode = "read-only"
        self.assertFalse(operation._is_pi_runtime_entry(".pi/tasks/session/file"))
        operation.provider = "antigravity:print"
        operation.mode = "workspace-write"
        self.assertFalse(operation._is_pi_runtime_entry(".pi/tasks/session"))

    def test_pi_runtime_roots_must_be_directories(self):
        with tempfile.TemporaryDirectory(prefix="native-pi-runtime-unit-") as directory:
            workspace = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "pi:json"
            operation.mode = "workspace-write"
            operation.workspace = workspace
            operation._validate_pi_runtime_roots()
            (workspace / ".pi").write_text("invalid runtime root")
            with self.assertRaisesRegex(gate.AcceptanceFailure, "not a directory"):
                operation._validate_pi_runtime_roots()

            (workspace / ".pi").unlink()
            (workspace / ".pi").mkdir()
            (workspace / ".pi" / "tasks").write_text("invalid task root")
            with self.assertRaisesRegex(gate.AcceptanceFailure, "not a directory"):
                operation._validate_pi_runtime_roots()

    def test_replay_delegates_complete_collection_receipts(self):
        payload = {"basename": "result.txt", "length": 1, "sha256": "a" * 64}
        fresh_outcome = {"verdict": "committed", "payload": payload,
                         "evidence_sha256": "b" * 64}
        resume_payload = dict(payload, sha256="c" * 64)
        resume_outcome = {"verdict": "committed", "payload": resume_payload,
                          "evidence_sha256": "d" * 64}
        captured = []

        def replay(records):
            captured.append(records)

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "e" * 32
        operation.ops = SimpleNamespace(root_id=None, replay=replay)
        operation.records = {
            "fresh-collect": {"directory": Path("/fixture/fresh"),
                               "outcome": fresh_outcome, "payload": payload},
            "resume-collect": {"directory": Path("/fixture/resume"),
                                "outcome": resume_outcome, "payload": resume_payload},
        }

        operation.replay()

        self.assertEqual(operation.ops.root_id, operation.root_id)
        self.assertEqual(len(captured), 1)
        records = captured[0]
        self.assertEqual(records["fresh"]["directory"], Path("/fixture/fresh"))
        self.assertEqual(records["fresh"]["payload"], payload)
        self.assertEqual(records["fresh"]["outcome"]["evidence_sha256"], "b" * 64)
        self.assertEqual(records["resume"]["payload"], resume_payload)
        self.assertEqual(records["resume"]["outcome"]["evidence_sha256"], "d" * 64)

    def test_session_and_replay_oracles_are_strict(self):
        gate.require_same_session("session-1", "session-1")
        with self.assertRaises(gate.AcceptanceFailure):
            gate.require_same_session("session-1", "session-2")

        before = {"outcome.json": {"sha256": "a"}}
        gate.require_replay_unchanged(before, {"outcome.json": {"sha256": "a"}})
        with self.assertRaises(gate.AcceptanceFailure):
            gate.require_replay_unchanged(before, {"outcome.json": {"sha256": "b"}})

    def test_task_binding_uses_validated_provider_manifest_digest(self):
        root_id = "a" * 32
        task_id = "task-binding-manifest"
        meta_digest = "b" * 64
        spec_digest = "c" * 64
        manifest_digest = "d" * 64
        predicate = {"adapter": "codex:exec", "mode": "read-only",
                     "version": "fixture-v1", "sha256": "e" * 64}
        directory = Path("/fixture/tasks") / task_id
        supervisor = {"client_executable": "/fixture/pueue",
                      "config_path": "/fixture/pueue.yml",
                      "observed_version": "pueue fixture-v1"}
        receipts = {
            "submit.json": {
                "root_id": root_id, "task_id": task_id,
                "spec_sha256": spec_digest, "meta_sha256": meta_digest,
                "label": f"delegate:{root_id}:{task_id}", "supervisor": supervisor,
            },
            "provider.exit": {
                "root_id": root_id, "task_id": task_id,
                "spec_sha256": spec_digest, "meta_sha256": meta_digest,
                "invocation_state": "started", "exit_code": 0, "error": "",
                "predicate": predicate, "raw_manifest": [],
                "manifest_sha256": manifest_digest,
            },
            "provider.ref.json": {
                "root_id": root_id, "task_id": task_id,
                "spec_sha256": spec_digest, "meta_sha256": meta_digest,
                "provider": "codex:exec", "conversation_id": "session-1",
            },
            "outcome.json": {
                "root_id": root_id, "task_id": task_id,
                "spec_sha256": spec_digest, "meta_sha256": meta_digest,
                "verdict": "committed", "predicate": predicate,
                "evidence_sha256": manifest_digest,
            },
        }
        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = root_id
        operation.provider = "codex:exec"
        operation.pueue = Path("/fixture/pueue")
        operation.config = Path("/fixture/pueue.yml")
        operation.pueue_version = "pueue fixture-v1"
        operation._request_binding = Mock(return_value=(
            directory, {"prior_session": None}, {}, meta_digest,
            spec_digest, directory / "task.json"))
        operation._validate_task_selection = Mock()
        operation._validate_task_policy = Mock(return_value=predicate)
        operation._read_bound_provider_evidence = Mock()

        def read_receipt(path):
            return receipts[Path(path).name]

        with patch.object(collection_impl, "read_json", side_effect=read_receipt):
            operation.task_binding(task_id)
            receipts["outcome.json"]["evidence_sha256"] = "f" * 64
            with self.assertRaisesRegex(gate.AcceptanceFailure, "outcome authority binding"):
                operation.task_binding(task_id)
