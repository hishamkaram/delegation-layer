"""Hermetic checks that the contributor gate cannot hide runner failures."""
from types import SimpleNamespace
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from acceptance_contributor import ContributorAcceptance, verify_case_evidence
from acceptance_supervisor_common import sha


ROOT_ID = "a" * 32
TASK_ID = "b" * 32
LABEL = "delegate:" + ROOT_ID + ":" + TASK_ID


def gate_with_statuses(statuses):
    gate = object.__new__(ContributorAcceptance)

    def client(_name, _operation):
        status = statuses.pop(0)
        return SimpleNamespace(json=lambda: {"tasks": {"0": {"label": LABEL, "status": status}}})

    gate.client = client
    return gate


class ContributorHarnessTests(unittest.TestCase):
    def test_rejection_verdict_cannot_mask_wrong_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary) / TASK_ID
            (directory / "raw").mkdir(parents=True)
            (directory / "provider.exit").write_text(json.dumps({"exit_code": 0, "error": ""}))
            (directory / "raw/stderr").write_bytes(b"")
            envelope = {"protocol": "contributor-proof/v1", "task_id": TASK_ID,
                        "session_id": "session-" + TASK_ID, "status": "complete", "answer": "answer"}
            (directory / "raw/stdout").write_text(json.dumps(envelope))
            (directory / "raw/answer.txt").write_bytes(b"")
            (directory / "publish.reject").write_bytes(b"empty-output")
            verify_case_evidence("empty", directory, "answer", "session-" + TASK_ID)
            # A catch-all rejection cannot certify the empty-file behavior.
            (directory / "publish.reject").write_bytes(b"malformed-envelope")
            with self.assertRaisesRegex(RuntimeError, "rejection reason mismatch"):
                verify_case_evidence("empty", directory, "answer", "session-" + TASK_ID)
            (directory / "publish.reject").write_bytes(b"empty-output")
            (directory / "raw/stdout").write_text(json.dumps({**envelope, "task_id": "c" * 32}))
            with self.assertRaisesRegex(RuntimeError, "sealed envelope mismatch"):
                verify_case_evidence("empty", directory, "answer", "session-" + TASK_ID)
            (directory / "raw/stdout").write_text(json.dumps(envelope))
            (directory / "provider.exit").write_text(json.dumps({"exit_code": 2, "error": ""}))
            with self.assertRaisesRegex(RuntimeError, "provider exit mismatch"):
                verify_case_evidence("empty", directory, "answer", "session-" + TASK_ID)

    def test_replay_must_return_the_original_outcome(self):
        for response in ({}, {"outcome": {"verdict": "rejected"}}, "original"):
            with self.subTest(response=response), tempfile.TemporaryDirectory() as temporary:
                gate = object.__new__(ContributorAcceptance)
                gate.root = Path(temporary)
                directory = gate.root / "tasks" / TASK_ID
                (directory / "provider-input").mkdir(parents=True)
                (directory / "provider-output").mkdir()
                gate.provider = gate.root / "provider"
                gate.provider.write_bytes(b"fixture")
                gate.tasks = {"present": TASK_ID}
                gate.rows = {"present": {"verdict": "committed"}}
                receipts = {kind + "/" + TASK_ID + ".json": "digest" for kind in ("launches", "completions")}
                gate.receipt_snapshot = lambda: receipts.copy()
                gate.snapshot = lambda _task_id: {"unchanged": "digest"}
                outcome = {"verdict": "committed", "payload": {"basename": "result.txt", "length": 6,
                                                               "sha256": sha(b"answer")}}
                (directory / "outcome.json").write_text(json.dumps(outcome))
                (directory / "result.txt").write_bytes(b"answer")
                gate.collect = lambda _name, _task_id, _expected: {"outcome": outcome} if response == "original" else response
                if response == "original":
                    gate.verify_replay()
                else:
                    with self.assertRaisesRegex(RuntimeError, "sole outcome authority disagree"):
                        gate.verify_replay()

    def test_published_outcome_cannot_mask_failed_runner(self):
        for result in ({"Failed": 1}, "Killed", None):
            with self.subTest(result=result):
                gate = gate_with_statuses([{"Done": {"result": result}}])
                with self.assertRaisesRegex(RuntimeError, "failed runner"):
                    gate.wait_runner_done(ROOT_ID, TASK_ID)

    def test_waits_for_successful_runner_completion(self):
        statuses = ["Running", {"Done": {"result": "Success"}}]
        gate = gate_with_statuses(statuses)
        with patch("acceptance_contributor.time.sleep") as sleep:
            gate.wait_runner_done(ROOT_ID, TASK_ID)
        self.assertEqual(statuses, [])
        sleep.assert_called_once()

    def test_rejects_an_unrelated_supervisor_row(self):
        gate = gate_with_statuses([{"Done": {"result": "Success"}}])
        with self.assertRaisesRegex(RuntimeError, "no unique supervisor row"):
            gate.wait_runner_done("c" * 32, TASK_ID)

    def test_completion_wait_has_a_finite_deadline(self):
        gate = gate_with_statuses([])
        with patch("acceptance_contributor.time.monotonic", side_effect=[0, 151]):
            with self.assertRaisesRegex(RuntimeError, "within acceptance deadline"):
                gate.wait_runner_done(ROOT_ID, TASK_ID)


if __name__ == "__main__":
    unittest.main()
