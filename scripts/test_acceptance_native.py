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
import native_acceptance_evidence as native_evidence
import acceptance_provider_common as provider_common
import acceptance_supervisor_common as supervisor_common


AGGREGATE_WRAPPER_TEST_TIMEOUT = 60


def fake_native_python_script() -> str:
    return '''#!/bin/sh
if [ "$1" = -c ]; then
    printf '3\\n'
    exit 0
fi
case "$1" in
    *native_acceptance_evidence.py)
        if [ "${FAKE_HELPER_EXIT:-0}" -ne 0 ] && [ "$2" = aggregate ]; then
            exit "${FAKE_HELPER_EXIT}"
        fi
        exec "$REAL_PYTHON" "$@"
        ;;
    *acceptance_native.py)
        output=
        provider=
        mode=
        scenario=
        while [ "$#" -gt 0 ]; do
            case "$1" in
                --output) output=$2; shift 2 ;;
                --provider) provider=$2; shift 2 ;;
                --permission) mode=$2; shift 2 ;;
                --scenario) scenario=$2; shift 2 ;;
                *) shift ;;
            esac
        done
        code=2
        case "${FAKE_NATIVE_RESULT:-blocked}" in
            mixed)
                case "$provider" in
                    claude:print) code=0 ;;
                    codex:exec) code=1 ;;
                esac
                ;;
            timeout)
                if [ "$scenario" = timeout-resume ] || [ "$provider" = claude:print ]; then
                    code=0
                fi
                ;;
            only-timeout)
                if [ "$scenario" = timeout-resume ]; then
                    code=0
                fi
                ;;
        esac
        if [ "$code" -eq 0 ]; then
            if [ "$scenario" = timeout-resume ]; then
                tasks='{"timeout_source":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","timeout_resume":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}'
                sessions='{"timeout_source":"fixture-session","timeout_resume":"fixture-session"}'
            elif [ "$provider" = codex:review ]; then
                tasks='{"fresh":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}'
                sessions='{"fresh":"fixture-session"}'
                versions='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"1.0"}'
                runtimes='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"executable":"/fixture/provider","version":"1.0","sha256":"1111111111111111111111111111111111111111111111111111111111111111"}}'
            else
                tasks='{"fresh":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resume":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}'
                sessions='{"fresh":"fixture-session","resume":"fixture-session"}'
                versions='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"1.0","bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":"1.1"}'
                runtimes='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"executable":"/fixture/provider","version":"1.0","sha256":"1111111111111111111111111111111111111111111111111111111111111111"},"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":{"executable":"/fixture/provider","version":"1.1","sha256":"6666666666666666666666666666666666666666666666666666666666666666"}}'
            fi
            if [ "$scenario" = timeout-resume ]; then
                versions='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"1.0","bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":"1.1"}'
                runtimes='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"executable":"/fixture/provider","version":"1.0","sha256":"1111111111111111111111111111111111111111111111111111111111111111"},"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":{"executable":"/fixture/provider","version":"1.1","sha256":"6666666666666666666666666666666666666666666666666666666666666666"}}'
            fi
            printf '{"gate":"acceptance-native","status":"passed","provider":"%s","mode":"%s","scenario":"%s","tasks":%s,"provider_sessions":%s,"provider_versions":%s,"provider_runtime_identities":%s,"natural_daemon_shutdown":true}\\n' "$provider" "$mode" "$scenario" "$tasks" "$sessions" "$versions" "$runtimes" >"$output/success.json"
            printf '{"provider":"%s","mode":"%s","scenario":"%s","provider_identity_scope":"first-admitted-task","provider_task_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","provider_executable":"/fixture/provider","provider_version":"1.0","provider_sha256":"1111111111111111111111111111111111111111111111111111111111111111","delegate_sha256":"2222222222222222222222222222222222222222222222222222222222222222","runner_sha256":"3333333333333333333333333333333333333333333333333333333333333333","pueue_sha256":"4444444444444444444444444444444444444444444444444444444444444444","pueued_sha256":"5555555555555555555555555555555555555555555555555555555555555555"}\\n' "$provider" "$mode" "$scenario" >"$output/binding.json"
            printf '{"status":"removed","supervisor_shutdown_observed":true,"raw_process_captures":"removed"}\\n' >"$output/cleanup.json"
        elif [ "$code" -eq 2 ]; then
            printf '{"gate":"acceptance-native","status":"BLOCKED","provider":"%s","mode":"%s","scenario":"%s"}\\n' "$provider" "$mode" "$scenario" >"$output/failure.json"
        else
            printf '{"gate":"acceptance-native","status":"failed","provider":"%s","mode":"%s","scenario":"%s"}\\n' "$provider" "$mode" "$scenario" >"$output/failure.json"
            printf 'unresolved raw evidence\\n' >"$output/raw-provider-output"
        fi
        exit "$code"
        ;;
esac
exit 7
'''


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
        args = gate.build_parser().parse_args([
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
                "task_id": task, "label": label, "group": group,
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
        args = gate.build_parser().parse_args([
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
        operation.interaction_observation_number = 0
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

            with patch.object(gate, "verify_collected_outcome",
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

    @staticmethod
    def _timeout_lease_operation(root: Path) -> object:
        output = root / "evidence"
        workspace = root / "workspace"
        briefs = root / "briefs"
        lease_directory = root / "leases"
        output.mkdir(parents=True)
        owner = supervisor_common.DirectoryOwner()
        workspace_token = owner.create("timeout workspace", workspace, root)
        briefs_token = owner.create("timeout briefs", briefs, root)
        lease_token = owner.create("timeout liveness leases", lease_directory, root)
        raw_token = owner.create("raw process captures", output / "processes", output)
        operation = object.__new__(gate.NativeAcceptance)
        operation.output = output
        operation.workspace = workspace_token.directory
        operation.briefs = briefs_token.directory
        operation.owned_directories = owner.transfer()
        operation.processes = SimpleNamespace(capture_root=raw_token)
        operation.timeout_lease_directory = lease_directory
        operation.timeout_parent_fifo = lease_directory / "parent.fifo"
        operation.timeout_child_fifo = lease_directory / "child.fifo"
        token = hashlib.sha256(str(root).encode()).hexdigest()
        operation.timeout_parent_ready_marker = "parent-ready:" + token
        operation.timeout_child_ready_marker = "child-ready:" + token
        operation.timeout_marker = "timeout-marker:" + token
        operation.timeout_marker_file = workspace / "acceptance-timeout-marker.txt"
        operation.timeout_resume_file = workspace / "acceptance-timeout-resumed.txt"
        operation.timeout_leases = {
            "parent": gate.TimeoutFifoLease(
                operation.timeout_parent_fifo, "timeout parent",
                operation.timeout_parent_ready_marker),
            "child": gate.TimeoutFifoLease(
                operation.timeout_child_fifo, "timeout child",
                operation.timeout_child_ready_marker),
        }
        return operation

    @staticmethod
    def _timeout_writer(lease: gate.TimeoutFifoLease) -> int:
        return os.open(lease.path, os.O_WRONLY | os.O_NONBLOCK)

    def test_timeout_liveness_requires_exact_fifo_inode_and_distinct_ready_evidence(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-ready-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_leases["parent"]
            child = operation.timeout_leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            try:
                os.write(parent_writer, parent.expected)
                os.write(child_writer, child.expected)
                self.assertTrue(operation._capture_timeout_liveness())
                receipt = gate.read_json(operation.output / "timeout-liveness.json")
                self.assertTrue(receipt["ready"])
                self.assertFalse(receipt["closed"])
                self.assertNotEqual(parent.inode, child.inode)
                for name, lease in operation.timeout_leases.items():
                    metadata = lease.path.lstat()
                    self.assertEqual(metadata.st_dev, lease.device)
                    self.assertEqual(metadata.st_ino, lease.inode)
                    self.assertTrue(stat.S_ISFIFO(metadata.st_mode))
                    self.assertEqual(receipt["resources"][name]["inode"], lease.inode)
            finally:
                os.close(parent_writer)
                os.close(child_writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_marker_only_forgery(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-marker-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_leases["parent"]
            child = operation.timeout_leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            try:
                os.write(parent_writer, parent.expected)
                os.write(child_writer, child.expected)
                os.close(parent_writer)
                os.close(child_writer)
                with self.assertRaisesRegex(gate.AcceptanceFailure, "closed before"):
                    operation._capture_timeout_liveness()
                self.assertFalse((operation.output / "timeout-liveness.json").exists())
            finally:
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_malformed_fifo_evidence(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-malformed-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_leases["parent"]
            writer = self._timeout_writer(parent)
            try:
                os.write(writer, b"marker-only\n")
                with self.assertRaisesRegex(gate.AcceptanceFailure, "malformed"):
                    operation._timeout_liveness_ready()
            finally:
                os.close(writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_marker_file_alone_is_not_readiness(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-marker-file-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            operation.timeout_marker_file.write_text("timeout-marker:forged\n")
            self.assertFalse(operation._timeout_liveness_ready())
            self.assertFalse((operation.output / "timeout-liveness.json").exists())
            operation._cleanup_timeout_leases()

    def test_timeout_liveness_one_side_eof_is_insufficient(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-one-eof-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_leases["parent"]
            child = operation.timeout_leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            os.write(parent_writer, parent.expected)
            os.write(child_writer, child.expected)
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                os.close(parent_writer)
                self.assertFalse(operation._timeout_liveness_closed())
                self.assertFalse(operation.timeout_leases["child"].eof)
            finally:
                os.close(child_writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_requires_both_fifo_eof_for_success(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-both-eof-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_leases["parent"]
            child = operation.timeout_leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            os.write(parent_writer, parent.expected)
            os.write(child_writer, child.expected)
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                os.close(parent_writer)
                os.close(child_writer)
                self.assertTrue(operation._timeout_liveness_closed())
                operation.wait_timeout_leases_closed()
                receipt = gate.read_json(operation.output / "timeout-liveness.json")
                self.assertTrue(receipt["closed"])
                self.assertTrue(all(item["closed"] for item in receipt["resources"].values()))
            finally:
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_observes_eof_without_a_poll_event(self):
        class SilentPoll:
            def register(self, _fd, _events):
                return None

            def poll(self, _timeout):
                return []

        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-silent-eof-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_leases["parent"]
            writer = self._timeout_writer(parent)
            try:
                os.write(writer, parent.expected)
                parent.observe()
                self.assertTrue(parent.ready)
                self.assertFalse(parent.eof)
                os.close(writer)
                writer = None
                with patch.object(gate.select, "poll", return_value=SilentPoll()):
                    parent.observe()
                self.assertTrue(parent.eof)
            finally:
                if writer is not None:
                    os.close(writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_cleanup_removes_only_owned_fifos_and_artifacts(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-cleanup-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            operation.timeout_marker_file.write_text("owned\n")
            operation.timeout_resume_file.write_text("owned\n")
            paths = [lease.path for lease in operation.timeout_leases.values()]

            self.assertTrue(operation._cleanup_timeout_leases())
            self.assertTrue(all(not path.exists() for path in paths))
            self.assertTrue(operation.timeout_lease_directory.is_dir())
            self.assertFalse(operation.timeout_marker_file.exists())
            self.assertFalse(operation.timeout_resume_file.exists())
            self.assertTrue(all(lease.fd is None for lease in operation.timeout_leases.values()))

    def test_finalize_retains_timeout_leases_when_shutdown_is_unproven(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-finalize-retain-") as directory:
            root = Path(directory)
            operation = self._timeout_lease_operation(root)
            base_owner = supervisor_common.DirectoryOwner()
            operation.base_parent = root / "private"
            operation.base_parent_token = base_owner.create(
                "private parent", operation.base_parent, root)
            operation.base = base_owner.create(
                "private base", operation.base_parent / "run", operation.base_parent).directory
            operation.owned_directories = (
                *base_owner.transfer(), *operation.owned_directories,
            )
            operation.closed = False
            operation.safe_shutdown = lambda: False
            try:
                self.assertFalse(operation.finalize(True))
                self.assertTrue(operation.timeout_lease_directory.is_dir())
                self.assertTrue(all(lease.path.is_fifo()
                                    for lease in operation.timeout_leases.values()))
                receipt = gate.read_json(operation.output / "cleanup.json")
                self.assertEqual(receipt["status"], "retained")
                self.assertFalse(receipt["supervisor_shutdown_observed"])
            finally:
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_fifo_replacement_and_preserves_replacement(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-replace-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_leases["parent"]
            child = operation.timeout_leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            os.write(parent_writer, parent.expected)
            os.write(child_writer, child.expected)
            replacement = parent.path
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                replacement.unlink()
                replacement.write_text("replacement")
                with self.assertRaisesRegex(gate.AcceptanceFailure, "replaced"):
                    operation._timeout_liveness_closed()
                self.assertTrue(replacement.is_file())
            finally:
                os.close(parent_writer)
                os.close(child_writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_rejects_an_extra_writer_until_it_closes(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-extra-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            parent = operation.timeout_leases["parent"]
            child = operation.timeout_leases["child"]
            parent_writer = self._timeout_writer(parent)
            child_writer = self._timeout_writer(child)
            extra_writer = None
            os.write(parent_writer, parent.expected)
            os.write(child_writer, child.expected)
            try:
                self.assertTrue(operation._capture_timeout_liveness())
                extra_writer = self._timeout_writer(child)
                os.close(parent_writer)
                os.close(child_writer)
                self.assertFalse(operation._timeout_liveness_closed())
                os.close(extra_writer)
                extra_writer = None
                self.assertTrue(operation._timeout_liveness_closed())
            finally:
                if extra_writer is not None:
                    os.close(extra_writer)
                operation._cleanup_timeout_leases()

    def test_timeout_liveness_isolated_between_concurrent_acceptance_runs(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-isolation-") as directory:
            root = Path(directory)
            first = self._timeout_lease_operation(root / "first")
            second = self._timeout_lease_operation(root / "second")
            first_parent = first.timeout_leases["parent"]
            first_child = first.timeout_leases["child"]
            parent_writer = self._timeout_writer(first_parent)
            child_writer = self._timeout_writer(first_child)
            try:
                os.write(parent_writer, first_parent.expected)
                os.write(child_writer, first_child.expected)
                self.assertTrue(first._capture_timeout_liveness())
                self.assertFalse(second._timeout_liveness_ready())
                self.assertNotEqual(first_parent.inode, second.timeout_leases["parent"].inode)
                forged = self._timeout_writer(second.timeout_leases["parent"])
                try:
                    with self.assertRaisesRegex(gate.AcceptanceFailure, "malformed"):
                        os.write(forged, first_parent.expected)
                        second._timeout_liveness_ready()
                finally:
                    os.close(forged)
            finally:
                os.close(parent_writer)
                os.close(child_writer)
                first._cleanup_timeout_leases()
                second._cleanup_timeout_leases()

    def test_timeout_brief_uses_harness_fifo_leases_and_portable_shell_syntax(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-brief-") as directory:
            operation = self._timeout_lease_operation(Path(directory))
            brief = operation.timeout_brief("timeout-source").read_text()
            command = brief.split("`", 2)[1]

            self.assertIn(str(operation.timeout_parent_fifo), command)
            self.assertIn(str(operation.timeout_child_fifo), command)
            self.assertIn(operation.timeout_parent_ready_marker, command)
            self.assertIn(operation.timeout_child_ready_marker, command)
            self.assertTrue(command.startswith("set -eu; "))
            self.assertIn("exec 3>", command)
            self.assertIn("exec 4>", command)
            self.assertIn("exec sleep 600", command)
            self.assertIn("wait", command)
            for forbidden in ("process_ref", "/proc", "pgrep", "$!", "ps -"):
                self.assertNotIn(forbidden, command)
            syntax = subprocess.run(["sh", "-n", "-c", command], capture_output=True,
                                    text=True, check=False)
            self.assertEqual(syntax.returncode, 0, syntax.stderr)
            operation._cleanup_timeout_leases()

    def test_timeout_leases_are_inside_the_native_workspace_write_boundary(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-root-unit-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            home = root / "home"
            args = SimpleNamespace(
                provider="codex:exec", mode="workspace-write", scenario="timeout-resume",
                discover_models=False, output=str(output), delegate="/bin/sh",
                runner="/bin/sh", pueue="/bin/sh", pueued="/bin/sh",
                provider_executable="/bin/sh", model=None, effort=None,
            )
            with patch.object(gate.Path, "home", return_value=home), \
                    patch.object(gate, "reject_tmp"), \
                    patch.object(gate, "require_discovery"):
                operation = gate.NativeAcceptance(args)
            try:
                self.assertEqual(
                    operation.timeout_lease_directory.parent,
                    operation.workspace,
                )
                self.assertTrue(all(
                    lease.path.is_relative_to(operation.workspace)
                    for lease in operation.timeout_leases.values()
                ))
            finally:
                self.assertTrue(operation._cleanup_timeout_leases())
                self.assertTrue(supervisor_common.rollback_owned_directories(
                    operation.owned_directories))

    def test_timeout_liveness_fifo_unavailability_is_blocked(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-lease-platform-") as directory:
            root = Path(directory)
            with patch.object(gate.sys, "platform", "freebsd"):
                with self.assertRaises(gate.BlockedFailure):
                    gate.TimeoutFifoLease(root / "unsupported.fifo", "timeout", "ready")

    def test_timeout_liveness_setup_allows_slow_queued_and_running_startup(self):
        task_id = "e" * 32
        clock = [0.0]
        statuses = []

        def status(_name, observed_task, expected, timeout):
            self.assertEqual(observed_task, task_id)
            self.assertEqual(expected, {0, 1})
            self.assertGreater(timeout, 0)
            statuses.append("queued" if len(statuses) % 2 == 0 else "running")
            return {"liveness": "running", "publication": "pending",
                    "status": statuses[-1]}

        def sleep(delay):
            clock[0] += delay

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "a" * 32
        operation.ops = SimpleNamespace(root_id=None, status=status)
        operation._capture_timeout_liveness = lambda: clock[0] >= 31.0

        operation.record_timeout_liveness(
            task_id, timeout=40.0, sleep_fn=sleep, monotonic_fn=lambda: clock[0])

        self.assertGreaterEqual(clock[0], 31.0)
        self.assertGreaterEqual(len(statuses), 6)
        self.assertEqual(operation.ops.root_id, operation.root_id)

    def test_timeout_liveness_setup_reports_terminal_delegate_state_immediately(self):
        task_id = "f" * 32
        clock = [0.0]

        def status(_name, observed_task, expected, timeout):
            self.assertEqual(observed_task, task_id)
            self.assertEqual(expected, {0, 1})
            self.assertGreater(timeout, 0)
            return {
                "liveness": "ended", "publication": "rejected", "status": "failed",
                "failure": {"code": "provider_failed", "stage": "run-provider",
                            "next_action": "stop_and_report"},
            }

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "a" * 32
        operation.ops = SimpleNamespace(root_id=None, status=status)
        operation._capture_timeout_liveness = lambda: False

        with self.assertRaisesRegex(
                gate.AcceptanceFailure,
                "ended before both liveness leases.*failure.code='provider_failed'"):
            operation.record_timeout_liveness(
                task_id, timeout=40.0, sleep_fn=lambda delay: clock.__setitem__(0, clock[0] + delay),
                monotonic_fn=lambda: clock[0])

        self.assertEqual(clock[0], 0.0)

    def test_timeout_liveness_setup_stops_on_durable_timeout_state(self):
        task_id = "1" * 32
        clock = [0.0]

        def status(_name, observed_task, expected, timeout):
            self.assertEqual(observed_task, task_id)
            self.assertEqual(expected, {0, 1})
            self.assertGreater(timeout, 0)
            return {"liveness": "undetermined", "publication": "pending",
                    "status": "timed_out"}

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "a" * 32
        operation.ops = SimpleNamespace(root_id=None, status=status)
        operation._capture_timeout_liveness = lambda: False

        with self.assertRaisesRegex(
                gate.AcceptanceFailure,
                "ended before both liveness leases.*status='timed_out'"):
            operation.record_timeout_liveness(
                task_id, timeout=40.0, sleep_fn=lambda delay: clock.__setitem__(0, clock[0] + delay),
                monotonic_fn=lambda: clock[0])

        self.assertEqual(clock[0], 0.0)

    def test_timeout_liveness_setup_uses_one_bounded_deadline(self):
        task_id = "0" * 32
        clock = [0.0]
        calls = []

        def status(_name, observed_task, expected, timeout):
            self.assertEqual(observed_task, task_id)
            self.assertEqual(expected, {0, 1})
            self.assertGreater(timeout, 0)
            self.assertLessEqual(timeout, 3.0 - clock[0])
            calls.append((clock[0], timeout))
            clock[0] += 0.4
            return {"liveness": "running", "publication": "pending"}

        def sleep(delay):
            clock[0] += delay

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "a" * 32
        operation.ops = SimpleNamespace(root_id=None, status=status)
        operation._capture_timeout_liveness = lambda: False

        with self.assertRaisesRegex(gate.AcceptanceFailure, "bounded deadline"):
            operation.record_timeout_liveness(
                task_id, timeout=3.0, sleep_fn=sleep, monotonic_fn=lambda: clock[0])

        self.assertEqual(len(calls), 1)
        self.assertLessEqual(clock[0], 3.0)

    def test_provider_status_polling_passes_remaining_timeout_to_client(self):
        root = "a" * 32
        task = "b" * 32
        clock = [0.0]
        calls = []
        running = {"label": f"delegate:{root}:{task}", "status": {"Running": {}}}
        done = {"label": f"delegate:{root}:{task}",
                "status": {"Done": {"result": "Success"}}}
        responses = iter((running, done))

        def client(_name, _operation, timeout):
            calls.append((clock[0], timeout))
            clock[0] += 0.4
            row = next(responses)
            return SimpleNamespace(json=lambda: {"tasks": {"7": row}})

        def sleep(delay):
            clock[0] += delay

        observed = provider_common.wait_runner_terminal(
            client, root, task, "Success", timeout=1.0,
            sleep_fn=sleep, monotonic_fn=lambda: clock[0])

        self.assertEqual(observed, done)
        self.assertEqual(len(calls), 2)
        self.assertGreater(calls[0][1], calls[1][1])
        self.assertLessEqual(calls[0][1], 1.0)
        self.assertLessEqual(calls[1][1], 1.0 - calls[1][0])

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
        operation.ops = provider_common.NativeTaskOps(
            operation.processes, operation.delegate, operation.runner,
            operation.pueue, state.parent, state, operation.output)
        operation.ops.expected_results = {"fresh": "Success"}
        operation.provider_executable = runner
        operation.provider_runtime_identities = {}
        operation.daemon = None
        task_directory = state / "tasks" / task
        task_directory.mkdir(parents=True)
        gate.write_json(task_directory / "meta.json", {
            "root_id": operation.root_id,
            "task_id": task,
            "provider_executable": str(runner),
            "provider_version": "fixture-v1",
            "effective_config": {
                "policy": {"runtime_sha256": gate.digest(runner)},
            },
        })

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
            inspection_command = shlex.join([str(runner), "--inspection", "--guardian", "--supervised",
                                               "--root", str(state), task])
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

    def test_terminal_queue_accepts_expected_failed_ordinary_task(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = self._queue_oracle(Path(directory))
            task = next(iter(operation.tasks.values()))
            operation.tasks = {"timeout-source": task}
            operation.ops.expected_results = {"timeout-source": "Failed"}
            status["tasks"]["7"]["status"] = {"Done": {"result": {"Failed": 1}}}
            self.assertTrue(operation.terminal_private_queue(status))

    def test_terminal_queue_rejects_unknown_inspection_rows(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = self._queue_oracle(Path(directory))
            status["tasks"]["9"] = dict(status["tasks"]["8"])
            status["tasks"]["9"]["id"] = 9
            self.assertFalse(operation.terminal_private_queue(status))

    def test_model_discovery_group_is_filtered_by_its_distinct_root(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-unit-") as directory:
            operation, status = self._queue_oracle(Path(directory), with_inspection=False)
            operation.models_root_id = "c" * 32
            group = gate.INSPECTION_GROUP_PREFIX + operation.models_root_id
            status["groups"][group] = {"status": "Running", "parallel_tasks": 1}
            status["tasks"]["8"] = {
                "id": 8, "label": group + "-" + "d" * 32, "group": group,
                "status": {"Done": {"result": "Success"}},
            }
            filtered = operation._without_model_discovery_queue(status)
            self.assertEqual(set(filtered["tasks"]), {"7"})
            self.assertEqual(set(filtered["groups"]), {"default"})

    def test_standalone_model_shutdown_rejects_terminal_row_without_journal(self):
        with tempfile.TemporaryDirectory(prefix="native-model-queue-unit-") as directory:
            root = Path(directory)
            operation, status = self._queue_oracle(root, with_inspection=False)
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
                "helper_sha256": gate.digest(operation.provider_executable),
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

    def test_inspection_queue_binds_each_turn_to_its_admitted_runtime(self):
        with tempfile.TemporaryDirectory(prefix="native-queue-runtime-drift-") as directory:
            operation, _ = self._queue_oracle(Path(directory))
            del operation._inspection_queue_records
            first = operation.tasks["fresh"]
            second = "c" * 32
            second_sha256 = "b" * 64
            operation.tasks["resume"] = second
            second_directory = operation.state / "tasks" / second
            second_directory.mkdir(parents=True)
            gate.write_json(second_directory / "meta.json", {
                "root_id": operation.root_id,
                "task_id": second,
                "provider_executable": str(operation.provider_executable),
                "provider_version": "fixture-v2",
                "effective_config": {"policy": {"runtime_sha256": second_sha256}},
            })
            records = {
                first: {"request": {"binding": {
                    "helper_executable": str(operation.provider_executable),
                    "helper_sha256": gate.digest(operation.provider_executable),
                }}},
                second: {"request": {"binding": {
                    "helper_executable": str(operation.provider_executable),
                    "helper_sha256": second_sha256,
                }}},
            }
            with patch.object(gate, "NativeTaskOps") as validator_type:
                validator_type.return_value.validate_inspection_journals.return_value = records
                self.assertEqual(set(operation._inspection_queue_records()), {first, second})
            self.assertEqual(
                operation.provider_runtime_identities[second],
                {"executable": str(operation.provider_executable),
                 "version": "fixture-v2", "sha256": second_sha256},
            )

            records[second]["request"]["binding"]["helper_sha256"] = gate.digest(
                operation.provider_executable)
            with patch.object(gate, "NativeTaskOps") as validator_type, \
                    self.assertRaisesRegex(gate.AcceptanceFailure,
                                            "does not match admitted runtime"):
                validator_type.return_value.validate_inspection_journals.return_value = records
                operation._inspection_queue_records()

    def test_runtime_identity_rejects_a_different_provider_executable(self):
        with tempfile.TemporaryDirectory(prefix="native-runtime-executable-") as directory:
            root = Path(directory)
            selected = root / "provider"
            selected.write_text("selected\n")
            foreign = root / "foreign-provider"
            foreign.write_text("foreign\n")
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider_executable = selected.resolve()
            operation.provider_runtime_identities = {}
            task_id = "a" * 32
            meta = {
                "provider_executable": str(foreign.resolve()),
                "provider_version": "fixture-v2",
                "effective_config": {"policy": {"runtime_sha256": "b" * 64}},
            }

            with self.assertRaisesRegex(gate.AcceptanceFailure,
                                        "no admitted provider runtime identity"):
                operation._record_provider_runtime_identity(task_id, meta)

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

            with patch.object(gate, "NativeTaskOps") as validator_type:
                validator = validator_type.return_value
                validator.validate_inspection_journals.return_value = {task_id: journal}
                operation._validate_model_discovery_evidence(status, model_group)

                journal["request"]["binding"]["helper_executable"] = str(foreign.resolve())
                with self.assertRaisesRegex(gate.AcceptanceFailure,
                                            "model discovery provider binding changed"):
                    operation._validate_model_discovery_evidence(status, model_group)

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
            gate.choose_output("pi:json", "/tmp/native-acceptance-output")

    def test_choose_output_accepts_only_the_exact_prepared_aggregate_case(self):
        with tempfile.TemporaryDirectory(prefix="native-prepared-output-") as directory:
            root = Path(directory) / "aggregate"
            (root / native_evidence.CASE_ROOT_NAME).mkdir(parents=True)
            definition = native_evidence.EXPECTED_CASES[0]
            prepared = native_evidence.prepare_case_directory(root, definition.case_id)

            with patch.object(gate, "reject_tmp"):
                selected = gate.choose_output(
                    definition.provider, str(prepared), definition.permission, definition.scenario)
            self.assertEqual(selected, prepared.resolve())

            with patch.object(gate, "reject_tmp"), \
                    self.assertRaisesRegex(gate.AcceptanceFailure,
                                            "prepared live acceptance output"):
                gate.choose_output(definition.provider, str(prepared),
                                   "read-only", definition.scenario)

    def test_main_accepts_prepared_case_before_the_provider_prerequisite_gate(self):
        with tempfile.TemporaryDirectory(prefix="native-prepared-main-") as directory:
            root = Path(directory) / "aggregate"
            (root / native_evidence.CASE_ROOT_NAME).mkdir(parents=True)
            definition = native_evidence.EXPECTED_CASES[2]
            prepared = native_evidence.prepare_case_directory(root, definition.case_id)

            with patch.object(gate, "reject_tmp"), \
                    patch.object(gate, "NativeAcceptance") as acceptance:
                code = gate.main([
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
            root = Path(directory) / "aggregate"
            definition = native_evidence.EXPECTED_CASES[0]
            unowned = root / native_evidence.CASE_ROOT_NAME / definition.case_id
            unowned.mkdir(parents=True)

            with patch.object(gate, "reject_tmp"), \
                    patch.object(gate, "NativeAcceptance") as acceptance:
                code = gate.main([
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

    def test_opencode_without_explicit_tool_capable_model_blocks_before_dispatch(self):
        with tempfile.TemporaryDirectory(prefix="native-opencode-model-") as directory:
            output = Path(directory) / "receipt"
            output.mkdir()
            with patch.object(gate, "choose_output", return_value=output), \
                    patch.object(gate, "NativeAcceptance") as acceptance:
                code = gate.main([
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

    def test_native_lifecycle_owner_adopts_daemon_before_started_receipt_failure(self):
        with tempfile.TemporaryDirectory(prefix="native-owner-receipt-unit-") as directory:
            root = Path(directory)
            cwd = root / "cwd"
            cwd.mkdir()

            class FakePopen:
                def __init__(self, *_args, **_kwargs):
                    self.pid = 702
                    self.returncode = None

                def poll(self):
                    return self.returncode

                def wait(self):
                    return self.returncode

            original_write_json = supervisor_common.write_json

            def fail_started(path, *args, **kwargs):
                if Path(path).name == "started.json":
                    raise OSError("injected started receipt failure")
                return original_write_json(path, *args, **kwargs)

            operation = object.__new__(gate.NativeAcceptance)
            operation.daemon = None
            processes = gate.Processes(root / "processes", {"PATH": "/usr/bin"})
            with patch.object(gate.subprocess, "Popen", FakePopen), \
                    patch.object(supervisor_common, "write_json", side_effect=fail_started):
                with self.assertRaisesRegex(OSError, "started receipt"):
                    processes.start("pueued", ["/bin/sh"], cwd,
                                    on_spawned=operation._adopt_daemon)

            self.assertIs(operation.daemon, processes.entries[0])
            self.assertEqual(operation.daemon.pid, 702)
            operation.daemon.process.returncode = 0
            operation.daemon.poll()

    def test_cleanup_stays_open_until_every_owned_process_exits_naturally(self):
        with tempfile.TemporaryDirectory(prefix="native-natural-exit-unit-") as directory:
            root = Path(directory)
            cwd = root / "cwd"
            cwd.mkdir()

            class FakePopen:
                def __init__(self, *_args, **_kwargs):
                    self.pid = 703
                    self.returncode = None

                def poll(self):
                    return self.returncode

                def wait(self):
                    return self.returncode

            operation = object.__new__(gate.NativeAcceptance)
            operation.daemon = None
            operation.closed = False
            operation.processes = gate.Processes(root / "processes", {"PATH": "/usr/bin"})
            with patch.object(gate.subprocess, "Popen", FakePopen):
                process = operation.processes.start("pueued", ["/bin/sh"], cwd)

            self.assertFalse(operation.safe_shutdown())
            self.assertFalse(operation.closed)
            process.process.returncode = 0
            self.assertTrue(operation.safe_shutdown())
            self.assertTrue(operation.closed)

    def test_constructor_rolls_back_only_the_roots_created_before_failure(self):
        with tempfile.TemporaryDirectory(prefix="native-constructor-rollback-unit-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            home = root / "home"
            live_parent = home / "delegation-layer-live"
            live_parent.mkdir(parents=True)
            sentinel = live_parent / "preexisting"
            sentinel.write_text("keep")
            args = SimpleNamespace(
                provider="pi:json", mode="read-only", scenario="read-only",
                discover_models=False, output=str(output), delegate="/bin/sh",
                runner="/bin/sh", pueue="/bin/sh", pueued="/bin/sh",
                provider_executable="/bin/sh", model=None, effort=None,
            )
            original_mkdir = gate.Path.mkdir

            def fail_workspace(path, *mkdir_args, **mkdir_kwargs):
                if path.name == "workspace":
                    raise OSError("injected constructor failure")
                return original_mkdir(path, *mkdir_args, **mkdir_kwargs)

            with patch.object(gate.Path, "home", return_value=home), \
                    patch.object(gate, "reject_tmp"), \
                    patch.object(gate, "require_discovery"), \
                    patch.object(gate.Path, "mkdir", new=fail_workspace):
                with self.assertRaisesRegex(OSError, "constructor failure"):
                    gate.NativeAcceptance(args)

            self.assertEqual(sentinel.read_text(), "keep")
            self.assertEqual(list(live_parent.iterdir()), [sentinel])

    def test_constructor_rolls_back_base_when_mkdir_fails_after_creation(self):
        with tempfile.TemporaryDirectory(prefix="native-constructor-base-rollback-unit-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            home = root / "home"
            args = SimpleNamespace(
                provider="pi:json", mode="read-only", scenario="read-only",
                discover_models=False, output=str(output), delegate="/bin/sh",
                runner="/bin/sh", pueue="/bin/sh", pueued="/bin/sh",
                provider_executable="/bin/sh", model=None, effort=None,
            )
            with patch.object(gate.Path, "home", return_value=home), \
                    patch.object(gate, "reject_tmp"), \
                    patch.object(gate, "require_discovery"), \
                    patch.object(gate.NativeAcceptance, "_initialize_runtime_roots",
                                 side_effect=OSError("injected runtime setup failure")):
                with self.assertRaisesRegex(OSError, "runtime setup failure"):
                    gate.NativeAcceptance(args)

            self.assertFalse((home / "delegation-layer-live").exists())

    def test_failure_diagnostics_do_not_skip_shutdown_or_finalization(self):
        with tempfile.TemporaryDirectory(prefix="native-finalize-call-unit-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = supervisor_common.DirectoryOwner()
            base = owner.create("acceptance root", root / "private", root)
            raw = owner.create("raw process captures", output / "processes", output)
            operation = object.__new__(gate.NativeAcceptance)
            operation.closed = False
            operation.output = output
            operation.base = base.directory
            operation.base_parent = root
            operation.owned_directories = (base, raw)
            operation.processes = SimpleNamespace(capture_root=raw)
            calls = []
            operation._write_failure_diagnostics = lambda: calls.append("diagnostics") or False
            operation.safe_shutdown = lambda: calls.append("shutdown") or False
            with patch.object(gate, "finalize_acceptance_artifacts", return_value=False) as finalize:
                self.assertFalse(operation.finalize(False))
            self.assertEqual(calls, ["diagnostics", "shutdown"])
            finalize.assert_called_once_with(
                operation.output, False, (base, raw), False,
                remove_failed_process_captures=False,
                raw_process_captures_token=raw,
            )

    def test_cleanup_removes_closed_owned_base_and_preserves_evidence(self):
        with tempfile.TemporaryDirectory(prefix="native-cleanup-unit-") as directory:
            root = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.base_parent = root / "delegation-layer-live"
            operation.base = operation.base_parent / "run"
            operation.output = root / "evidence"
            operation.output.mkdir()
            owner = supervisor_common.DirectoryOwner()
            base_parent = owner.create("acceptance base parent", operation.base_parent, root)
            base_token = owner.create("acceptance base", operation.base, operation.base_parent)
            raw_token = owner.create("raw process captures", operation.output / "processes", operation.output)
            operation.owned_directories = (base_parent, base_token, raw_token)
            operation.processes = SimpleNamespace(capture_root=raw_token)
            (operation.base / "state").mkdir()
            (operation.base / "state" / "record").write_text("disposable")
            operation.closed = True

            self.assertTrue(operation.finalize(True))

            self.assertFalse(operation.base.exists())
            self.assertFalse(operation.base_parent.exists())
            receipt = gate.read_json(operation.output / "cleanup.json")
            self.assertEqual(receipt["status"], "removed")
            self.assertTrue(receipt["supervisor_shutdown_observed"])

    def test_cleanup_retains_unclosed_owned_base(self):
        with tempfile.TemporaryDirectory(prefix="native-cleanup-unit-") as directory:
            root = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.base_parent = root / "delegation-layer-live"
            operation.base = operation.base_parent / "run"
            operation.output = root / "evidence"
            operation.output.mkdir()
            owner = supervisor_common.DirectoryOwner()
            base_parent = owner.create("acceptance base parent", operation.base_parent, root)
            base_token = owner.create("acceptance base", operation.base, operation.base_parent)
            raw_token = owner.create("raw process captures", operation.output / "processes", operation.output)
            operation.owned_directories = (base_parent, base_token, raw_token)
            operation.processes = SimpleNamespace(capture_root=raw_token)
            operation.closed = False

            self.assertFalse(operation.finalize(False))

            self.assertTrue(operation.base.is_dir())
            receipt = gate.read_json(operation.output / "cleanup.json")
            self.assertEqual(receipt["status"], "retained")
            self.assertFalse(receipt["supervisor_shutdown_observed"])

    @staticmethod
    def _failure_operation(root: Path) -> tuple[object, Path, str]:
        operation = object.__new__(gate.NativeAcceptance)
        operation.base_parent = root / "delegation-layer-live"
        operation.base = operation.base_parent / "run"
        operation.output = root / "evidence"
        operation.state = operation.base / "state"
        operation.workspace = operation.base / "workspace"
        operation.models_state = operation.base / "models-state"
        operation.output.mkdir()
        owner = supervisor_common.DirectoryOwner()
        base_parent_token = owner.create("acceptance base parent", operation.base_parent, root)
        base_token = owner.create("acceptance base", operation.base, operation.base_parent)
        state_token = owner.create("acceptance state", operation.state, operation.base)
        workspace_token = owner.create("acceptance workspace", operation.workspace, operation.base)
        models_token = owner.create("model discovery state", operation.models_state, operation.base)
        raw_token = owner.create("raw process captures", operation.output / "processes", operation.output)
        operation.owned_directories = owner.transfer()
        operation.processes = SimpleNamespace(capture_root=raw_token)
        (operation.state / "tasks").mkdir()
        operation.closed = True
        operation.provider = "pi:json"
        operation.mode = "workspace-write"
        operation.scenario = "write"
        operation.root_id = "a" * 32
        task_id = "b" * 32
        operation.tasks = {"fresh": task_id}
        operation.numeric_ids = {task_id: 7}
        operation.model_discovery_task_id = None
        task = operation.state / "tasks" / task_id
        task.mkdir()
        gate.write_json(operation.output / "failure.json", {
            "gate": "acceptance-native", "status": "failed", "provider": operation.provider,
            "mode": operation.mode, "scenario": operation.scenario, "task_budget": "5m",
            "requested_model": None, "requested_effort": None, "discover_models": False,
            "no_retry": True, "credentials_in_receipt": False,
            "error": "write task did not leave the exact edited target",
            "traceback": "RAW TRACEBACK MUST NOT APPEAR",
        })
        gate.write_json(task / "outcome.json", {
            "verdict": "committed", "predicate": {"adapter": "pi:json", "mode": operation.mode,
                                                        "version": "v1", "sha256": "c" * 64},
            "payload": {"basename": "result.txt", "length": 18, "sha256": "d" * 64},
            "evidence_sha256": "e" * 64,
        })
        gate.write_json(task / "provider.exit", {
            "invocation_state": "started", "exit_code": 0,
            "error": "RAW PROVIDER OUTPUT MUST NOT APPEAR",
            "raw_manifest": [{"path": "raw/stdout", "bytes": 44, "sha256": "f" * 64}],
            "manifest_sha256": "1" * 64,
        })
        (task / "result.txt").write_text("RAW RESULT TEXT MUST NOT APPEAR")
        (operation.workspace / "acceptance-write-target.txt").write_text("edited:fixture")
        (operation.output / "processes" / "001-provider").mkdir()
        (operation.output / "processes" / "001-provider" / "stdout").write_text(
            "RAW PROCESS CAPTURE MUST NOT APPEAR")
        return operation, task, task_id

    def test_failure_snapshot_is_compact_and_removes_closed_state_and_captures(self):
        with tempfile.TemporaryDirectory(prefix="native-diagnostic-unit-") as directory:
            operation, _task, task_id = self._failure_operation(Path(directory))

            self.assertTrue(operation.finalize(False))

            snapshot = operation.output / "failure-diagnostics.json"
            data = snapshot.read_bytes()
            self.assertLessEqual(len(data), gate.FAILURE_DIAGNOSTICS_BYTES)
            value = json.loads(data)
            self.assertEqual(value["provider"], "pi:json")
            self.assertEqual(value["mode"], "workspace-write")
            self.assertEqual(value["scenario"], "write")
            self.assertEqual(value["task_ids"], [task_id])
            self.assertEqual(value["status"]["partial"], False)
            self.assertNotIn("RAW", data.decode())
            self.assertFalse(operation.base.exists())
            self.assertFalse((operation.output / "processes").exists())

            self.assertFalse(operation.finalize(False))
            self.assertEqual(snapshot.read_bytes(), data)

    def test_failure_snapshot_preserves_sanitized_rejection_and_payload_descriptor(self):
        with tempfile.TemporaryDirectory(prefix="native-diagnostic-rejected-unit-") as directory:
            operation, task, task_id = self._failure_operation(Path(directory))
            refusal = b"UNRESTRICTED_REFUSAL_PAYLOAD"
            gate.write_json(task / "outcome.json", {
                "verdict": "rejected",
                "payload": {
                    "basename": "publish.reject", "length": len(refusal),
                    "sha256": hashlib.sha256(refusal).hexdigest(),
                },
                "evidence_sha256": "e" * 64,
                "diagnostics": [{
                    "code": "incomplete_output", "field": "result.text",
                    "message": "provider output did not contain a complete answer",
                    "raw": "MUST NOT APPEAR",
                }],
            }, replace=True)
            (task / "publish.reject").write_bytes(refusal)

            self.assertTrue(operation.finalize(False))

            value = gate.read_json(operation.output / "failure-diagnostics.json")
            diagnostic_task = next(item for item in value["tasks"] if item["task_id"] == task_id)
            self.assertEqual(diagnostic_task["outcome"]["diagnostics"], [{
                "code": "incomplete_output", "field": "result.text",
                "message": "provider output did not contain a complete answer",
            }])
            descriptor = diagnostic_task["files"]["publish.reject"]
            self.assertEqual(descriptor["bytes"], len(refusal))
            self.assertEqual(descriptor["sha256"], hashlib.sha256(refusal).hexdigest())
            snapshot = (operation.output / "failure-diagnostics.json").read_text()
            self.assertNotIn("UNRESTRICTED_REFUSAL_PAYLOAD", snapshot)
            self.assertNotIn("MUST NOT APPEAR", snapshot)
            self.assertFalse(operation.base.exists())

    def test_failure_snapshot_is_bounded_and_marks_truncation(self):
        with tempfile.TemporaryDirectory(prefix="native-diagnostic-bound-unit-") as directory:
            operation, _task, _task_id = self._failure_operation(Path(directory))
            gate.write_json(_task / "provider.exit", {
                "invocation_state": "started", "exit_code": 1,
                "raw_manifest": [
                    {"path": "raw/" + ("m" * 220), "bytes": index, "sha256": "f" * 64}
                    for index in range(3000)
                ],
            }, replace=True)
            for index in range(1500):
                name = f"{index:04d}-" + ("x" * 230)
                (operation.workspace / name).write_bytes(b"workspace metadata only")

            self.assertTrue(operation.finalize(False))

            snapshot = operation.output / "failure-diagnostics.json"
            self.assertLessEqual(snapshot.stat().st_size, gate.FAILURE_DIAGNOSTICS_BYTES)
            value = gate.read_json(snapshot)
            self.assertTrue(value["status"]["partial"])
            self.assertTrue(value["status"]["truncated"])
            self.assertFalse(operation.base.exists())
            self.assertFalse((operation.output / "processes").exists())

    def test_failure_snapshot_error_keeps_raw_captures_and_still_cleans_closed_state(self):
        with tempfile.TemporaryDirectory(prefix="native-diagnostic-failure-unit-") as directory:
            operation, _task, _task_id = self._failure_operation(Path(directory))
            with patch.object(operation, "_failure_diagnostics",
                              side_effect=OSError("injected snapshot failure")):
                self.assertTrue(operation.finalize(False))

            self.assertFalse(operation.base.exists())
            self.assertTrue((operation.output / "processes").is_dir())
            self.assertFalse((operation.output / "failure-diagnostics.json").exists())
            receipt = gate.read_json(operation.output / "cleanup.json")
            self.assertEqual(receipt["raw_process_captures"], "retained")

    def test_main_finalizes_the_owned_acceptance_root_before_passing(self):
        with tempfile.TemporaryDirectory(prefix="native-main-cleanup-unit-") as directory:
            output = Path(directory) / "evidence"
            output.mkdir()
            calls = []

            class Run:
                def run(self):
                    calls.append("run")
                    return 0

                def finalize(self, success):
                    calls.append(("finalize", success))
                    return True

                def publish_success(self):
                    calls.append("publish")

            with patch.object(gate, "choose_output", return_value=output), \
                    patch.object(gate, "NativeAcceptance", return_value=Run()):
                code = gate.main([
                    "--provider", "pi:json", "--permission", "read-only",
                    "--output", str(output),
                ])

            self.assertEqual(code, 0)
            self.assertEqual(calls, ["run", ("finalize", True), "publish"])

    def test_main_treats_cleanup_failure_as_fatal(self):
        with tempfile.TemporaryDirectory(prefix="native-main-cleanup-failure-unit-") as directory:
            output = Path(directory) / "evidence"
            output.mkdir()

            class Run:
                def run(self):
                    return 0

                def finalize(self, _success):
                    return False

                def publish_success(self):
                    raise AssertionError("success was published before cleanup")

            with patch.object(gate, "choose_output", return_value=output), \
                    patch.object(gate, "NativeAcceptance", return_value=Run()):
                self.assertEqual(gate.main([
                    "--provider", "pi:json", "--permission", "read-only",
                    "--output", str(output),
                ]), 1)
            self.assertFalse((output / "success.json").exists())

    def test_model_discovery_main_persists_sanitized_failure_without_success(self):
        for exit_code, status in ((1, "failed"), (2, "BLOCKED")):
            with self.subTest(exit_code=exit_code), tempfile.TemporaryDirectory(
                    prefix="native-model-main-failure-") as directory:
                output = Path(directory) / "evidence"
                output.mkdir()
                calls = []

                class Run:
                    model_discovery_summary = {
                        "status": "blocked" if exit_code == 2 else "failed",
                        "reason_code": "authentication_unavailable" if exit_code == 2 else "provider_error",
                        "complete": False, "model_count": 0, "effort_count": None,
                        "raw_error": "must not be copied",
                    }

                    def run(self):
                        return exit_code

                    def finalize(self, success):
                        calls.append(("finalize", success))
                        return True

                    def publish_success(self):
                        raise AssertionError("model discovery failure published success")

                with patch.object(gate, "choose_output", return_value=output), \
                        patch.object(gate, "NativeAcceptance", return_value=Run()):
                    code = gate.main([
                        "--provider", "codex:exec", "--permission", "read-only",
                        "--discover-models", "--output", str(output),
                    ])

                self.assertEqual(code, exit_code)
                self.assertEqual(calls, [("finalize", False)])
                self.assertFalse((output / "success.json").exists())
                receipt = gate.read_json(output / "failure.json")
                self.assertEqual(receipt["status"], status)
                self.assertEqual(receipt["discovery_exit_code"], exit_code)
                self.assertEqual(receipt["model_discovery"]["reason_code"],
                                 Run.model_discovery_summary["reason_code"])
                self.assertNotIn("raw_error", receipt["model_discovery"])

    def test_cleanup_preserves_preexisting_raw_process_captures(self):
        with tempfile.TemporaryDirectory(prefix="native-preexisting-captures-unit-") as directory:
            root = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.base_parent = root / "delegation-layer-live"
            operation.base = operation.base_parent / "run"
            operation.output = root / "evidence"
            operation.base_parent.mkdir()
            operation.output.mkdir()
            owner = supervisor_common.DirectoryOwner()
            base_token = owner.create("acceptance base", operation.base, operation.base_parent)
            captures = operation.output / "processes"
            captures.mkdir()
            sentinel = captures / "keep.txt"
            sentinel.write_text("pre-existing capture")
            operation.owned_directories = (base_token,)
            operation.processes = SimpleNamespace(capture_root=None)
            operation.closed = True

            self.assertFalse(operation.finalize(True))

            self.assertTrue(sentinel.is_file())
            self.assertEqual(sentinel.read_text(), "pre-existing capture")
            self.assertFalse((operation.output / "cleanup.json").exists())

    def test_model_discovery_failure_cleanup_tracks_its_inspection_root(self):
        with tempfile.TemporaryDirectory(prefix="native-model-cleanup-unit-") as directory:
            root = Path(directory)
            models_state = root / "models-state"
            models_state.mkdir()
            gate.write_json(models_state / "root.json", {"root_id": "c" * 32})
            task_id = "d" * 32
            (models_state / "inspections" / task_id).mkdir(parents=True)
            operation = object.__new__(gate.NativeAcceptance)
            operation.processes = SimpleNamespace()
            operation.delegate = root / "delegate"
            operation.runner = root / "runner"
            operation.pueue = root / "pueue"
            operation.models_state = models_state
            operation.output = root / "output"
            operation.model_discovery_task_id = None
            operation.models_root_id = None
            cleanup = gate.ModelDiscoveryCleanupOps(operation)
            status = {"tasks": {}, "groups": {}}
            with patch.object(gate.NativeTaskOps, "failure_queue_finished", return_value=True):
                self.assertTrue(cleanup.failure_queue_finished(status))
            self.assertEqual(cleanup.root_id, "c" * 32)
            self.assertEqual(cleanup.dispatch_attempts, {task_id})
            self.assertEqual(cleanup.inspection_attempts, {task_id})

    def test_model_discovery_failure_cleanup_uses_model_owner(self):
        with tempfile.TemporaryDirectory(prefix="native-model-owner-unit-") as directory:
            root = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.closed = False
            operation.discover_models_only = True
            operation.root_id = None
            operation.config = root / "pueue.yml"
            operation.daemon = object()
            operation.models_state = root / "models-state"
            operation.output = root / "output"
            operation.delegate = root / "delegate"
            operation.runner = root / "runner"
            operation.pueue = root / "pueue"
            operation.processes = SimpleNamespace()
            operation.tasks = {}
            operation.numeric_ids = {}

            def unexpected_owner(*_args):
                raise AssertionError("ordinary cleanup owner was selected")

            operation.ops = SimpleNamespace(
                root_id=None, tasks={}, labels={}, numbers={}, dispatch_attempts=set(),
                bind_supervisor=unexpected_owner, retain_failure_ownership=unexpected_owner,
            )
            with patch.object(gate.ModelDiscoveryCleanupOps, "retain_failure_ownership",
                              return_value=True) as retain:
                self.assertTrue(operation.safe_shutdown())
            retain.assert_called_once()
            self.assertTrue(operation.closed)

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
            operation.provider_version = "pi-runtime-v1"
            first_task = "a" * 32
            first_identity = {
                "executable": str(files["provider"]),
                "version": operation.provider_version,
                "sha256": "a" * 64,
            }
            operation.provider_runtime_identities = {first_task: first_identity}
            operation.delegate = files["delegate"]
            operation.runner = files["runner"]
            operation.pueue = files["pueue"]
            operation.pueued = files["pueued"]
            operation.pueue_version = "pueue 4.0.4"
            operation.pueued_version = "pueued 4.0.4"
            operation.write_binding(first_task, first_identity)
            receipt = gate.read_json(output / "binding.json")
            self.assertEqual(receipt["task_budget"], gate.TASK_BUDGET)
            self.assertEqual(receipt["canonical_task_budget"], gate.CANONICAL_TASK_BUDGET)
            self.assertIsNone(receipt["requested_model"])
            self.assertIsNone(receipt["requested_effort"])
            self.assertIsNone(receipt["model_discovery"])
            self.assertIsNone(receipt["native_mode"])
            self.assertEqual(receipt["provider_identity_scope"], "first-admitted-task")
            self.assertEqual(receipt["provider_task_id"], first_task)
            self.assertEqual(receipt["provider_sha256"], first_identity["sha256"])
            original = (output / "binding.json").read_bytes()

            second_task = "b" * 32
            second_identity = {
                "executable": str(files["provider"]),
                "version": "pi-runtime-v2",
                "sha256": "b" * 64,
            }
            operation.provider_runtime_identities[second_task] = second_identity
            operation.tasks = {"fresh": first_task, "resume": second_task}
            runtime_receipt = operation.provider_runtime_receipt()
            self.assertEqual(runtime_receipt["provider_versions"], {
                first_task: "pi-runtime-v1", second_task: "pi-runtime-v2",
            })
            self.assertEqual(runtime_receipt["provider_runtime_identities"], {
                first_task: first_identity, second_task: second_identity,
            })
            with self.assertRaises(gate.AcceptanceFailure):
                operation.write_binding(second_task, second_identity)
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

    def test_antigravity_read_only_brief_requires_file_read_tool(self):
        with tempfile.TemporaryDirectory(prefix="native-read-brief-unit-") as directory:
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "antigravity:print"
            operation.mode = "read-only"
            operation.briefs = Path(directory)
            operation.workspace_marker = Path(directory) / "acceptance-marker.txt"
            path = operation.brief("fresh")
            text = path.read_text()
            self.assertIn("provider's file-read tool only", text)
            self.assertIn("Do not use a command or shell tool", text)

    def test_pi_runtime_entries_are_limited_to_workspace_write(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.provider = "pi:json"
        operation.mode = "workspace-write"
        self.assertTrue(operation._is_pi_runtime_entry(".pi"))
        self.assertTrue(operation._is_pi_runtime_entry(".pi/tasks/session/file"))
        operation.mode = "read-only"
        self.assertFalse(operation._is_pi_runtime_entry(".pi/tasks/session/file"))
        operation.provider = "antigravity:print"
        operation.mode = "workspace-write"
        self.assertFalse(operation._is_pi_runtime_entry(".pi/tasks/session"))

    def test_pi_runtime_roots_must_be_directories(self):
        with tempfile.TemporaryDirectory(prefix="native-pi-runtime-unit-") as directory:
            workspace = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "pi:json"
            operation.mode = "workspace-write"
            operation.workspace = workspace
            operation._validate_pi_runtime_roots()
            (workspace / ".pi").write_text("invalid runtime root")
            with self.assertRaisesRegex(gate.AcceptanceFailure, "not a directory"):
                operation._validate_pi_runtime_roots()

            (workspace / ".pi").unlink()
            (workspace / ".pi").mkdir()
            (workspace / ".pi" / "tasks").write_text("invalid task root")
            with self.assertRaisesRegex(gate.AcceptanceFailure, "not a directory"):
                operation._validate_pi_runtime_roots()

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
        expected = {"antigravity:print", "ccr:print", "claude:print", "codex:exec", "codex:review", "pi:json", "opencode:run"}
        self.assertEqual(set(gate.PROFILES), expected)
        self.assertEqual(gate.PROFILES["antigravity:print"]["default_mode"], "workspace-write")
        for profile in gate.PROFILES.values():
            if profile is not gate.PROFILES["codex:review"]:
                self.assertIn("workspace-write", profile["modes"])
        self.assertEqual(gate.PROFILES["claude:print"]["default_mode"], "read-only")
        self.assertEqual(gate.PROFILES["codex:exec"]["default_mode"], "read-only")
        self.assertEqual(gate.PROFILES["codex:review"]["modes"], ("read-only",))
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
        discovery_args = gate.build_parser().parse_args([
            "--provider", "antigravity:print", "--permission", "read-only",
            "--scenario", "read-only", "--discover-models",
        ])
        self.assertTrue(discovery_args.discover_models)
        with self.assertRaises(SystemExit):
            gate.build_parser().parse_args([
                "--provider", "pi:json", "--permission", "workspace-write", "--scenario", "timeout",
            ])

    def test_public_option_docs_cover_ccr_required_model_and_effort(self):
        repository = Path(__file__).parents[1]
        cli_reference = (repository / "docs/cli-reference.md").read_text(encoding="utf-8")
        troubleshooting = (repository / "docs/troubleshooting.md").read_text(encoding="utf-8")
        self.assertIn("Codex, CCR, Claude, AGY, Pi, or OpenCode", cli_reference)
        self.assertIn("`ccr:print` requires a configured CCR alias", cli_reference)
        self.assertIn("CCR, Claude, and AGY pass their native effort flag", cli_reference)
        self.assertIn("Codex, CCR, and Claude support both modes plus `model` and `effort`", troubleshooting)
        self.assertIn("`ccr:print`\nrequires `--model` to be a configured CCR alias", troubleshooting)
        self.assertIn("CCR, Claude, and AGY pass native effort", troubleshooting)

    def test_aggregate_wrapper_runs_write_and_read_profiles(self):
        wrapper = Path(__file__).with_name("acceptance_native_all.sh").read_text()
        for provider in ("antigravity:print", "claude:print", "ccr:print", "codex:exec", "codex:review", "pi:json", "opencode:run"):
            self.assertIn(provider, wrapper)
        self.assertIn('--permission "$case_permission"', wrapper)
        self.assertIn('--scenario "$case_scenario"', wrapper)
        self.assertIn('if [ "$case_provider" = opencode:run ] && [ -n "${OPENCODE_LIVE_MODEL:-}" ]; then', wrapper)
        self.assertIn('set -- "$@" --model "$OPENCODE_LIVE_MODEL"', wrapper)
        individual = Path(__file__).with_name("acceptance_native.sh").read_text()
        self.assertIn('if [ "$provider" = opencode:run ] && [ -n "${OPENCODE_LIVE_MODEL:-}" ]; then', individual)
        self.assertIn('set -- --model "$OPENCODE_LIVE_MODEL" "$@"', individual)
        self.assertIn('run_case "$provider" workspace-write write', wrapper)
        self.assertIn('run_case "$provider" read-only read-only', wrapper)
        self.assertIn('run_case codex:exec workspace-write timeout-resume', wrapper)
        self.assertNotIn('timeout-process.json', wrapper)
        self.assertIn('run_case codex:review read-only read-only', wrapper)
        self.assertIn('for provider in antigravity:print claude:print ccr:print codex:exec pi:json opencode:run; do', wrapper)
        self.assertIn("2) # User-authorized authentication/prerequisite blocks remain neutral.", wrapper)
        self.assertIn("passed=%s blocked=%s failed=%s", wrapper)
        self.assertIn("aggregate_status=BLOCKED", wrapper)

    def test_aggregate_reports_counts_and_neutral_all_blocked_status(self):
        wrapper = Path(__file__).with_name("acceptance_native_all.sh")
        with tempfile.TemporaryDirectory(prefix="native-aggregate-unit-") as directory:
            fake_python = Path(directory) / "python3"
            fake_python.write_text(fake_native_python_script())
            fake_python.chmod(0o700)
            environment = os.environ.copy()
            environment["PATH"] = str(fake_python.parent) + os.pathsep + environment["PATH"]
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(Path(directory) / "aggregate")
            environment["REAL_PYTHON"] = sys.executable
            environment["FAKE_NATIVE_RESULT"] = "blocked"
            environment.pop("DELEGATION_REQUIRE_LIVE_ACCEPTANCE", None)
            blocked = subprocess.run(
                ["sh", str(wrapper)], cwd=Path(__file__).parents[1], env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT)
            self.assertEqual(blocked.returncode, 0)
            self.assertIn("status=BLOCKED passed=0 blocked=14 failed=0", blocked.stdout)
            self.assertIn("auth_blocks_neutral=true", blocked.stdout)

            environment["DELEGATION_REQUIRE_LIVE_ACCEPTANCE"] = "1"
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(Path(directory) / "aggregate-required")
            required = subprocess.run(
                ["sh", str(wrapper)], cwd=Path(__file__).parents[1], env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT)
            self.assertEqual(required.returncode, 2)
            self.assertIn("status=BLOCKED passed=0 blocked=14 failed=0", required.stdout)
            environment.pop("DELEGATION_REQUIRE_LIVE_ACCEPTANCE")

            fake_python.write_text(fake_native_python_script())
            environment["FAKE_NATIVE_RESULT"] = "mixed"
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(Path(directory) / "aggregate-mixed")
            mixed = subprocess.run(
                ["sh", str(wrapper)], cwd=Path(__file__).parents[1], env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT)
            self.assertEqual(mixed.returncode, 1)
            self.assertIn("status=failed passed=2 blocked=9 failed=3", mixed.stdout)

            fake_python.write_text(fake_native_python_script())
            environment["FAKE_NATIVE_RESULT"] = "timeout"
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(Path(directory) / "aggregate-timeout")
            successful_timeout = subprocess.run(
                ["sh", str(wrapper)], cwd=Path(__file__).parents[1], env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT)
            self.assertEqual(successful_timeout.returncode, 0)
            self.assertIn("status=passed passed=3 blocked=11 failed=0", successful_timeout.stdout)

    def test_aggregate_uses_each_case_exit_contract_without_a_second_receipt_oracle(self):
        wrapper = Path(__file__).with_name("acceptance_native_all.sh")
        with tempfile.TemporaryDirectory(prefix="native-aggregate-timeout-unit-") as directory:
            fake_python = Path(directory) / "python3"
            fake_python.write_text(fake_native_python_script())
            fake_python.chmod(0o700)
            environment = os.environ.copy()
            environment["PATH"] = str(fake_python.parent) + os.pathsep + environment["PATH"]
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(Path(directory) / "aggregate")
            environment["REAL_PYTHON"] = sys.executable
            environment["FAKE_NATIVE_RESULT"] = "only-timeout"

            result = subprocess.run(
                ["sh", str(wrapper)], cwd=Path(__file__).parents[1], env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT)

            self.assertEqual(result.returncode, 0)
            self.assertIn("status=passed passed=1 blocked=13 failed=0", result.stdout)

    def test_aggregate_rejects_unexpected_evidence_helper_exit(self):
        wrapper = Path(__file__).with_name("acceptance_native_all.sh")
        with tempfile.TemporaryDirectory(prefix="native-aggregate-helper-exit-unit-") as directory:
            fake_python = Path(directory) / "python3"
            fake_python.write_text(fake_native_python_script())
            fake_python.chmod(0o700)
            environment = os.environ.copy()
            environment["PATH"] = str(fake_python.parent) + os.pathsep + environment["PATH"]
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(Path(directory) / "aggregate")
            environment["REAL_PYTHON"] = sys.executable
            environment["FAKE_NATIVE_RESULT"] = "blocked"
            environment["FAKE_HELPER_EXIT"] = "7"
            result = subprocess.run(
                ["sh", str(wrapper)], cwd=Path(__file__).parents[1], env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT,
            )
            self.assertEqual(result.returncode, 1)

    def test_normal_setup_does_not_probe_provider_version_directly(self):
        source = Path(__file__).with_name("acceptance_native.py").read_text()
        self.assertNotIn('[self.provider_executable, "--version"]', source)


if __name__ == "__main__":
    unittest.main()
