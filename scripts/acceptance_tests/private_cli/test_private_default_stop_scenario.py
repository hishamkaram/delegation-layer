"""Implementation methods for PrivateDefaultStopScenarioTests; collected once by the acceptance_tests discovery."""

from pathlib import Path
import json
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.delegate_acceptance.private_cli import stop as stop_impl
from scripts.delegate_acceptance.private_cli import admission as admission_impl
from scripts.delegate_acceptance.private_cli import commands as commands_impl
from scripts.acceptance_tests.private_cli.fixtures import (
    set_up_private_capture_root,
)


class PrivateDefaultStopScenarioTests(unittest.TestCase):
    def setUp(self):
        super().setUp()
        self.capture_root = set_up_private_capture_root(self)

    def test_orchestration_requires_live_leases_and_canonical_closure(self):
        records = _run_stop_scenario(self)
        _assert_stop_expectations(self=self, **records)

    def test_user_stop_observation_refuses_undocumented_interruptions(self):
        user = {"continuation": {"resumable": True}}
        records = _run_stop_scenario(self, error="operational failure", ended_changes={"user": user})
        self.assertEqual([command[2] for command in records["calls"]], ["dispatch", "cancel", "status", "status"])
        self.assertEqual(records["lifecycle"], [
            "enter", "cancel", "closure", "interrupted:running", "interrupted:ended", "exit",
        ])
        self.assertEqual(records["lease_cleanup_calls"], [])
        self.assertEqual(len(records["dispatches"]), 1)

        budget = {"failure": {"code": "task_interrupted", "stage": "status", "next_action": "stop_and_report"}}
        records = _run_stop_scenario(self, error="operational failure", ended_changes={"budget": budget})
        self.assertEqual([command[2] for command in records["calls"]], [
            "dispatch", "cancel", "status", "status", "dispatch", "status", "status",
        ])
        self.assertEqual(records["lease_cleanup_calls"], [True])
        self.assertEqual(records["shutdown"], [])

    def test_closure_watches_follow_the_recorded_deadline_for_budget_and_stay_fixed_for_user(self):
        records = _run_stop_scenario(self, budget={"now_ns": BUDGET_DEADLINE_NS - 40 * 10**9})
        user_task, budget_task = records["dispatches"][0][0], records["dispatches"][1][0]
        self.assertEqual(records["closure_timeouts"], [(user_task, 30), (budget_task, 70)])
        self.assertEqual(records["evidence"]["tasks"]["budget_stop"]["closure"]["budget_deadline"], {
            "deadline": BUDGET_DEADLINE, "termination_observed_at": "2026-10-14T00:00:02Z",
        })

    def test_an_already_past_deadline_keeps_the_fixed_closure_allowance(self):
        records = _run_stop_scenario(self, budget={"now_ns": BUDGET_DEADLINE_NS + 10**9})
        self.assertEqual(records["closure_timeouts"][1][1], 30)

    def test_a_lease_observed_at_or_after_the_deadline_is_refused_inside_the_cohort(self):
        for observed in (BUDGET_DEADLINE_NS, BUDGET_DEADLINE_NS + 1):
            with self.subTest(observed=observed):
                records = _run_stop_scenario(
                    self, error="strictly before its budget deadline", budget={"lease_observed_ns": observed}
                )
                self.assertEqual(len(records["closure_timeouts"]), 1)
                self.assertEqual(records["lifecycle"][-2:], ["enter", "exit"])
                self.assertEqual(records["lease_cleanup_calls"], [True])

    def test_a_malformed_recorded_deadline_fails_inside_the_cohort_before_closure(self):
        records = _run_stop_scenario(self, error="budget lifetime deadline", budget={"deadline": "soon"})
        self.assertEqual(len(records["closure_timeouts"]), 1)
        self.assertEqual(records["lifecycle"][-2:], ["enter", "exit"])

    def test_budget_closure_before_deadline_with_other_deadline_or_without_launch_is_refused(self):
        cases = (
            ({"terminated_at": "2026-10-14T00:00:00.999999999Z"}, "recorded before its deadline"),
            ({"closure_deadline": "2026-10-14T00:00:03Z"}, "deadline differs from the observed budget deadline"),
            ({"launches": []}, "launch"),
        )
        for budget, error in cases:
            with self.subTest(budget=budget):
                records = _run_stop_scenario(self, error=error, budget=budget)
                self.assertEqual(records["lifecycle"][-3:], ["enter", "closure", "exit"])
                self.assertEqual(records["lease_cleanup_calls"], [True])

    def test_wrong_user_closure_cause_precedes_status_observation(self):
        records = _run_stop_scenario(self, error="wrong termination cause", user_closure_cause="budget")
        self.assertEqual([command[2] for command in records["calls"]], ["dispatch", "cancel"])
        self.assertEqual(records["lifecycle"], ["enter", "cancel", "closure", "exit"])
        self.assertEqual(records["lease_cleanup_calls"], [])
        self.assertEqual(len(records["dispatches"]), 1)


class DefaultStopDispatchReceiptTests(unittest.TestCase):
    def setUp(self):
        self.capture_root = set_up_private_capture_root(self)
        temporary = tempfile.TemporaryDirectory(prefix="default-stop-dispatch-receipt-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.delegate = self.root / "delegate"
        self.delegate.write_bytes(b"fixture executable\n")
        self.receipts = self.root / "responses"
        self.receipts.mkdir()
        self.receipt = private_cli.AcceptanceReceipt(self.root / "receipt.json", 2, 2, {"delegate": self.delegate})
        self.receipt.phase("default_stop_user")
        self.dispatch, _close = stop_impl._default_stop_task_operations(
            capture_root=self.capture_root, delegate=self.delegate, env={"PRIVATE_CANARY": "PRIVATE_DIAGNOSTIC_CANARY"},
            receipts=self.receipts, run_receipt=self.receipt, state=self.root / "state",
            waiting_brief=self.root / "brief.md", workspace=self.root,
        )

    def invoke(self, response: object, *, exit_code: int = 0, error: str | None = None):
        stdout = (json.dumps(response) + "\n").encode()
        completed = subprocess.CompletedProcess([], exit_code, stdout, b"PRIVATE_DIAGNOSTIC_CANARY")
        with patch.object(commands_impl, "run_captured_command", return_value=completed) as command:
            if error is None:
                observed = self.dispatch(TASK, "5m", "dispatch-default-stop-user.json")
                self.assertEqual(observed, response)
                self.receipt.write("PASS", {"status": "not-required"})
            else:
                with self.assertRaisesRegex(RuntimeError, error) as raised:
                    self.dispatch(TASK, "5m", "dispatch-default-stop-user.json")
                self.receipt.write("FAIL", {"status": "retained"}, raised.exception)
        command.assert_called_once()
        text = self.receipt.output.read_text()
        self.assertNotIn("PRIVATE_DIAGNOSTIC_CANARY", text)
        recorded = json.loads(text)
        self.assertIn("default_stop_dispatch", recorded)
        return recorded

    def test_nonzero_public_resource_refusal_survives_creation_rejection(self):
        response = {
            "task_id": TASK, "task_record": "not_created", "admission": "unknown",
            "failure": {"code": "resource_pressure", "reason_code": "host_cpu_sustained",
                        "stage": "check-resource-admission", "next_action": "retry_later",
                        "retry_after_seconds": 30, "recovery_command": "PRIVATE_DIAGNOSTIC_CANARY"},
            "resource_admission": {"status": "pressure", "warnings": ["PRIVATE_DIAGNOSTIC_CANARY"]},
            "error": "PRIVATE_DIAGNOSTIC_CANARY", "profile": {"secret": "PRIVATE_DIAGNOSTIC_CANARY"},
        }
        recorded = self.invoke(response, exit_code=1, error="did not create the requested task")
        self.assertEqual(recorded["default_stop_dispatch"], {
            "phase": "default_stop_user", "receipt": "dispatch-default-stop-user.json",
            "expected_task_id_match": True, "task_id": TASK, "root_id": None,
            "task_record": "not_created", "admission": "unknown",
            "failure": {"code": "resource_pressure", "reason_code": "host_cpu_sustained",
                        "stage": "check-resource-admission", "next_action": "retry_later", "retry_after_seconds": 30},
            "resource_admission": {"status": "pressure"},
        })
        self.assertEqual(recorded["failure"]["exception_type"], "RuntimeError")
        self.assertEqual(recorded["counts"]["dispatch_completed_count"], 0)
        self.assertEqual(recorded["counts"]["admitted_task_count"], 0)
        command_record = json.loads((self.receipts / "dispatch-default-stop-user.json").read_text())
        self.assertEqual(command_record["exit_code"], 1)
        self.assertIn("PRIVATE_DIAGNOSTIC_CANARY", command_record["stdout_tail"])

    def test_zero_exit_wrong_identity_survives_rejection_without_another_command(self):
        recorded = self.invoke({"task_id": "c" * 32, "root_id": ROOT, "task_record": "created",
                                "admission": "admitted", "stdout": "PRIVATE_DIAGNOSTIC_CANARY"},
                               error="did not create the requested task")
        self.assertEqual(recorded["default_stop_dispatch"], {
            "phase": "default_stop_user", "receipt": "dispatch-default-stop-user.json",
            "expected_task_id_match": False, "task_id": "c" * 32, "root_id": ROOT,
            "task_record": "created", "admission": "admitted", "failure": {},
        })
        self.assertEqual(recorded["counts"]["admitted_task_count"], 0)
        command_record = json.loads((self.receipts / "dispatch-default-stop-user.json").read_text())
        self.assertNotIn("stdout_tail", command_record)
        self.assertNotIn("response", command_record)

    def test_created_but_unadmitted_response_is_recorded_before_the_admission_guard(self):
        recorded = self.invoke({"task_id": TASK, "root_id": ROOT, "task_record": "created",
                                "admission": "unknown", "error": "PRIVATE_DIAGNOSTIC_CANARY",
                                "failure": {"code": "task_submission_uncertain", "stage": "submit-task",
                                            "next_action": "check_status"}},
                               exit_code=1, error="did not admit the requested task")
        self.assertEqual(recorded["default_stop_dispatch"], {
            "phase": "default_stop_user", "receipt": "dispatch-default-stop-user.json",
            "expected_task_id_match": True, "task_id": TASK, "root_id": ROOT,
            "task_record": "created", "admission": "unknown",
            "failure": {"code": "task_submission_uncertain", "stage": "submit-task", "next_action": "check_status"},
        })
        self.assertEqual(recorded["counts"]["dispatch_completed_count"], 0)
        self.assertEqual(recorded["counts"]["admitted_task_count"], 0)

    def test_success_keeps_public_projection_and_one_admission(self):
        recorded = self.invoke({"task_id": TASK, "root_id": ROOT, "task_record": "created",
                                "admission": "admitted", "resource_admission": {"status": "clear"},
                                "profile": "PRIVATE_DIAGNOSTIC_CANARY"})
        self.assertEqual(recorded["default_stop_dispatch"], {
            "phase": "default_stop_user", "receipt": "dispatch-default-stop-user.json",
            "expected_task_id_match": True, "task_id": TASK, "root_id": ROOT,
            "task_record": "created", "admission": "admitted", "failure": {},
            "resource_admission": {"status": "clear"},
        })
        self.assertEqual(recorded["counts"]["dispatch_completed_count"], 1)
        self.assertEqual(recorded["counts"]["admitted_task_count"], 1)

    def test_malformed_diagnostic_fields_never_replace_the_creation_error(self):
        for failure in (["PRIVATE_DIAGNOSTIC_CANARY"], {"code": {"private": "PRIVATE_DIAGNOSTIC_CANARY"},
                "stage": "PRIVATE_DIAGNOSTIC_CANARY", "next_action": "bad action PRIVATE_DIAGNOSTIC_CANARY",
                "reason_code": "x" * 129, "retry_after_seconds": True}):
            with self.subTest(failure=failure):
                recorded = self.invoke({"task_id": {"secret": "PRIVATE_DIAGNOSTIC_CANARY"},
                                        "root_id": "PRIVATE_DIAGNOSTIC_CANARY", "task_record": ["created"],
                                        "admission": {"secret": "PRIVATE_DIAGNOSTIC_CANARY"},
                                        "failure": failure, "resource_admission": {"status": {"secret": "PRIVATE_DIAGNOSTIC_CANARY"}}},
                                       exit_code=1, error="did not create the requested task")
                self.assertEqual(recorded["default_stop_dispatch"], {
                    "phase": "default_stop_user", "receipt": "dispatch-default-stop-user.json",
                    "expected_task_id_match": False, "task_id": None, "root_id": None,
                    "task_record": None, "admission": None, "failure": {},
                })
                self.assertEqual(recorded["failure"]["exception_type"], "RuntimeError")
                self.assertEqual(recorded["counts"]["admitted_task_count"], 0)


class DefaultStopDispatchProjectionTests(unittest.TestCase):
    def test_retry_advice_is_a_positive_go_integer_and_never_a_boolean_or_raw_value(self):
        cases = (
            (1, {"retry_after_seconds": 1}), (30, {"retry_after_seconds": 30}),
            (2**63 - 1, {"retry_after_seconds": 2**63 - 1}),
            (False, {}), (True, {}), (0, {}), (-1, {}), (30.0, {}), ("30", {}),
            (2**63, {}), (None, {}), ({}, {}), ([], {}),
        )
        for value, expected in cases:
            with self.subTest(value=value):
                evidence = stop_impl.default_stop_dispatch_evidence({"failure": {"retry_after_seconds": value}})
                self.assertEqual(evidence["failure"], expected)

    def test_non_object_response_has_no_untrusted_projection(self):
        for value in (None, ["PRIVATE_DIAGNOSTIC_CANARY"], "PRIVATE_DIAGNOSTIC_CANARY"):
            with self.subTest(value=value):
                evidence = stop_impl.default_stop_dispatch_evidence(value, expected_task_id=TASK)
                self.assertEqual(evidence, {"task_id": None, "root_id": None, "task_record": None,
                                           "admission": None, "failure": {}, "expected_task_id_match": False})


TASK = "a" * 32
ROOT = "b" * 32
LIVE_LEASE_OBSERVATION_SECONDS = 30
PUEUE_RUNNING_WATCH_SECONDS = 30
CLOSURE_WAIT_SECONDS = 30
BUDGET_DEADLINE = "2026-10-14T00:00:01Z"
# Independent oracle for BUDGET_DEADLINE in Unix nanoseconds (calendar.timegm * 1e9).
BUDGET_DEADLINE_NS = 1791936001000000000


def _seconds(duration):
    """Parse the fixture's Go-style durations (ms, s, m) without a second policy engine."""
    for unit, scale in (("ms", 0.001), ("s", 1), ("m", 60)):
        if duration.endswith(unit):
            return int(duration[: -len(unit)]) * scale
    raise ValueError("unsupported fixture duration: " + duration)


class DefaultStopBudgetTests(unittest.TestCase):
    def test_user_stop_budget_outlives_lease_observation_and_closure_wait(self):
        self.assertEqual(private_cli.DEFAULT_STOP_TASK_BUDGET, "5m")
        self.assertGreater(
            _seconds(private_cli.DEFAULT_STOP_TASK_BUDGET), LIVE_LEASE_OBSERVATION_SECONDS + CLOSURE_WAIT_SECONDS
        )

    def test_budget_fixture_allowance_covers_both_setup_watch_windows(self):
        # A fixture allowance, not a native startup guarantee: Pueue Running watch plus lease watch plus margin.
        self.assertEqual(private_cli.DEFAULT_STOP_BUDGET, "90s")
        self.assertGreater(
            _seconds(private_cli.DEFAULT_STOP_BUDGET), PUEUE_RUNNING_WATCH_SECONDS + LIVE_LEASE_OBSERVATION_SECONDS
        )
        self.assertNotEqual(private_cli.DEFAULT_STOP_BUDGET, private_cli.DEFAULT_STOP_TASK_BUDGET)


def _budget_closure(*, deadline=BUDGET_DEADLINE, terminated_at="2026-10-14T00:00:02Z", launches=({"pid": 101},)):
    return {
        "intent": {"deadline": deadline, "created_at": "2026-10-14T00:00:00Z"},
        "registration": {"scope_kind": "process-group", "assurance": "cooperative"},
        "worker_wait": {"child_pid": 101, "result": "exit-error", "exit_code": 143,
                        "observed_at": "2026-10-14T00:00:01Z"},
        "launches": list(launches),
        "termination": {"cause": "budget", "scope_empty": True, "observed_at": terminated_at},
    }


class BudgetDeadlinePolicyTests(unittest.TestCase):
    def test_lease_must_be_observed_strictly_before_the_recorded_deadline(self):
        self.assertEqual(stop_impl.budget_deadline_ns(BUDGET_DEADLINE, BUDGET_DEADLINE_NS - 1, TASK), BUDGET_DEADLINE_NS)
        for observed in (BUDGET_DEADLINE_NS, BUDGET_DEADLINE_NS + 1):
            with self.subTest(observed=observed), self.assertRaisesRegex(RuntimeError, "strictly before its budget deadline"):
                stop_impl.budget_deadline_ns(BUDGET_DEADLINE, observed, TASK)

    def test_deadline_keeps_sub_microsecond_precision(self):
        deadline = "2026-10-14T00:00:01.000000001Z"
        self.assertEqual(stop_impl.budget_deadline_ns(deadline, BUDGET_DEADLINE_NS, TASK), BUDGET_DEADLINE_NS + 1)
        with self.assertRaisesRegex(RuntimeError, "strictly before"):
            stop_impl.budget_deadline_ns(deadline, BUDGET_DEADLINE_NS + 1, TASK)

    def test_malformed_or_missing_deadline_and_non_integer_observation_are_refused(self):
        for deadline, observed in ((None, 1), ("2026-10-14 00:00:01", 1), (BUDGET_DEADLINE, None), (BUDGET_DEADLINE, True)):
            with self.subTest(deadline=deadline, observed=observed), self.assertRaises(RuntimeError):
                stop_impl.budget_deadline_ns(deadline, observed, TASK)

    def test_closure_watch_is_remaining_time_plus_the_fixed_allowance(self):
        self.assertEqual(stop_impl.budget_closure_watch_seconds(BUDGET_DEADLINE_NS, BUDGET_DEADLINE_NS - 40 * 10**9), 70)
        self.assertEqual(stop_impl.budget_closure_watch_seconds(BUDGET_DEADLINE_NS, BUDGET_DEADLINE_NS), 30)
        self.assertEqual(stop_impl.budget_closure_watch_seconds(BUDGET_DEADLINE_NS, BUDGET_DEADLINE_NS + 5 * 10**9), 30)

    def test_termination_at_or_after_the_deadline_is_accepted_and_before_is_refused(self):
        for terminated_at in (BUDGET_DEADLINE, "2026-10-14T00:00:01.000000001Z"):
            with self.subTest(terminated_at=terminated_at):
                evidence = stop_impl.budget_closure_evidence(
                    _budget_closure(terminated_at=terminated_at), TASK, BUDGET_DEADLINE
                )
                self.assertEqual(evidence["budget_deadline"]["deadline"], BUDGET_DEADLINE)
                self.assertEqual(evidence["termination"]["cause"], "budget")
        with self.assertRaisesRegex(RuntimeError, "recorded before its deadline"):
            stop_impl.budget_closure_evidence(
                _budget_closure(terminated_at="2026-10-14T00:00:00.999999999Z"), TASK, BUDGET_DEADLINE
            )

    def test_mismatched_deadline_and_no_launch_closures_are_refused(self):
        with self.assertRaisesRegex(RuntimeError, "deadline differs from the observed budget deadline"):
            stop_impl.budget_closure_evidence(_budget_closure(deadline="2026-10-14T00:00:03Z"), TASK, BUDGET_DEADLINE)
        with self.assertRaises(RuntimeError):
            stop_impl.budget_closure_evidence(_budget_closure(launches=()), TASK, BUDGET_DEADLINE)

    def test_a_user_stop_cause_is_not_budget_evidence(self):
        closure = _budget_closure()
        closure["termination"]["cause"] = "requested"
        with self.assertRaises(RuntimeError):
            stop_impl.budget_closure_evidence(closure, TASK, BUDGET_DEADLINE)


class DefaultStopLiveLeaseTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-live-lease-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.lifetime = self.root / "tasks" / TASK / "lifetime"
        self.lifetime.mkdir(parents=True)
        self.clock = 1000.0
        self.sleeps = []
        self.waits = []

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.clock += seconds

    def observe(self, specifications=(), cohort=None):
        response = {"task_id": TASK, "root_id": ROOT}
        with patch.object(stop_impl.time, "monotonic", side_effect=lambda: self.clock), \
                patch.object(stop_impl.time, "sleep", side_effect=self.sleep), \
                patch.object(stop_impl, "wait_for_pueue_state", side_effect=lambda *a, **k: self.waits.append(a)), \
                patch.object(stop_impl, "capacity_lease_specifications", return_value=list(specifications)) as records, \
                patch.object(stop_impl.LiveFIFOLeaseCohort, "acquire", return_value=cohort):
            result = stop_impl.default_stop_live_lease(
                "client", "config", {}, self.root, response, self.root / "waiters", "n" * 32,
                capture_root=self.capture_root if hasattr(self, "capture_root") else None,
            )
        self.records = records
        return result

    def test_an_already_terminated_task_without_a_launch_fails_immediately(self):
        (self.lifetime / "terminated.json").write_text("{}")
        with self.assertRaisesRegex(RuntimeError, "termination record appeared before live lease: " + TASK):
            self.observe()
        self.assertEqual(self.sleeps, [])
        self.assertEqual(len(self.waits), 1)

    def test_a_dangling_termination_symlink_is_still_a_presence_fault(self):
        (self.lifetime / "terminated.json").symlink_to(self.root / "missing")
        with self.assertRaisesRegex(RuntimeError, "termination record appeared before live lease"):
            self.observe()
        self.assertEqual(self.sleeps, [])

    def test_a_termination_record_that_appears_while_polling_fails_on_the_next_pass(self):
        real_sleep = self.sleep

        def sleep_then_terminate(seconds):
            real_sleep(seconds)
            (self.lifetime / "terminated.json").write_text("{}")

        with patch.object(self, "sleep", side_effect=sleep_then_terminate):
            with self.assertRaisesRegex(RuntimeError, "termination record appeared before live lease"):
                self.observe()
        self.assertEqual(len(self.sleeps), 1)

    def test_without_termination_the_bounded_observation_deadline_is_unchanged(self):
        with self.assertRaisesRegex(RuntimeError, "did not reach a live lease before its bounded observation deadline"):
            self.observe()
        self.assertAlmostEqual(sum(self.sleeps), LIVE_LEASE_OBSERVATION_SECONDS)
        self.assertEqual(max(self.sleeps), 0.05)

    def test_a_live_candidate_wins_over_a_termination_record(self):
        (self.lifetime / "terminated.json").write_text("{}")
        specification = {"task_id": TASK, "root_id": ROOT, "pid": 7}
        class Cohort:
            checks = 0

            def assert_live(self):
                self.checks += 1

        cohort = Cohort()
        with patch.object(stop_impl, "lease_candidate_specifications", side_effect=lambda value: value):
            found, acquired, lease = self.observe([specification], cohort)
        self.assertIs(found, specification)
        self.assertIs(acquired, cohort)
        self.assertEqual(cohort.checks, 1)
        self.assertEqual(self.sleeps, [])
        self.assertEqual(set(lease), {"observed_at_unix_ns", "acquire_elapsed_ns"})


def _run_stop_scenario(self, *, error=None, ended_changes=None, user_closure_cause="requested", budget=None):
    """Run the real stop orchestration over fixture commands and return its recorded effects."""
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
        records = {"attempted": [], "calls": [], "lease_cleanup_calls": [], "dispatches": [],
                   "lifecycle": [], "receipt": receipt, "closure_timeouts": []}
        budget = {"now_ns": BUDGET_DEADLINE_NS - 40 * 10**9, "lease_observed_ns": 1, "deadline": BUDGET_DEADLINE,
                  "closure_deadline": BUDGET_DEADLINE, "terminated_at": "2026-10-14T00:00:02Z",
                  "launches": [{"pid": 101}], **(budget or {})}
        status_reads = {}
        user_request_id = "u" * 32

        gate, run_json, live_lease, closure, shutdown, shutdown_private = _stop_scenario_callbacks(
            calls=records["calls"], dispatches=records["dispatches"], ended_changes=ended_changes or {},
            gate_path=gate_path, lease_cleanup_calls=records["lease_cleanup_calls"],
            lifecycle=records["lifecycle"], root=root, self=self, status_reads=status_reads,
            user_closure_cause=user_closure_cause, user_request_id=user_request_id,
            budget=budget, closure_timeouts=records["closure_timeouts"])
        records["shutdown"] = shutdown

        def run():
            return private_cli.run_default_stop_acceptance(
                delegate=provider, client=provider, state=state, workspace=workspace, brief=brief,
                gate=gate_path, waiter_directory=waiters,
                config=root / "pueue.yml", env={}, response_directory=responses, provider_fixture=provider,
                lease_nonce="e" * 32,
                run_receipt=receipt, attempted_task_ids=records["attempted"], capture_root=self.capture_root, disposal=None, supervisor_cleanups=[],
            )

        with patch.object(private_cli.LockedReleaseGate, "acquire", return_value=gate), \
                patch.object(stop_impl, "run_json", side_effect=run_json), \
                patch.object(admission_impl, "run_json", side_effect=run_json), \
                patch.object(private_cli.time, "sleep"), \
                patch.object(stop_impl.time, "time_ns", return_value=budget["now_ns"]), \
                patch.object(stop_impl, "default_stop_live_lease", side_effect=live_lease), \
                patch.object(stop_impl, "await_managed_lifetime_closure", side_effect=closure), \
                patch.object(stop_impl, "shutdown_terminal_private_supervisor", side_effect=shutdown_private):
            if error is None:
                records["evidence"] = run()
            else:
                with self.assertRaisesRegex(RuntimeError, error):
                    run()
        return records


def _stop_scenario_callbacks(*, budget, calls, closure_timeouts, dispatches, ended_changes, gate_path,
                             lease_cleanup_calls, lifecycle, root, self, status_reads, user_closure_cause,
                             user_request_id):
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

    run_json = _stop_command_callback(calls, dispatches, ended_changes, lifecycle, self, status_reads, user_request_id)
    def live_lease(_client, _config, _env, _state, response, _waiters, _nonce,
                  **_options):
        return ({
            "task_id": response["task_id"], "root_id": response["root_id"],
            "pid": 100 + len(dispatches), "scope_kind": "process-group",
            "assurance": "cooperative", "owner_sha256": "a" * 64,
            "registration_sha256": "b" * 64,
            "scope_plan_sha256": "c" * 64, "scope_id_sha256": "d" * 64,
            "path": root / (response["task_id"] + ".fifo"), "deadline": budget["deadline"],
        }, Cohort(), {"observed_at_unix_ns": budget["lease_observed_ns"], "acquire_elapsed_ns": 1})

    def closure(_state, task_ids, timeout=30):
        lifecycle.append("closure")
        task_id = tuple(task_ids)[0]
        closure_timeouts.append((task_id, timeout))
        is_user = task_id == dispatches[0][0]
        return {"tasks": {task_id: {
            "intent": {"deadline": BUDGET_DEADLINE if is_user else budget["closure_deadline"],
                        "created_at": "2026-10-14T00:00:00Z"},
            "registration": {"scope_kind": "process-group", "assurance": "cooperative"},
            "worker_wait": {"child_pid": 101, "result": "exit-error",
                             "exit_code": 143, "observed_at": "2026-10-14T00:00:01Z"},
            "launches": [{"pid": 101}] if is_user else budget["launches"],
            "termination": {"cause": user_closure_cause if is_user else "budget", "scope_empty": True,
                            "observed_at": "2026-10-14T00:00:02Z" if is_user else budget["terminated_at"]},
        }}}

    shutdown = []

    def shutdown_private(*_args, **_kwargs):
        self.assertTrue(gate.released, "supervisor shutdown preceded fixture gate release")
        shutdown.append(True)
    return gate, run_json, live_lease, closure, shutdown, shutdown_private


def _stop_command_callback(calls, dispatches, ended_changes, lifecycle, self, status_reads, user_request_id):
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
        task_id = args[3]
        status_reads[task_id] = status_reads.get(task_id, 0) + 1
        identity = {"root_id": "r" * 32, "task_id": task_id}
        if task_id == dispatches[0][0]:
            if status_reads[task_id] > 1:
                observed = {**_user_interrupted_status(identity, user_request_id), **ended_changes.get("user", {})}
            else:
                observed = _user_pending_interruption_status(identity, user_request_id)
        else:
            self.assertEqual(task_id, dispatches[1][0])
            if status_reads[task_id] > 1:
                observed = {**_budget_timed_out_status(identity), **ended_changes.get("budget", {})}
            else:
                observed = {**identity, "liveness": "running", "stops": [{
                    "request_id": "budget", "cause": "budget",
                    "requested": True, "terminated": False, "observed_state": "running",
                }]}
        label = "interrupted" if observed.get("status") == "interrupted" else "status"
        liveness = observed["liveness"]
        if not isinstance(liveness, str):
            raise AssertionError("the status fixture must record textual liveness")
        lifecycle.append(label + ":" + liveness)
        return observed
    return run_json


def _user_interrupted_status(identity, request_id):
    """Mirror the documented `status --json` envelope after a native default user stop."""
    return {
        "schema_version": 1, "command": "status", **identity,
        "status": "interrupted", "admission": "admitted", "liveness": "ended", "publication": "unknown",
        "supervisor": {"matched": True, "state": "ended", "numeric_task_id": 0},
        "stops": [{
            "request_id": request_id, "cause": "user", "requested": True, "matched": False,
            "attempted": False, "numeric_task_id": 0, "in_flight": False,
            "observed_state": "ended", "terminated": True,
        }],
        "continuation": {"resumable": False, "mode": "native", "predecessor_task_id": identity["task_id"],
                         "reason": "task was cancelled by the user"},
        "failure": {"code": "task_interrupted", "stage": "status", "next_action": "stop_and_report"},
    }


def _user_pending_interruption_status(identity, request_id):
    """Mirror the `status --json` envelope a native user stop reports after closure, before the queue ends."""
    status = _user_interrupted_status(identity, request_id)
    return {
        **status, "liveness": "running",
        "supervisor": {"matched": True, "state": "running", "numeric_task_id": 0},
        "stops": [{**status["stops"][0], "observed_state": "", "terminated": False}],
    }


def _budget_timed_out_status(identity):
    return {
        **identity, "status": "timed_out", "liveness": "ended",
        "stops": [{"request_id": "budget", "cause": "budget", "requested": True,
                   "terminated": True, "observed_state": "ended"}],
    }


def _assert_stop_expectations(*, attempted, calls, closure_timeouts, dispatches, evidence, lease_cleanup_calls,
                              lifecycle, receipt, self, shutdown):
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
        "enter", "cancel", "closure", "interrupted:running", "interrupted:ended", "remove", "exit",
        "enter", "closure", "status:running", "status:ended", "remove", "exit",
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
    self.assertEqual(evidence["tasks"]["user_stop"]["status"]["status"], "interrupted")
    self.assertEqual(
        evidence["tasks"]["user_stop"]["status"]["failure"],
        {"code": "task_interrupted", "next_action": "stop_and_report"},
    )
    self.assertNotIn("failure", evidence["tasks"]["budget_stop"]["status"])
    self.assertEqual(calls[3][2:], ["status", dispatches[0][0], "--json"])
    self.assertFalse(evidence["stop_poll_policy"]["override"])
    self.assertEqual(evidence["cleanup"]["fixture_waiter_entries"], 0)
    self.assertEqual(receipt.counts["dispatch_completed_count"], 2)
    self.assertEqual(receipt.counts["admitted_task_count"], 2)
    self.assertEqual(shutdown, [True])
