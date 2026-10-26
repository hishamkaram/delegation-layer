"""Native prelaunch request intents for dispatch, continuation and model discovery; launches are fakes."""

from __future__ import annotations

from dataclasses import FrozenInstanceError
import hashlib
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.native import acceptance as gate
from scripts.delegate_acceptance.native.constants import (
    MODEL_DISCOVERY_TASK_BUDGET,
    MODEL_DISCOVERY_TASK_BUDGET_NANOS,
    PROFILES,
    TASK_BUDGET,
    TIMEOUT_RESUME_SOURCE_BUDGET,
)
from scripts.delegate_acceptance.native.model_discovery import ModelDiscoveryState
from scripts.delegate_acceptance.native.model_discovery_intent import (
    MODEL_DISCOVERY_DEFINITION_REVISION,
    ModelDiscoveryIntent,
)
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps


PROVIDER = "codex:exec"
PREDECESSOR = "c" * 32
SECRET = "native-intent-secret-value"


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


class LaunchProbe:
    """Fake process owner: snapshots the owner's intents at the launch call, then fails or answers."""

    def __init__(self, snapshot, outcome: object) -> None:
        self.snapshot, self.outcome, self.snapshots = snapshot, outcome, []

    def run(self, name: str, argv: list[object], cwd: Path, **_options: object) -> object:
        self.snapshots.append(self.snapshot())
        if isinstance(self.outcome, BaseException):
            raise self.outcome
        return self.outcome


class NativeRequestIntentTests(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="native-request-intent-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.brief = self.base / "brief"
        self.brief.write_bytes(b"timeout brief\n")

    def operation(self, outcome: object) -> tuple[gate.NativeAcceptance, LaunchProbe]:
        operation = object.__new__(gate.NativeAcceptance)
        operation.delegate, operation.config, operation.runner = (
            self.base / "delegate", self.base / "pueue.yml", self.base / "runner")
        operation.base, operation.state, operation.workspace = (
            self.base, self.base / "state", self.base / "workspace")
        operation.models_state = self.base / "models-state"
        for directory in (operation.state, operation.workspace, operation.models_state):
            directory.mkdir()
        operation.provider, operation.mode, operation.scenario = PROVIDER, "workspace-write", "timeout-resume"
        operation.args = SimpleNamespace(model="gpt-test", effort="high")
        operation.environment = {"PATH": "/usr/bin", "OPENAI_API_KEY": SECRET}
        operation.root_id, operation.tasks, operation.numeric_ids = None, {}, {}
        operation._sync_ops_owned_tasks = Mock()
        probe = LaunchProbe(lambda: list(operation.ops.request_intents), outcome)
        operation.processes = probe
        operation.ops = NativeTaskOps(probe, operation.delegate, operation.runner, self.base / "pueue",
                                      self.base, operation.state, self.base)
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
        operation, probe = self.operation(SimpleNamespace(directory=directory,
                                                          result={"exit_code": 1, "natural_wait": True}))
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

    def discovery(self, outcome: object) -> tuple[gate.NativeAcceptance, LaunchProbe]:
        operation, _ = self.operation(outcome)
        (operation.models_state / "existing-entry").mkdir()
        operation.provider_executable = self.base / "codex"
        operation.provider_executable.write_bytes(b"helper fixture\n")
        probe = LaunchProbe(lambda: operation._discovery_state().intent, outcome)
        operation.processes = probe
        return operation, probe

    def test_discovery_records_one_owned_intent_before_launch_without_environment(self) -> None:
        operation, probe = self.discovery(RuntimeError("reply lost"))
        with self.assertRaisesRegex(RuntimeError, "reply lost"):
            operation.discover_models()
        self.assertEqual(len(probe.snapshots), 1)
        intent = probe.snapshots[0]
        self.assertIsNotNone(intent, "discovery must retain its intent before launch")
        self.assertIs(operation._discovery_state().intent, intent)
        self.assertEqual((intent.models_root, intent.initial_entries, intent.provider, intent.cwd),
                         (str(operation.models_state), ("existing-entry",), PROVIDER, str(operation.workspace)))
        self.assertEqual((intent.permission, intent.definition_revision, intent.budget, intent.budget_nanos),
                         (PROFILES[PROVIDER]["modes"][0], MODEL_DISCOVERY_DEFINITION_REVISION,
                          MODEL_DISCOVERY_TASK_BUDGET, MODEL_DISCOVERY_TASK_BUDGET_NANOS))
        self.assertEqual((intent.brief_sha256, intent.helper_executable, intent.helper_sha256),
                         (sha(b"model discovery"), str(operation.provider_executable),
                          sha(b"helper fixture\n")))
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

    def test_unreadable_helper_fingerprint_refuses_before_launch(self) -> None:
        operation, probe = self.discovery(RuntimeError("must not launch"))
        operation.provider_executable.unlink()
        with self.assertRaises(BaseException) as refused:
            operation.discover_models()
        self.assertIsInstance(refused.exception, FileNotFoundError)
        self.assertEqual(probe.snapshots, [])
        self.assertIsNone(operation._discovery_state().intent)


INVALID_INTENT = "model discovery request intent is invalid"


def intent_fields(**overrides: object) -> dict[str, object]:
    fields: dict[str, object] = {
        "models_root": "/models-state", "initial_entries": ("existing-entry",), "provider": PROVIDER,
        "permission": PROFILES[PROVIDER]["modes"][0], "cwd": "/workspace",
        "definition_revision": MODEL_DISCOVERY_DEFINITION_REVISION, "budget": MODEL_DISCOVERY_TASK_BUDGET,
        "budget_nanos": MODEL_DISCOVERY_TASK_BUDGET_NANOS, "brief_sha256": sha(b"model discovery"),
        "helper_executable": "/bin/codex", "helper_sha256": sha(b"helper fixture\n")}
    fields.update(overrides)
    return fields


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
                            ("helper_sha256", sha(b"other"))):
            with self.subTest(field=name), self.assertRaises(FrozenInstanceError):
                setattr(intent, name, value)
        self.assertEqual(ModelDiscoveryIntent(**intent_fields()), intent)

    def test_malformed_brief_or_helper_digest_is_refused(self) -> None:
        for name in ("brief_sha256", "helper_sha256"):
            for digest in ("", "a" * 63, "a" * 65, "A" * 64, "g" * 64, None):
                with self.subTest(field=name, digest=digest), \
                        self.assertRaisesRegex(AcceptanceFailure, INVALID_INTENT):
                    ModelDiscoveryIntent(**intent_fields(**{name: digest}))

    def test_boolean_zero_or_negative_budget_is_refused(self) -> None:
        for budget in (True, False, 0, -1, -MODEL_DISCOVERY_TASK_BUDGET_NANOS):
            with self.subTest(budget=budget), self.assertRaisesRegex(AcceptanceFailure, INVALID_INTENT):
                ModelDiscoveryIntent(**intent_fields(budget_nanos=budget))

    def test_mutable_initial_entries_are_refused_without_changing_the_caller_value(self) -> None:
        entries = ["existing-entry"]
        with self.assertRaisesRegex(AcceptanceFailure, INVALID_INTENT):
            ModelDiscoveryIntent(**intent_fields(initial_entries=entries))
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
