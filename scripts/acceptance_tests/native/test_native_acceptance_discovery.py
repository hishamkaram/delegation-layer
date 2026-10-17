"""Hermetic oracles for a native acceptance behavior cohort."""
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

import acceptance_provider_common as provider_common
from scripts import acceptance_native as entry
from scripts.acceptance_tests.resources import track_directory_pins
from scripts.acceptance_tests.native.support import (
    acceptance_task_ops,
    queue_oracle,
)
from scripts.delegate_acceptance.native import (
    acceptance as gate,
)


class NativeAcceptanceOracleTests(unittest.TestCase):
    def test_model_discovery_uses_shared_config_and_separate_state_root(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.delegate = Path("/delegate")
        operation.models_state = Path("/models-state")
        operation.config = Path("/pueue.yml")
        operation.runner = Path("/runner")
        operation.provider = "claude:print"
        operation.workspace = Path("/workspace")
        argv = operation.model_discovery_argv()
        self.assertEqual(argv[argv.index("--pueue-config") + 1], operation.config)
        self.assertEqual(argv[argv.index("--root") + 1], operation.models_state)
        self.assertEqual(argv[argv.index("--runner") + 1], operation.runner)
        self.assertEqual(argv[argv.index("--provider") + 1], operation.provider)
        self.assertEqual(argv[argv.index("--cwd") + 1], operation.workspace)
        self.assertNotIn("pueued", [str(value) for value in argv])
        self.assertEqual(gate.MODEL_DISCOVERY_TIMEOUT_SECONDS, 330)

    def test_model_discovery_success_propagates_zero_exit(self):
        with tempfile.TemporaryDirectory(prefix="native-model-run-") as directory:
            operation = object.__new__(gate.NativeAcceptance)
            operation.output = Path(directory)
            operation.provider = "codex:exec"
            operation.mode = "read-only"
            operation.args = SimpleNamespace(model=None, effort=None)
            operation.discover_models_only = True
            operation.model_discovery_exit_code = None
            operation.model_discovery_summary = {"status": "available"}
            operation.closed = False
            operation.setup = lambda: None
            operation.discover_models = lambda: setattr(operation, "model_discovery_exit_code", 0)
            operation.shutdown = lambda: setattr(operation, "closed", True)
            operation.native_mode_receipt = lambda: None
            self.assertEqual(operation.run(), 0)
            self.assertFalse((Path(directory) / "success.json").exists())
            operation.publish_success()
            self.assertEqual(gate.read_json(Path(directory) / "success.json")["discovery_exit_code"], 0)

    def test_pi_empty_native_catalog_blocks_before_task_dispatch(self):
        with tempfile.TemporaryDirectory(prefix="native-pi-prerequisite-") as directory:
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "pi:json"
            operation.mode = "read-only"
            operation.scenario = "read-only"
            operation.args = SimpleNamespace(model=None)
            operation.discover_models_only = False
            operation.setup = Mock()
            operation.workspace = Path(directory)
            operation.brief = Mock(return_value=Path(directory) / "brief")
            operation.dispatch = Mock(side_effect=AssertionError("paid task must not start"))
            operation.discover_models = Mock()
            operation.model_discovery = {
                "status": "unavailable", "reason_code": "models_unavailable", "models": [],
            }
            with self.assertRaisesRegex(gate.BlockedFailure, "no locally available model"):
                operation.run()
            operation.setup.assert_called_once()
            operation.discover_models.assert_called_once()
            operation.dispatch.assert_not_called()

    def test_pi_model_prerequisite_preserves_explicit_choices_and_unknown_readiness(self):
        for provider, model in (("pi:json", "chosen/model"), ("pi:json", ""),
                                ("codex:exec", None)):
            with self.subTest(provider=provider, model=model):
                operation = object.__new__(gate.NativeAcceptance)
                operation.provider = provider
                operation.args = SimpleNamespace(model=model)
                operation.discover_models = Mock()
                operation.require_default_model_prerequisite()
                operation.discover_models.assert_not_called()
        for status, reason in (("available", "ok"),
                               ("unavailable", "interface_unavailable")):
            with self.subTest(status=status):
                operation = object.__new__(gate.NativeAcceptance)
                operation.provider = "pi:json"
                operation.args = SimpleNamespace(model=None)
                operation.discover_models = Mock()
                operation.model_discovery = {"status": status, "reason_code": reason}
                operation.require_default_model_prerequisite()
                operation.discover_models.assert_called_once()

    def test_pi_model_prerequisite_keeps_discovery_failure_failed(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.provider = "pi:json"
        operation.args = SimpleNamespace(model=None)
        operation.discover_models = Mock()
        operation.model_discovery = {"status": "failed", "reason_code": "invalid_native_output"}
        with self.assertRaisesRegex(gate.AcceptanceFailure, "default-model discovery failed") as raised:
            operation.require_default_model_prerequisite()
        self.assertNotIsInstance(raised.exception, gate.BlockedFailure)

    def test_pi_model_prerequisite_consumes_structured_authentication_block(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.provider = "pi:json"
        operation.args = SimpleNamespace(model=None)
        operation.discover_models = Mock()
        operation.model_discovery = {"status": "blocked", "reason_code": "authentication_unavailable"}
        with self.assertRaisesRegex(gate.BlockedFailure, "authentication"):
            operation.require_default_model_prerequisite()

    def test_pi_model_prerequisite_rejects_non_authentication_block(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.provider = "pi:json"
        operation.args = SimpleNamespace(model=None)
        operation.discover_models = Mock()
        operation.model_discovery = {"status": "blocked", "reason_code": "supervisor_upgrade_required"}
        with self.assertRaisesRegex(gate.AcceptanceFailure, "supervisor_upgrade_required") as raised:
            operation.require_default_model_prerequisite()
        self.assertNotIsInstance(raised.exception, gate.BlockedFailure)

    def test_default_model_failure_receipt_retains_sanitized_discovery_reason(self):
        cases = (
            (gate.BlockedFailure("Pi model discovery authentication is unavailable"),
             "blocked", "authentication_unavailable", 2, "BLOCKED"),
            (gate.AcceptanceFailure("Pi default-model discovery is blocked: supervisor_upgrade_required"),
             "blocked", "supervisor_upgrade_required", 1, "failed"),
        )
        for error, status, reason, expected_code, expected_status in cases:
            with self.subTest(reason=reason), tempfile.TemporaryDirectory(
                    prefix="native-default-model-receipt-") as directory:
                output = Path(directory) / "evidence"
                output.mkdir()

                class Run:
                    model_discovery_summary = {
                        "status": status, "reason_code": reason, "complete": False,
                        "model_count": 0, "effort_count": None,
                        "raw_error": "must not be copied",
                    }

                    def run(self):
                        raise error

                    def finalize(self, _success, *, blocked=False):
                        return True

                    def publish_success(self):
                        raise AssertionError("default-model failure published success")

                with patch.object(entry, "choose_output", return_value=output), \
                        patch.object(entry, "NativeAcceptance", return_value=Run()):
                    code = entry.main([
                        "--provider", "pi:json", "--permission", "read-only",
                        "--output", str(output),
                    ])

                self.assertEqual(code, expected_code)
                receipt = gate.read_json(output / "failure.json")
                self.assertEqual(receipt["status"], expected_status)
                self.assertEqual(receipt["model_discovery"], {
                    "status": status, "reason_code": reason, "complete": False,
                    "model_count": 0, "effort_count": None,
                })

    def test_native_discovery_environment_reaches_process_without_credentials(self):
        selectors = {name: "/native/" + name.lower() for name in (
            "CODEX_HOME", "CLAUDE_CONFIG_DIR", "ANTHROPIC_CONFIG_DIR",
            "PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR",
            "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG", "OPENCODE_TUI_CONFIG",
            "AGY_HOME", "GEMINI_HOME", "ANTIGRAVITY_HOME",
            "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS",
            "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
        )}
        credentials = {name: "secret-must-not-propagate" for name in (
            "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
            "CLAUDE_CODE_OAUTH_TOKEN", "OPENCODE_CONFIG_CONTENT", "UNRELATED_SECRET",
            "XDG_SECRET", "CODEX_SECRET",
        )}
        with tempfile.TemporaryDirectory(prefix="native-environment-unit-") as directory:
            root = Path(directory)
            with patch.dict(os.environ, {"HOME": str(root), "PATH": "/usr/bin:/bin",
                                         **selectors, **credentials}, clear=True):
                environment = gate.native_environment()
                fixture_environment = gate.inherited_environment()
            self.assertEqual({key: environment[key] for key in selectors}, selectors)
            self.assertTrue(set(credentials).isdisjoint(environment))
            self.assertNotIn("CODEX_HOME", fixture_environment)
            self.assertNotIn("CLAUDE_CONFIG_DIR", fixture_environment)
            self.assertNotIn("PI_CODING_AGENT_DIR", fixture_environment)
            self.assertNotIn("OPENCODE_CONFIG_DIR", fixture_environment)

            processes = gate.Processes(root / "processes", environment)
            track_directory_pins(self, (processes.capture_root,))
            process = processes.run("environment-probe", [sys.executable, "-c",
                "import json, os; print(json.dumps(dict(os.environ)))"], root)
            observed = gate.read_json(process.directory / "stdout")
            self.assertEqual({key: observed[key] for key in selectors}, selectors)
            self.assertTrue(set(credentials).isdisjoint(observed))
            receipt = gate.read_json(process.directory / "invocation.json")
            self.assertIn("environment_sha256", receipt)
            self.assertNotIn("environment", receipt)
            encoded = json.dumps(receipt)
            for value in (*selectors.values(), *credentials.values()):
                self.assertNotIn(value, encoded)

    def test_workspace_snapshot_tracks_empty_directories_and_modes(self):
        with tempfile.TemporaryDirectory(prefix="native-snapshot-unit-") as directory:
            workspace = Path(directory)
            before = gate.workspace_snapshot(workspace)
            empty = workspace / "empty"
            empty.mkdir(mode=0o700)
            with_directory = gate.workspace_snapshot(workspace)
            self.assertNotEqual(before, with_directory)
            self.assertEqual(with_directory["empty"], {"type": stat.S_IFDIR, "mode": 0o700})
            empty.chmod(0o755)
            self.assertNotEqual(with_directory, gate.workspace_snapshot(workspace))
            empty.rmdir()
            self.assertEqual(before, gate.workspace_snapshot(workspace))
            regular = workspace / "file"
            regular.write_bytes(b"same bytes")
            regular.chmod(0o600)
            before_chmod = gate.workspace_snapshot(workspace)
            regular.chmod(0o700)
            self.assertNotEqual(before_chmod, gate.workspace_snapshot(workspace))

    def test_workspace_snapshot_records_fifo_without_opening_it(self):
        with tempfile.TemporaryDirectory(prefix="native-snapshot-unit-") as directory:
            workspace = Path(directory)
            before = gate.workspace_snapshot(workspace)
            os.mkfifo(workspace / "pipe", mode=0o600)
            with patch.object(Path, "read_bytes", side_effect=AssertionError("opened FIFO")):
                after = gate.workspace_snapshot(workspace)
            self.assertNotEqual(before, after)
            self.assertEqual(after["pipe"], {"type": stat.S_IFIFO, "mode": 0o600})
            (workspace / "pipe").unlink()
            (workspace / "pipe").mkdir(mode=0o600)
            self.assertNotEqual(after, gate.workspace_snapshot(workspace))

    def test_workspace_snapshot_still_refuses_symlinks_and_keeps_task_semantics(self):
        with tempfile.TemporaryDirectory(prefix="native-snapshot-unit-") as directory:
            workspace = Path(directory)
            for name in (".ack-proof", "proof.staging"):
                (workspace / name).write_bytes(b"evidence")
            self.assertEqual(set(gate.workspace_snapshot(workspace)), {".ack-proof", "proof.staging"})
            self.assertEqual(gate.task_digest_snapshot(workspace), {})
            (workspace / "link").symlink_to(workspace / "missing")
            with self.assertRaisesRegex(gate.AcceptanceFailure, "symlink"):
                gate.workspace_snapshot(workspace)


    def test_write_digest_command_and_optional_final_newline(self):
        with tempfile.TemporaryDirectory(prefix="native-write-unit-") as directory:
            root = Path(directory)
            target = root / "acceptance-write-target.txt"
            artifact = root / "shell-digest.txt"
            for newline in ("", "\n"):
                target.write_text("edited:fixture" + newline)
                subprocess.run(gate.shell_digest_command(artifact.name), shell=True,
                               cwd=root, check=True, timeout=10)
                gate.verify_write_artifacts("fixture", target, artifact)
            target.write_text("edited:fixture\n\n")
            with self.assertRaises(gate.AcceptanceFailure):
                gate.verify_write_artifacts("fixture", target, artifact)

    def test_terminal_queue_accepts_owned_inspection_rows(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = queue_oracle(Path(directory))
            self.assertTrue(operation.terminal_private_queue(status))

    def test_terminal_queue_accepts_expected_failed_ordinary_task(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = queue_oracle(Path(directory))
            task = next(iter(operation.tasks.values()))
            operation.tasks = {"timeout-source": task}
            operation.ops.tasks = {"timeout-source": task}
            operation.ops.labels = {"timeout-source": f"delegate:{operation.root_id}:{task}"}
            operation.ops.numbers = {"timeout-source": 7}
            operation.ops.expected_results = {"timeout-source": "Failed"}
            status["tasks"]["7"]["status"] = {"Done": {"result": {"Failed": 1}}}
            self.assertTrue(operation.terminal_private_queue(status))

    def test_runtime_only_queue_uses_canonical_owner_without_inspection(self):
        with tempfile.TemporaryDirectory(prefix="native-runtime-only-queue-") as directory:
            operation, status = queue_oracle(
                Path(directory), with_inspection=False, runtime_only=True)

            self.assertTrue(operation.terminal_private_queue(status))

    def test_runtime_only_queue_rejects_an_inspection_journal(self):
        with tempfile.TemporaryDirectory(prefix="native-runtime-only-queue-") as directory:
            operation, status = queue_oracle(
                Path(directory), with_inspection=False, runtime_only=True)
            (operation.state / "inspections" / operation.tasks["fresh"]).mkdir(parents=True)

            self.assertFalse(operation.terminal_private_queue(status))

    def test_terminal_queue_requires_running_default_group_at_parallel_one(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-group-") as directory:
            for group in (
                    {"status": "Paused", "parallel_tasks": 1},
                    {"status": "Running", "parallel_tasks": 2}):
                with self.subTest(group=group):
                    fixture_root = Path(directory) / str(group["parallel_tasks"])
                    fixture_root.mkdir()
                    operation, status = queue_oracle(
                        fixture_root,
                        with_inspection=False, runtime_only=True)
                    status["groups"]["default"] = group

                    self.assertFalse(operation.terminal_private_queue(status))

    def test_terminal_queue_rejects_unknown_inspection_rows(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = queue_oracle(Path(directory))
            status["tasks"]["9"] = dict(status["tasks"]["8"])
            status["tasks"]["9"]["id"] = 9
            self.assertFalse(operation.terminal_private_queue(status))

    def test_write_continuation_command_preserves_content_and_line_boundary(self):
        with tempfile.TemporaryDirectory(prefix="native-write-continuation-") as directory:
            root = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.mode = "workspace-write"
            operation.briefs = root / "briefs"
            operation.briefs.mkdir()
            command = gate.shell_append_continuation_command()
            self.assertIn("`" + command + "`", operation.brief("resume", continuation=True).read_text())
            target = root / "acceptance-write-target.txt"
            artifact = root / "acceptance-write-continue-check.txt"
            nonce = "a" * 48
            for ending in (b"", b"\n"):
                with self.subTest(ending=ending):
                    target.write_bytes(f"edited:{nonce}".encode() + ending)
                    subprocess.run(["/bin/sh", "-c", command], cwd=root, check=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                    subprocess.run(["/bin/sh", "-c", gate.shell_digest_command(artifact.name)],
                                   cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                    self.assertEqual(target.read_bytes(), f"edited:{nonce}\ncontinued\n".encode())
                    gate.verify_write_artifacts(nonce, target, artifact, continuation=True)

    def test_write_continuation_does_not_repair_invalid_content(self):
        with tempfile.TemporaryDirectory(prefix="native-write-continuation-invalid-") as directory:
            root = Path(directory)
            target = root / "acceptance-write-target.txt"
            artifact = root / "acceptance-write-continue-check.txt"
            nonce = "a" * 48
            for initial in (f"corrupt:{nonce}".encode(), f"edited:{nonce}\n\n".encode()):
                with self.subTest(initial=initial):
                    target.write_bytes(initial)
                    subprocess.run(["/bin/sh", "-c", gate.shell_append_continuation_command()],
                                   cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                    self.assertTrue(target.read_bytes().startswith(initial))
                    subprocess.run(["/bin/sh", "-c", gate.shell_digest_command(artifact.name)],
                                   cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                    with self.assertRaisesRegex(gate.AcceptanceFailure, "exact edited target"):
                        gate.verify_write_artifacts(nonce, target, artifact, continuation=True)
            target.write_bytes(f"edited:{nonce}continued\n".encode())
            subprocess.run(["/bin/sh", "-c", gate.shell_digest_command(artifact.name)],
                           cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "exact edited target"):
                gate.verify_write_artifacts(nonce, target, artifact, continuation=True)

    def test_model_discovery_group_is_filtered_by_its_distinct_root(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = queue_oracle(Path(directory), with_inspection=False)
            operation.models_root_id = "c" * 32
            group = gate.INSPECTION_GROUP_PREFIX + operation.models_root_id
            status["groups"][group] = {"status": "Running", "parallel_tasks": 1}
            status["tasks"]["8"] = {
                "id": 8, "label": group + "-" + "d" * 32, "group": group,
                "status": {"Done": {"result": "Success"}},
            }
            operation._validate_model_discovery_evidence = Mock()
            filtered = operation._without_model_discovery_queue(status)
            self.assertEqual(set(filtered["tasks"]), {"7"})
            self.assertEqual(set(filtered["groups"]), {"default"})
            operation._validate_model_discovery_evidence.assert_called_once()
            projected, projected_group = operation._validate_model_discovery_evidence.call_args.args
            self.assertEqual(projected_group, group)
            self.assertEqual(set(projected["tasks"]), {"8"})
            self.assertEqual(set(projected["groups"]), {"default", group})

    def test_standalone_model_shutdown_rejects_terminal_row_without_journal(self):
        with tempfile.TemporaryDirectory(prefix="native-model-queue-unit-") as directory:
            root = Path(directory)
            operation, status = queue_oracle(root, with_inspection=False)
            operation.root_id = None
            operation.tasks = {}
            operation.numeric_ids = {}
            operation.ops = SimpleNamespace(dispatch_attempts=set(), inspection_attempts=set())
            operation.discover_models_only = True
            operation.models_state = root / "models-state"
            operation.models_state.mkdir()
            operation.models_root_id = "c" * 32
            group = gate.INSPECTION_GROUP_PREFIX + operation.models_root_id
            status["groups"][group] = {"status": "Running", "parallel_tasks": 1}
            status["tasks"]["8"] = {
                "id": 8, "label": group + "-" + "d" * 32, "group": group,
                "status": {"Done": {"result": "Success"}},
            }
            with self.assertRaises(gate.AcceptanceFailure):
                operation._without_model_discovery_queue(status)

    def test_model_prerequisite_shutdown_requires_its_own_journal(self):
        with tempfile.TemporaryDirectory(prefix="native-model-prerequisite-queue-") as directory:
            root = Path(directory)
            operation, status = queue_oracle(root, with_inspection=False)
            operation.discover_models_only = False
            operation.models_state = root / "models-state"
            operation.models_state.mkdir()
            operation.models_root_id = "c" * 32
            group = gate.INSPECTION_GROUP_PREFIX + operation.models_root_id
            status["groups"][group] = {"status": "Running", "parallel_tasks": 1}
            status["tasks"]["8"] = {
                "id": 8, "label": group + "-" + "d" * 32, "group": group,
                "status": {"Done": {"result": "Success"}},
            }
            with self.assertRaisesRegex(gate.AcceptanceFailure, "inspection journal is absent"):
                operation._without_model_discovery_queue(status)

    def test_default_model_failure_uses_model_owner_before_ordinary_dispatch(self):
        with tempfile.TemporaryDirectory(prefix="native-model-owner-selection-") as directory:
            root = Path(directory)
            operation, status = queue_oracle(root, with_inspection=False)
            operation.root_id = None
            operation.tasks = {}
            operation.models_root_id = "c" * 32
            operation.discover_models_only = False
            daemon = object()
            operation.daemon = daemon
            ops = acceptance_task_ops(operation, root)
            def assert_model_owner_bound(model_owner, observed_status):
                self.assertEqual(model_owner.pueue_config, operation.config)
                self.assertIs(model_owner.daemon, daemon)
                self.assertIs(observed_status, status)
                return True

            with patch.object(provider_common.NativeTaskOps, "failure_queue_finished",
                              autospec=True, side_effect=assert_model_owner_bound) as canonical:
                self.assertTrue(ops.failure_queue_finished(status))
            canonical.assert_called_once()

    def test_replay_membership_projects_model_group_before_canonical_validation(self):
        with tempfile.TemporaryDirectory(prefix="native-model-replay-membership-") as directory:
            root = Path(directory)
            operation, status = queue_oracle(root, with_inspection=False)
            operation.models_root_id = "c" * 32
            group = gate.INSPECTION_GROUP_PREFIX + operation.models_root_id
            status["groups"][group] = {"status": "Running", "parallel_tasks": 1}
            status["tasks"]["8"] = {
                "id": 8, "label": group + "-" + "d" * 32, "group": group,
                "status": {"Done": {"result": "Success"}},
            }
            status["groups"]["foreign"] = {"status": "Running", "parallel_tasks": 1}
            status["tasks"]["9"] = {
                "id": 9, "label": "foreign-row", "group": "foreign",
                "status": {"Done": {"result": "Success"}},
            }
            operation._validate_model_discovery_evidence = Mock()
            ops = acceptance_task_ops(operation, root)
            task = next(iter(operation.tasks.values()))
            row = status["tasks"]["7"]
            ops.tasks = {"fresh": task}
            ops.labels = {"fresh": row["label"]}
            ops.numbers = {"fresh": 7}
            ops.expected_results = {"fresh": "Success"}
            ops.dispatch_attempts = {task}
            ops.inspection_attempts = set()
            result = b"replay proof\n"
            payload = {"basename": "result.txt", "length": len(result),
                       "sha256": provider_common.sha(result)}
            outcome = {"verdict": "committed", "payload": payload,
                       "evidence_sha256": "a" * 64}
            task_directory = ops.state / "tasks" / task
            (task_directory / "result.txt").write_bytes(result)
            gate.write_json(task_directory / "outcome.json", outcome)
            ops.queue_status = Mock(side_effect=(status, status))
            ops._inspection_snapshot = Mock(return_value={})
            ops.collect = Mock(return_value={
                "outcome": outcome, "payload": payload,
                "evidence_sha256": outcome["evidence_sha256"],
            })
            with patch.object(provider_common.NativeTaskOps, "_validate_queue_membership") as canonical:
                ops.replay({
                    "fresh": {"directory": task_directory, "outcome": outcome,
                              "payload": payload},
                }, names=("fresh",))
            self.assertEqual(canonical.call_count, 2)
            for call in canonical.call_args_list:
                projected = call.args[0]
                self.assertEqual(set(projected["tasks"]), {"7", "9"})
                self.assertEqual(set(projected["groups"]), {"default", "foreign"})
            self.assertEqual(ops.queue_status.call_count, 2)
            self.assertEqual(ops.collect.call_count, 1)
            operation._validate_model_discovery_evidence.assert_called()

    def test_mixed_failure_projects_model_group_before_canonical_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="native-model-mixed-failure-") as directory:
            root = Path(directory)
            operation, status = queue_oracle(root, with_inspection=False)
            operation.models_root_id = "c" * 32
            group = gate.INSPECTION_GROUP_PREFIX + operation.models_root_id
            status["groups"][group] = {"status": "Running", "parallel_tasks": 1}
            status["tasks"]["8"] = {
                "id": 8, "label": group + "-" + "d" * 32, "group": group,
                "status": {"Done": {"result": {"Failed": 1}}},
            }
            operation._validate_model_discovery_evidence = Mock()
            ops = acceptance_task_ops(operation, root)
            task = next(iter(operation.tasks.values()))
            ops.tasks = {"fresh": task}
            ops.dispatch_attempts = {task}
            with patch.object(provider_common.NativeTaskOps, "failure_queue_finished",
                              return_value=True) as canonical:
                self.assertTrue(ops.failure_queue_finished(status))
            canonical.assert_called_once()
            projected = canonical.call_args.args[0]
            self.assertEqual(set(projected["tasks"]), {"7"})
            self.assertEqual(set(projected["groups"]), {"default"})

    def test_terminal_queue_rejects_malformed_inspection_record(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = queue_oracle(Path(directory))
            operation.ops._inspection_journals = lambda *_args: {
                "a" * 32: {"task_id": "a" * 32,
                           "group": "delegation-inspection-" + "b" * 32,
                           "label": "malformed-inspection-label",
                           "numeric_task_id": 8,
                           "inspection_exit_authority": {"mode": "queue"},
                           "result": {"reason": "eligible"}}
            }
            self.assertFalse(operation.terminal_private_queue(status))

    def test_terminal_projection_sets_exact_inspection_coverage(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, _ = queue_oracle(Path(directory))
            task = next(iter(operation.tasks.values()))
            self.assertEqual(operation._expected_inspection_attempts(), {task})
            self.assertEqual(operation.ops.inspection_attempts, {task})

    def test_inspection_expectations_exclude_pending_runtime_tasks(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-runtime-drift-") as directory:
            operation, _ = queue_oracle(Path(directory), with_inspection=False)
            first = operation.tasks["fresh"]
            second = "c" * 32
            operation.tasks["resume"] = second
            second_directory = operation.state / "tasks" / second
            second_directory.mkdir(parents=True)
            gate.write_json(second_directory / "meta.json", {
                "root_id": operation.root_id,
                "task_id": second,
                "provider_executable": str(operation.provider_executable),
                "provider_version": "",
                "runtime_preflight_sha256": "b" * 64,
                "environment_recorded": True,
            })
            self.assertEqual(operation._expected_inspection_attempts(), {first})
            self.assertEqual(operation.ops.inspection_attempts, {first})

    def test_runtime_identity_rejects_a_different_provider_executable(self):
        with tempfile.TemporaryDirectory(prefix="native-runtime-executable-") as directory:
            root = Path(directory)
            selected = root / "provider"
            selected.write_text("selected\n")
            foreign = root / "foreign-provider"
            foreign.write_text("foreign\n")
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider_executable = selected.resolve()
            task_id = "a" * 32
            meta = {
                "provider_executable": str(foreign.resolve()),
                "provider_version": "fixture-v2",
                "effective_config": {"policy": {"runtime_sha256": "b" * 64}},
            }

            with self.assertRaisesRegex(gate.AcceptanceFailure,
                                        "no admitted provider runtime identity"):
                operation._record_provider_runtime_identity(task_id, meta)

    def test_pending_runtime_identity_uses_terminal_launch_evidence(self):
        with tempfile.TemporaryDirectory(prefix="native-runtime-pending-") as directory:
            root = Path(directory)
            selected = root / "provider"
            selected.write_text("selected\n")
            refreshed = root / "provider-v2"
            refreshed.write_text("refreshed\n")
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider_executable = selected.resolve()
            task_id = "a" * 32
            meta = {
                "provider_executable": str(selected.resolve()),
                "provider_version": "",
                "runtime_preflight_sha256": "a" * 64,
                "environment_recorded": True,
            }
            started = {
                "provider_executable": str(refreshed.resolve()),
                "provider_version": "fixture-v2",
                "provider_sha256": "b" * 64,
            }
            exited = dict(started)

            identity = operation._record_provider_runtime_identity(
                task_id, meta, (started, exited))

            self.assertEqual(identity, {
                "executable": str(refreshed.resolve()),
                "version": "fixture-v2",
                "sha256": "b" * 64,
            })
            changed_exit = dict(exited, provider_version="fixture-v3")
            with self.assertRaisesRegex(gate.AcceptanceFailure, "runtime identities differ"):
                operation._record_provider_runtime_identity(
                    "c" * 32, meta, (started, changed_exit))

    def test_pending_runtime_identity_uses_started_evidence_without_exit(self):
        with tempfile.TemporaryDirectory(prefix="native-runtime-started-") as directory:
            root = Path(directory)
            selected = root / "provider"
            selected.write_text("selected\n")
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider_executable = selected.resolve()
            task_id = "a" * 32
            meta = {
                "provider_executable": str(selected.resolve()),
                "provider_version": "",
                "runtime_preflight_sha256": "a" * 64,
                "environment_recorded": True,
            }
            started = {
                "provider_executable": str(selected.resolve()),
                "provider_version": "fixture-v2",
                "provider_sha256": "b" * 64,
            }

            self.assertEqual(
                operation._record_provider_runtime_identity(
                    task_id, meta, started_evidence=started),
                {"executable": str(selected.resolve()),
                 "version": "fixture-v2", "sha256": "b" * 64},
            )
