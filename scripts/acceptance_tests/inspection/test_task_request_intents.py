"""Prelaunch request intents retained by the canonical task ledger; every launch is a fake."""

from __future__ import annotations

import hashlib
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest

from scripts.acceptance_supervisor_common import write_json
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps
from scripts.delegate_acceptance.tasks.request_intent import CONTINUE, DISPATCH, TaskRequest


ROOT = "a" * 32
TASK = "b" * 32
PREDECESSOR = "c" * 32
SUCCESSOR = "d" * 32
BRIEF = hashlib.sha256(b"brief\n").hexdigest()


class RecordingProcesses:
    """Fake process owner that snapshots the ledger's intents at the launch boundary."""

    def __init__(self, ops_holder: list[NativeTaskOps], outcome: object) -> None:
        self.ops_holder, self.outcome = ops_holder, outcome
        self.intents_at_launch: list[tuple[object, ...]] = []

    def run(self, name: str, argv: list[object], cwd: Path, **_options: object) -> object:
        self.intents_at_launch.append(tuple(self.ops_holder[0].request_intents))
        if isinstance(self.outcome, BaseException):
            raise self.outcome
        return self.outcome


class TaskRequestIntentTests(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="task-request-intent-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        for name in ("state", "state-parent", "output"):
            (self.base / name).mkdir()

    def ops(self, outcome: object) -> tuple[NativeTaskOps, RecordingProcesses]:
        holder: list[NativeTaskOps] = []
        processes = RecordingProcesses(holder, outcome)
        ops = NativeTaskOps(processes, self.base / "delegate", self.base / "runner", self.base / "pueue",
                            self.base / "state-parent", self.base / "state", self.base / "output")
        ops.root_id = ROOT
        holder.append(ops)
        return ops, processes

    def process(self, stdout: object, exit_code: int = 0) -> SimpleNamespace:
        directory = self.base / f"process-{len(list(self.base.iterdir()))}"
        directory.mkdir()
        if isinstance(stdout, bytes):
            (directory / "stdout").write_bytes(stdout)
        elif stdout is not None:
            write_json(directory / "stdout", stdout)
        return SimpleNamespace(directory=directory, result={"exit_code": exit_code, "natural_wait": True})

    def continuation(self) -> TaskRequest:
        return TaskRequest(role="timeout-resume", brief_sha256=BRIEF, budget="5m",
                           predecessor_task_id=PREDECESSOR)

    def admitted(self, task: str) -> dict[str, object]:
        return {"schema_version": 1, "command": "continue", "root_id": ROOT, "task_id": task,
                "parent_task_id": PREDECESSOR, "admission": "admitted",
                "supervisor": {"matched": True, "state": "Queued", "numeric_task_id": 9}}

    def test_dispatch_intent_exists_at_launch_and_survives_a_lost_reply(self) -> None:
        ops, processes = self.ops(RuntimeError("reply lost"))
        request = TaskRequest(role="timeout-source", brief_sha256=BRIEF, budget="3m",
                              provider="codex:exec", permission="workspace-write", cwd="/workspace")
        with self.assertRaisesRegex(RuntimeError, "reply lost"):
            ops.dispatch("timeout-source", TASK, ["delegate"], request=request)
        self.assertEqual(len(processes.intents_at_launch), 1)
        at_launch = processes.intents_at_launch[0]
        self.assertEqual(len(at_launch), 1, "the intent must exist when the launch is called")
        intent = at_launch[0]
        self.assertEqual((intent.ordinal, intent.command, intent.task_id, intent.root_id, intent.state_root),
                         (0, DISPATCH, TASK, ROOT, str(self.base / "state")))
        self.assertEqual((intent.request.role, intent.request.budget), ("timeout-source", "3m"))
        self.assertEqual(ops.request_intents, [intent])
        self.assertEqual(ops.dispatch_attempts, {TASK})
        self.assertEqual(ops.tasks, {})

    def test_legacy_dispatch_without_metadata_gains_no_intent(self) -> None:
        ops, processes = self.ops(RuntimeError("reply lost"))
        with self.assertRaisesRegex(RuntimeError, "reply lost"):
            ops.dispatch("fresh", TASK, ["delegate"])
        self.assertEqual((processes.intents_at_launch, ops.request_intents), ([()], []))
        self.assertEqual(ops.dispatch_attempts, {TASK})

    def test_continuation_intent_survives_raise_malformed_and_missing_identity(self) -> None:
        outcomes = (("raise", RuntimeError("reply lost"), RuntimeError, "reply lost"),
                    ("malformed", b"{not json", AcceptanceFailure, "invalid JSON evidence"),
                    ("missing-id", {"admission": "unknown"}, AcceptanceFailure, "no valid successor"))
        for label, stdout, error, message in outcomes:
            with self.subTest(label):
                outcome = stdout if isinstance(stdout, BaseException) else self.process(stdout, exit_code=1)
                ops, processes = self.ops(outcome)
                with self.assertRaisesRegex(error, message):
                    ops.continue_task("timeout-resume", ["delegate"], request=self.continuation())
                self.assertEqual(len(processes.intents_at_launch), 1)
                at_launch = processes.intents_at_launch[0]
                self.assertEqual(len(at_launch), 1, "the intent must exist when the launch is called")
                intent = at_launch[0]
                self.assertEqual((intent.command, intent.task_id, intent.request.predecessor_task_id),
                                 (CONTINUE, None, PREDECESSOR))
                self.assertEqual(ops.request_intents, [intent])
                self.assertEqual((ops.continuation_successors, ops.dispatch_attempts), ({}, set()))

    def test_valid_continuation_binds_its_successor_once(self) -> None:
        ops, _ = self.ops(self.process(self.admitted(SUCCESSOR)))
        task, _response = ops.continue_task("timeout-resume", ["delegate"], request=self.continuation())
        self.assertEqual(len(ops.request_intents), 1, "the continuation must retain its intent")
        self.assertEqual(task, SUCCESSOR)
        self.assertEqual(ops.continuation_successors, {0: SUCCESSOR})
        self.assertIn(SUCCESSOR, ops.dispatch_attempts)
        ops.bind_continuation_successor(0, SUCCESSOR)
        with self.assertRaisesRegex(AcceptanceFailure, "already bound to another successor"):
            ops.bind_continuation_successor(0, "e" * 32)
        self.assertEqual(ops.continuation_successors, {0: SUCCESSOR})

    def test_successor_binding_refuses_undeclared_dispatch_and_shared_successors(self) -> None:
        ops, _ = self.ops(RuntimeError("reply lost"))
        for _ in range(2):
            with self.assertRaises(RuntimeError):
                ops.continue_task("timeout-resume", ["delegate"], request=self.continuation())
        self.assertEqual(len(ops.request_intents), 2, "each continuation must retain its intent")
        ops.processes.outcome = RuntimeError("dispatch reply lost")
        with self.assertRaises(RuntimeError):
            ops.dispatch("fresh", TASK, ["delegate"],
                         request=TaskRequest(role="fresh", brief_sha256=BRIEF, budget="5m"))
        ops.bind_continuation_successor(0, SUCCESSOR)
        for ordinal, task, message in ((5, "e" * 32, "no declared continuation"),
                                       (2, "e" * 32, "no declared continuation"),
                                       (1, SUCCESSOR, "already bound to another request"),
                                       (1, "not-a-task", "successor identity is invalid")):
            with self.subTest(ordinal=ordinal, task=task), self.assertRaisesRegex(AcceptanceFailure, message):
                ops.bind_continuation_successor(ordinal, task)
        self.assertEqual(ops.continuation_successors, {0: SUCCESSOR})

    def test_invalid_request_refuses_before_any_launch_or_attempt(self) -> None:
        ops, processes = self.ops(RuntimeError("must not launch"))
        with self.assertRaisesRegex(AcceptanceFailure, "brief digest is invalid"):
            ops.dispatch("fresh", TASK, ["delegate"],
                         request=TaskRequest(role="fresh", brief_sha256="raw", budget="5m"))
        self.assertEqual((processes.intents_at_launch, ops.request_intents, ops.dispatch_attempts), ([], [], set()))


if __name__ == "__main__":
    unittest.main()
