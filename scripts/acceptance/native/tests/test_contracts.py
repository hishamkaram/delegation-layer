"""Hermetic oracles for a native acceptance behavior cohort."""

from argparse import Namespace
from copy import deepcopy
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from scripts.acceptance.native import workflow as gate
from scripts.acceptance.native import collection as collection_impl
from scripts.acceptance.native.binding_policy import _validate_bound_provider_record
from scripts.acceptance.native.bindings import ProviderRuntimeBindings
from scripts.acceptance.native.constants import case_task_budget


class OptionalRuntimeDiagnosticsTests(unittest.TestCase):
    def setUp(self):
        self.task_id = "a" * 32
        self.meta: dict[str, object] = {"provider_executable": "/admitted/provider", "provider_version": ""}
        self.started: dict[str, object] = {"provider_executable": "/upgraded/provider"}
        self.bindings = ProviderRuntimeBindings()

    def test_launch_without_diagnostics_uses_actual_path(self):
        identity = self.bindings.record(
            self.task_id, self.meta, Path("/admitted/provider"),
            launch_evidence=(self.started, dict(self.started)))
        self.assertEqual(identity, {"executable": "/upgraded/provider", "version": ""})
        self.assertEqual(self.bindings.receipt({self.task_id})["provider_versions"], {self.task_id: ""})

    def test_historical_fallback_does_not_require_policy_hash(self):
        self.assertEqual(self.bindings.record(self.task_id, self.meta, Path("/admitted/provider")),
                         {"executable": "/admitted/provider", "version": ""})

    def test_historical_fallback_retains_and_validates_optional_hash(self):
        for digest in ("b" * 64, "", None, "bad"):
            with self.subTest(digest=digest):
                bindings = ProviderRuntimeBindings()
                meta = dict(self.meta, effective_config={"policy": {"runtime_sha256": digest}})
                if digest in ("b" * 64, ""):
                    self.assertEqual(bindings.record(self.task_id, meta, Path("/admitted/provider"))[
                        "sha256"], digest)
                else:
                    with self.assertRaises(gate.AcceptanceFailure):
                        bindings.record(self.task_id, meta, Path("/admitted/provider"))

    def test_started_envelope_allows_absent_diagnostics(self):
        record = dict(self.started, schema_version=1, root_id="b" * 32, task_id=self.task_id,
                      spec_sha256="c" * 64, meta_sha256="d" * 64,
                      started_at="2026-10-15T08:21:52Z", diagnostic_nanos=0)
        self.assertEqual(_validate_bound_provider_record(
            record, "provider.started.json", "b" * 32, self.task_id, "d" * 64, "c" * 64), record)
        for field, value in (("provider_version", None), ("provider_sha256", None),
                             ("provider_version", " \t"), ("provider_sha256", "bad")):
            with self.subTest(field=field, value=value), self.assertRaises(gate.AcceptanceFailure):
                _validate_bound_provider_record(dict(record, **{field: value}),
                    "provider.started.json", "b" * 32, self.task_id, "d" * 64, "c" * 64)

    def test_start_exit_mismatch_and_task_coverage_stay_strict(self):
        for field, value in (("provider_executable", "/another/provider"),
                             ("provider_version", "upgraded")):
            with self.subTest(field=field), self.assertRaisesRegex(
                    gate.AcceptanceFailure, "runtime identities differ"):
                self.bindings.record(self.task_id, self.meta, Path("/admitted/provider"),
                    launch_evidence=(self.started, dict(self.started, **{field: value})))
        self.bindings.record(self.task_id, self.meta, Path("/admitted/provider"),
                             started_evidence=self.started)
        with self.assertRaisesRegex(gate.AcceptanceFailure, "coverage is incomplete"):
            self.bindings.receipt({self.task_id, "b" * 32})
        with self.assertRaisesRegex(gate.AcceptanceFailure, "changed its admitted"):
            self.bindings.record(self.task_id, self.meta, Path("/admitted/provider"),
                                 started_evidence={"provider_executable": "/other/provider"})


class NativeTaskBindingFixture:
    """Own consumer record fields while substituting filesystem observations."""

    def __init__(self, root: Path, *, pending: bool = False):
        self.workspace = root / "workspace"
        self.task_id = "b" * 32
        self.root_id = "a" * 32
        self.directory = root / "state" / "tasks" / self.task_id
        self.budget = case_task_budget("codex:exec")
        self.bound = {"root_id": self.root_id, "task_id": self.task_id,
                      "meta_sha256": "b" * 64, "spec_sha256": "c" * 64}
        self.predicate = {"adapter": "codex:exec", "mode": "read-only",
                          "version": "fixture-v1", "sha256": "e" * 64}
        self.policy = {"workspace": str(self.workspace), "runtime_sha256": "",
                       "profile_revision": "native-permissions-v1"}
        self.meta = self.metadata(root, pending)
        self.records = self.documents(root)
        self.reads = []
        self.operation = self.acceptance(root)

    def metadata(self, root: Path, pending: bool) -> dict[str, object]:
        meta = {"root_id": self.root_id, "task_id": self.task_id, "spec_sha256": "c" * 64,
                "provider_executable": str(root / "provider-old"),
                "provider_version": "", "containment": "read-only", "approval": "plan",
                "effective_config": {"containment": "read-only", "approval": "plan",
                                     "digest": "d" * 64, "policy": self.policy},
                "predicate": self.predicate}
        if pending:
            meta.update(runtime_preflight_sha256="e" * 64, environment_recorded=True)
        return meta

    def documents(self, root: Path) -> dict[str, dict[str, object]]:
        launch = {"provider_executable": str(root / "provider-new")}
        return {
            "task.json": {"root_id": self.root_id, "task_id": self.task_id,
                          "provider": "codex:exec", "mode": "read-only",
                          "canonical_cwd": str(self.workspace),
                          "requested_config": {"permission": "read-only", "budget": self.budget.canonical},
                          "budget_nanos": self.budget.nanos, "brief_length": 6,
                          "brief_sha256": "f" * 64, "prior_session": None},
            "meta.json": self.meta,
            "submit.json": {**self.bound, "label": f"delegate:{self.root_id}:{self.task_id}",
                            "supervisor": {"client_executable": str(root / "pueue"),
                                           "config_path": str(root / "pueue.yml"),
                                           "observed_version": "pueue fixture-v1"}},
            "provider.start": {**self.bound, "schema_version": 1, "budget_nanos": self.budget.nanos,
                               "created_at": "2026-10-15T08:21:52Z"},
            "provider.started.json": {**self.bound, **launch, "schema_version": 1,
                                      "started_at": "2026-10-15T08:21:52Z", "diagnostic_nanos": 0},
            "provider.exit": {**self.bound, **launch, "invocation_state": "started", "exit_code": 0,
                              "error": "", "predicate": self.predicate, "raw_manifest": [],
                              "manifest_sha256": "d" * 64},
            "provider.ref.json": {**self.bound, "provider": "codex:exec", "conversation_id": "session-1"},
            "outcome.json": {**self.bound, "verdict": "committed", "predicate": self.predicate,
                             "evidence_sha256": "d" * 64},
        }

    def acceptance(self, root: Path):
        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id, operation.state = self.root_id, root / "state"
        operation.workspace, operation.provider = self.workspace, "codex:exec"
        operation.mode, operation.scenario = "read-only", "read-only"
        operation.provider_executable = root / "provider-old"
        # An earlier observation already emitted the unrelated discovery receipt.
        operation.provider_version = ""
        operation.pueue, operation.config = root / "pueue", root / "pueue.yml"
        operation.pueue_version = "pueue fixture-v1"
        operation.args = Namespace(model=None, effort=None)
        operation._provider_bindings = ProviderRuntimeBindings()
        return operation

    def read_record(self, path):
        self.reads.append(Path(path).name)
        return deepcopy(self.records[Path(path).name])

    def bind(self) -> None:
        digests = {"task.json": "c" * 64, "meta.json": "b" * 64, "brief.md": "f" * 64}
        with patch.object(collection_impl, "read_json", side_effect=self.read_record), \
             patch.object(collection_impl, "digest", side_effect=lambda path: digests[Path(path).name]), \
             patch.multiple(Path, is_dir=Mock(return_value=True), is_symlink=Mock(return_value=False),
                            is_file=Mock(return_value=True), stat=Mock(return_value=SimpleNamespace(st_size=6))):
            self.operation.task_binding(self.task_id)


class NativePolicyBindingTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-policy-unit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()

    def test_current_pending_policy_accepts_empty_diagnostic(self):
        fixture = NativeTaskBindingFixture(self.root, pending=True)
        fixture.bind()
        self.assertIn("outcome.json", fixture.reads)

    def test_historical_policy_does_not_pin_launch_diagnostics(self):
        for launch_digest in (None, "2" * 64):
            with self.subTest(launch_digest=launch_digest):
                fixture = NativeTaskBindingFixture(self.root)
                fixture.policy["runtime_sha256"] = "1" * 64
                fixture.meta["provider_version"] = "admission-v1"
                for name in ("provider.started.json", "provider.exit"):
                    fixture.records[name]["provider_version"] = "launch-v2"
                    if launch_digest is not None:
                        fixture.records[name]["provider_sha256"] = launch_digest
                fixture.bind()
                self.assertIn("outcome.json", fixture.reads)

    def test_policy_diagnostic_requires_present_valid_string(self):
        damages = (
            lambda policy: policy.pop("runtime_sha256"),
            *(lambda policy, value=value: policy.update(runtime_sha256=value)
              for value in (None, "bad", "A" * 64, "a" * 63, 0, False)),
        )
        for pending in (False, True):
            for index, damage in enumerate(damages):
                with self.subTest(pending=pending, damage=index):
                    fixture = NativeTaskBindingFixture(self.root, pending=pending)
                    damage(fixture.policy)
                    with self.assertRaises(gate.AcceptanceFailure):
                        fixture.bind()

    def test_other_contracts_and_start_exit_consistency_stay_strict(self):
        damages = (
            lambda fixture: fixture.meta.update(containment="workspace-write"),
            lambda fixture: fixture.policy.update(workspace=str(self.root / "other")),
            lambda fixture: fixture.predicate.update(sha256="bad"),
            lambda fixture: fixture.records["provider.exit"].update(provider_executable=str(self.root / "other")),
            lambda fixture: fixture.records["task.json"]["requested_config"].update(model="other"),
        )
        for index, damage in enumerate(damages):
            with self.subTest(damage=index):
                fixture = NativeTaskBindingFixture(self.root)
                damage(fixture)
                with self.assertRaises(gate.AcceptanceFailure):
                    fixture.bind()


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
        ops = SimpleNamespace(root_id=None, replay=replay)
        operation.records = {
            "fresh-collect": {"directory": Path("/fixture/fresh"),
                               "outcome": fresh_outcome, "payload": payload},
            "resume-collect": {"directory": Path("/fixture/resume"),
                                "outcome": resume_outcome, "payload": resume_payload},
        }

        with patch.object(operation, "ops", ops, create=True):
            operation.replay()

        self.assertEqual(ops.root_id, operation.root_id)
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

        before: dict[str, dict[str, object]] = {"outcome.json": {"sha256": "a"}}
        gate.require_replay_unchanged(before, {"outcome.json": {"sha256": "a"}})
        with self.assertRaises(gate.AcceptanceFailure):
            gate.require_replay_unchanged(before, {"outcome.json": {"sha256": "b"}})

    def test_task_binding_uses_validated_provider_manifest_digest(self):
        with tempfile.TemporaryDirectory(prefix="native-manifest-unit-") as directory:
            fixture = NativeTaskBindingFixture(Path(directory).resolve())
            fixture.bind()
            fixture.records["outcome.json"]["evidence_sha256"] = "f" * 64
            with self.assertRaisesRegex(gate.AcceptanceFailure, "outcome authority binding"):
                fixture.bind()
