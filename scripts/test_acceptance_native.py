"""Hermetic oracles for the Pi/OpenCode live acceptance gate."""
import json
from pathlib import Path
import shlex
import tempfile
import unittest
from unittest.mock import patch

import acceptance_native as gate


class NativeAcceptanceOracleTests(unittest.TestCase):
    def _queue_oracle(self, root: Path, with_inspection: bool = True):
        state = root / "state"
        state.mkdir()
        runner = root / "delegate-run"
        runner.write_text("#!/bin/sh\n")
        runner.chmod(0o700)
        task = "a" * 32
        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "b" * 32
        operation.state = state
        operation.runner = runner
        operation.tasks = {"fresh": task}
        operation.numeric_ids = {task: 7}
        operation.processes = object()
        operation.delegate = runner
        operation.pueue = root / "pueue"
        operation.config = root / "pueue.yml"
        operation.output = root / "output"
        operation.output.mkdir()
        operation.pueue.write_text("pueue\n")
        operation.config.write_text("config\n")
        operation.provider_executable = runner
        operation.provider_sha256 = gate.digest(runner)
        operation.daemon = None

        ordinary_command = shlex.join([str(runner), "--root", str(state), task])
        rows = {
            "7": {
                "id": 7, "label": f"delegate:{operation.root_id}:{task}",
                "group": "default", "original_command": ordinary_command,
                "command": ordinary_command, "path": str(state),
                "status": {"Done": {"result": "Success"}},
            },
        }
        groups = {"default": {"status": "Running", "parallel_tasks": 1}}
        if with_inspection:
            inspection_group = gate.INSPECTION_GROUP_PREFIX + operation.root_id
            inspection_label = f"{inspection_group}-{task}"
            inspection_command = shlex.join([str(runner), "--inspection", "--root", str(state), task])
            rows["8"] = {
                "id": 8, "label": inspection_label, "group": inspection_group,
                "original_command": inspection_command, "command": inspection_command,
                "path": str(state),
                "status": {"Done": {"result": "Success"}},
            }
            groups[inspection_group] = {"status": "Running", "parallel_tasks": 1}
            operation._inspection_queue_records = lambda: {
                task: {"group": inspection_group, "label": inspection_label,
                       "numeric_task_id": 8}
            }
        else:
            operation._inspection_queue_records = lambda: {}
        return operation, {"tasks": rows, "groups": groups}

    def test_terminal_queue_accepts_owned_inspection_rows(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = self._queue_oracle(Path(directory))
            self.assertTrue(operation.terminal_private_queue(status))

    def test_terminal_queue_rejects_unknown_inspection_rows(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = self._queue_oracle(Path(directory))
            status["tasks"]["9"] = dict(status["tasks"]["8"])
            status["tasks"]["9"]["id"] = 9
            self.assertFalse(operation.terminal_private_queue(status))

    def test_terminal_queue_rejects_malformed_inspection_record(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = self._queue_oracle(Path(directory))
            operation._inspection_queue_records = lambda: {
                "a" * 32: {"group": "delegation-inspection-" + "b" * 32,
                           "label": "delegation-inspection-" + "b" * 32 + "-" + "a" * 32}
            }
            self.assertFalse(operation.terminal_private_queue(status))

    def test_inspection_queue_uses_complete_validator_and_exact_coverage(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = self._queue_oracle(Path(directory))
            del operation._inspection_queue_records
            task = next(iter(operation.tasks.values()))
            group = gate.INSPECTION_GROUP_PREFIX + operation.root_id
            binding = {
                "helper_executable": str(operation.provider_executable),
                "helper_sha256": operation.provider_sha256,
            }
            with patch.object(gate, "NativeTaskOps") as validator_type:
                validator = validator_type.return_value
                validator.validate_inspection_journals.return_value = {
                    task: {"group": group, "label": group + "-" + task,
                           "numeric_task_id": 8, "request": {"binding": binding}}
                }
                self.assertTrue(operation.terminal_private_queue(status))
                validator.validate_inspection_journals.assert_called_once_with(
                    allow_failure=False, expected_binding_required=False)

    def test_inspection_queue_rejects_missing_admitted_task_journal(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = self._queue_oracle(Path(directory))
            del operation._inspection_queue_records
            with patch.object(gate, "NativeTaskOps") as validator_type:
                validator_type.return_value.validate_inspection_journals.return_value = {}
                self.assertFalse(operation.terminal_private_queue(status))

    def test_empty_queue_is_terminal_before_any_task_is_admitted(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, _ = self._queue_oracle(Path(directory), with_inspection=False)
            operation.root_id = None
            operation.tasks = {}
            status = {"tasks": {}, "groups": {
                "default": {"status": "Running", "parallel_tasks": 1},
            }}
            self.assertTrue(operation.terminal_private_queue(status))
            self.assertTrue(operation.terminal_private_queue({"tasks": {}, "groups": {}}))
            self.assertFalse(operation.terminal_private_queue({
                "tasks": {"1": {}}, "groups": status["groups"],
            }))

    def test_authentication_unavailable_is_positive_and_bounded(self):
        with tempfile.TemporaryDirectory(prefix="native-auth-unit-") as directory:
            root = Path(directory)
            raw = root / "raw"
            raw.mkdir()
            (root / "outcome.json").write_text(json.dumps({"verdict": "rejected"}))
            (raw / "stderr").write_text("please log in before using this provider")
            self.assertTrue(gate.authentication_unavailable(root))

            (root / "outcome.json").write_text(json.dumps({"verdict": "committed"}))
            self.assertFalse(gate.authentication_unavailable(root))

            (root / "outcome.json").write_text(json.dumps({"verdict": "rejected"}))
            (raw / "stderr").write_text("provider returned a normal task refusal")
            self.assertFalse(gate.authentication_unavailable(root))

    def test_missing_or_unreadable_outcome_cannot_block(self):
        with tempfile.TemporaryDirectory(prefix="native-auth-unit-") as directory:
            root = Path(directory)
            self.assertFalse(gate.authentication_unavailable(root))
            (root / "outcome.json").write_bytes(b"{")
            self.assertFalse(gate.authentication_unavailable(root))

    def test_output_paths_are_outside_temporary_roots(self):
        with self.assertRaises(gate.AcceptanceFailure):
            gate.choose_output("pi:json", "/tmp/native-acceptance-output")

    def test_missing_prerequisite_is_blocked(self):
        with self.assertRaises(gate.BlockedFailure):
            gate.resolve_executable(None, Path("/does/not/exist"), "provider")

    def test_executable_alias_resolves_to_canonical_target(self):
        with tempfile.TemporaryDirectory(prefix="native-executable-unit-") as directory:
            root = Path(directory)
            target = root / "provider"
            target.write_text("#!/bin/sh\nexit 0\n")
            target.chmod(0o700)
            alias = root / "provider-alias"
            alias.symlink_to(target)
            self.assertEqual(gate.resolve_executable(str(alias), target, "provider"), target.resolve())

    def test_dispatch_binding_requires_exact_supervisor_identity(self):
        response = {
            "task_id": "a" * 32,
            "root_id": "b" * 32,
            "supervisor": {"matched": True, "numeric_task_id": 7},
        }
        self.assertEqual(gate.dispatch_binding(response, "a" * 32, "fresh"), ("b" * 32, 7))
        with self.assertRaises(gate.AcceptanceFailure):
            gate.dispatch_binding(response, "c" * 32, "fresh")
        response["supervisor"]["matched"] = False
        with self.assertRaises(gate.AcceptanceFailure):
            gate.dispatch_binding(response, "a" * 32, "fresh")


if __name__ == "__main__":
    unittest.main()
