"""The native harness proves production OpenCode ACP from admitted task records."""

from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock

from scripts.delegate_acceptance.native import (
    acceptance as gate,
    acceptance_operations as operations_impl,
)
from scripts.delegate_acceptance.native.production_acp import (
    ProductionAcpEvidence,
    production_acp_marker,
)


ROOT = "e" * 32
FRESH = "1" * 32
RESUME = "2" * 32
PREDICATE = {"adapter": "opencode:run", "mode": "workspace-write",
             "version": "acp-v1", "sha256": "f" * 64}


class AcpTask:
    """One on-disk admitted OpenCode task with the records the core writes."""

    def __init__(self, base: Path, task_id: str, *, prior: dict[str, object] | None = None):
        self.task_id = task_id
        self.directory = base / "tasks" / task_id
        (self.directory / "raw").mkdir(parents=True)
        self.record = {"root_id": ROOT, "task_id": task_id, "provider": "opencode:run",
                       "requested_config": {"permission": "workspace-write"},
                       "prior_session": prior}
        gate.write_json(self.directory / "task.json", self.record)
        self.spec_digest = gate.digest(self.directory / "task.json")
        self.meta = {"root_id": ROOT, "task_id": task_id, "spec_sha256": self.spec_digest,
                     "requested_config": {"permission": "workspace-write"},
                     "effective_config": {"policy": {"profile_revision": "opencode-acp-v1"}},
                     "predicate": dict(PREDICATE)}
        self.write_meta()
        gate.write_json(self.directory / "provider.exit", {
            "task_id": task_id, "predicate": dict(PREDICATE), "exit_code": 0, "error": "",
            "raw_manifest": [{"path": "raw/stderr"}, {"path": "raw/stdout"}],
            "manifest_sha256": "a" * 64,
        })
        gate.write_json(self.directory / "outcome.json", {
            "task_id": task_id, "verdict": "committed", "predicate": dict(PREDICATE),
            "evidence_sha256": "a" * 64,
        })

    def write_meta(self) -> None:
        gate.write_json(self.directory / "meta.json", self.meta, replace=True)
        self.meta_digest = gate.digest(self.directory / "meta.json")
        gate.write_json(self.directory / "provider.ref.json", {
            "root_id": ROOT, "task_id": self.task_id, "provider": "opencode:run",
            "spec_sha256": self.spec_digest, "meta_sha256": self.meta_digest,
            "conversation_id": "ses-acp-1",
        }, replace=True)

    def marker(self, committed: bool = True) -> dict[str, object]:
        return production_acp_marker(
            task_id=self.task_id, directory=self.directory, record=self.record, meta=self.meta,
            meta_digest=self.meta_digest, spec_digest=self.spec_digest, committed=committed)


class ProductionAcpMarkerTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-production-acp-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.fresh = AcpTask(self.base, FRESH)

    def test_committed_acp_task_projects_bound_digests_without_raw_output(self):
        marker = self.fresh.marker()
        self.assertEqual(marker, {
            "task_id": FRESH, "session_id": "ses-acp-1", "predecessor_task_id": None,
            "profile_revision": "opencode-acp-v1", "predicate_version": "acp-v1",
            "predicate_sha256": "f" * 64, "spec_sha256": self.fresh.spec_digest,
            "meta_sha256": self.fresh.meta_digest, "noninteractive": True,
            "interaction_journal": "absent", "publication": "committed",
            "manifest_sha256": "a" * 64,
            "seal_sha256": gate.digest(self.fresh.directory / "provider.exit"),
            "outcome_sha256": gate.digest(self.fresh.directory / "outcome.json"),
        })

    def test_resume_binds_the_exact_predecessor_session(self):
        prior = {"provider": "opencode:run", "predecessor_task_id": FRESH,
                 "conversation_id": "ses-acp-1"}
        self.assertEqual(AcpTask(self.base, RESUME, prior=prior).marker()["predecessor_task_id"], FRESH)
        for key, value in (("conversation_id", "ses-fresh-fallback"),
                           ("predecessor_task_id", "not-a-task"), ("provider", "codex:exec")):
            with self.subTest(key=key):
                task = AcpTask(self.base / key, RESUME, prior={**prior, key: value})
                with self.assertRaisesRegex(gate.AcceptanceFailure, "exact predecessor ACP session"):
                    task.marker()

    def test_legacy_run_or_wrong_predicate_is_never_production_acp(self):
        for label, change, message in (
                ("native-v2 run", {"effective_config": {"policy": {"profile_revision": "opencode-native-v2"}}},
                 "production OpenCode ACP profile"),
                ("pure profile", {"effective_config": {}}, "production OpenCode ACP profile"),
                ("run predicate", {"predicate": {**PREDICATE, "version": "native-v1"}},
                 "production ACP predicate"),
                ("other adapter", {"predicate": {**PREDICATE, "adapter": "codex:exec"}},
                 "production ACP predicate")):
            with self.subTest(label=label):
                task = AcpTask(self.base / label.replace(" ", "-"), FRESH)
                task.meta = {**task.meta, **change}
                task.write_meta()
                with self.assertRaisesRegex(gate.AcceptanceFailure, message):
                    task.marker()

    def test_interactive_request_or_interaction_journal_is_refused(self):
        for target in ("record", "meta"):
            with self.subTest(target=target):
                task = AcpTask(self.base / target, FRESH)
                requested = getattr(task, target)["requested_config"]
                requested["interactive"] = True
                with self.assertRaisesRegex(gate.AcceptanceFailure, "requested interactive"):
                    task.marker()
        for entry in ("interaction", ".interaction.lock", "raw/interaction"):
            with self.subTest(entry=entry):
                task = AcpTask(self.base / entry.replace("/", "-"), FRESH)
                (task.directory / entry).mkdir()
                with self.assertRaisesRegex(gate.AcceptanceFailure, "user interaction journal"):
                    task.marker()

    def test_session_and_core_seal_tampering_is_refused(self):
        reference = gate.read_json(self.fresh.directory / "provider.ref.json")
        gate.write_json(self.fresh.directory / "provider.ref.json",
                        {**reference, "meta_sha256": "0" * 64}, replace=True)
        with self.assertRaisesRegex(gate.AcceptanceFailure, "exact ACP session binding"):
            self.fresh.marker()
        for index, (name, change) in enumerate((
                ("provider.exit", {"predicate": {**PREDICATE, "version": "native-v1"}}),
                ("provider.exit", {"raw_manifest": [{"path": "raw/interaction"}]}),
                ("outcome.json", {"verdict": "rejected"}),
                ("outcome.json", {"evidence_sha256": "b" * 64}))):
            with self.subTest(name=name, change=change):
                task = AcpTask(self.base / f"seal-{index}", FRESH)
                record = gate.read_json(task.directory / name)
                gate.write_json(task.directory / name, {**record, **change}, replace=True)
                with self.assertRaisesRegex(gate.AcceptanceFailure, "committed production ACP core seal"):
                    task.marker()

    def test_uncommitted_timeout_source_records_only_that_it_published_nothing(self):
        (self.fresh.directory / "outcome.json").unlink()
        marker = self.fresh.marker(committed=False)
        self.assertEqual(
            (marker["publication"], marker["manifest_sha256"], marker["seal_sha256"],
             marker["outcome_sha256"]), ("unpublished", None, None, None))
        published = AcpTask(self.base / "published", FRESH)
        with self.assertRaisesRegex(gate.AcceptanceFailure, "published an outcome"):
            published.marker(committed=False)


class ProductionAcpOwnerTests(unittest.TestCase):
    def test_owner_records_each_task_once_and_requires_exact_coverage(self):
        owner = ProductionAcpEvidence()
        marker = {"task_id": FRESH, "publication": "committed"}
        owner.record(FRESH, marker)
        owner.record(FRESH, dict(marker))
        with self.assertRaisesRegex(gate.AcceptanceFailure, "changed its production ACP marker"):
            owner.record(FRESH, {**marker, "publication": "unpublished"})
        with self.assertRaisesRegex(gate.AcceptanceFailure, "marker identity"):
            owner.record(RESUME, marker)
        with self.assertRaisesRegex(gate.AcceptanceFailure, "coverage is incomplete"):
            owner.receipt({"fresh": FRESH, "resume": RESUME})
        receipt = owner.receipt({"fresh": FRESH})
        self.assertEqual((receipt["kind"], receipt["profile_revision"], receipt["tasks"]),
                         ("opencode-production-acp", "opencode-acp-v1", {"fresh": marker}))

    def test_success_receipt_projection_is_opencode_only(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.provider = "codex:exec"
        self.assertEqual(operations_impl.production_acp_receipt(operation, {"fresh": FRESH}), {})
        operation.provider = "opencode:run"
        with self.assertRaisesRegex(gate.AcceptanceFailure, "coverage is incomplete"):
            operation.production_acp_receipt({"fresh": FRESH})
        operation._production_acp.record(FRESH, {"task_id": FRESH})
        self.assertEqual(operation.production_acp_receipt({"fresh": FRESH})["production_acp"]["tasks"],
                         {"fresh": {"task_id": FRESH}})


class ProductionAcpBindingWiringTests(unittest.TestCase):
    def operation(self, provider: str, task: AcpTask) -> object:
        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = ROOT
        operation.provider = provider
        operation._request_binding = Mock(return_value=(
            task.directory, task.record, task.meta, task.meta_digest,
            task.spec_digest, task.directory / "brief.md"))
        operation._validate_task_selection = Mock()
        operation._validate_task_policy = Mock(return_value=PREDICATE)
        operation._validate_task_provider_receipts = Mock(return_value="a" * 64)
        operation._validate_task_outcome = Mock()
        return operation

    def test_task_binding_records_the_marker_from_the_admitted_records(self):
        with tempfile.TemporaryDirectory(prefix="native-acp-binding-") as directory:
            task = AcpTask(Path(directory), FRESH)
            operation = self.operation("opencode:run", task)
            operation.task_binding(FRESH)
            self.assertEqual(operation._production_acp.receipt({"fresh": FRESH})["tasks"]["fresh"],
                             task.marker())
            codex = self.operation("codex:exec", task)
            codex.task_binding(FRESH)
            self.assertEqual(codex.production_acp_receipt({"fresh": FRESH}), {})
            with self.assertRaisesRegex(gate.AcceptanceFailure, "coverage is incomplete"):
                codex._production_acp.receipt({"fresh": FRESH})

            task.meta["effective_config"]["policy"]["profile_revision"] = "opencode-native-v2"
            task.write_meta()
            legacy = self.operation("opencode:run", task)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "production OpenCode ACP profile"):
                legacy.task_binding(FRESH)

    def test_timeout_source_binding_records_an_unpublished_marker(self):
        with tempfile.TemporaryDirectory(prefix="native-acp-timeout-binding-") as directory:
            task = AcpTask(Path(directory), FRESH)
            (task.directory / "outcome.json").unlink()
            operation = self.operation("opencode:run", task)
            operation.state = Path(directory)
            operation.mode = "workspace-write"
            operation.args = SimpleNamespace(model=None, effort=None)
            self.assertEqual(operation.timeout_source_binding(FRESH), task.directory)
            marker = operation._production_acp.receipt({"timeout_source": FRESH})["tasks"]
            self.assertEqual(marker["timeout_source"]["publication"], "unpublished")
