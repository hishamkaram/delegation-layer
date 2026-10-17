"""Hermetic oracles for a native acceptance behavior cohort."""
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import native_acceptance_evidence as native_evidence
from scripts import acceptance_native as entry
from scripts.acceptance_tests.native.support import (
    physical_test_root,
    queue_oracle,
)
from scripts.delegate_acceptance.native import (
    acceptance as gate,
    acceptance_discovery as discovery_impl,
    output as output_impl,
)


class NativeAcceptanceOracleTests(unittest.TestCase):
    @staticmethod
    def _timeout_source_executables(root: Path) -> tuple[Path, Path, Path, Path]:
        delegate, runner, pueue, pueued = (root / name for name in (
            "delegate", "delegate-run", "pueue", "pueued"))
        for executable in (delegate, runner, pueue, pueued):
            executable.write_text(executable.name + "\n")
            executable.chmod(0o700)
        return delegate, runner, pueue, pueued

    @staticmethod
    def _timeout_source_binding_operation(root: Path) -> tuple[object, str, Path]:
        root_id = "b" * 32
        task_id = "a" * 32
        state = root / "state"
        task = state / "tasks" / task_id
        workspace = root / "workspace"
        output = root / "output"
        task.mkdir(parents=True)
        workspace.mkdir()
        output.mkdir()
        provider = root / "provider"
        provider.write_text("provider\n")
        provider.chmod(0o700)
        delegate, runner, pueue, pueued = (
            NativeAcceptanceOracleTests._timeout_source_executables(root))
        config = root / "pueue.yml"
        config.write_text("config\n")
        brief = b"timeout source\n"
        (task / "brief.md").write_bytes(brief)
        gate.write_json(task / "task.json", {
            "root_id": root_id, "task_id": task_id, "provider": "codex:exec",
            "mode": "workspace-write", "canonical_cwd": str(workspace),
            "requested_config": {
                "permission": "workspace-write", "budget": "3m0s",
            },
            "budget_nanos": gate.TIMEOUT_RESUME_SOURCE_BUDGET_NANOS,
            "brief_length": len(brief),
            "brief_sha256": hashlib.sha256(brief).hexdigest(),
        })
        spec_digest = gate.digest(task / "task.json")
        gate.write_json(task / "meta.json", {
            "root_id": root_id, "task_id": task_id, "spec_sha256": spec_digest,
            "containment": "workspace-write",
            "provider_executable": str(provider.resolve()), "provider_version": "",
            "runtime_preflight_sha256": "e" * 64, "environment_recorded": True,
            "effective_config": {"policy": {"runtime_sha256": "f" * 64}},
        })
        meta_digest = gate.digest(task / "meta.json")
        start = {
            "schema_version": 1, "root_id": root_id, "task_id": task_id,
            "meta_sha256": meta_digest, "spec_sha256": spec_digest,
            "budget_nanos": gate.TIMEOUT_RESUME_SOURCE_BUDGET_NANOS,
            "created_at": "2026-10-15T08:21:52Z",
        }
        started = {
            "schema_version": 1, "root_id": root_id, "task_id": task_id,
            "meta_sha256": meta_digest, "spec_sha256": spec_digest,
            "started_at": "2026-10-15T08:21:52Z", "diagnostic_nanos": 1,
            "provider_executable": str(provider.resolve()),
            "provider_version": "fixture-v2", "provider_sha256": "1" * 64,
        }
        gate.write_json(task / "provider.start", start)
        gate.write_json(task / "provider.started.json", started)
        gate.write_json(task / "provider.ref.json", {
            "root_id": root_id, "task_id": task_id, "provider": "codex:exec",
            "conversation_id": "fixture-session",
        })

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = root_id
        operation.state = state
        operation.workspace = workspace
        operation.output = output
        operation.provider = "codex:exec"
        operation.mode = "workspace-write"
        operation.scenario = "timeout-resume"
        operation.args = SimpleNamespace(model=None, effort=None)
        operation.provider_executable = provider.resolve()
        operation.provider_version = "fixture-v2"
        operation.tasks = {"timeout_source": task_id}
        operation.delegate = delegate
        operation.runner = runner
        operation.pueue = pueue
        operation.pueued = pueued
        operation.config = config
        operation.pueue_version = "pueue 4.0.4"
        operation.pueued_version = "pueued 4.0.4"
        return operation, task_id, task

    def test_timeout_source_binding_accepts_started_record_without_exit(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-source-binding-") as directory:
            operation, task_id, task = self._timeout_source_binding_operation(Path(directory))

            self.assertEqual(operation.timeout_source_binding(task_id), task)
            self.assertEqual(operation.provider_runtime_receipt()[
                "provider_runtime_identities"][task_id], {
                "executable": str(operation.provider_executable),
                "version": "fixture-v2", "sha256": "1" * 64,
            })
            self.assertFalse((task / "provider.exit").exists())

    def test_timeout_source_binding_writes_refreshed_runtime_identity(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-binding-receipt-") as directory:
            operation, task_id, task = self._timeout_source_binding_operation(Path(directory))
            original = Path(directory) / "provider-original"
            original.write_text("original\n")
            operation.provider_executable = original.resolve()
            operation.provider_version = None

            operation.timeout_source_binding(task_id)

            binding = gate.read_json(operation.output / "binding.json")
            self.assertEqual(binding["provider_executable"],
                             str((Path(directory) / "provider").resolve()))
            self.assertEqual(binding["provider_version"], "fixture-v2")
            self.assertFalse((task / "provider.exit").exists())
            self.assertFalse((task / "outcome.json").exists())

    def test_timeout_source_binding_rejects_tampered_started_record_binding(self):
        for field, value in (
                ("root_id", "c" * 32), ("task_id", "c" * 32),
                ("meta_sha256", "c" * 64), ("spec_sha256", "c" * 64)):
            with self.subTest(field=field), tempfile.TemporaryDirectory(
                    prefix="native-timeout-source-binding-tamper-") as directory:
                operation, task_id, task = self._timeout_source_binding_operation(Path(directory))
                started = gate.read_json(task / "provider.started.json")
                started[field] = value
                gate.write_json(task / "provider.started.json", started, replace=True)
                with self.assertRaisesRegex(gate.AcceptanceFailure, "provider.started.json binding"):
                    operation.timeout_source_binding(task_id)

    def test_timeout_source_binding_rejects_malformed_provider_record_envelopes(self):
        cases = (
            ("provider.start", "schema_version", 2),
            ("provider.start", "schema_version", True),
            ("provider.start", "schema_version", 1.0),
            ("provider.start", "budget_nanos", 0),
            ("provider.start", "budget_nanos", 2 ** 63),
            ("provider.start", "created_at", None),
            ("provider.started.json", "schema_version", 2),
            ("provider.started.json", "schema_version", True),
            ("provider.started.json", "schema_version", 1.0),
            ("provider.started.json", "diagnostic_nanos", -1),
            ("provider.started.json", "diagnostic_nanos", 2 ** 63),
            ("provider.started.json", "started_at", None),
        )
        for name, field, value in cases:
            with self.subTest(name=name, field=field), tempfile.TemporaryDirectory(
                    prefix="native-timeout-source-envelope-") as directory:
                operation, task_id, task = self._timeout_source_binding_operation(Path(directory))
                record = gate.read_json(task / name)
                if value is None:
                    del record[field]
                else:
                    record[field] = value
                gate.write_json(task / name, record, replace=True)

                with self.assertRaisesRegex(gate.AcceptanceFailure, name):
                    operation.timeout_source_binding(task_id)

    def test_completed_binding_still_requires_a_matching_exit_record(self):
        with tempfile.TemporaryDirectory(prefix="native-completed-binding-") as directory:
            operation, task_id, task = self._timeout_source_binding_operation(Path(directory))
            with self.assertRaises(FileNotFoundError):
                operation._request_binding(
                    task_id, require_launch_evidence=True,
                    budget=gate.CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET,
                    budget_nanos=gate.TIMEOUT_RESUME_SOURCE_BUDGET_NANOS)

            started = gate.read_json(task / "provider.started.json")
            exited = dict(started, provider_version="fixture-v3")
            gate.write_json(task / "provider.exit", exited)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "runtime identities differ"):
                operation._request_binding(
                    task_id, require_launch_evidence=True,
                    budget=gate.CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET,
                    budget_nanos=gate.TIMEOUT_RESUME_SOURCE_BUDGET_NANOS)

    def test_model_discovery_binds_helper_to_selected_provider_executable(self):
        with tempfile.TemporaryDirectory(prefix="native-model-executable-") as directory:
            root = Path(directory)
            selected = root / "provider"
            selected.write_text("selected\n")
            foreign = root / "foreign-provider"
            foreign.write_text("foreign\n")
            task_id = "d" * 32
            operation = object.__new__(gate.NativeAcceptance)
            operation.models_root_id = "c" * 32
            operation.root_id = None
            operation.tasks = {}
            operation.numeric_ids = {}
            operation.ops = SimpleNamespace(dispatch_attempts=set(), inspection_attempts=set())
            operation.state = root / "state"
            operation.state.mkdir()
            operation.models_state = root / "models-state"
            (operation.models_state / "inspections" / task_id).mkdir(parents=True)
            operation.model_discovery_task_id = None
            operation.processes = object()
            operation.delegate = selected
            operation.runner = selected
            operation.pueue = selected
            operation.output = root / "output"
            operation.output.mkdir()
            operation.config = root / "pueue.yml"
            operation.config.write_text("config\n")
            operation.daemon = object()
            operation.provider = "pi:json"
            operation.workspace = root / "workspace"
            operation.workspace.mkdir()
            operation.provider_executable = selected.resolve()
            model_group = gate.INSPECTION_GROUP_PREFIX + operation.models_root_id
            journal = {
                "numeric_task_id": 8,
                "request": {
                    "binding": {
                        "definition_revision": "model-discovery-v1",
                        "helper_executable": str(selected.resolve()),
                        "helper_sha256": gate.digest(selected),
                    },
                    "task": {
                        "provider": operation.provider,
                        "mode": gate.PROFILES[operation.provider]["modes"][0],
                        "canonical_cwd": str(operation.workspace),
                        "requested_config": {
                            "permission": gate.PROFILES[operation.provider]["modes"][0],
                            "budget": gate.MODEL_DISCOVERY_TASK_BUDGET,
                        },
                        "budget_nanos": gate.MODEL_DISCOVERY_TASK_BUDGET_NANOS,
                        "brief_length": len(b"model discovery"),
                        "brief_sha256": hashlib.sha256(b"model discovery").hexdigest(),
                    },
                },
                "result": {"reason": "blocked"},
            }
            status = {
                "groups": {"default": {}, model_group: {}},
                "tasks": {"8": {"id": 8}},
            }

            with patch.object(discovery_impl, "NativeTaskOps") as validator_type:
                validator = validator_type.return_value
                validator.validate_inspection_journals.return_value = {task_id: journal}
                operation._validate_model_discovery_evidence(status, model_group)

                journal["request"]["binding"]["helper_executable"] = str(foreign.resolve())
                with self.assertRaisesRegex(gate.AcceptanceFailure,
                                            "model discovery provider binding changed"):
                    operation._validate_model_discovery_evidence(status, model_group)

    def test_inspection_queue_rejects_missing_admitted_task_journal(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = queue_oracle(Path(directory))
            del operation.ops._inspection_journals
            self.assertFalse(operation.terminal_private_queue(status))

    def test_empty_queue_is_terminal_before_any_task_is_admitted(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, _ = queue_oracle(Path(directory), with_inspection=False)
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
            for message in (
                "please log in before using this provider",
                "provider authentication unavailable",
                "authentication required",
            ):
                with self.subTest(message=message):
                    (raw / "stderr").write_text(message)
                    self.assertFalse(gate.authentication_unavailable(root))

            (root / "outcome.json").write_text(json.dumps({"verdict": "committed"}))
            self.assertFalse(gate.authentication_unavailable(root))

            (root / "outcome.json").write_text(json.dumps({"verdict": "rejected"}))
            (raw / "stderr").write_text("provider returned a normal task refusal")
            self.assertFalse(gate.authentication_unavailable(root))

            for task_output in (
                "This task explains how credentials are stored.",
                "Use an API key for the example request.",
                "Users can sign in from the account page.",
            ):
                with self.subTest(task_output=task_output):
                    (raw / "stderr").write_text("")
                    (raw / "stdout").write_text(task_output)
                    self.assertFalse(gate.authentication_unavailable(root))

    def test_native_authentication_status_blocks_before_a_provider_turn(self):
        with tempfile.TemporaryDirectory(prefix="native-auth-status-") as directory:
            process_directory = Path(directory)
            stdout = process_directory / "stdout"
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "claude:print"
            operation.profile = gate.PROFILES[operation.provider]
            operation.provider_executable = Path("/usr/bin/claude")
            calls = []

            def direct(name, argv, expected, timeout):
                calls.append((name, argv, expected, timeout))
                return SimpleNamespace(directory=process_directory,
                                       result={"exit_code": 1})

            operation.direct = direct
            stdout.write_text('{"loggedIn":false}\n', encoding="utf-8")
            with self.assertRaisesRegex(gate.BlockedFailure,
                                        "authentication is unavailable"):
                operation.require_authentication_prerequisite()
            self.assertEqual(calls, [(
                "provider-authentication-status",
                [Path("/usr/bin/claude"), "auth", "status"], {0, 1}, 20,
            )])

            calls.clear()
            stdout.write_text('{"loggedIn":true}\n', encoding="utf-8")

            def authenticated(name, argv, expected, timeout):
                calls.append((name, argv, expected, timeout))
                return SimpleNamespace(directory=process_directory,
                                       result={"exit_code": 0})

            operation.direct = authenticated
            operation.require_authentication_prerequisite()
            self.assertEqual(len(calls), 1)

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

            (raw / "stdout").write_text("")
            (raw / "stderr").write_text("")
            for diagnostic in (
                {"code": "authentication-unavailable", "event": 1,
                 "message": "Provider authentication was unavailable."},
            ):
                (root / "outcome.json").write_text(json.dumps({
                    "verdict": "rejected", "diagnostics": [diagnostic],
                }))
                self.assertTrue(gate.authentication_unavailable(root))

            for diagnostic in (
                {"code": "authentication_unavailable", "event": 1,
                 "message": "Provider authentication was unavailable."},
                {"code": "authentication-unavailable ", "event": 1,
                 "message": "Provider authentication was unavailable."},
                {"code": "provider-failed", "event": 1,
                 "message": "Provider authentication was unavailable."},
                {"code": "authentication-unavailable", "event": 1},
            ):
                (root / "outcome.json").write_text(json.dumps({
                    "verdict": "rejected", "diagnostics": [diagnostic],
                }))
                self.assertFalse(gate.authentication_unavailable(root))

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
            self.assertFalse(gate.authentication_blocked({}, diagnostic))
        for diagnostic in (
            "The task explains how credentials and API keys work.",
            "Users can sign in from the account page.",
        ):
            self.assertFalse(gate.authentication_blocked({}, diagnostic))
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
            entry.choose_output("pi:json", "/tmp/native-acceptance-output")

    def test_choose_output_accepts_only_the_exact_prepared_aggregate_case(self):
        with tempfile.TemporaryDirectory(prefix="native-prepared-output-") as directory:
            root = physical_test_root(directory) / "aggregate"
            (root / native_evidence.CASE_ROOT_NAME).mkdir(parents=True)
            definition = native_evidence.EXPECTED_CASES[0]
            prepared = native_evidence.prepare_case_directory(root, definition.case_id)

            with patch.object(output_impl, "reject_tmp"):
                selected = entry.choose_output(
                    definition.provider, str(prepared), definition.permission, definition.scenario)
            self.assertEqual(selected, prepared.resolve())

            with patch.object(output_impl, "reject_tmp"), \
                    self.assertRaisesRegex(gate.AcceptanceFailure,
                                            "prepared live acceptance output"):
                entry.choose_output(definition.provider, str(prepared),
                                   "read-only", definition.scenario)

    def test_main_accepts_prepared_case_before_the_provider_prerequisite_gate(self):
        with tempfile.TemporaryDirectory(prefix="native-prepared-main-") as directory:
            root = physical_test_root(directory) / "aggregate"
            (root / native_evidence.CASE_ROOT_NAME).mkdir(parents=True)
            definition = native_evidence.EXPECTED_CASES[2]
            prepared = native_evidence.prepare_case_directory(root, definition.case_id)

            with patch.object(output_impl, "reject_tmp"), \
                    patch.object(entry, "NativeAcceptance") as acceptance:
                code = entry.main([
                    "--provider", definition.provider,
                    "--permission", definition.permission,
                    "--scenario", definition.scenario,
                    "--output", str(prepared),
                ])

            self.assertEqual(code, 2)
            acceptance.assert_not_called()
            receipt = gate.read_json(prepared / "failure.json")
            self.assertEqual(receipt["status"], "BLOCKED")
            self.assertEqual(receipt["provider"], definition.provider)
            self.assertIn("requires --model", receipt["error"])

    def test_main_rejects_an_unowned_preexisting_case_directory(self):
        with tempfile.TemporaryDirectory(prefix="native-unowned-main-") as directory:
            root = physical_test_root(directory) / "aggregate"
            definition = native_evidence.EXPECTED_CASES[0]
            unowned = root / native_evidence.CASE_ROOT_NAME / definition.case_id
            unowned.mkdir(parents=True)

            with patch.object(output_impl, "reject_tmp"), \
                    patch.object(entry, "NativeAcceptance") as acceptance:
                code = entry.main([
                    "--provider", definition.provider,
                    "--permission", definition.permission,
                    "--scenario", definition.scenario,
                    "--output", str(unowned),
                ])

            self.assertEqual(code, 1)
            acceptance.assert_not_called()

    def test_missing_prerequisite_is_blocked(self):
        with self.assertRaises(gate.BlockedFailure):
            gate.resolve_executable(None, Path("/does/not/exist"), "provider")

    def test_native_defaults_use_repository_bin_and_preserve_explicit_paths(self):
        with tempfile.TemporaryDirectory(prefix="native-runtime-defaults-") as directory:
            root = Path(directory)
            output = root / "output"
            output.mkdir()
            calls = []

            def resolve(value, default, label):
                calls.append((value, default, label))
                return Path(default)

            def construct(extra_arguments):
                owner = SimpleNamespace(
                    create=lambda label, path, parent:
                        SimpleNamespace(directory=path),
                    transfer=lambda: ())
                args = entry.build_parser().parse_args([
                    "--provider", "pi:json", "--permission", "read-only",
                    "--output", str(output), *extra_arguments,
                ])
                with patch.object(Path, "home", return_value=root), \
                        patch.object(gate, "native_environment",
                                     return_value={"PATH": "/fixture"}), \
                        patch.object(gate, "resolve_executable", side_effect=resolve), \
                        patch.object(gate, "require_discovery"), \
                        patch.object(gate, "reject_tmp"), \
                        patch.object(gate, "DirectoryOwner", return_value=owner), \
                        patch.object(gate.shutil, "which", return_value=None), \
                        patch.object(gate.NativeAcceptance, "_initialize_runtime_roots"):
                    gate.NativeAcceptance(args)
                return args

            repository = Path(gate.__file__).resolve().parents[3]
            defaults = (
                (None, repository / "bin" / "delegate", "delegate"),
                (None, repository / "bin" / "delegate-run", "delegate-run"),
            )
            omitted = construct([])
            self.assertIsNone(omitted.delegate)
            self.assertIsNone(omitted.runner)
            self.assertEqual(calls[:2], list(defaults))

            calls.clear()
            delegate = str(root / "custom-delegate")
            runner = str(root / "custom-runner")
            explicit = construct(["--delegate", delegate, "--runner", runner])
            self.assertEqual(calls[:2], [
                (delegate, defaults[0][1], "delegate"),
                (runner, defaults[1][1], "delegate-run"),
            ])
            self.assertEqual((explicit.delegate, explicit.runner), (delegate, runner))

    def test_opencode_without_explicit_tool_capable_model_blocks_before_dispatch(self):
        with tempfile.TemporaryDirectory(prefix="native-opencode-model-") as directory:
            output = Path(directory) / "receipt"
            output.mkdir()
            with patch.object(entry, "choose_output", return_value=output), \
                    patch.object(entry, "NativeAcceptance") as acceptance:
                code = entry.main([
                    "--provider", "opencode:run", "--permission", "workspace-write",
                    "--scenario", "write",
                ])
            self.assertEqual(code, 2)
            acceptance.assert_not_called()
            receipt = gate.read_json(output / "failure.json")
            self.assertEqual(receipt["status"], "BLOCKED")
            self.assertEqual(receipt["requested_model"], None)
            self.assertIn("tool-capable configured route", receipt["error"])

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
            root = Path(directory).resolve()
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
