"""Hermetic oracles for a native acceptance behavior cohort."""
import hashlib
from pathlib import Path
import shlex
import stat
import tempfile
import unittest
from types import MappingProxyType, SimpleNamespace
from unittest.mock import patch

import acceptance_provider_common as provider_common
from scripts import acceptance_native as entry
from scripts.acceptance_tests.inspection.fixtures import (
    ROOT as INSPECTION_ROOT,
    TASK as INSPECTION_TASK,
    InspectionFixture,
)
from scripts.delegate_acceptance.native import (
    acceptance as gate,
    acceptance_collection as collection_impl,
)
from scripts.delegate_acceptance.supervisor.runner_layout import (
    BUNDLE_RUNNER,
    STATE_INSTALLED_RUNNER,
    RunnerPlacement,
    managed_state_runner,
)


OLD_TASK = "a" * 32
NEW_TASK = "c" * 32
UNBOUND_TASK = "d" * 32


class PreallocatedRunnerOps(provider_common.NativeTaskOps):
    """Consumer selection that binds each preallocated task to one runner."""

    def __init__(self, bindings: dict[str, Path], *, runner: Path, base: Path,
                 state_parent: Path, state: Path, managed_runner: bool = False,
                 runner_placement: RunnerPlacement = BUNDLE_RUNNER) -> None:
        super().__init__(SimpleNamespace(), base / "delegate", runner, base / "pueue",
                         state_parent, state, base / "output", managed_runner=managed_runner,
                         runner_placement=runner_placement)
        self.bindings = MappingProxyType(dict(bindings))

    def _expected_runner(self, task: str) -> Path:
        runner = self.bindings.get(task)
        if runner is None:
            raise gate.AcceptanceFailure(f"task {task} has no bound expected runner")
        return runner


def runner_fixtures(root: Path) -> dict[str, Path]:
    runners = {}
    for name in ("old", "new", "third"):
        runner = root / f"delegate-run-{name}"
        runner.write_bytes(f"{name} runner fixture\n".encode())
        runner.chmod(0o700)
        runners[name] = runner
    return runners


def ordinary_row(runner: Path, state: Path, task: str, supervised: bool = False) -> dict[str, object]:
    argv = [str(runner)]
    if supervised:
        argv.extend(["--guardian", "--supervised"])
    command = shlex.join([*argv, "--root", str(state), task])
    return {
        "id": 7, "label": "delegate:" + "b" * 32 + ":" + task, "group": "default",
        "path": str(state), "original_command": command, "command": command,
    }


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
        operation.ops = SimpleNamespace(managed_runner=True)
        operation.args = SimpleNamespace(model="gemini-3-pro", effort="high")
        argv = operation.dispatch_argv("a" * 32, Path("/brief"))
        self.assertNotIn("--runner", argv)
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

    def _command_operation(self, provider: str, mode: str, scenario: str,
                           root: Path) -> gate.NativeAcceptance:
        operation = object.__new__(gate.NativeAcceptance)
        operation.delegate = root / "delegate"
        operation.state = root / "state"
        operation.models_state = root / "models-state"
        operation.base = root
        operation.output = root / "output"
        operation.processes = SimpleNamespace()
        operation.pueue = root / "pueue"
        operation.config = root / "pueue.yml"
        operation.runner = root / "runner"
        operation.workspace = root / "workspace"
        operation.provider = provider
        operation.mode = mode
        operation.scenario = scenario
        operation.args = SimpleNamespace(model=None, effort=None)
        return operation

    def test_timeout_resume_uses_managed_runner_commands(self):
        self._assert_managed_runner_commands((
            ("codex:exec", "workspace-write", "timeout-resume"),
            ("opencode:run", "workspace-write", "timeout-resume")))

    def test_every_scenario_admits_through_the_managed_runner_by_default(self):
        self._assert_managed_runner_commands((
            ("claude:print", "read-only", "read-only"), ("ccr:print", "workspace-write", "write"),
            ("codex:exec", "read-only", "non-git"), ("codex:exec", "workspace-write", "non-git"),
            ("claude:print", "read-only", "interactive")))

    def _assert_managed_runner_commands(self, cases: tuple[tuple[str, str, str], ...]) -> None:
        root = Path("/fixture")
        for provider, mode, scenario in cases:
            with self.subTest(provider=provider, scenario=scenario):
                operation = self._command_operation(provider, mode, scenario, root)
                operation.ops = gate.NativeAcceptanceTaskOps(operation)
                self.assertTrue(operation.ops.managed_runner)
                prefix = operation.task_command_prefix()
                dispatch = operation.dispatch_argv("a" * 32, Path("/brief"))
                models = operation.model_discovery_argv()
                for argv in (prefix, dispatch, models):
                    self.assertNotIn("--runner", argv)
                    self.assertNotIn(operation.runner, argv)
                self.assertEqual(dispatch[:len(prefix)], prefix)
                self.assertEqual(prefix, [operation.delegate, "--root", operation.state,
                                          "--pueue-config", operation.config])
                self.assertEqual(models[:5], [operation.delegate, "--root", operation.models_state,
                                              "--pueue-config", operation.config])
                cleanup = gate.ModelDiscoveryCleanupOps(operation)
                self.assertTrue(cleanup.managed_runner)

    def test_internal_custom_fixture_keeps_its_explicit_runner(self):
        operation = self._command_operation("codex:exec", "workspace-write", "timeout-resume",
                                            Path("/fixture"))
        operation.ops = gate.NativeAcceptanceTaskOps(operation, managed_runner=False)
        for argv in (operation.task_command_prefix(),
                     operation.dispatch_argv("a" * 32, Path("/brief")),
                     operation.model_discovery_argv()):
            self.assertEqual(argv[argv.index("--runner") + 1], operation.runner)
        self.assertFalse(gate.ModelDiscoveryCleanupOps(operation).managed_runner)

    def test_managed_runner_queue_validation_requires_guardian(self):
        with tempfile.TemporaryDirectory(prefix="native-managed-runner-") as directory:
            root = Path(directory)
            state = root / "state"
            task = "a" * 32
            task_directory = state / "tasks" / task
            task_directory.mkdir(parents=True)
            source_runner = root / "delegate-run"
            source_runner.write_bytes(b"managed runner fixture\n")
            managed_runner = managed_state_runner(state, gate.digest(source_runner))
            managed_runner.parent.mkdir()
            managed_runner.write_bytes(source_runner.read_bytes())
            source_runner.chmod(0o700)
            managed_runner.chmod(0o700)
            gate.write_json(task_directory / "meta.json", {
                "runner_executable": str(managed_runner),
                "runner_ownership": "managed",
            })
            ops = provider_common.NativeTaskOps(
                SimpleNamespace(), root / "delegate", source_runner, root / "pueue",
                root, state, root / "output", managed_runner=True,
                runner_placement=STATE_INSTALLED_RUNNER)
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

            # Go names the state copy by digest; a same-bytes copy elsewhere
            # is not the runner a managed admission executes.
            other = state / ".supervisor" / "delegate-run-managed"
            other.write_bytes(source_runner.read_bytes())
            other.chmod(0o700)
            gate.write_json(task_directory / "meta.json", {
                "runner_executable": str(other), "runner_ownership": "managed",
            }, replace=True)
            with self.assertRaisesRegex(gate.AcceptanceFailure,
                                        "ordinary task managed runner executable changed"):
                ops._ordinary_runner_executable(task)
            gate.write_json(task_directory / "meta.json", {
                "runner_executable": str(managed_runner), "runner_ownership": "managed",
            }, replace=True)
            managed_runner.write_bytes(b"replaced runner\n")
            with self.assertRaisesRegex(gate.AcceptanceFailure, "managed runner executable changed"):
                ops._ordinary_runner_executable(task)

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
        operation.ops = SimpleNamespace(managed_runner=True)
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

    def test_timeout_resume_selection_accepts_only_codex_or_opencode_workspace_write(self):
        refused = (
            ("opencode:run", "read-only"), ("codex:exec", "read-only"),
            ("claude:print", "workspace-write"), ("pi:json", "workspace-write"),
            ("ccr:print", "workspace-write"), ("antigravity:print", "workspace-write"),
        )
        for provider, mode in refused:
            with self.subTest(provider=provider, mode=mode):
                args = SimpleNamespace(provider=provider, mode=mode, scenario="timeout-resume")
                with self.assertRaisesRegex(
                        gate.AcceptanceFailure,
                        "timeout-resume scenario requires codex:exec or opencode:run workspace-write"):
                    gate.NativeAcceptance(args)
        with self.assertRaisesRegex(gate.AcceptanceFailure, "codex:review does not support"):
            gate.NativeAcceptance(SimpleNamespace(
                provider="codex:review", mode="workspace-write", scenario="timeout-resume"))
        args = entry.build_parser().parse_args([
            "--provider", "opencode:run", "--permission", "workspace-write",
            "--scenario", "timeout-resume", "--output", "/evidence",
        ])
        self.assertEqual((args.provider, args.mode, args.scenario),
                         ("opencode:run", "workspace-write", "timeout-resume"))

    def test_opencode_timeout_workspace_has_no_codex_git_or_baseline(self):
        with tempfile.TemporaryDirectory(prefix="native-opencode-timeout-workspace-") as directory:
            workspace = Path(directory) / "workspace"
            workspace.mkdir()
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "opencode:run"
            operation.scenario = "timeout-resume"
            operation.git = None
            operation.workspace = workspace
            operation.processes = SimpleNamespace()

            operation._prepare_codex_workspace()

            self.assertEqual(gate.workspace_snapshot(workspace), {})

    def test_expected_runner_defaults_to_the_single_ledger_runner(self):
        with tempfile.TemporaryDirectory(prefix="native-default-runner-") as directory:
            root = Path(directory)
            state = root / "state"
            runners = runner_fixtures(root)
            ops = provider_common.NativeTaskOps(
                SimpleNamespace(), root / "delegate", runners["new"], root / "pueue",
                root, state, root / "output")

            for task in (OLD_TASK, NEW_TASK, UNBOUND_TASK):
                self.assertEqual(ops._expected_runner(task), runners["new"])
            row = ordinary_row(runners["new"], state, OLD_TASK)
            ops.validate_ordinary_queue_row(row, row["label"], OLD_TASK, 7, "default")
            row = ordinary_row(runners["old"], state, OLD_TASK)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "exact runner/state/task"):
                ops.validate_ordinary_queue_row(row, row["label"], OLD_TASK, 7, "default")

    def test_custom_queue_rows_bind_only_each_preallocated_task_runner(self):
        with tempfile.TemporaryDirectory(prefix="native-bound-custom-runner-") as directory:
            root = Path(directory)
            state = root / "state"
            runners = runner_fixtures(root)
            ops = PreallocatedRunnerOps(
                {OLD_TASK: runners["old"], NEW_TASK: runners["new"]},
                runner=runners["new"], base=root, state_parent=root, state=state)

            for task, runner in ((OLD_TASK, runners["old"]), (NEW_TASK, runners["new"])):
                row = ordinary_row(runner, state, task)
                ops.validate_ordinary_queue_row(row, row["label"], task, 7, "default")
            for task, runner in ((OLD_TASK, runners["new"]), (NEW_TASK, runners["old"]),
                                 (OLD_TASK, runners["third"]), (NEW_TASK, runners["third"])):
                with self.subTest(task=task, runner=runner.name):
                    row = ordinary_row(runner, state, task)
                    with self.assertRaisesRegex(gate.AcceptanceFailure, "exact runner/state/task"):
                        ops.validate_ordinary_queue_row(row, row["label"], task, 7, "default")
            row = ordinary_row(runners["new"], state, UNBOUND_TASK)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "no bound expected runner"):
                ops.validate_ordinary_queue_row(row, row["label"], UNBOUND_TASK, 7, "default")
            with self.assertRaises(TypeError):
                ops.bindings[UNBOUND_TASK] = runners["third"]

    def test_managed_runner_binding_compares_each_task_with_its_selected_runner(self):
        with tempfile.TemporaryDirectory(prefix="native-bound-managed-runner-") as directory:
            root = Path(directory)
            state = root / "state"
            runners = runner_fixtures(root)
            managed = {}
            for task, name in ((OLD_TASK, "old"), (NEW_TASK, "new"), (UNBOUND_TASK, "new")):
                (state / "tasks" / task).mkdir(parents=True)
                managed[task] = managed_state_runner(state, provider_common.digest(runners[name]))
                managed[task].parent.mkdir(exist_ok=True)
                managed[task].write_bytes(runners[name].read_bytes())
                managed[task].chmod(0o700)
                gate.write_json(state / "tasks" / task / "meta.json", {
                    "runner_executable": str(managed[task]), "runner_ownership": "managed",
                })
            ops = PreallocatedRunnerOps(
                {OLD_TASK: runners["old"], NEW_TASK: runners["new"]},
                runner=runners["new"], base=root, state_parent=root, state=state,
                managed_runner=True, runner_placement=STATE_INSTALLED_RUNNER)

            for task in (OLD_TASK, NEW_TASK):
                self.assertEqual(ops._ordinary_runner_executable(task), managed[task])
                row = ordinary_row(managed[task], state, task, supervised=True)
                ops.validate_ordinary_queue_row(row, row["label"], task, 7, "default")
            with self.assertRaisesRegex(gate.AcceptanceFailure, "no bound expected runner"):
                ops._ordinary_runner_executable(UNBOUND_TASK)
            swapped = PreallocatedRunnerOps(
                {OLD_TASK: runners["new"], NEW_TASK: runners["third"]},
                runner=runners["old"], base=root, state_parent=root, state=state,
                managed_runner=True, runner_placement=STATE_INSTALLED_RUNNER)
            for task in (OLD_TASK, NEW_TASK):
                with self.subTest(task=task):
                    with self.assertRaisesRegex(gate.AcceptanceFailure, "managed runner executable changed"):
                        swapped._ordinary_runner_executable(task)

    def test_managed_bundle_binding_keeps_each_task_on_its_selected_bundle_runner(self):
        with tempfile.TemporaryDirectory(prefix="native-bound-bundle-runner-") as directory:
            root = Path(directory)
            state = root / "state"
            runners = runner_fixtures(root)
            for task, name in ((OLD_TASK, "old"), (NEW_TASK, "new"), (UNBOUND_TASK, "new")):
                (state / "tasks" / task).mkdir(parents=True)
                gate.write_json(state / "tasks" / task / "meta.json", {
                    "runner_executable": str(runners[name]), "runner_ownership": "managed",
                })
            ops = PreallocatedRunnerOps(
                {OLD_TASK: runners["old"], NEW_TASK: runners["new"]},
                runner=runners["new"], base=root, state_parent=root, state=state,
                managed_runner=True, runner_placement=BUNDLE_RUNNER)
            for task, name in ((OLD_TASK, "old"), (NEW_TASK, "new")):
                self.assertEqual(ops._ordinary_runner_executable(task), runners[name])
                row = ordinary_row(runners[name], state, task, supervised=True)
                ops.validate_ordinary_queue_row(row, row["label"], task, 7, "default")
            with self.assertRaisesRegex(gate.AcceptanceFailure, "no bound expected runner"):
                ops._ordinary_runner_executable(UNBOUND_TASK)
            swapped = PreallocatedRunnerOps(
                {OLD_TASK: runners["new"], NEW_TASK: runners["third"]},
                runner=runners["old"], base=root, state_parent=root, state=state,
                managed_runner=True, runner_placement=BUNDLE_RUNNER)
            for task in (OLD_TASK, NEW_TASK):
                with self.subTest(task=task):
                    with self.assertRaisesRegex(gate.AcceptanceFailure, "managed runner executable changed"):
                        swapped._ordinary_runner_executable(task)

    def _bound_inspection_fixture(self, bindings: dict[str, str]) -> tuple[InspectionFixture, Path]:
        fixture = InspectionFixture(self)
        runners = {"old": fixture.runner, **{
            name: path for name, path in runner_fixtures(fixture.base).items() if name != "old"}}
        fixture.ops = PreallocatedRunnerOps(
            {task: runners[name] for task, name in bindings.items()},
            runner=runners["new"], base=fixture.base,
            state_parent=fixture.state_parent, state=fixture.state)
        fixture.ops.bind_supervisor(fixture.config)
        fixture.ops.root_id = INSPECTION_ROOT
        fixture.ops.tasks = {"fresh": INSPECTION_TASK}
        fixture.ops.labels = {"fresh": f"delegate:{INSPECTION_ROOT}:{INSPECTION_TASK}"}
        fixture.ops.numbers = {"fresh": 7}
        fixture.ops.dispatch_attempts = {INSPECTION_TASK}
        return fixture, fixture.inspection_records()

    def test_inspection_journal_and_queue_row_retain_selected_runner_provenance(self):
        fixture, directory = self._bound_inspection_fixture({INSPECTION_TASK: "old"})
        journal = fixture.ops.validate_inspection_journal(INSPECTION_TASK, directory)
        fixture.ops._validate_inspection_queue_row(
            fixture.inspection_row(), journal, allow_failure=False)
        fixture.ops._validate_queue_membership(fixture.status(fixture.inspection_row()), False)
        self.assertNotEqual(provider_common.digest(fixture.ops.runner),
                            provider_common.digest(fixture.runner))

        for bindings, message in (({INSPECTION_TASK: "new"}, "worker mismatch"),
                                  ({INSPECTION_TASK: "third"}, "worker mismatch"),
                                  ({}, "no bound expected runner")):
            with self.subTest(bindings=bindings):
                other, other_directory = self._bound_inspection_fixture(bindings)
                with self.assertRaisesRegex(gate.AcceptanceFailure, message):
                    other.ops.validate_inspection_journal(INSPECTION_TASK, other_directory)
                expected_row_error = "argv mismatch" if bindings else message
                with self.assertRaisesRegex(gate.AcceptanceFailure, expected_row_error):
                    other.ops._validate_inspection_queue_row(
                        other.inspection_row(), journal, allow_failure=False)

    def test_inspection_queue_row_requires_a_journal_task_identity(self):
        fixture, _directory = self._bound_inspection_fixture({INSPECTION_TASK: "old"})
        with self.assertRaisesRegex(gate.AcceptanceFailure, "journal has no task identity"):
            fixture.ops._validate_inspection_queue_row(
                fixture.inspection_row(), {"task_id": None}, allow_failure=False)


# Delegate persists an omitted Codex review budget as its public one-hour
# default (internal/config/policy/policy.go ParseBudgetForProvider).
REVIEW_DEFAULT_BUDGET = "1h0m0s"
REVIEW_DEFAULT_BUDGET_NANOS = 3_600_000_000_000
ORDINARY_BUDGET = "5m0s"
ORDINARY_BUDGET_NANOS = 300_000_000_000
NATIVE_SUITE_SECONDS = 4 * 60 * 60


class NativeCaseBudgetContractTests(unittest.TestCase):
    """Dispatch, the persisted request, the outer wait, and the receipt share one case budget."""

    @staticmethod
    def _operation(provider: str, root: Path, mode: str = "read-only") -> object:
        operation = object.__new__(gate.NativeAcceptance)
        operation.delegate = Path("/delegate")
        operation.state = root / "state"
        operation.config = Path("/pueue.yml")
        operation.runner = Path("/runner")
        operation.workspace = root / "workspace"
        operation.workspace.mkdir()
        operation.provider = provider
        operation.mode = mode
        operation.scenario = "read-only"
        operation.root_id = "b" * 32
        operation.ops = SimpleNamespace(managed_runner=True)
        operation.args = SimpleNamespace(model=None, effort=None)
        return operation

    def _persist_request(self, operation, budget: str, budget_nanos: int) -> str:
        task_id = "a" * 32
        task = operation.state / "tasks" / task_id
        task.mkdir(parents=True)
        brief = b"review\n"
        (task / "brief.md").write_bytes(brief)
        gate.write_json(task / "task.json", {
            "root_id": operation.root_id, "task_id": task_id, "provider": operation.provider,
            "mode": operation.mode, "canonical_cwd": str(operation.workspace),
            "requested_config": {"permission": operation.mode, "budget": budget},
            "budget_nanos": budget_nanos, "brief_length": len(brief),
            "brief_sha256": hashlib.sha256(brief).hexdigest(),
        })
        return task_id

    @staticmethod
    def _admission(operation, brief: Path, **options) -> tuple[list[object], object]:
        """Capture the argv and retained request one public dispatch hands to admission."""
        captured: dict[str, object] = {}

        def admit(_name, _task_id, argv, *, request, environment):
            captured.update(argv=argv, request=request)
            raise RuntimeError("admission captured")

        operation.ops = SimpleNamespace(managed_runner=True, dispatch=admit)
        try:
            operation.dispatch("fresh", brief, **options)
        except RuntimeError as error:
            if str(error) != "admission captured":
                raise
        return captured["argv"], captured["request"]

    def _assert_request_budget_accepted(self, operation, task_id: str, **expected) -> None:
        # The request check precedes provider metadata; reaching the absent
        # meta.json proves the persisted budget satisfied the case contract.
        with self.assertRaises(FileNotFoundError):
            operation._request_binding(task_id, **expected)

    def _wait_timeout(self, provider: str, root: Path) -> float:
        owner = SimpleNamespace(
            provider=provider,
            processes=gate.Processes(root / "processes", {"PATH": "/usr/bin:/bin"}),
            delegate=Path("/delegate"), runner=Path("/runner"), pueue=Path("/pueue"),
            base=root, state=root / "state", output=root / "evidence")
        ops = gate.NativeAcceptanceTaskOps(owner)
        ops.root_id = "b" * 32
        observed: dict[str, float] = {}

        def stop(*_args, timeout, **_kwargs):
            observed["timeout"] = timeout
            raise RuntimeError("observed")

        with patch("scripts.delegate_acceptance.tasks.operations.wait_runner_terminal", stop), \
                self.assertRaisesRegex(RuntimeError, "observed"):
            ops.wait_task("fresh", "a" * 32)
        return observed["timeout"]

    def test_codex_review_dispatch_omits_budget_and_binds_the_public_default(self):
        with tempfile.TemporaryDirectory(prefix="native-review-budget-") as directory:
            root = Path(directory)
            operation = self._operation("codex:review", root)
            brief = root / "brief.md"
            brief.write_text("review\n")
            argv, request = self._admission(operation, brief)

        self.assertNotIn("--budget", argv)
        self.assertNotIn("--model", argv)
        self.assertNotIn("--effort", argv)
        self.assertEqual(argv[argv.index("--permission") + 1], "read-only")
        self.assertEqual(argv[argv.index("--review-target") + 1], "uncommitted")
        self.assertEqual(request.budget, REVIEW_DEFAULT_BUDGET)

    def test_ordinary_profiles_keep_the_finite_five_minute_and_timeout_budgets(self):
        for provider in sorted(set(gate.PROFILES) - {"codex:review"}):
            with self.subTest(provider=provider), tempfile.TemporaryDirectory(
                    prefix="native-ordinary-budget-") as directory:
                root = Path(directory)
                operation = self._operation(provider, root)
                brief = root / "brief.md"
                brief.write_text("fresh\n")
                argv, request = self._admission(operation, brief)
                timeout_argv, timeout_request = self._admission(operation, brief, budget="3m")
                self.assertEqual(argv[argv.index("--budget") + 1], "5m")
                self.assertEqual(timeout_argv[timeout_argv.index("--budget") + 1], "3m")
                self.assertEqual((request.budget, timeout_request.budget), ("5m", "3m"))

    def test_persisted_request_budget_matches_the_provider_case(self):
        cases = (
            ("codex:review", REVIEW_DEFAULT_BUDGET, REVIEW_DEFAULT_BUDGET_NANOS,
             ORDINARY_BUDGET, ORDINARY_BUDGET_NANOS),
            ("codex:exec", ORDINARY_BUDGET, ORDINARY_BUDGET_NANOS,
             REVIEW_DEFAULT_BUDGET, REVIEW_DEFAULT_BUDGET_NANOS),
        )
        for provider, budget, nanos, foreign, foreign_nanos in cases:
            with self.subTest(provider=provider):
                with tempfile.TemporaryDirectory(prefix="native-persisted-budget-") as directory:
                    operation = self._operation(provider, Path(directory))
                    task_id = self._persist_request(operation, budget, nanos)
                    self._assert_request_budget_accepted(operation, task_id)
                with tempfile.TemporaryDirectory(prefix="native-persisted-budget-") as directory:
                    operation = self._operation(provider, Path(directory))
                    task_id = self._persist_request(operation, foreign, foreign_nanos)
                    with self.assertRaisesRegex(gate.AcceptanceFailure, "request binding is invalid"):
                        operation._request_binding(task_id)

    def test_timeout_source_budget_override_stays_three_minutes(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-budget-") as directory:
            operation = self._operation("codex:exec", Path(directory), mode="workspace-write")
            task_id = self._persist_request(operation, "3m0s", 180_000_000_000)
            self._assert_request_budget_accepted(
                operation, task_id, budget="3m0s", budget_nanos=180_000_000_000)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "request binding is invalid"):
                operation._request_binding(task_id)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "both its text and nanoseconds"):
                operation._request_binding(task_id, budget="3m0s")

    def test_outer_runner_wait_outlives_the_persisted_budget_within_the_native_suite(self):
        for provider, budget_seconds in (("codex:review", REVIEW_DEFAULT_BUDGET_NANOS // 10 ** 9),
                                         ("codex:exec", ORDINARY_BUDGET_NANOS // 10 ** 9),
                                         ("claude:print", ORDINARY_BUDGET_NANOS // 10 ** 9)):
            with self.subTest(provider=provider), tempfile.TemporaryDirectory(
                    prefix="native-outer-wait-") as directory:
                timeout = self._wait_timeout(provider, Path(directory))
                self.assertGreater(timeout, budget_seconds)
                self.assertLessEqual(timeout, budget_seconds + 60)
                self.assertLess(timeout, NATIVE_SUITE_SECONDS)

    def test_codex_review_receipt_records_the_omitted_request_and_persisted_default(self):
        with tempfile.TemporaryDirectory(prefix="native-review-receipt-") as directory:
            operation = self._operation("codex:review", Path(directory))
            task_id = "a" * 32
            dispatched: list[tuple[tuple[object, ...], dict[str, object]]] = []
            operation.tasks = {"fresh": task_id}
            operation.records = {"fresh-collect": {}}
            operation.model_discovery_summary = None
            operation.brief = lambda name: Path("/brief")
            operation.dispatch = lambda *args, **kwargs: (dispatched.append((args, kwargs)), task_id)[1]
            for name in ("wait_done", "collect", "task_binding", "replay", "shutdown"):
                setattr(operation, name, lambda *_args, **_kwargs: None)
            operation.session_ref = lambda _task: "session"
            operation.native_mode_receipt = lambda: {}
            operation.provider_runtime_receipt = lambda: {}
            operation.lifecycle_receipt = lambda: {}
            self.assertEqual(operation._run_review(), 0)

        self.assertEqual(dispatched, [(("fresh", Path("/brief")), {})])
        receipt = operation.success_receipt
        self.assertIsNone(receipt["task_budget"])
        self.assertEqual(receipt["canonical_task_budget"], REVIEW_DEFAULT_BUDGET)
        self.assertEqual((receipt["requested_model"], receipt["requested_effort"]), (None, None))
