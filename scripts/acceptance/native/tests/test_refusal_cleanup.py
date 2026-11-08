"""Failure cleanup accounts for refused calls without inventing task authority."""

from __future__ import annotations

from argparse import Namespace
from copy import deepcopy
import json
from pathlib import Path
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
import sys


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.inspections.tests.fixtures import CapturedCommand
from scripts.acceptance.inspections.tests.fixtures import FixtureProcesses
from scripts.acceptance.inspections.tests.fixtures import InspectionFixture
from scripts.acceptance.inspections.tests.fixtures import ObservedDaemon
from scripts.acceptance.inspections.tests.fixtures import ROOT
from scripts.acceptance.inspections.tests.fixtures import TASK
from scripts.acceptance.inspections.tests.fixtures import forbid_installed_content
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.files import write_json
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.native import dispatch as acceptance_dispatch
from scripts.acceptance.native import lifecycle as acceptance_lifecycle
from scripts.acceptance.native.model_discovery import ModelDiscoveryState
from scripts.acceptance.native.model_discovery_intent import ModelDiscoveryIntent
from scripts.acceptance.tasks.operations import AdmissionFailure


REFUSED = "c" * 32
OTHER = "d" * 32


def resource_refusal() -> dict[str, object]:
    return {
        "schema_version": 1, "command": "dispatch", "root_id": ROOT,
        "task_id": REFUSED, "task_record": "unknown", "admission": "unknown",
        "liveness": "undetermined", "publication": "unknown",
        "failure": {"code": "resource_pressure", "stage": "check-resources",
                    "reason_code": "host_cpu_sustained", "next_action": "stop_and_report"},
        "resource_admission": {"status": "pressure"},
        "error": "Delegate refused admission because host resources are under pressure.",
    }


class RefusalCleanupTests(unittest.TestCase):
    def setUp(self) -> None:
        self.enterContext(forbid_installed_content())
        self.fixture = InspectionFixture(self)
        directory = self.fixture.ordinary_only_admission("managed")
        self.fixture.managed_ordinary_lifetime(directory)
        self.ops = self.fixture.ops
        self.ops.tasks.clear()
        self.ops.labels.clear()
        self.ops.numbers.clear()
        self.ops.dispatch_attempts.clear()
        self.capture_count = 0
        self.launch = Mock()
        self.ops.processes = FixtureProcesses(run=self.launch)
        admitted: dict[str, object] = {"schema_version": 1, "command": "dispatch", "root_id": ROOT,
                    "task_id": TASK, "task_record": "created", "admission": "admitted",
                    "supervisor": {"matched": True, "numeric_task_id": 7}}
        self.launch.return_value = self.capture(admitted, exit_code=0)
        self.ops.dispatch("fresh", TASK, ["dispatch"])
        self.status = self.fixture.ordinary_only_status()

    def capture(self, response: dict[str, object] | None, *, exit_code: int = 1) -> CapturedCommand:
        self.capture_count += 1
        directory = self.fixture.output / f"control-{self.capture_count}"
        directory.mkdir()
        if response is None:
            (directory / "stdout").write_text("not JSON", encoding="utf-8")
        else:
            write_json(directory / "stdout", response)
        return CapturedCommand(directory, {"natural_wait": True, "exit_code": exit_code})

    def refuse(self, response: dict[str, object] | None = None, *,
               name: str = "refused") -> dict[str, object]:
        response = resource_refusal() if response is None else response
        self.launch.return_value = self.capture(response)
        with self.assertRaisesRegex(AcceptanceFailure, "was not admitted"):
            self.ops.dispatch(name, REFUSED, ["dispatch"])
        return response

    def seal(self) -> None:
        self.ops.seal_admissions()
        # Supply this isolated membership check's finite owner budget; no wall-time claim.
        self.ops._cleanup_deadline = time.monotonic() + 10

    def test_mixed_run_needs_independent_absence_proof_and_keeps_refusal_unknown(self) -> None:
        response = self.refuse()
        original = deepcopy(response)
        self.seal()
        self.assertFalse(self.ops.failure_queue_finished(self.status))
        proof = Mock(return_value=frozenset({REFUSED}))
        with patch.object(self.ops, "_prove_absent_refusals", proof):
            self.assertTrue(self.ops.failure_queue_finished(self.status))
        self.assertEqual(proof.call_args.args[0], frozenset({REFUSED}))
        self.assertEqual(self.ops.dispatch_attempts, {TASK, REFUSED})
        self.assertEqual(self.ops.tasks, {"fresh": TASK})
        self.assertEqual(response, original)
        self.assertFalse((self.fixture.state / "tasks" / REFUSED).exists())
        self.assertEqual(self.launch.call_count, 2, "cleanup cannot dispatch or observe the unknown ID")

    def test_absence_proof_error_or_different_identity_cannot_close_membership(self) -> None:
        self.refuse()
        self.seal()
        for result in (frozenset(), frozenset({OTHER}), OSError("observer unreadable")):
            with self.subTest(result=repr(result)):
                proof = Mock(side_effect=result) if isinstance(result, OSError) else Mock(return_value=result)
                with patch.object(self.ops, "_prove_absent_refusals", proof):
                    self.assertFalse(self.ops.failure_queue_finished(self.status))
                proof.assert_called_once()
        self.assertEqual(self.ops.dispatch_attempts, {TASK, REFUSED})

    def assert_refusal_cannot_close(self, field: str, value: object) -> None:
        response = resource_refusal()
        response[field] = value
        self.refuse(response)
        self.seal()
        proof = Mock(return_value=frozenset({REFUSED}))
        with patch.object(self.ops, "_prove_absent_refusals", proof):
            self.assertFalse(self.ops.failure_queue_finished(self.status))
        proof.assert_not_called()

    def test_workspace_residue_never_reaches_absence_observer(self) -> None:
        self.assert_refusal_cannot_close("workspace", {"state": "cleanup_pending"})

    def test_foreign_root_refusal_never_reaches_absence_observer(self) -> None:
        self.assert_refusal_cannot_close("root_id", OTHER)

    def test_non_resource_failure_never_reaches_absence_observer(self) -> None:
        self.assert_refusal_cannot_close("failure", {"code": "other"})

    def test_empty_error_never_reaches_absence_observer(self) -> None:
        self.assert_refusal_cannot_close("error", "")

    def test_nonstring_error_never_reaches_absence_observer(self) -> None:
        self.assert_refusal_cannot_close("error", {"message": "private-error-canary"})

    def test_empty_reason_never_reaches_absence_observer(self) -> None:
        self.assert_refusal_cannot_close("failure", {
            "code": "resource_pressure", "stage": "check-resources",
            "reason_code": "", "next_action": "stop_and_report"})

    def test_nonstring_reason_never_reaches_absence_observer(self) -> None:
        self.assert_refusal_cannot_close("failure", {
            "code": "resource_pressure", "stage": "check-resources",
            "reason_code": ["host_cpu_sustained"], "next_action": "stop_and_report"})

    def test_auxiliary_intent_callback_is_not_executed_after_seal(self) -> None:
        record = Mock()
        self.ops.record_auxiliary_intent(record)
        record.assert_called_once_with()
        record.reset_mock()
        self.seal()
        with self.assertRaisesRegex(AcceptanceFailure, "admissions are closed"):
            self.ops.record_auxiliary_intent(record)
        record.assert_not_called()

    def test_incomplete_or_duplicate_completion_evidence_cannot_authorize_absence(self) -> None:
        self.refuse()
        self.seal()
        original = deepcopy(self.ops.dispatch_observations[-1])
        faults = (("receipt_written", False), ("natural_wait", False),
                  ("call_ordinal", 0), ("call_ordinal", None), ("call_ordinal", True))
        for field, value in faults:
            with self.subTest(field=field, value=value):
                damaged = deepcopy(original)
                damaged[field] = value
                self.ops.dispatch_observations[-1] = damaged
                proof = Mock(return_value=frozenset({REFUSED}))
                with patch.object(self.ops, "_prove_absent_refusals", proof):
                    self.assertFalse(self.ops.failure_queue_finished(self.status))
                proof.assert_not_called()
        self.ops.dispatch_observations[-1] = original

    def test_attempt_added_after_seal_cannot_borrow_existing_completion_proofs(self) -> None:
        self.refuse()
        self.seal()
        self.ops.dispatch_attempts.add(OTHER)
        proof = Mock(return_value=frozenset({REFUSED}))
        with patch.object(self.ops, "_prove_absent_refusals", proof):
            self.assertFalse(self.ops.failure_queue_finished(self.status))
        proof.assert_not_called()

    def test_attempt_added_during_absence_observation_keeps_cleanup_unproven(self) -> None:
        self.refuse()
        self.seal()

        def changed_inventory(_tasks, _deadline):
            self.ops.dispatch_attempts.add(OTHER)
            return frozenset({REFUSED})

        proof = Mock(side_effect=changed_inventory)
        with patch.object(self.ops, "_prove_absent_refusals", proof):
            self.assertFalse(self.ops.failure_queue_finished(self.status))
        proof.assert_called_once()
        self.assertFalse(self.ops.closed)

    def test_refused_id_with_queue_row_cannot_be_classified_absent(self) -> None:
        self.refuse()
        self.seal()
        rows = self.status["tasks"]
        assert isinstance(rows, dict)
        original = rows["7"]
        assert isinstance(original, dict)
        row = deepcopy(original)
        row.update(id=8, label=f"delegate:{ROOT}:{REFUSED}")
        rows["8"] = row
        proof = Mock(return_value=frozenset({REFUSED}))
        with patch.object(self.ops, "_prove_absent_refusals", proof):
            self.assertFalse(self.ops.failure_queue_finished(self.status))
        proof.assert_not_called()

    def test_sealed_dispatch_and_continuation_launch_nothing(self) -> None:
        self.seal()
        calls = self.launch.call_count
        for invoke in (lambda: self.ops.dispatch("late", REFUSED, ["dispatch"]),
                       lambda: self.ops.continue_task("late", ["continue"])):
            with self.subTest(invoke=invoke):
                with self.assertRaisesRegex(AcceptanceFailure, "admissions are closed"):
                    invoke()
        self.assertEqual(self.launch.call_count, calls)
        self.assertEqual(self.ops.dispatch_attempts, {TASK})

    def test_unparseable_legacy_continuation_blocks_refusal_cleanup(self) -> None:
        self.refuse()
        self.launch.return_value = self.capture(None)
        with self.assertRaisesRegex(AcceptanceFailure, "invalid JSON evidence"):
            self.ops.continue_task("lost-continuation", ["continue"], request=None)
        self.seal()
        proof = Mock(return_value=frozenset({REFUSED}))
        with patch.object(self.ops, "_prove_absent_refusals", proof):
            self.assertFalse(self.ops.failure_queue_finished(self.status))
        proof.assert_not_called()

    def test_prelaunch_call_must_join_before_private_shutdown(self) -> None:
        entered, release = threading.Event(), threading.Event()
        failures: list[BaseException] = []
        response = self.capture(resource_refusal())

        def gated_run(*_args, **_kwargs):
            entered.set()
            if not release.wait(5):
                raise AssertionError("test did not release its owned command gate")
            return response

        def invoke() -> None:
            try:
                self.ops.dispatch("in-flight", REFUSED, ["dispatch"])
            except BaseException as error:
                failures.append(error)

        self.launch.side_effect = gated_run
        self.ops.daemon = ObservedDaemon()
        thread = threading.Thread(target=invoke)
        thread.start()
        try:
            self.assertTrue(entered.wait(5), "admission did not reach its process-owner gate")
            self.ops.seal_admissions()
            client = Mock(side_effect=AssertionError("incomplete admission cannot query or shut down"))
            with patch.object(self.ops, "client", client):
                self.assertFalse(self.ops.safe_failure_shutdown())
            client.assert_not_called()
        finally:
            release.set()
            thread.join(6)
        self.assertFalse(thread.is_alive(), "owned test thread was not joined")
        self.assertEqual(len(failures), 1)
        self.assertIsInstance(failures[0], AdmissionFailure)

    def test_failed_call_does_not_borrow_another_calls_authentication_response(self) -> None:
        brief = self.fixture.base / "brief.md"
        brief.write_text("bounded fixture brief\n", encoding="utf-8")
        unrelated = {"status": "blocked", "error": "unrelated authentication",
                     "capability": {"live_acceptance": {"status": "blocked", "authentication": "blocked",
                                                           "reason_code": "authentication_unavailable"}}}
        for operation in ("dispatch", "continue"):
            with self.subTest(operation=operation):
                own_response = resource_refusal()
                own_response["command"] = operation
                own_failure = AdmissionFailure("own refusal", own_response)
                ops = SimpleNamespace(
                    root_id=ROOT, dispatch=Mock(side_effect=own_failure),
                    continue_task=Mock(side_effect=own_failure),
                    dispatch_observations=[{"response": unrelated}])
                owner = SimpleNamespace(
                    ops=ops, root_id=ROOT, provider="ccr:print", mode="workspace-write", scenario="fresh",
                    args=Namespace(model="fixture", effort=None), workspace=self.fixture.base,
                    task_command_prefix=lambda: ["delegate"],
                    dispatch_argv=lambda *_args: ["dispatch"], _sync_ops_owned_tasks=Mock())
                with self.assertRaises(AdmissionFailure) as caught:
                    if operation == "dispatch":
                        acceptance_dispatch.dispatch(owner, "own", brief)
                    else:
                        acceptance_dispatch.continue_public(owner, "own", brief, TASK)
                self.assertIs(caught.exception, own_failure)
                self.assertEqual(dict(own_failure.response), own_response)


class NativeRefusalProofTests(unittest.TestCase):
    """Real proof orchestration and file decoding; canonical closure/processes are controlled boundaries."""

    def case(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-refusal-proof-")
        self.addCleanup(temporary.cleanup)
        base = Path(temporary.name).resolve()
        pins = DirectoryOwner()
        self.addCleanup(pins.rollback)
        state = pins.create("ordinary state", base / "state", base)
        models = pins.create("model state", base / "models-state", base)
        write_json(state.directory / "root.json", {
            "schema_version": 1, "root_id": ROOT, "created_at": "2026-09-15T00:00:00Z"})
        owner = SimpleNamespace(
            base=base, state=state.directory, models_state=models.directory,
            owned_directories=(state, models), daemon=object(), environment={"PATH": "/usr/bin"},
            ops=SimpleNamespace(managed_runner=True, tasks={"fresh": TASK}),
            models_root_id=None, model_discovery_task_id=None, model_discovery_state=ModelDiscoveryState())
        inventory = {"root_id": ROOT, "root_device": state.identity[0],
                     "root_inode": state.identity[1], "task_ids": [TASK]}
        entry = {"root_id": ROOT, "root_device": state.identity[0], "root_inode": state.identity[1],
                 "task_id": REFUSED, "entry_exists": False}
        ordinary_closure = {"root_id": ROOT, "tasks": {TASK: {}}, "inspections": {}}
        observers = base / "observers"
        observers.mkdir()
        for name in ("task-inventory-fixture", "nested-lifetime-fixture"):
            # Metadata-only fixtures: process execution is replaced at its owner boundary.
            (observers / name).touch(mode=0o700)
        case = SimpleNamespace(
            observers=observers,
            owner=owner, token=state, models_token=models, inventory=inventory, entry=entry,
            closure=ordinary_closure, model_closure={"root_id": OTHER, "tasks": {}, "inspections": {}},
            inventory_bytes=None, entry_bytes=None, command_result={"natural_wait": True, "exit_code": 0})

        def output(_name, arguments, _cwd, **_kwargs):
            capture = base / ("capture-" + str(case.run.call_count))
            capture.mkdir()
            if Path(arguments[0]).name == "task-inventory-fixture":
                data = case.inventory_bytes
                if data is None:
                    data = ("TASK_INVENTORY_OBSERVATION\t" + json.dumps(case.inventory) + "\n").encode()
            else:
                data = case.entry_bytes
                if data is None:
                    data = ("NESTED_TASK_OBSERVATION\t" + json.dumps(case.entry) + "\n").encode()
            (capture / "stdout").write_bytes(data)
            return CapturedCommand(capture, case.command_result)

        case.run = Mock(side_effect=output)
        owner.processes = SimpleNamespace(drain=Mock(return_value=[]), run=case.run)

        def closure_reader(state_path, *_args):
            value = case.closure if state_path == owner.state else case.model_closure
            if isinstance(value, BaseException):
                raise value
            return value

        case.reader = Mock(side_effect=closure_reader)
        return case

    def prove(self, case):
        with (patch.object(acceptance_lifecycle, "validate_managed_lifetime_closure", case.reader),
              patch.object(acceptance_lifecycle, "NATIVE_OBSERVER_DIRECTORY", case.observers)):
            return acceptance_lifecycle.prove_absent_resource_refusals(
                case.owner, frozenset({REFUSED}), time.monotonic() + 5)

    def assert_retained(self, case) -> None:
        for token in (case.token, case.models_token):
            self.assertFalse(token.consumed)
            self.assertTrue(token.directory.is_dir())

    def test_pinned_inventory_and_absent_entry_prove_only_the_requested_refusal(self) -> None:
        case = self.case()
        self.assertEqual(self.prove(case), frozenset({REFUSED}))
        case.reader.assert_called_once_with(case.owner.state, {TASK})
        self.assertEqual(case.run.call_count, 2)
        first, second = case.run.call_args_list
        self.assertEqual(Path(first.args[1][0]).name, "task-inventory-fixture")
        self.assertEqual(first.kwargs["environment"]["DELEGATE_TASK_INVENTORY_ROOT_ID"], ROOT)
        self.assertEqual(Path(second.args[1][0]).name, "nested-lifetime-fixture")
        self.assertEqual(second.kwargs["environment"]["DELEGATE_NESTED_FIXTURE_TASK"], REFUSED)
        self.assert_retained(case)

    def test_missing_or_nonexecutable_observer_does_not_launch_or_consume_tokens(self) -> None:
        for name, missing in (("task-inventory-fixture", True), ("nested-lifetime-fixture", False)):
            with self.subTest(name=name, missing=missing):
                case = self.case()
                helper = case.observers / name
                if missing:
                    helper.unlink()
                else:
                    helper.chmod(0o600)
                with self.assertRaisesRegex(AcceptanceFailure, "native cleanup observer is unavailable"):
                    self.prove(case)
                case.run.assert_not_called()
                self.assert_retained(case)

    def test_untrusted_inventory_cannot_authorize_an_entry_observation(self) -> None:
        for fault in ("root", "device", "schema", "duplicate", "extra", "malformed", "oversized"):
            with self.subTest(fault=fault):
                case = self.case()
                if fault == "root":
                    case.inventory["root_id"] = OTHER
                elif fault == "device":
                    case.inventory["root_device"] = case.token.identity[0] + 1
                elif fault == "schema":
                    case.inventory["extra"] = "private-inventory-canary"
                elif fault == "duplicate":
                    case.inventory["task_ids"] = [TASK, TASK]
                elif fault == "extra":
                    case.inventory["task_ids"] = [TASK, OTHER]
                elif fault == "malformed":
                    case.inventory_bytes = b"TASK_INVENTORY_OBSERVATION\t{private-inventory-canary"
                else:
                    case.inventory_bytes = b"x" * (1_048_576 + 1)
                with self.assertRaises((AcceptanceFailure, json.JSONDecodeError)) as failure:
                    self.prove(case)
                self.assertNotIn("private-inventory-canary", str(failure.exception))
                self.assertEqual(case.run.call_count, 1)
                self.assert_retained(case)

    def test_present_or_foreign_refused_entry_stays_unresolved(self) -> None:
        for field, value in (("entry_exists", True), ("root_id", OTHER)):
            with self.subTest(field=field):
                case = self.case()
                case.entry[field] = value
                with self.assertRaises(AcceptanceFailure):
                    self.prove(case)
                self.assertEqual(case.run.call_count, 2)
                self.assert_retained(case)

    def test_original_state_replacement_rejects_before_closure_or_command(self) -> None:
        case = self.case()
        original = case.owner.base / "original-state"
        case.owner.state.rename(original)
        case.owner.state.mkdir(mode=0o700)
        try:
            with self.assertRaisesRegex(RuntimeError, "changed|replaced"):
                self.prove(case)
            case.reader.assert_not_called()
            case.run.assert_not_called()
            self.assert_retained(case)
        finally:
            case.owner.state.rmdir()
            original.rename(case.owner.state)

    def test_active_control_prevents_all_closure_and_store_observations(self) -> None:
        case = self.case()
        case.owner.processes.drain.return_value = [{"state": "running"}]
        with self.assertRaisesRegex(AcceptanceFailure, "controls remain unfinished"):
            self.prove(case)
        case.reader.assert_not_called()
        case.run.assert_not_called()
        self.assert_retained(case)

    def test_incomplete_ordinary_or_model_closure_prevents_native_observation(self) -> None:
        for fault in ("ordinary", "model-unclosed", "model-unbound", "uninitialized-nonempty",
                      "known-model-root-missing"):
            with self.subTest(fault=fault):
                case = self.case()
                if fault == "ordinary":
                    case.closure = AcceptanceFailure("ordinary scope is not closed")
                elif fault == "uninitialized-nonempty":
                    (case.owner.models_state / "unknown-operation").write_text("pending", encoding="utf-8")
                elif fault == "known-model-root-missing":
                    case.owner.models_root_id = OTHER
                    case.owner.model_discovery_task_id = "e" * 32
                else:
                    write_json(case.owner.models_state / "root.json", {
                        "schema_version": 1, "root_id": OTHER, "created_at": "2026-09-15T00:00:00Z"})
                    if fault == "model-unclosed":
                        case.owner.models_root_id = OTHER
                        case.model_closure = AcceptanceFailure("model scope is not closed")
                with self.assertRaises(AcceptanceFailure):
                    self.prove(case)
                case.run.assert_not_called()
                self.assert_retained(case)

    def test_pending_model_evidence_without_root_cannot_be_inferred_empty(self) -> None:
        for field in ("intent", "response", "summary", "exit_code"):
            with self.subTest(field=field):
                case = self.case()
                discovery = case.owner.model_discovery_state
                if field == "intent":
                    discovery.record_intent(ModelDiscoveryIntent(
                        models_root=str(case.owner.models_state), initial_entries=(), provider="ccr:print",
                        permission="read-only", cwd=str(case.owner.base),
                        definition_revision="model-discovery-v1", budget="5m0s", budget_nanos=300_000_000_000,
                        brief_sha256="a" * 64, helper_executable=str(case.owner.base / "helper")))
                elif field == "response":
                    discovery.response = {}
                elif field == "summary":
                    discovery.summary = {}
                else:
                    discovery.exit_code = 0
                with self.assertRaisesRegex(AcceptanceFailure, "known or unknown work"):
                    self.prove(case)
                case.run.assert_not_called()
                self.assert_retained(case)

    def test_failed_or_unwaited_observer_retains_original_tokens(self) -> None:
        for result in ({"natural_wait": True, "exit_code": 1},
                       {"natural_wait": False, "exit_code": 0}):
            with self.subTest(result=result):
                case = self.case()
                case.command_result = result
                with self.assertRaisesRegex(AcceptanceFailure, "did not complete naturally"):
                    self.prove(case)
                self.assertEqual(case.run.call_count, 1)
                self.assert_retained(case)
