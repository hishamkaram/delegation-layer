"""Hermetic checks that the contributor gate cannot hide runner failures."""
from types import SimpleNamespace
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import acceptance_contributor as gate_module
from acceptance_contributor import ContributorAcceptance, verify_case_evidence
from acceptance_provider_common import AcceptanceFailure
from acceptance_supervisor_common import DirectoryOwner, sha


ROOT_ID = "a" * 32
TASK_ID = "b" * 32
LABEL = "delegate:" + ROOT_ID + ":" + TASK_ID


def gate_with_statuses(statuses):
    gate = object.__new__(ContributorAcceptance)

    def client(_name, _operation, timeout):
        if timeout <= 0:
            raise AssertionError("runner status timeout must remain positive")
        status = statuses.pop(0)
        return SimpleNamespace(json=lambda: {"tasks": {"0": {"label": LABEL, "status": status}}})

    gate.client = client
    return gate


class ContributorHarnessTests(unittest.TestCase):
    def test_finalize_removes_closed_private_state_and_raw_process_captures(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            base_parent = root / "temporary"
            base = base_parent / "dc-run"
            output.mkdir()
            base_parent.mkdir()
            owner = DirectoryOwner()
            base_token = owner.create("contributor private state", base, base_parent)
            raw_token = owner.create("raw process captures", output / "processes", output)
            gate = object.__new__(ContributorAcceptance)
            gate.output = output
            gate.base = base
            gate.owned_directories = (base_token, raw_token)
            gate.processes = type("Processes", (), {"capture_root": raw_token})()
            gate.closed = True

            self.assertTrue(gate.finalize(True))

            self.assertFalse(base.exists())
            self.assertFalse((output / "processes").exists())
            receipt = json.loads((output / "cleanup.json").read_text())
            self.assertEqual(receipt["status"], "removed")

    def test_constructor_rolls_back_allocated_private_base(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tools = root / "tools"
            tools.mkdir()
            pueue = root / "pueue"
            pueued = root / "pueued"
            pueue.write_bytes(b"pueue")
            pueued.write_bytes(b"pueued")
            output = root / "output"
            base = root / "private"

            def allocate(**_kwargs):
                base.mkdir()
                return str(base)

            with patch.object(gate_module.tempfile, "mkdtemp", side_effect=allocate), \
                    patch.object(gate_module, "Processes",
                                 side_effect=OSError("injected process owner failure")):
                with self.assertRaisesRegex(OSError, "process owner failure"):
                    ContributorAcceptance(tools, pueue, pueued, output)
            self.assertFalse(base.exists())

    def test_constructor_rolls_back_when_base_resolve_fails(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tools = root / "tools"
            tools.mkdir()
            pueue = root / "pueue"
            pueued = root / "pueued"
            pueue.write_bytes(b"pueue")
            pueued.write_bytes(b"pueued")
            output = root / "output"
            base = root / "private"

            def allocate(**_kwargs):
                base.mkdir()
                return str(base)

            original_resolve = gate_module.Path.resolve

            def fail_base_resolve(path, *args, **kwargs):
                if path == base:
                    raise OSError("injected base resolve failure")
                return original_resolve(path, *args, **kwargs)

            with patch.object(gate_module.tempfile, "mkdtemp", side_effect=allocate), \
                    patch.object(gate_module.Path, "resolve", new=fail_base_resolve):
                with self.assertRaisesRegex(OSError, "base resolve failure"):
                    ContributorAcceptance(tools, pueue, pueued, output)
            self.assertFalse(base.exists())

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
            # A catch-all rejection cannot prove the empty-file behavior.
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
                with self.assertRaisesRegex(AcceptanceFailure, "supervisor result"):
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

    def test_diagnostic_failure_does_not_skip_contributor_finalization(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            calls = []
            gate = object.__new__(ContributorAcceptance)
            gate.output = output
            gate.base = root / "private"
            gate.closed = False
            gate.tasks = {}
            gate.daemon = None
            gate.processes = SimpleNamespace(drain=lambda **_options: [])

            def fail_setup():
                raise RuntimeError("primary failure")

            gate.setup = fail_setup
            gate.finalize = lambda success: calls.append(("finalize", success)) or True
            with patch("acceptance_contributor.write_json", side_effect=OSError("diagnostic disk full")):
                with self.assertRaisesRegex(RuntimeError, "primary failure"):
                    gate.run()

            self.assertEqual(calls, [("finalize", False)])

    def test_contributor_cleanup_failure_is_fatal_after_primary_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            gate = object.__new__(ContributorAcceptance)
            gate.output = output
            gate.base = root / "private"
            gate.closed = False
            gate.tasks = {}
            gate.daemon = None
            gate.processes = SimpleNamespace(drain=lambda **_options: [])

            def fail_setup():
                raise RuntimeError("primary failure")

            gate.setup = fail_setup
            gate.finalize = lambda _success: False
            with patch("acceptance_contributor.write_json"):
                with self.assertRaisesRegex(RuntimeError, "cleanup failed"):
                    gate.run()

    def test_behavioral_success_is_not_published_when_finalization_fails(self):
        for failure in (False, OSError("injected finalization failure")):
            with self.subTest(failure=repr(failure)), \
                    tempfile.TemporaryDirectory(prefix="contributor-finalize-unit-") as temporary:
                root = Path(temporary)
                output = root / "evidence"
                output.mkdir()
                owner = object.__new__(ContributorAcceptance)
                owner.output = output
                owner.base = root / "private"
                owner.root = root / "state"
                owner.tasks = {}
                owner.rows = {}
                owner.preflight = {}
                owner.closed = True
                owner.daemon = None
                owner.processes = SimpleNamespace(drain=lambda **_options: [])
                owner.setup = lambda: None
                owner.snapshot = lambda task_id: {"task_id": task_id}
                owner.verify_preflight_rejections = lambda: None
                owner.verify_replay = lambda: None
                owner.shutdown = lambda: None
                sequence = iter(range(1, 32))

                def run_case(name, verdict, *_args, **_kwargs):
                    task_id = f"{next(sequence):032x}"
                    owner.tasks[name] = task_id
                    owner.rows[name] = {"verdict": verdict}
                    return task_id

                owner.run_case = run_case
                calls = []

                def finalize(success):
                    calls.append(success)
                    if success and isinstance(failure, BaseException):
                        raise failure
                    return not success or failure is not False

                owner.finalize = finalize
                with patch("acceptance_contributor.read_json",
                           return_value={"conversation_id": "fixture-session"}):
                    with self.assertRaises((RuntimeError, OSError)):
                        owner.run()

                self.assertEqual(calls, [True, False])
                self.assertFalse((output / "success.json").exists())
                self.assertTrue((output / "failure.json").is_file())
                cleanup = json.loads((output / "failure-cleanup.json").read_text())
                self.assertTrue(cleanup["passed_receipt_absent"])
                self.assertTrue(cleanup["artifact_finalization_completed"])


if __name__ == "__main__":
    unittest.main()
