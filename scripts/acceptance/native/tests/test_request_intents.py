"""Native prelaunch request intents for dispatch, continuation and model discovery; launches are fakes."""

from __future__ import annotations

from argparse import Namespace

from dataclasses import FrozenInstanceError
from collections.abc import Callable, Iterable, Mapping, Sequence
import hashlib
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
from typing import TypedDict

from scripts.acceptance.native.tests.support import CapturedCommand
from scripts.acceptance.native.tests.support import construct_with_invalid_fields
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.native import workflow as gate
from scripts.acceptance.native.constants import MODEL_DISCOVERY_TASK_BUDGET
from scripts.acceptance.native.constants import MODEL_DISCOVERY_TASK_BUDGET_NANOS
from scripts.acceptance.native.constants import PROFILES
from scripts.acceptance.native.constants import TASK_BUDGET
from scripts.acceptance.native.constants import TIMEOUT_RESUME_SOURCE_BUDGET
from scripts.acceptance.native.model_discovery import ModelDiscoveryState
from scripts.acceptance.native.model_discovery_intent import MODEL_DISCOVERY_DEFINITION_REVISION
from scripts.acceptance.native.model_discovery_intent import ModelDiscoveryIntent
from scripts.acceptance.tasks.contracts import TaskCommand
from scripts.acceptance.tasks.request_intent import TaskRequestIntent


PROVIDER = "codex:exec"
PREDECESSOR = "c" * 32
SECRET = "native-intent-secret-value"


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


class LaunchProbe[T]:
    """Fake process owner: snapshots the owner's intents at the launch call, then fails or answers."""

    def __init__(self, snapshot: Callable[[], T], outcome: TaskCommand | BaseException) -> None:
        self.snapshot, self.outcome = snapshot, outcome
        self.snapshots: list[T] = []

    def run(self, name: str, argv: list[object], cwd: Path,
            expected: int | set[int] | None = 0, timeout: float = 15,
            stdin: bytes | None = None, *, environment: Mapping[str, str] | None = None) -> TaskCommand:
        self.snapshots.append(self.snapshot())
        if isinstance(self.outcome, BaseException):
            raise self.outcome
        return self.outcome

    def drain(self, timeout: float = 70, exclude: Iterable[object] = ()) -> Sequence[Mapping[str, object]]:
        raise AssertionError("the launch probe must not be used for process cleanup")


class NativeRequestIntentTests(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="native-request-intent-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.brief = self.base / "brief"
        self.brief.write_bytes(b"timeout brief\n")

    def operation(self, outcome: TaskCommand | BaseException) -> tuple[
            gate.NativeAcceptance, LaunchProbe[list[TaskRequestIntent]]]:
        operation = object.__new__(gate.NativeAcceptance)
        operation.delegate, operation.config, operation.runner = (
            self.base / "delegate", self.base / "pueue.yml", self.base / "runner")
        operation.base, operation.state, operation.workspace = (
            self.base, self.base / "state", self.base / "workspace")
        operation.models_state = self.base / "models-state"
        operation.output, operation.pueue = self.base, self.base / "pueue"
        for directory in (operation.state, operation.workspace, operation.models_state):
            directory.mkdir()
        operation.provider, operation.mode, operation.scenario = PROVIDER, "workspace-write", "timeout-resume"
        operation.args = Namespace(model="gpt-test", effort="high")
        operation.environment = {"PATH": "/usr/bin", "OPENAI_API_KEY": SECRET}
        operation.root_id, operation.tasks, operation.numeric_ids = None, {}, {}
        operation._sync_ops_owned_tasks = Mock()
        probe = LaunchProbe(lambda: list(operation.ops.request_intents), outcome)
        process_patch = patch.object(operation, "processes", probe, create=True)
        process_patch.start()
        self.addCleanup(process_patch.stop)
        operation.ops = gate.NativeAcceptanceTaskOps(operation, managed_runner=False)
        return operation, probe

    def test_lost_timeout_source_reply_retains_role_budget_and_brief(self) -> None:
        operation, probe = self.operation(RuntimeError("reply lost"))
        with self.assertRaisesRegex(RuntimeError, "reply lost"):
            operation.dispatch("timeout-source", self.brief, budget=TIMEOUT_RESUME_SOURCE_BUDGET)
        self.assertEqual(len(probe.snapshots), 1)
        at_launch = probe.snapshots[0]
        self.assertEqual(len(at_launch), 1, "dispatch must retain its intent before launch")
        self.assertEqual(at_launch, operation.ops.request_intents)
        intent = at_launch[0]
        request = intent.request
        self.assertEqual((request.role, request.budget, request.brief_sha256),
                         ("timeout-source", TIMEOUT_RESUME_SOURCE_BUDGET, sha(b"timeout brief\n")))
        self.assertEqual((request.provider, request.permission, request.scenario, request.model,
                          request.effort, request.cwd),
                         (PROVIDER, "workspace-write", "timeout-resume", "gpt-test", "high",
                          str(operation.workspace)))
        self.assertEqual(operation.ops.dispatch_attempts, {intent.task_id})
        self.assertEqual((operation.tasks, operation.ops.tasks), ({}, {}))

    def test_unparseable_continuation_retains_unresolved_intent(self) -> None:
        directory = self.base / "continuation"
        directory.mkdir()
        (directory / "stdout").write_bytes(b"not json")
        operation, probe = self.operation(CapturedCommand(
            directory=directory, result={"exit_code": 1, "natural_wait": True}))
        operation.root_id = "a" * 32
        with self.assertRaisesRegex(AcceptanceFailure, "invalid JSON evidence"):
            operation.continue_public("timeout-resume", self.brief, PREDECESSOR)
        self.assertEqual(len(probe.snapshots), 1)
        at_launch = probe.snapshots[0]
        self.assertEqual(len(at_launch), 1, "continuation must retain its intent before launch")
        intent = at_launch[0]
        self.assertEqual(operation.ops.request_intents, [intent])
        self.assertEqual((intent.command, intent.task_id, intent.root_id), ("continue", None, "a" * 32))
        self.assertEqual((intent.request.role, intent.request.predecessor_task_id, intent.request.budget),
                         ("timeout-resume", PREDECESSOR, TASK_BUDGET))
        self.assertIsNone(intent.request.provider)
        self.assertEqual((operation.ops.continuation_successors, operation.ops.dispatch_attempts), ({}, set()))

    def discovery(self, outcome: TaskCommand | BaseException) -> tuple[
            gate.NativeAcceptance, LaunchProbe[ModelDiscoveryIntent | None]]:
        operation, _ = self.operation(outcome)
        (operation.models_state / "existing-entry").mkdir()
        operation.provider_executable = self.base / "codex"
        operation.provider_executable.write_bytes(b"helper fixture\n")
        probe = LaunchProbe(lambda: operation._discovery_state().intent, outcome)
        process_patch = patch.object(operation, "processes", probe)
        process_patch.start()
        self.addCleanup(process_patch.stop)
        return operation, probe

    def test_discovery_records_one_owned_intent_before_launch_without_environment(self) -> None:
        operation, probe = self.discovery(RuntimeError("reply lost"))
        with self.assertRaisesRegex(RuntimeError, "reply lost"):
            operation.discover_models()
        self.assertEqual(len(probe.snapshots), 1)
        intent = probe.snapshots[0]
        if intent is None:
            self.fail("discovery must retain its intent before launch")
        self.assertIs(operation._discovery_state().intent, intent)
        self.assertEqual((intent.models_root, intent.initial_entries, intent.provider, intent.cwd),
                         (str(operation.models_state), ("existing-entry",), PROVIDER, str(operation.workspace)))
        self.assertEqual((intent.permission, intent.definition_revision, intent.budget, intent.budget_nanos),
                         (PROFILES[PROVIDER]["modes"][0], MODEL_DISCOVERY_DEFINITION_REVISION,
                          MODEL_DISCOVERY_TASK_BUDGET, MODEL_DISCOVERY_TASK_BUDGET_NANOS))
        self.assertEqual((intent.brief_sha256, intent.helper_executable),
                         (sha(b"model discovery"), str(operation.provider_executable)))
        self.assertNotIn(SECRET, repr(intent))
        self.assertEqual((operation.models_root_id, operation.model_discovery_task_id), (None, None))

    def test_discovery_refuses_a_second_attempt_before_launch(self) -> None:
        operation, probe = self.discovery(RuntimeError("reply lost"))
        with self.assertRaises(RuntimeError):
            operation.discover_models()
        first = operation._discovery_state().intent
        with self.assertRaises(BaseException) as retried:
            operation.discover_models()
        self.assertIsInstance(retried.exception, AcceptanceFailure)
        self.assertIn("intent was already recorded", str(retried.exception))
        self.assertEqual(len(probe.snapshots), 1)
        self.assertIs(operation._discovery_state().intent, first)

    def test_discovery_does_not_read_provider_bytes_before_launch(self) -> None:
        operation, probe = self.discovery(RuntimeError("reply lost"))
        operation.provider_executable.unlink()
        with self.assertRaisesRegex(RuntimeError, "reply lost"):
            operation.discover_models()
        self.assertEqual(len(probe.snapshots), 1)
        self.assertIs(operation._discovery_state().intent, probe.snapshots[0])


INVALID_INTENT = "model discovery request intent is invalid"


class IntentFields(TypedDict):
    models_root: str
    initial_entries: tuple[str, ...]
    provider: str
    permission: str
    cwd: str
    definition_revision: str
    budget: str
    budget_nanos: int
    brief_sha256: str
    helper_executable: str


def intent_fields(initial_entries: tuple[str, ...] = ("existing-entry",)) -> IntentFields:
    return {
        "models_root": "/models-state", "initial_entries": initial_entries, "provider": PROVIDER,
        "permission": PROFILES[PROVIDER]["modes"][0], "cwd": "/workspace",
        "definition_revision": MODEL_DISCOVERY_DEFINITION_REVISION, "budget": MODEL_DISCOVERY_TASK_BUDGET,
        "budget_nanos": MODEL_DISCOVERY_TASK_BUDGET_NANOS, "brief_sha256": sha(b"model discovery"),
        "helper_executable": "/bin/codex"}


class ModelDiscoveryIntentValueTests(unittest.TestCase):
    def test_valid_intent_keeps_every_supplied_fact(self) -> None:
        for entries in (("existing-entry",), ()):
            with self.subTest(entries=entries):
                fields = intent_fields(initial_entries=entries)
                intent = ModelDiscoveryIntent(**fields)
                self.assertEqual({name: getattr(intent, name) for name in fields}, fields)

    def test_intent_is_frozen(self) -> None:
        intent = ModelDiscoveryIntent(**intent_fields())
        for name, value in (("budget_nanos", 1), ("initial_entries", ("other",)),
                            ("helper_executable", "/bin/other")):
            with self.subTest(field=name), self.assertRaises(FrozenInstanceError):
                setattr(intent, name, value)
        self.assertEqual(ModelDiscoveryIntent(**intent_fields()), intent)

    def test_malformed_brief_digest_is_refused(self) -> None:
        for digest in ("", "a" * 63, "a" * 65, "A" * 64, "g" * 64, None):
            with self.subTest(digest=digest), \
                    self.assertRaisesRegex(AcceptanceFailure, INVALID_INTENT):
                construct_with_invalid_fields(ModelDiscoveryIntent, {**intent_fields(), "brief_sha256": digest})

    def test_boolean_zero_or_negative_budget_is_refused(self) -> None:
        for budget in (True, False, 0, -1, -MODEL_DISCOVERY_TASK_BUDGET_NANOS):
            with self.subTest(budget=budget), self.assertRaisesRegex(AcceptanceFailure, INVALID_INTENT):
                construct_with_invalid_fields(ModelDiscoveryIntent, {**intent_fields(), "budget_nanos": budget})

    def test_mutable_initial_entries_are_refused_without_changing_the_caller_value(self) -> None:
        entries = ["existing-entry"]
        with self.assertRaisesRegex(AcceptanceFailure, INVALID_INTENT):
            construct_with_invalid_fields(ModelDiscoveryIntent, {**intent_fields(), "initial_entries": entries})
        self.assertEqual(entries, ["existing-entry"])

    def test_legacy_state_without_an_intent_keeps_its_released_behavior(self) -> None:
        state = ModelDiscoveryState({"status": "available"}, {"status": "available"}, 0, "a" * 32)
        self.assertIsNone(state.intent)
        self.assertEqual((state.response, state.exit_code, state.root_id, state.task_id),
                         ({"status": "available"}, 0, "a" * 32, None))
        state.record_observation({"status": "failed"}, {"status": "failed"}, 1)
        state.record_task("b" * 32)
        state.record_task("b" * 32)
        with self.assertRaisesRegex(AcceptanceFailure, "task identity changed"):
            state.record_task("c" * 32)
        self.assertEqual((state.response, state.exit_code, state.task_id, state.intent),
                         ({"status": "failed"}, 1, "b" * 32, None))


if __name__ == "__main__":
    unittest.main()
