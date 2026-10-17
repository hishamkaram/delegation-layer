"""Implementation methods for PrivateDefaultStopScenarioTests; collected once by the acceptance_tests discovery."""

from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.delegate_acceptance.private_cli import stop as stop_impl
from scripts.delegate_acceptance.private_cli import admission as admission_impl
from scripts.acceptance_tests.private_cli.fixtures import (
    set_up_private_capture_root,
)


class PrivateDefaultStopScenarioTests(unittest.TestCase):
    def setUp(self):
        super().setUp()
        set_up_private_capture_root(self)

    def test_orchestration_requires_live_leases_and_canonical_closure(self):
        with tempfile.TemporaryDirectory(prefix="delegate-default-stop-") as directory:
            root = Path(directory)
            state = root / "state"
            workspace = root / "workspace"
            workspace.mkdir()
            brief = root / "brief.md"
            brief.write_text("bounded stop fixture\n")
            gate_path = root / "gate"
            waiters = root / "waiters"
            waiters.mkdir()
            responses = root / "responses"
            responses.mkdir()
            provider = root / "pi"
            provider.write_text("#!/bin/sh\n")
            provider.chmod(0o700)
            receipt = private_cli.AcceptanceReceipt(
                root / "receipt.json", private_cli.DEFAULT_STOP_TASK_COUNT, 2,
                {"delegate": provider, "runner": provider,
                 "pueue": provider, "pueued": provider},
            )
            attempted = []
            calls = []
            lease_cleanup_calls = []
            dispatches = []
            status_reads = {}
            lifecycle = []
            user_request_id = "u" * 32

            gate, run_json, live_lease, closure, shutdown, shutdown_private = _stop_scenario_callbacks(calls, dispatches, gate_path, lease_cleanup_calls, lifecycle, root, self, status_reads, user_request_id)
            with patch.object(private_cli.LockedReleaseGate, "acquire", return_value=gate), \
                    patch.object(stop_impl, "run_json", side_effect=run_json), \
                    patch.object(admission_impl, "run_json", side_effect=run_json), \
                    patch.object(private_cli.time, "sleep"), \
                    patch.object(stop_impl, "default_stop_live_lease", side_effect=live_lease), \
                    patch.object(stop_impl, "await_managed_lifetime_closure", side_effect=closure), \
                    patch.object(stop_impl, "shutdown_private_supervisor", side_effect=shutdown_private):
                evidence = private_cli.run_default_stop_acceptance(
                    provider, provider, state, workspace, brief, gate_path, waiters,
                    root / "pueue.yml", {}, responses, provider, "e" * 32,
                    receipt, attempted, capture_root=self.capture_root,
                )

            _assert_stop_expectations(attempted, calls, dispatches, evidence, lease_cleanup_calls, lifecycle, receipt, self, shutdown)


def _stop_scenario_callbacks(calls, dispatches, gate_path, lease_cleanup_calls, lifecycle, root, self, status_reads, user_request_id):
    class Gate:
        def __init__(self, path):
            self.path = path
            self.released = False

        def release(self):
            self.released = True
            self.path.touch()

    gate = Gate(gate_path)

    class Cohort:
        def __enter__(self):
            lifecycle.append("enter")
            return self

        def __exit__(self, _error_type, _error, _traceback):
            lifecycle.append("exit")
            return False

        def remove_closed_fifos(self):
            lifecycle.append("remove")
            lease_cleanup_calls.append(True)
            return [{"state": "removed"}]

    run_json = _stop_command_callback(calls, dispatches, lifecycle, self, status_reads, user_request_id)
    def live_lease(_client, _config, _env, _state, response, _waiters, _nonce,
                  **_options):
        return ({
            "task_id": response["task_id"], "root_id": response["root_id"],
            "pid": 100 + len(dispatches), "scope_kind": "process-group",
            "assurance": "cooperative", "owner_sha256": "a" * 64,
            "registration_sha256": "b" * 64,
            "scope_plan_sha256": "c" * 64, "scope_id_sha256": "d" * 64,
            "path": root / (response["task_id"] + ".fifo"),
        }, Cohort(), {"observed_at_unix_ns": 1, "acquire_elapsed_ns": 1})

    def closure(_state, task_ids, timeout=30):
        lifecycle.append("closure")
        task_id = tuple(task_ids)[0]
        cause = "requested" if task_id == dispatches[0][0] else "budget"
        return {"tasks": {task_id: {
            "intent": {"deadline": "2026-10-14T00:00:01Z",
                        "created_at": "2026-10-14T00:00:00Z"},
            "registration": {"scope_kind": "process-group", "assurance": "cooperative"},
            "worker_wait": {"child_pid": 101, "result": "exit-error",
                             "exit_code": 143, "observed_at": "2026-10-14T00:00:01Z"},
            "launches": [{"pid": 101}],
            "termination": {"cause": cause, "scope_empty": True,
                            "observed_at": "2026-10-14T00:00:02Z"},
        }}}

    shutdown = []

    def shutdown_private(*_args, **_kwargs):
        self.assertTrue(gate.released, "supervisor shutdown preceded fixture gate release")
        shutdown.append(True)
    return gate, run_json, live_lease, closure, shutdown, shutdown_private


def _stop_command_callback(calls, dispatches, lifecycle, self, status_reads, user_request_id):
    def run_json(_delegate, _env, args, **_options):
        command = args[2]
        calls.append(args)
        if command == "dispatch":
            task_id = args[args.index("--id") + 1]
            dispatches.append((task_id, args[args.index("--budget") + 1]))
            return {
                "root_id": "r" * 32, "task_id": task_id,
                "task_record": "created", "admission": "admitted",
            }
        if command == "cancel":
            lifecycle.append("cancel")
            self.assertEqual(args[3], dispatches[0][0])
            return {
                "root_id": "r" * 32, "task_id": args[3],
                "stop": {
                    "request_id": user_request_id, "cause": "user",
                    "requested": True, "attempted": True, "in_flight": True,
                    "terminated": False,
                },
            }
        self.assertEqual(command, "status")
        lifecycle.append("status")
        task_id = args[3]
        status_reads[task_id] = status_reads.get(task_id, 0) + 1
        ended = status_reads[task_id] > 1
        if task_id == dispatches[0][0]:
            stop = {
                "request_id": user_request_id, "cause": "user",
                "requested": True, "terminated": ended,
                "observed_state": "ended" if ended else "running",
            }
        else:
            self.assertEqual(task_id, dispatches[1][0])
            stop = {
                "request_id": "budget", "cause": "budget",
                "requested": True, "terminated": ended,
                "observed_state": "ended" if ended else "running",
            }
        return {
            "root_id": "r" * 32, "task_id": task_id,
            "liveness": "ended" if ended else "running", "stops": [stop],
        }
    return run_json


def _assert_stop_expectations(attempted, calls, dispatches, evidence, lease_cleanup_calls, lifecycle, receipt, self, shutdown):
    self.assertEqual([command[2] for command in calls], [
        "dispatch", "cancel", "status", "status", "dispatch", "status", "status",
    ])
    self.assertEqual(
        [budget for _, budget in dispatches],
        [private_cli.DEFAULT_STOP_TASK_BUDGET, private_cli.DEFAULT_STOP_BUDGET],
    )
    self.assertEqual(attempted, [dispatches[0][0], dispatches[1][0]])
    self.assertEqual(evidence["scenario"], private_cli.DEFAULT_STOP_SCENARIO)
    self.assertEqual(set(evidence["tasks"]), {"user_stop", "budget_stop"})
    self.assertEqual(lease_cleanup_calls, [True, True])
    self.assertEqual(lifecycle, [
        "enter", "cancel", "closure", "status", "status", "remove", "exit",
        "enter", "closure", "status", "status", "remove", "exit",
    ])
    self.assertEqual(
        evidence["tasks"]["user_stop"]["status"]["queue_ended_observation"],
        {
            "kind": "queue-ended-observation",
            "final_receipt": "status-default-stop-user-001.json",
            "poll_receipts": [
                "status-default-stop-user-000.json",
                "status-default-stop-user-001.json",
            ],
            "poll_count": 2,
        },
    )
    self.assertEqual(
        evidence["tasks"]["budget_stop"]["status"]["queue_ended_observation"],
        {
            "kind": "queue-ended-observation",
            "final_receipt": "status-default-stop-budget-001.json",
            "poll_receipts": [
                "status-default-stop-budget-000.json",
                "status-default-stop-budget-001.json",
            ],
            "poll_count": 2,
        },
    )
    self.assertEqual(
        evidence["tasks"]["user_stop"]["fixture_lease_cleanup"],
        [{"state": "removed"}],
    )
    self.assertFalse(evidence["stop_poll_policy"]["override"])
    self.assertEqual(evidence["cleanup"]["fixture_waiter_entries"], 0)
    self.assertEqual(receipt.counts["dispatch_completed_count"], 2)
    self.assertEqual(receipt.counts["admitted_task_count"], 2)
    self.assertEqual(shutdown, [True])
