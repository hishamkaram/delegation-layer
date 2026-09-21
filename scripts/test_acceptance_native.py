"""Hermetic oracles for the native provider live acceptance gate."""
import hashlib
import json
import os
from pathlib import Path
import shlex
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace

import acceptance_native as gate


class NativeAcceptanceOracleTests(unittest.TestCase):
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

    def test_structured_authentication_refusal_is_blocked(self):
        with tempfile.TemporaryDirectory(prefix="native-auth-unit-") as directory:
            root = Path(directory)
            raw = root / "raw"
            raw.mkdir()
            (root / "outcome.json").write_text(json.dumps({"verdict": "rejected"}))
            for status_code in (401, 403):
                (raw / "stdout").write_text(json.dumps({
                    "type": "error",
                    "error": {
                        "name": "APIError",
                        "data": {"statusCode": status_code, "message": "redacted"},
                    },
                }) + "\n")
                self.assertTrue(gate.authentication_unavailable(root))

            (raw / "stdout").write_text(json.dumps({
                "type": "error",
                "error": {"name": "APIError", "data": {"statusCode": 500}},
            }) + "\n")
            self.assertFalse(gate.authentication_unavailable(root))

    def test_admission_authentication_diagnostic_is_blocked_without_task_outcome(self):
        structured = {
            "status": "blocked",
            "capability": {
                "live_acceptance": {
                    "status": "blocked",
                    "authentication": "blocked",
                    "reason_code": "authentication_unavailable",
                },
            },
        }
        self.assertTrue(gate.authentication_blocked(structured, ""))
        for diagnostic in (
            "provider authentication unavailable: persistent ChatGPT authentication is required",
            "persistent ChatGPT authentication is required",
        ):
            self.assertTrue(gate.authentication_blocked({}, diagnostic))
        self.assertFalse(gate.authentication_blocked({}, "provider returned a normal refusal"))
        changed = json.loads(json.dumps(structured))
        changed["capability"]["live_acceptance"]["authentication"] = "unknown"
        self.assertFalse(gate.authentication_blocked(changed, ""))

    def test_deeply_nested_structured_failure_is_bounded(self):
        with tempfile.TemporaryDirectory(prefix="native-auth-unit-") as directory:
            root = Path(directory)
            raw = root / "raw"
            raw.mkdir()
            (root / "outcome.json").write_text(json.dumps({"verdict": "rejected"}))
            (raw / "stdout").write_text("[" * 20_000 + "0" + "]" * 20_000 + "\n")
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

    def test_pueue_discovery_rejects_same_bytes_at_a_different_path(self):
        with tempfile.TemporaryDirectory(prefix="native-pueue-discovery-unit-") as directory:
            root = Path(directory)
            discovered = root / "pueue"
            selected = root / "selected-pueue"
            discovered.write_text("pueue 4.0.4\n")
            selected.write_bytes(discovered.read_bytes())
            discovered.chmod(0o700)
            selected.chmod(0o700)
            environment = {"PATH": str(root)}

            gate.require_discovery("pueue", discovered, environment)
            with self.assertRaises(gate.BlockedFailure):
                gate.require_discovery("pueue", selected, environment)

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

    def test_dispatch_failure_syncs_only_positive_ops_ownership(self):
        root = "a" * 32
        task = "b" * 32
        uncertain = "c" * 32
        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = None
        operation.tasks = {}
        operation.numeric_ids = {}
        operation.ops = SimpleNamespace(
            root_id=root,
            tasks={"fresh": task, "uncertain": uncertain},
            labels={"fresh": f"delegate:{root}:{task}",
                    "uncertain": f"delegate:{root}:{uncertain}"},
            numbers={"fresh": 7},
            dispatch_attempts={task, uncertain},
        )

        operation._sync_ops_owned_tasks()

        self.assertEqual(operation.root_id, root)
        self.assertEqual(operation.tasks, {"fresh": task})
        self.assertEqual(operation.numeric_ids, {task: 7})

    def test_failure_cleanup_delegates_retention_with_owned_state(self):
        root = "a" * 32
        task = "b" * 32
        config = Path("/fixture/pueue.yml")
        daemon = object()
        calls = []
        operation = object.__new__(gate.NativeAcceptance)
        operation.closed = False
        operation.root_id = None
        operation.config = config
        operation.daemon = daemon
        operation.tasks = {}
        operation.numeric_ids = {}

        def bind_supervisor(bound_config, bound_daemon):
            calls.append(("bind", bound_config, bound_daemon))

        def retain_failure_ownership():
            calls.append(("retain", operation.ops.root_id, dict(operation.tasks),
                          dict(operation.numeric_ids)))
            return True

        operation.ops = SimpleNamespace(
            root_id=root,
            tasks={"fresh": task},
            labels={"fresh": f"delegate:{root}:{task}"},
            numbers={"fresh": 7},
            dispatch_attempts={task},
            bind_supervisor=bind_supervisor,
            retain_failure_ownership=retain_failure_ownership,
        )

        self.assertTrue(operation.safe_shutdown())
        self.assertTrue(operation.closed)
        self.assertEqual(calls, [("bind", config, daemon),
                                 ("retain", root, {"fresh": task}, {task: 7})])

    def test_failure_cleanup_refuses_unknown_queue_when_shared_retention_refuses(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.closed = False
        operation.root_id = None
        operation.config = Path("/fixture/pueue.yml")
        operation.daemon = object()
        operation.tasks = {}
        operation.numeric_ids = {}
        retained = []
        operation.ops = SimpleNamespace(
            root_id=None,
            tasks={}, labels={}, numbers={}, dispatch_attempts=set(),
            bind_supervisor=lambda *_args: None,
            retain_failure_ownership=lambda: retained.append(True) or False,
        )

        self.assertFalse(operation.safe_shutdown())
        self.assertEqual(retained, [True])
        self.assertFalse(operation.closed)

    def test_runtime_binding_receipt_is_create_once(self):
        with tempfile.TemporaryDirectory(prefix="native-binding-unit-") as directory:
            root = Path(directory)
            output = root / "output"
            output.mkdir()
            files = {}
            for name in ("provider", "delegate", "runner", "pueue", "pueued"):
                path = root / name
                path.write_text(name + "\n")
                files[name] = path

            operation = object.__new__(gate.NativeAcceptance)
            operation.output = output
            operation.provider = "pi:json"
            operation.mode = "read-only"
            operation.scenario = "read-only"
            operation.provider_executable = files["provider"]
            operation.provider_sha256 = gate.digest(files["provider"])
            operation.provider_version = "pi-runtime-v1"
            operation.delegate = files["delegate"]
            operation.runner = files["runner"]
            operation.pueue = files["pueue"]
            operation.pueued = files["pueued"]
            operation.write_binding()
            original = (output / "binding.json").read_bytes()

            operation.provider_version = "pi-runtime-v2"
            with self.assertRaises(gate.AcceptanceFailure):
                operation.write_binding()
            self.assertEqual((output / "binding.json").read_bytes(), original)

    def test_codex_git_baseline_is_allowed_but_immutable(self):
        with tempfile.TemporaryDirectory(prefix="native-codex-git-unit-") as directory:
            workspace = Path(directory) / "workspace"
            workspace.mkdir()
            git_directory = workspace / ".git"
            git_directory.mkdir()
            (git_directory / "config").write_text("[core]\n")
            marker = workspace / "acceptance-marker.txt"
            nonce_file = workspace / "acceptance-write-nonce.txt"
            fixture = workspace / "acceptance-write-fixture.txt"
            ack = workspace / ".ack-proof"
            staging = workspace / "proof.staging"
            target = workspace / "acceptance-write-target.txt"
            check = workspace / "acceptance-write-check.txt"
            marker.write_bytes(b"DELEGATION-LIVE-MARKER\n")
            nonce_file.write_bytes(b"nonce\n")
            fixture.write_bytes(b"preexisting:nonce\n")
            ack.write_text("stable\n")
            staging.write_text("stable\n")

            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "codex:exec"
            operation.workspace = workspace
            operation.workspace_marker = marker
            operation.marker_bytes = marker.read_bytes()
            operation.nonce_file = nonce_file
            operation.fixture_file = fixture
            operation.target_file = target
            operation.check_file = check
            operation.nonce = "nonce"
            operation.workspace_baseline = gate.workspace_snapshot(workspace)
            fixture.write_bytes(b"edited-fixture:nonce")
            target.write_bytes(b"edited:nonce\n")
            check.write_text(f"{hashlib.sha256(target.read_bytes()).hexdigest()}  {target.name}\n")
            operation._workspace_write_snapshot(continuation=False)

            ack.write_text("changed\n")
            with self.assertRaises(gate.AcceptanceFailure):
                operation._workspace_write_snapshot(continuation=False)
            ack.write_text("stable\n")
            (git_directory / "config").write_text("[core]\nrepositoryformatversion = 1\n")
            with self.assertRaises(gate.AcceptanceFailure):
                operation._workspace_write_snapshot(continuation=False)

    def test_write_oracle_requires_filesystem_digest_not_provider_prose(self):
        with tempfile.TemporaryDirectory(prefix="native-write-unit-") as directory:
            root = Path(directory)
            fixture = root / "acceptance-write-fixture.txt"
            target = root / "acceptance-write-target.txt"
            artifact = root / "acceptance-write-check.txt"
            fixture.write_bytes(b"preexisting:nonce\n")
            fixture.write_bytes(b"edited-fixture:nonce")
            gate.verify_write_fixture("nonce", fixture)
            target.write_bytes(b"edited:nonce\n")
            digest = hashlib.sha256(target.read_bytes()).hexdigest()
            artifact.write_text(f"{digest}  {target.name}\n")
            gate.verify_write_artifacts("nonce", target, artifact)

            artifact.write_text("I created and checked the target successfully.\n")
            with self.assertRaises(gate.AcceptanceFailure):
                gate.verify_write_artifacts("nonce", target, artifact)

    def test_read_only_brief_and_answer_require_the_random_marker(self):
        with patch.object(gate.secrets, "token_hex", side_effect=["a" * 48, "b" * 48]):
            first_marker = gate.random_marker()
            second_marker = gate.random_marker()
        self.assertNotEqual(first_marker, second_marker)
        marker_text = first_marker.decode().removesuffix("\n")
        expected = gate.marker_answer(marker_text, continuation=False)
        continued = gate.marker_answer(marker_text, continuation=True)
        gate.verify_read_only_answer(expected.encode(), expected, "fresh")
        gate.verify_read_only_answer((" \n" + expected + "\n ").encode(), expected, "fresh")
        gate.verify_read_only_answer((continued + "\n").encode(), continued, "resume")
        for answer in (b"arbitrary success", (expected + " extra").encode()):
            with self.assertRaises(gate.AcceptanceFailure):
                gate.verify_read_only_answer(answer, expected, "fresh")

        with tempfile.TemporaryDirectory(prefix="native-brief-unit-") as directory:
            operation = object.__new__(gate.NativeAcceptance)
            operation.mode = "read-only"
            operation.marker_text = marker_text
            operation.workspace_marker = Path(directory) / "workspace" / "acceptance-marker.txt"
            operation.briefs = Path(directory) / "briefs"
            operation.briefs.mkdir()
            fresh = operation.brief("fresh")
            resume = operation.brief("resume", continuation=True)
            fresh_text = fresh.read_text()
            resume_text = resume.read_text()
            self.assertIn("DELEGATION_LIVE_OK:", fresh_text)
            self.assertIn("DELEGATION_LIVE_CONTINUED:", resume_text)
            self.assertNotIn(marker_text, fresh_text)
            self.assertNotIn(marker_text, resume_text)
            for text in (fresh_text, resume_text):
                self.assertIn(str(operation.workspace_marker), text)
                self.assertIn("file-read tool or a read-only shell command", text)

    def test_replay_delegates_complete_collection_receipts(self):
        payload = {"basename": "result.txt", "length": 1, "sha256": "a" * 64}
        fresh_outcome = {"verdict": "committed", "payload": payload,
                         "evidence_sha256": "b" * 64}
        resume_payload = dict(payload, sha256="c" * 64)
        resume_outcome = {"verdict": "committed", "payload": resume_payload,
                          "evidence_sha256": "d" * 64}
        captured = []

        def replay(records):
            captured.append(records)

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "e" * 32
        operation.ops = SimpleNamespace(root_id=None, replay=replay)
        operation.records = {
            "fresh-collect": {"directory": Path("/fixture/fresh"),
                               "outcome": fresh_outcome, "payload": payload},
            "resume-collect": {"directory": Path("/fixture/resume"),
                                "outcome": resume_outcome, "payload": resume_payload},
        }

        operation.replay()

        self.assertEqual(operation.ops.root_id, operation.root_id)
        self.assertEqual(len(captured), 1)
        records = captured[0]
        self.assertEqual(records["fresh"]["directory"], Path("/fixture/fresh"))
        self.assertEqual(records["fresh"]["payload"], payload)
        self.assertEqual(records["fresh"]["outcome"]["evidence_sha256"], "b" * 64)
        self.assertEqual(records["resume"]["payload"], resume_payload)
        self.assertEqual(records["resume"]["outcome"]["evidence_sha256"], "d" * 64)

    def test_session_and_replay_oracles_are_strict(self):
        gate.require_same_session("session-1", "session-1")
        with self.assertRaises(gate.AcceptanceFailure):
            gate.require_same_session("session-1", "session-2")

        before = {"outcome.json": {"sha256": "a"}}
        gate.require_replay_unchanged(before, {"outcome.json": {"sha256": "a"}})
        with self.assertRaises(gate.AcceptanceFailure):
            gate.require_replay_unchanged(before, {"outcome.json": {"sha256": "b"}})

    def test_profiles_and_parser_expose_normal_permission_matrix(self):
        expected = {"antigravity:print", "claude:print", "codex:exec", "pi:json", "opencode:run"}
        self.assertEqual(set(gate.PROFILES), expected)
        self.assertEqual(gate.PROFILES["antigravity:print"]["default_mode"], "workspace-write")
        for profile in gate.PROFILES.values():
            self.assertIn("workspace-write", profile["modes"])
        self.assertEqual(gate.PROFILES["claude:print"]["default_mode"], "read-only")
        self.assertEqual(gate.PROFILES["codex:exec"]["default_mode"], "read-only")
        self.assertEqual(gate.PROFILES["pi:json"]["default_mode"], "read-only")
        self.assertEqual(gate.PROFILES["opencode:run"]["default_mode"], "read-only")

        write_args = gate.build_parser().parse_args([
            "--provider", "codex:exec", "--permission", "workspace-write", "--scenario", "write",
        ])
        self.assertEqual(write_args.mode, "workspace-write")
        self.assertEqual(write_args.scenario, "write")
        legacy_args = gate.build_parser().parse_args([
            "--provider", "pi:json", "--mode", "workspace-write",
        ])
        self.assertEqual(legacy_args.mode, "workspace-write")
        self.assertIsNone(legacy_args.scenario)
        with self.assertRaises(SystemExit):
            gate.build_parser().parse_args([
                "--provider", "pi:json", "--permission", "workspace-write", "--scenario", "timeout",
            ])

    def test_aggregate_wrapper_runs_write_and_read_profiles(self):
        wrapper = Path(__file__).with_name("acceptance_native_all.sh").read_text()
        for provider in ("antigravity:print", "claude:print", "codex:exec", "pi:json", "opencode:run"):
            self.assertIn(provider, wrapper)
        self.assertIn('--permission "$case_permission"', wrapper)
        self.assertIn('--scenario "$case_scenario"', wrapper)
        self.assertIn('run_case "$provider" workspace-write write', wrapper)
        self.assertIn('run_case "$provider" read-only read-only', wrapper)
        self.assertIn("2) # User-authorized authentication/prerequisite blocks remain neutral.", wrapper)
        self.assertIn("passed=%s blocked=%s failed=%s", wrapper)
        self.assertIn("aggregate_status=BLOCKED", wrapper)

    def test_aggregate_reports_counts_and_neutral_all_blocked_status(self):
        wrapper = Path(__file__).with_name("acceptance_native_all.sh")
        with tempfile.TemporaryDirectory(prefix="native-aggregate-unit-") as directory:
            fake_python = Path(directory) / "python3"
            fake_python.write_text("#!/bin/sh\nexit 2\n")
            fake_python.chmod(0o700)
            environment = os.environ.copy()
            environment["PATH"] = str(fake_python.parent) + os.pathsep + environment["PATH"]
            blocked = subprocess.run(
                ["sh", str(wrapper)], cwd=Path(__file__).parents[1], env=environment,
                capture_output=True, text=True, check=False, timeout=10)
            self.assertEqual(blocked.returncode, 0)
            self.assertIn("status=BLOCKED passed=0 blocked=9 failed=0", blocked.stdout)
            self.assertIn("auth_blocks_neutral=true", blocked.stdout)

            fake_python.write_text(
                "#!/bin/sh\n"
                "case $* in\n"
                "    *claude:print*) exit 0 ;;\n"
                "    *codex:exec*) exit 1 ;;\n"
                "    *) exit 2 ;;\n"
                "esac\n"
            )
            mixed = subprocess.run(
                ["sh", str(wrapper)], cwd=Path(__file__).parents[1], env=environment,
                capture_output=True, text=True, check=False, timeout=10)
            self.assertEqual(mixed.returncode, 1)
            self.assertIn("status=failed passed=2 blocked=5 failed=2", mixed.stdout)

    def test_normal_setup_does_not_probe_provider_version_directly(self):
        source = Path(__file__).with_name("acceptance_native.py").read_text()
        self.assertNotIn('[self.provider_executable, "--version"]', source)


if __name__ == "__main__":
    unittest.main()
