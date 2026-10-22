"""Application recovery re-enters projection and canonical stage publication."""

from __future__ import annotations

from contextlib import ExitStack, contextmanager
from dataclasses import asdict
from collections.abc import Callable, Iterator, Mapping
from pathlib import Path
import tempfile
import unittest
from typing import Protocol
from unittest.mock import Mock, patch

from scripts import test_public_native_proof as proof_fixtures
from scripts import test_stage_publication as stage_fixtures
from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.bounded_files import BoundedReadError
from scripts.delegate_acceptance.native_evidence.stage_publication import StagePlan, publish_stage
from scripts.delegate_acceptance.native_evidence.staging import _check_source_digests
from scripts.delegate_acceptance.native_release import coordinator, projection
from scripts.delegate_acceptance.native_release.contract import Invocation, ReleaseError, ReleaseRequest
from scripts.delegate_acceptance.native_release.execution import file_sha, read_object
from scripts.delegate_acceptance.native_release.receipt import Receipt


PAID_ENTRYPOINTS = ("download", "archive", "native", "finalize")
# The canonical proof fixture records this model on every case, AGY included.
FIXTURE_AGY_MODEL = "fixture-model"


class StageCallback(Protocol):
    def __call__(self, aggregate: Path, output: Path, *, archive_receipt: Path,
                 native_receipt: Path, expected_tag: str, expected_source_revision: str,
                 directory_identity: dict[str, object],
                 expected_source_digests: dict[str, str]) -> None: ...


class _StageSourceBoundary:
    """Load invocation summaries, then use the canonical digest gate and publisher."""

    def __init__(self, case: ProjectionFixture, receipt: Receipt,
                 source: Path, interrupt: str | None):
        self.case = case
        self.receipt = receipt
        self.source = source
        self.interrupt = interrupt
        self.calls = 0
        self.publishes = 0

    def __call__(self, aggregate: Path, output: Path, *, archive_receipt: Path,
                 native_receipt: Path, expected_tag: str, expected_source_revision: str,
                 directory_identity: dict[str, object],
                 expected_source_digests: dict[str, str]) -> None:
        self.calls += 1
        invocation = self.receipt.invocation
        self.case.assertEqual(self.receipt.stage("project").state, "intended")
        self.case.assertEqual(aggregate, invocation.aggregate)
        self.case.assertEqual(archive_receipt, invocation.archive_receipt)
        self.case.assertEqual(native_receipt, invocation.native_receipt)
        self.case.assertEqual(expected_tag, invocation.request.tag)
        self.case.assertEqual(expected_source_revision,
                              self.case.expectation.harness_revision)
        self.case.assertEqual(directory_identity,
                              self.receipt.stage("download").result["stage_identity"])
        self.case.assertEqual(expected_source_digests,
                              projection.projection_source_digests(self.receipt))
        source_paths = {
            "aggregate-receipt.json": aggregate,
            "archive-receipt.json": archive_receipt,
            "native-acceptance-receipt.json": native_receipt,
        }
        fixture_plan = stage_fixtures._load_plan(self.source, output)
        plan = StagePlan(output)
        for item in fixture_plan.files():
            source_path = source_paths.get(item.name)
            data = item.data if source_path is None else source_path.read_bytes()
            plan.put(output / item.name, data, item.limit)
        _check_source_digests(plan, expected_source_digests)
        if self.calls == 1 and self.interrupt == "before-publish":
            raise KeyboardInterrupt
        publish_stage(plan, directory_identity)
        self.publishes += 1
        if self.calls == 1 and self.interrupt == "after-manifest":
            self.case.assertTrue((output / "manifest.json").is_file())
            raise KeyboardInterrupt


class ProjectionFixture(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="projection-source-", dir=Path.home())
        cls.addClassCleanup(temporary.cleanup)
        cls.source_root = Path(temporary.name).resolve(strict=True)
        producer = proof_fixtures._PublicNativeFixture(cls.source_root)
        cls.source = producer.build_stage()
        if producer.expectation is None:
            raise AssertionError("public proof fixture did not produce an expectation")
        cls.expectation = producer.expectation
        cls.source_snapshot = stage_fixtures._snapshot(cls.source)

    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="projection-invocation-", dir=Path.home())
        self.addCleanup(temporary.cleanup)
        self.area = Path(temporary.name).resolve(strict=True)
        self.receipt = self._create_receipt()

    def tearDown(self) -> None:
        self.assertEqual(stage_fixtures._snapshot(self.source), self.source_snapshot)

    def _create_receipt(self, agy_model: str | None = FIXTURE_AGY_MODEL,
                        label: str | None = None) -> Receipt:
        expected = self.expectation
        request = ReleaseRequest(expected.repo, expected.tag, expected.opencode_model,
                                 expected.ccr_model, False, agy_model=agy_model)
        invocation_id = "a" * 32
        area = self.area if label is None else self.area / label
        area.mkdir(exist_ok=True)
        root = area / f"{request.tag}-{invocation_id}"
        checkout = area / "checkout"
        checkout.mkdir()
        owner = DirectoryOwner()
        try:
            root_token = owner.create("projection invocation", root, area)
            root_identity = directory_identity_record(root_token)
            stage_token = owner.create("projection private stage", root / "private-stage", root)
            stage_identity = directory_identity_record(stage_token)
        finally:
            owner.release()
        invocation = Invocation(
            invocation_id, request, checkout, root, expected.platform_os,
            expected.platform_arch, root_identity,
        )
        receipt = Receipt.create(invocation, ["native-release", "resume"])
        self._copy_source_summaries(invocation)
        self._complete_input_stages(receipt, stage_identity)
        self.assertEqual(projection.expectation(receipt), expected)
        self.assertTrue(root.is_relative_to(Path.home().resolve()))
        return receipt

    def _copy_source_summaries(self, invocation: Invocation) -> None:
        for name, target in (
            ("aggregate-receipt.json", invocation.aggregate),
            ("archive-receipt.json", invocation.archive_receipt),
            ("native-acceptance-receipt.json", invocation.native_receipt),
        ):
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes((self.source / name).read_bytes())

    def _complete_input_stages(self, receipt: Receipt,
                               stage_identity: dict[str, object]) -> None:
        expected = self.expectation
        archive_sha = file_sha(receipt.invocation.archive_receipt)
        aggregate_sha = file_sha(receipt.invocation.aggregate)
        native_sha = file_sha(receipt.invocation.native_receipt)
        receipt.intend("preflight", {"request": receipt.invocation.request.document()})
        receipt.complete("preflight", {
            "release_revision": expected.release_revision,
            "harness_revision": expected.harness_revision,
            "source_harness_sha256": expected.source_harness_sha256,
            "candidate_harness": False,
        })
        receipt.intend("download", {"tag": expected.tag, "assets": "fixture-input"})
        receipt.complete("download", {"stage_identity": stage_identity})
        receipt.intend("archive", {"tag": expected.tag})
        receipt.complete("archive", {"receipt_sha256": archive_sha})
        receipt.intend("native", {"tag": expected.tag})
        receipt.complete("native", {"aggregate_sha256": aggregate_sha, "exit_code": 0})
        receipt.intend("finalize", {"tag": expected.tag})
        receipt.complete("finalize", {"receipt_sha256": native_sha})

    @contextmanager
    def resume_ports(self, callback: StageCallback) -> Iterator[dict[str, Mock]]:
        """Keep recovery real; isolate observations and all external submission edges."""
        with ExitStack() as stack:
            ports = {
                name: stack.enter_context(patch.object(coordinator, name))
                for name in PAID_ENTRYPOINTS
            }
            ports["observe_native"] = stack.enter_context(patch.object(
                coordinator, "observe_native",
                side_effect=lambda current: current.stage("native").result,
            ))
            ports["github_client"] = stack.enter_context(
                patch.object(coordinator, "github_client"),
            )
            ports["submit"] = stack.enter_context(patch.object(coordinator, "submit"))
            stack.enter_context(patch.object(projection, "stage_evidence", side_effect=callback))
            yield ports

    def assert_no_paid_entrypoints(self, ports: Mapping[str, Mock]) -> None:
        for name in PAID_ENTRYPOINTS:
            ports[name].assert_not_called()

    def assert_project_intent(self, receipt: Receipt) -> None:
        intent = receipt.stage("project").intent
        self.assertEqual(intent["expectation"], asdict(self.expectation))
        self.assertEqual(intent["directory_identity"],
                         receipt.stage("download").result["stage_identity"])
        self.assertEqual(intent["source_digests"],
                         projection.projection_source_digests(receipt))

    def assert_public_proof_passes(self, receipt: Receipt) -> None:
        self.assertEqual(receipt.stage("project").state, "complete")
        proof, facts = projection.read_public(receipt)
        self.assertTrue(proof)
        self.assertEqual((facts["status"], facts["case_results_verdict"]), ("PASS", "PASS"))

    def assert_copied_summaries_unchanged(self, receipt: Receipt) -> None:
        for name, target in (
            ("aggregate-receipt.json", receipt.invocation.aggregate),
            ("archive-receipt.json", receipt.invocation.archive_receipt),
            ("native-acceptance-receipt.json", receipt.invocation.native_receipt),
        ):
            self.assertEqual((self.source / name).read_bytes(), target.read_bytes())


class ProjectionRecoveryTests(ProjectionFixture):
    def test_resume_after_project_intent_republishes_and_validates_public_proof(self) -> None:
        boundary = _StageSourceBoundary(
            self, self.receipt, self.source, interrupt="before-publish",
        )
        with self.resume_ports(boundary) as ports:
            with self.assertRaises(KeyboardInterrupt):
                coordinator.resume(self.receipt)
            self.assertEqual(self.receipt.stage("project").state, "intended")
            self.assert_project_intent(self.receipt)
            self.assert_no_paid_entrypoints(ports)
        resumed = Receipt.load(self.receipt.path)
        boundary.receipt = resumed
        with self.resume_ports(boundary) as ports:
            coordinator.resume(resumed)
            self.assert_no_paid_entrypoints(ports)
            ports["submit"].assert_called_once()
            ports["github_client"].assert_called_once()
        self.assertEqual(boundary.calls, 2)
        self.assertEqual(boundary.publishes, 1)
        self.assert_public_proof_passes(resumed)
        self.assert_copied_summaries_unchanged(resumed)

    def test_resume_replays_manifest_committed_stage_before_building_proof(self) -> None:
        boundary = _StageSourceBoundary(
            self, self.receipt, self.source, interrupt="after-manifest",
        )
        stage = self.receipt.invocation.root / "private-stage"
        with self.resume_ports(boundary) as ports:
            with self.assertRaises(KeyboardInterrupt):
                coordinator.resume(self.receipt)
            self.assertTrue((stage / "manifest.json").is_file())
            self.assertFalse(self.receipt.invocation.proof.exists())
            self.assert_no_paid_entrypoints(ports)
        before = stage_fixtures._snapshot(stage)
        before_mtimes = {path: path.stat().st_mtime_ns for path in stage.rglob("*")
                         if path.is_file()}
        resumed = Receipt.load(self.receipt.path)
        boundary.receipt = resumed
        with self.resume_ports(boundary) as ports:
            coordinator.resume(resumed)
            self.assert_no_paid_entrypoints(ports)
        after_mtimes = {path: path.stat().st_mtime_ns for path in stage.rglob("*")
                        if path.is_file()}
        self.assertEqual(stage_fixtures._snapshot(stage), before)
        self.assertEqual(after_mtimes, before_mtimes)
        self.assertEqual(boundary.calls, 2)
        self.assertEqual(boundary.publishes, 2)
        self.assert_public_proof_passes(resumed)
        self.assert_copied_summaries_unchanged(resumed)

    def test_resume_rejects_changed_copied_summary_before_publication_or_zip(self) -> None:
        boundary = _StageSourceBoundary(
            self, self.receipt, self.source, interrupt="before-publish",
        )
        with self.resume_ports(boundary) as ports:
            with self.assertRaises(KeyboardInterrupt):
                coordinator.resume(self.receipt)
            self.assert_no_paid_entrypoints(ports)
        resumed = Receipt.load(self.receipt.path)
        boundary.receipt = resumed
        archive_receipt = resumed.invocation.archive_receipt
        archive_receipt.write_bytes(archive_receipt.read_bytes() + b"\n")
        with self.resume_ports(boundary) as ports:
            with self.assertRaises(ReleaseError) as raised:
                coordinator.resume(resumed)
            self.assertEqual(raised.exception.code, "private_projection_sources_changed")
            self.assert_no_paid_entrypoints(ports)
        self.assertEqual(boundary.calls, 2)
        self.assertEqual(boundary.publishes, 0)
        self.assertEqual(resumed.stage("project").state, "intended")
        self.assertFalse((resumed.invocation.root / "private-stage" / "manifest.json").exists())
        self.assertFalse(resumed.invocation.proof.exists())

    def test_existing_proof_symlink_is_rejected_without_changing_target(self) -> None:
        target = self.area / "unrelated-file"
        target.write_bytes(b"unchanged fixture sentinel")
        target.chmod(0o600)
        self.receipt.invocation.proof.symlink_to(target)
        boundary = _StageSourceBoundary(self, self.receipt, self.source, interrupt=None)
        with self.resume_ports(boundary) as ports:
            with self.assertRaises(BoundedReadError):
                coordinator.resume(self.receipt)
            self.assert_no_paid_entrypoints(ports)
            ports["submit"].assert_not_called()
        self.assertEqual(target.read_bytes(), b"unchanged fixture sentinel")
        self.assertEqual(target.stat().st_mode & 0o7777, 0o600)
        self.assertTrue(self.receipt.invocation.proof.is_symlink())
        self.assertEqual(Receipt.load(self.receipt.path).stage("project").state, "intended")



CaseChange = Callable[[dict[str, object]], None]


def _integer_size(facts: Mapping[str, object]) -> int:
    size = facts["size"]
    if type(size) is not int:
        raise AssertionError("projection facts size is not an integer")
    return size


class AgyModelBindingTests(ProjectionFixture):
    """read_public binds a PASS verdict's AGY PASS cases to the requested model."""

    public_zip: bytes | None = None

    def setUp(self) -> None:
        super().setUp()
        cases = read_object(self.source / "aggregate-receipt.json")["cases"]
        if not isinstance(cases, list):
            raise AssertionError("fixture aggregate cases missing")
        self.case_count = len(cases)
        self.agy_case_ids = [proof_fixtures._object(case)["case_id"] for case in cases
                             if proof_fixtures._object(case)["provider"] == "antigravity:print"]
        self.assertEqual(len(self.agy_case_ids), 2)
        if type(self).public_zip is None:
            boundary = _StageSourceBoundary(self, self.receipt, self.source, interrupt=None)
            with self.resume_ports(boundary):
                projection.project(self.receipt)
            type(self).public_zip = self.receipt.invocation.proof.read_bytes()

    def base_proof(self) -> bytes:
        if self.public_zip is None:
            raise AssertionError("base public proof was not projected")
        return self.public_zip

    def proof_with(self, changes: Mapping[str, CaseChange],
                   aggregate: Mapping[str, object] | None = None) -> bytes:
        editor = object.__new__(proof_fixtures.PublicNativeProofTests)
        editor.public_zip = self.base_proof()
        for case_id, change in changes.items():
            editor.public_zip = editor._change_case(f"cases/{case_id}.json", change)
        if aggregate:
            editor.public_zip = editor._change_case(
                "proof.json", lambda proof: proof_fixtures._object(proof["aggregate"]).update(aggregate))
        return editor.public_zip

    def read_with(self, receipt: Receipt, data: bytes) -> dict[str, object]:
        receipt.invocation.proof.write_bytes(data)
        receipt.intend("project", {"expectation": asdict(self.expectation)})
        return projection.read_public(receipt)[1]

    def assert_unbound(self, receipt: Receipt, data: bytes) -> None:
        with self.assertRaises(ReleaseError) as raised:
            self.read_with(receipt, data)
        self.assertEqual(raised.exception.code, "agy_requested_model_unbound")
        self.assertFalse(raised.exception.incomplete)

    @staticmethod
    def recorded(model: str | None) -> CaseChange:
        return lambda case: proof_fixtures._object(case["source_proof"]).__setitem__(
            "requested_model", model)

    def test_every_agy_pass_case_must_record_the_requested_model(self) -> None:
        requested = self._create_receipt(label="requested")
        facts = self.read_with(requested, self.base_proof())
        self.assertEqual((facts["status"], facts["case_results_verdict"]), ("PASS", "PASS"))
        for case_id in self.agy_case_ids:
            for model in ("other-model", None):
                with self.subTest(case_id=case_id, model=model):
                    self.assert_unbound(requested, self.proof_with({case_id: self.recorded(model)}))

    def test_default_request_binds_only_native_default_agy_proof(self) -> None:
        default = self._create_receipt(agy_model=None, label="default")
        self.assertNotIn("agy_model", default.invocation.request.document())
        self.assert_unbound(default, self.base_proof())
        native_default = self.proof_with({case_id: self.recorded(None)
                                          for case_id in self.agy_case_ids})
        facts = self.read_with(default, native_default)
        self.assertEqual((facts["status"], facts["case_results_verdict"]), ("PASS", "PASS"))

    def test_changed_completed_readback_precedes_agy_binding(self) -> None:
        default = self._create_receipt(agy_model=None, label="changed")
        facts = self.read_with(self._create_receipt(label="matching"), self.base_proof())
        default.invocation.proof.write_bytes(self.base_proof())
        default.intend("project", {"expectation": asdict(self.expectation)})
        default.complete("project", {**facts, "size": _integer_size(facts) + 1})
        with self.assertRaises(ReleaseError) as raised:
            projection.read_public(default)
        self.assertEqual(raised.exception.code, "public_proof_readback_changed")
        self.assertTrue(raised.exception.incomplete)

    def test_neutral_agy_block_and_fail_verdict_keep_their_existing_paths(self) -> None:
        def blocked_before_admission(case: dict[str, object]) -> None:
            case["status"], case["exit_code"] = "BLOCKED", 2
            proof = proof_fixtures._object(case["source_proof"])
            proof.update({
                "state": "blocked-before-runtime", "final_status": "BLOCKED",
                "source_status": "blocked", "requested_options_state": "missing",
                "requested_model": None, "requested_effort": None,
                "task_ids": [], "task_session_bindings": [], "diagnostic_task_ids": [],
                "session_ids": {"state": "not-recorded", "values": [], "by_alias": {}},
                "binary_identities": {"state": "unavailable", "sha256": {}, "task_ids": []},
            })
            proof_fixtures._object(proof["lifecycle_proof"])["state"] = "unresolved"

        requested = self._create_receipt(label="requested")
        blocked = self.proof_with(
            {case_id: blocked_before_admission for case_id in self.agy_case_ids},
            {"pass_count": self.case_count - len(self.agy_case_ids),
             "blocked_count": len(self.agy_case_ids)},
        )
        facts = self.read_with(requested, blocked)
        self.assertEqual((facts["status"], facts["case_results_verdict"]), ("PASS", "PASS"))
        self.assertEqual(facts["pass_count"], self.case_count - len(self.agy_case_ids))

        def submitted_fail(case: dict[str, object]) -> None:
            case["status"], case["exit_code"] = "FAIL", 1
            proof = proof_fixtures._object(case["source_proof"])
            proof.update({"state": "unresolved", "final_status": "FAIL",
                          "source_status": "fail", "requested_model": "other-model"})

        failed = self.proof_with(
            {self.agy_case_ids[0]: submitted_fail},
            {"status": "FAIL", "exit_code": 1, "case_results_verdict": "FAIL",
             "pass_count": self.case_count - 1, "failed_count": 1},
        )
        facts = self.read_with(self._create_receipt(agy_model=None, label="failed"), failed)
        self.assertEqual(facts["case_results_verdict"], "FAIL")

    def test_unbound_projection_and_completed_readback_stop_before_submission(self) -> None:
        legacy = self._create_receipt(agy_model=None, label="legacy")
        legacy_request = legacy.invocation.request.document()
        self.assertNotIn("agy_model", legacy_request)
        boundary = _StageSourceBoundary(self, legacy, self.source, interrupt=None)
        with self.resume_ports(boundary) as ports:
            with self.assertRaises(ReleaseError) as raised:
                coordinator.resume(legacy)
            self.assertEqual(raised.exception.code, "agy_requested_model_unbound")
            self.assert_no_paid_entrypoints(ports)
            ports["submit"].assert_not_called()
        self.assertEqual(legacy.stage("project").state, "intended")
        # A project completed before this binding existed: the same proof facts,
        # read through a request that matches the recorded AGY model.
        facts = self.read_with(self._create_receipt(label="matching"),
                               legacy.invocation.proof.read_bytes())
        legacy.complete("project", facts)
        resumed = Receipt.load(legacy.path)
        with self.resume_ports(boundary) as ports:
            with self.assertRaises(ReleaseError) as raised:
                coordinator.resume(resumed)
            self.assertEqual(raised.exception.code, "agy_requested_model_unbound")
            self.assert_no_paid_entrypoints(ports)
            ports["observe_native"].assert_not_called()
            ports["github_client"].assert_not_called()
            ports["submit"].assert_not_called()
        for name in ("upload", "dispatch", "watch"):
            self.assertEqual(resumed.stage(name).state, "pending")
        self.assertEqual(boundary.calls, 1)
        self.assertEqual(Receipt.load(legacy.path).invocation.request.document(), legacy_request)

if __name__ == "__main__":
    unittest.main()
