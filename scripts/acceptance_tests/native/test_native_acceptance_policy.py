"""Hermetic oracles for a native acceptance behavior cohort."""
import hashlib
from pathlib import Path
import shlex
import stat
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import acceptance_provider_common as provider_common
from scripts import acceptance_native as entry
from scripts.delegate_acceptance.native import (
    acceptance as gate,
    acceptance_collection as collection_impl,
)


class NativeAcceptanceOracleTests(unittest.TestCase):
    def test_lifetime_owner_lock_alone_is_not_durable_lifetime_evidence(self):
        with tempfile.TemporaryDirectory(prefix="native-lifetime-owner-lock-") as directory:
            task = Path(directory) / "task"
            lifetime = task / "lifetime"
            lifetime.mkdir(parents=True)
            (lifetime / "owner.lock").touch(mode=0o600)

            self.assertFalse(provider_common._ordinary_lifetime_control_started(task))

    def test_lifetime_journal_entry_requires_full_lifetime_validation(self):
        with tempfile.TemporaryDirectory(prefix="native-lifetime-journal-") as directory:
            task = Path(directory) / "task"
            lifetime = task / "lifetime"
            lifetime.mkdir(parents=True)
            (lifetime / "owner.lock").touch(mode=0o600)
            (lifetime / "intent.json").write_text("{}", encoding="utf-8")

            self.assertTrue(provider_common._ordinary_lifetime_control_started(task))

    def test_codex_timeout_workspace_owns_protected_metadata_baseline(self):
        with tempfile.TemporaryDirectory(prefix="native-codex-timeout-baseline-") as directory:
            workspace = Path(directory) / "workspace"
            workspace.mkdir()

            class GitInitProcesses:
                @staticmethod
                def run(_name, _argv, _cwd, timeout):
                    self.assertEqual(timeout, 20)
                    (workspace / ".git").mkdir()
                    return SimpleNamespace(result={"exit_code": 0})

            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "codex:exec"
            operation.scenario = "timeout-resume"
            operation.git = Path("/git")
            operation.workspace = workspace
            operation.processes = GitInitProcesses()

            operation._prepare_codex_workspace()

            self.assertEqual(
                set(gate.workspace_snapshot(workspace)),
                {".git", *gate.CODEX_TIMEOUT_BASELINE_DIRECTORIES},
            )
            for name in gate.CODEX_TIMEOUT_BASELINE_DIRECTORIES:
                self.assertEqual(
                    gate.workspace_snapshot(workspace)[name],
                    {"type": stat.S_IFDIR, "mode": 0o700},
                )

    def test_failed_runner_records_delegate_status_before_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="native-runner-failure-") as directory:
            task = "a" * 32
            state = Path(directory) / "state"
            (state / "tasks" / task).mkdir(parents=True)
            calls = []

            class FailedRunnerOps:
                root_id = None

                @staticmethod
                def wait_task(_name, _task, _expected_result):
                    raise gate.AcceptanceFailure(
                        "supervisor result 'Failed', expected 'Success'")

                @staticmethod
                def status(name, observed_task, expected=0):
                    calls.append((name, observed_task, expected))
                    return {
                        "admission": "admitted", "liveness": "stopped",
                        "publication": "unknown",
                        "failure": {"code": "runner_failed", "stage": "resolve-runner",
                                    "next_action": "stop_and_report"},
                        "error": "runner\nsetup failed",
                    }

            operation = object.__new__(gate.NativeAcceptance)
            operation.root_id = "b" * 32
            operation.state = state
            operation.provider = "ccr:print"
            operation.ops = FailedRunnerOps()

            with self.assertRaisesRegex(
                    gate.AcceptanceFailure,
                    "failure.code='runner_failed'.*error='runner setup failed'"):
                operation.wait_done("fresh", task)
            self.assertEqual(calls, [
                ("fresh-runner-failure-status", task, {0, 1}),
            ])

    def test_model_discovery_contract_preserves_unknown_efforts_and_exit_semantics(self):
        available = {
            "schema_version": 1, "command": "models", "provider": "codex:exec",
            "observed_at": "2026-09-21T10:20:30.123Z", "status": "available",
            "reason_code": "ok", "complete": True, "source": "codex models",
            "efforts": None,
            "models": [
                {"id": "account/model", "name": "Account model", "efforts": None},
                {"id": "fixed/model", "efforts": [], "default_effort": None},
            ],
        }
        summary = gate.validate_model_discovery_response(available, "codex:exec")
        self.assertEqual(summary["model_count"], 2)
        self.assertIsNone(summary["effort_count"])
        self.assertEqual(gate.model_discovery_exit_code("available"), 0)
        self.assertEqual(gate.model_discovery_exit_code("partial"), 0)
        self.assertEqual(gate.model_discovery_exit_code("blocked"), 2)
        self.assertEqual(gate.model_discovery_exit_code("unavailable"), 2)
        self.assertEqual(gate.model_discovery_exit_code("failed"), 1)

        partial = dict(available, status="partial", complete=False,
                       efforts=["low", "high"], models=[{"id": "partial/model", "efforts": ["high"]}])
        self.assertEqual(
            gate.validate_model_discovery_response(partial, "codex:exec")["status"], "partial")

        blocked = dict(available, status="blocked", complete=False, reason_code="authentication_unavailable",
                       models=[])
        self.assertEqual(
            gate.validate_model_discovery_response(blocked, "codex:exec")["status"], "blocked")

        with self.assertRaises(gate.AcceptanceFailure):
            gate.validate_model_discovery_response(dict(available, models=[{"id": "fixed/model", "efforts": []},
                                                                          {"id": "fixed/model", "efforts": []}]),
                                                   "codex:exec")

    def test_dispatch_passes_explicit_model_and_effort_with_the_canonical_budget(self):
        self.assertEqual(gate.TASK_BUDGET, "5m")
        self.assertEqual(gate.CANONICAL_TASK_BUDGET, "5m0s")
        self.assertEqual(gate.TASK_BUDGET_NANOS, 300_000_000_000)
        self.assertEqual(gate.TIMEOUT_RESUME_SOURCE_BUDGET, "3m")
        self.assertEqual(gate.CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET, "3m0s")
        self.assertEqual(gate.TIMEOUT_RESUME_SOURCE_BUDGET_NANOS, 180_000_000_000)
        self.assertEqual(gate.WATCH_SECONDS, 330)
        operation = object.__new__(gate.NativeAcceptance)
        operation.delegate = Path("/delegate")
        operation.state = Path("/state")
        operation.config = Path("/pueue.yml")
        operation.runner = Path("/runner")
        operation.workspace = Path("/workspace")
        operation.provider = "antigravity:print"
        operation.mode = "read-only"
        operation.args = SimpleNamespace(model="gemini-3-pro", effort="high")
        argv = operation.dispatch_argv("a" * 32, Path("/brief"))
        self.assertEqual(argv[argv.index("--budget") + 1], gate.TASK_BUDGET)
        self.assertEqual(argv[argv.index("--pueue-config") + 1], operation.config)
        self.assertEqual(argv[argv.index("--model") + 1], "gemini-3-pro")
        self.assertEqual(argv[argv.index("--effort") + 1], "high")
        self.assertEqual(operation.native_mode_receipt(),
                         {"sandbox": "--sandbox", "mode": "plan", "bypass": False})

        timeout_argv = operation.dispatch_argv("c" * 32, Path("/brief"),
                                               budget=gate.TIMEOUT_RESUME_SOURCE_BUDGET)
        self.assertEqual(timeout_argv[timeout_argv.index("--budget") + 1],
                         gate.TIMEOUT_RESUME_SOURCE_BUDGET)

        operation.args = SimpleNamespace(model=None, effort=None)
        default_argv = operation.dispatch_argv("b" * 32, Path("/brief"))
        self.assertNotIn("--model", default_argv)
        self.assertNotIn("--effort", default_argv)

    def test_parser_exposes_the_dedicated_timeout_resume_scenario(self):
        args = entry.build_parser().parse_args([
            "--provider", "codex:exec", "--permission", "workspace-write",
            "--scenario", "timeout-resume", "--output", "/evidence",
        ])
        self.assertEqual(args.scenario, "timeout-resume")

    def test_timeout_resume_uses_managed_runner_commands(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.delegate = Path("/delegate")
        operation.state = Path("/state")
        operation.config = Path("/pueue.yml")
        operation.runner = Path("/runner")
        operation.workspace = Path("/workspace")
        operation.provider = "codex:exec"
        operation.mode = "workspace-write"
        operation.scenario = "timeout-resume"
        operation.args = SimpleNamespace(model=None, effort=None)

        prefix = operation.task_command_prefix()
        dispatch = operation.dispatch_argv("a" * 32, Path("/brief"))

        self.assertNotIn("--runner", prefix)
        self.assertEqual(dispatch[:len(prefix)], prefix)
        self.assertIn("dispatch", dispatch)

    def test_managed_runner_queue_validation_requires_guardian(self):
        with tempfile.TemporaryDirectory(prefix="native-managed-runner-") as directory:
            root = Path(directory)
            state = root / "state"
            task = "a" * 32
            task_directory = state / "tasks" / task
            task_directory.mkdir(parents=True)
            source_runner = root / "delegate-run"
            managed_runner = state / ".supervisor" / "delegate-run-managed"
            managed_runner.parent.mkdir()
            source_runner.write_bytes(b"managed runner fixture\n")
            managed_runner.write_bytes(source_runner.read_bytes())
            source_runner.chmod(0o700)
            managed_runner.chmod(0o700)
            gate.write_json(task_directory / "meta.json", {
                "runner_executable": str(managed_runner),
                "runner_ownership": "managed",
            })
            ops = provider_common.NativeTaskOps(
                SimpleNamespace(), root / "delegate", source_runner, root / "pueue",
                root, state, root / "output", managed_runner=True)
            command = shlex.join([
                str(managed_runner), "--guardian", "--supervised",
                "--root", str(state), task,
            ])
            row = {
                "id": 7, "label": "delegate:" + "b" * 32 + ":" + task,
                "group": "default", "path": str(state),
                "original_command": command, "command": command,
            }

            ops.validate_ordinary_queue_row(row, row["label"], task, 7, "default")
            row["command"] = shlex.join([str(managed_runner), "--root", str(state), task])
            with self.assertRaisesRegex(gate.AcceptanceFailure, "exact runner/state/task"):
                ops.validate_ordinary_queue_row(row, row["label"], task, 7, "default")

    def test_inspection_queue_accepts_fresh_and_recovered_client_directories(self):
        with tempfile.TemporaryDirectory(prefix="native-inspection-cwd-") as directory:
            root = Path(directory)
            state_parent = root / "invocation"
            state = root / "state"
            supervisor = root / "pueue"
            state_parent.mkdir()
            state.mkdir()
            supervisor.mkdir()
            runner = root / "delegate-run"
            task = "a" * 32
            group = "delegate-inspection-" + "b" * 32
            label = group + "-" + task
            command = shlex.join([
                str(runner), "--inspection", "--guardian", "--supervised",
                "--root", str(state), task,
            ])
            row = {
                "id": 7, "label": label, "group": group,
                "original_command": command, "command": command,
                "status": {"Done": {"result": "Success"}},
            }
            journal = {
                "task_id": task, "operation_id": task, "label": label, "group": group,
                "numeric_task_id": 7, "result": {"reason": "eligible"},
            }
            ops = provider_common.NativeTaskOps(
                SimpleNamespace(), root / "delegate", runner, root / "pueue-client",
                state_parent, state, root / "output")
            ops.bind_supervisor(supervisor / "pueue.yml")

            for expected in (state_parent, supervisor):
                row["path"] = str(expected)
                ops._validate_inspection_queue_row(row, journal, allow_failure=False)

            row["path"] = str(root / "unowned")
            with self.assertRaisesRegex(gate.AcceptanceFailure, "working directory mismatch"):
                ops._validate_inspection_queue_row(row, journal, allow_failure=False)

    def test_parser_exposes_the_bounded_claude_interactive_scenario(self):
        args = entry.build_parser().parse_args([
            "--provider", "claude:print", "--permission", "read-only",
            "--scenario", "interactive", "--output", "/evidence",
        ])
        self.assertEqual(args.scenario, "interactive")

    def test_interactive_dispatch_persists_the_interactive_request_option(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.delegate = Path("/delegate")
        operation.state = Path("/state")
        operation.config = Path("/pueue.yml")
        operation.runner = Path("/runner")
        operation.workspace = Path("/workspace")
        operation.provider = "claude:print"
        operation.mode = "read-only"
        operation.scenario = "interactive"
        operation.args = SimpleNamespace(model=None, effort=None)
        argv = operation.dispatch_argv("a" * 32, Path("/brief"))
        self.assertIn("--interactive", argv)

    def test_interaction_event_receipts_are_unique_across_wait_phases(self):
        operation = object.__new__(gate.NativeAcceptance)
        names = []

        def events(name, task_id, after, watch_seconds):
            names.append(name)
            self.assertEqual(task_id, "a" * 32)
            self.assertGreater(watch_seconds, 0)
            if after == 0:
                return {
                    "cursor": 1,
                    "events": [],
                    "interaction": {"closed": False, "pending_requests": [{"id": "b" * 32}]},
                }
            return {
                "cursor": 2,
                "events": [],
                "interaction": {"closed": True, "pending_requests": []},
            }

        operation._interaction_events = events
        operation.wait_for_interaction("a" * 32, pending=True)
        operation.wait_for_interaction("a" * 32, after=1)

        self.assertEqual(names, ["interactive-events-1", "interactive-events-2"])

    def test_timeout_observation_uses_one_deadline_and_bounded_status_calls(self):
        task_id = "c" * 32
        clock = [0.0]
        calls = []
        responses = iter([
            {"status": "running"},
            {"status": "queued"},
            {"status": "timed_out", "continuation": {
                "resumable": True, "mode": "native", "predecessor_task_id": task_id,
                "continue_command": "delegate continue --task " + task_id,
            }},
        ])

        def status(_name, observed_task, expected, timeout):
            self.assertEqual(observed_task, task_id)
            self.assertEqual(expected, {0, 1})
            calls.append((clock[0], timeout))
            self.assertGreater(timeout, 0)
            self.assertLessEqual(timeout, 5.0 - clock[0])
            clock[0] += 0.6
            return next(responses)

        def sleep(delay):
            clock[0] += delay

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "a" * 32
        operation.ops = SimpleNamespace(root_id=None, status=status)

        observed = operation.wait_timed_out(task_id, timeout=5.0,
                                            sleep_fn=sleep, monotonic_fn=lambda: clock[0])

        self.assertEqual(observed["status"], "timed_out")
        self.assertEqual(len(calls), 3)
        self.assertGreater(calls[0][1], calls[-1][1])
        self.assertLessEqual(clock[0], 5.0)

    def test_timeout_observation_expires_by_wall_clock_deadline(self):
        task_id = "d" * 32
        clock = [0.0]
        calls = []

        def status(_name, observed_task, expected, timeout):
            self.assertEqual(observed_task, task_id)
            self.assertEqual(expected, {0, 1})
            calls.append((clock[0], timeout))
            self.assertGreater(timeout, 0)
            self.assertLessEqual(timeout, 3.0 - clock[0])
            clock[0] += 0.8
            return {"status": "running"}

        def sleep(delay):
            clock[0] += delay

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "a" * 32
        operation.ops = SimpleNamespace(root_id=None, status=status)

        with self.assertRaisesRegex(gate.AcceptanceFailure, "never published"):
            operation.wait_timed_out(task_id, timeout=3.0,
                                     sleep_fn=sleep, monotonic_fn=lambda: clock[0])

        self.assertGreaterEqual(len(calls), 2)
        self.assertLessEqual(clock[0], 3.0)
        self.assertTrue(all(timeout > 0 for _started, timeout in calls))

    def test_timed_out_source_accepts_no_publication(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-source-pending-") as directory:
            task = "a" * 32
            state = Path(directory) / "state"
            (state / "tasks" / task).mkdir(parents=True)
            calls = []

            def collect(name, observed_task, expected):
                calls.append((name, observed_task, expected))
                return {"publication": "unknown", "outcome": None}

            operation = object.__new__(gate.NativeAcceptance)
            operation.root_id = "b" * 32
            operation.state = state
            operation.ops = SimpleNamespace(root_id=None, collect=collect)

            response = operation.collect_timed_out_source(task)

            self.assertEqual(response["publication"], "unknown")
            self.assertEqual(calls, [("timeout-source-collect", task, {3, 4})])

    def test_timed_out_source_accepts_verified_rejection(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-source-rejected-") as directory:
            task = "a" * 32
            state = Path(directory) / "state"
            task_directory = state / "tasks" / task
            task_directory.mkdir(parents=True)
            response = {"publication": "rejected", "outcome": {"verdict": "rejected"}}

            def collect(_name, _observed_task, expected):
                self.assertEqual(expected, {3, 4})
                return response

            operation = object.__new__(gate.NativeAcceptance)
            operation.root_id = "b" * 32
            operation.state = state
            operation.ops = SimpleNamespace(root_id=None, collect=collect)

            with patch.object(collection_impl, "verify_collected_outcome",
                              return_value=(response["outcome"], b"rejected")) as verify:
                self.assertIs(operation.collect_timed_out_source(task), response)

            verify.assert_called_once_with(response, task_directory, "rejected")

    @staticmethod
    def _rejected_collection_operation(root: Path) -> tuple[object, Path, str, list[object]]:
        task = "a" * 32
        root_id = "b" * 32
        state = root / "state"
        task_directory = state / "tasks" / task
        task_directory.mkdir(parents=True)
        refusal = b"UNRESTRICTED_REFUSAL_PAYLOAD"
        payload = {"basename": "publish.reject", "length": len(refusal),
                   "sha256": hashlib.sha256(refusal).hexdigest()}
        evidence = "e" * 64
        outcome = {
            "verdict": "rejected", "payload": payload, "evidence_sha256": evidence,
            "diagnostics": [{
                "code": "incomplete_output", "field": "result.text",
                "message": "provider output did not contain a complete answer",
            }],
        }
        gate.write_json(task_directory / "outcome.json", outcome)
        (task_directory / "publish.reject").write_bytes(refusal)
        gate.write_json(task_directory / "provider.exit", {
            "root_id": root_id, "task_id": task, "invocation_state": "started",
            "exit_code": 0, "error": "", "raw_manifest": [],
            "manifest_sha256": evidence,
        })
        response = {
            "task_id": task, "publication": "rejected", "outcome": outcome,
            "payload": payload, "evidence_sha256": evidence,
        }
        calls: list[object] = []

        def collect(name, observed_task, expected):
            calls.append((name, observed_task, expected))
            return response

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = root_id
        operation.state = state
        operation.provider = "pi:json"
        operation.ops = SimpleNamespace(root_id=None, collect=collect)
        return operation, task_directory, task, calls

    def test_normal_collect_reports_a_verified_rejection_with_bounded_diagnostics(self):
        with tempfile.TemporaryDirectory(prefix="native-collect-rejected-") as directory:
            operation, _task_directory, task, calls = self._rejected_collection_operation(Path(directory))

            with self.assertRaisesRegex(
                    gate.AcceptanceFailure,
                    r"provider result rejected: code='incomplete_output', field='result\.text', "
                    r"message='provider output did not contain a complete answer'") as raised:
                operation.collect("fresh-collect", task)

            self.assertNotIn("UNRESTRICTED_REFUSAL_PAYLOAD", str(raised.exception))
            self.assertEqual(calls, [("fresh-collect", task, {0, 4})])

    def test_normal_collect_rejected_outcome_requires_provider_seal_binding(self):
        with tempfile.TemporaryDirectory(prefix="native-collect-rejected-seal-") as directory:
            operation, task_directory, task, calls = self._rejected_collection_operation(Path(directory))
            gate.write_json(task_directory / "provider.exit", {
                "root_id": "b" * 32, "task_id": task, "invocation_state": "started",
                "exit_code": 0, "error": "", "raw_manifest": [],
                "manifest_sha256": "f" * 64,
            }, replace=True)

            with self.assertRaisesRegex(gate.AcceptanceFailure, "lacks sealed provider evidence"):
                operation.collect("fresh-collect", task)
            self.assertEqual(calls, [("fresh-collect", task, {0, 4})])

    def test_malformed_rejected_outcome_is_an_acceptance_failure_not_blocked(self):
        with tempfile.TemporaryDirectory(prefix="native-collect-malformed-") as directory:
            operation, task_directory, task, _calls = self._rejected_collection_operation(Path(directory))
            (task_directory / "outcome.json").write_bytes(b"{")

            with self.assertRaises(gate.AcceptanceFailure) as raised:
                operation.collect("fresh-collect", task)
            self.assertNotIsInstance(raised.exception, gate.BlockedFailure)
            self.assertIn("collection evidence is malformed", str(raised.exception))
