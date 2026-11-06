"""Resource-pressure retries preserve logical slots, custody and cleanup evidence."""

from collections import Counter
from contextlib import ExitStack, redirect_stdout
from dataclasses import dataclass, replace
import io
import json
from pathlib import Path
import tempfile
import threading
from typing import Callable
import unittest
from unittest.mock import patch

from scripts.acceptance_tests.private_cli.fixtures import RecordingReleaseGate
from scripts.delegate_acceptance.directories.operations import DirectoryOwner, OwnedDirectory
from scripts.delegate_acceptance.private_cli import admission as admission_impl
from scripts.delegate_acceptance.private_cli import capacity_dispatch as dispatch_impl
from scripts.delegate_acceptance.private_cli import receipt as receipt_impl
from scripts.delegate_acceptance.private_cli.capacity_inputs import CapacityRequest


class RetryClock:
    """Own simulated monotonic time; tests advance it at explicit boundaries."""

    def __init__(self):
        self.lock = threading.Lock()
        self.started = 1234.0
        self.now = self.started
        self.sleeps: list[float] = []
        self.on_sleep: Callable[[], None] | None = None
        self.overshoot = 0.0

    def monotonic(self) -> float:
        with self.lock:
            return self.now

    def advance(self, seconds: float) -> None:
        with self.lock:
            self.now += seconds

    def sleep(self, seconds: float) -> None:
        if seconds <= 0 or len(self.sleeps) >= 1000:
            raise AssertionError("retry wait must make finite forward progress")
        if self.on_sleep is not None:
            self.on_sleep()
        with self.lock:
            self.sleeps.append(seconds)
            self.now += seconds + self.overshoot


@dataclass(frozen=True)
class DispatchInvocation:
    index: int
    attempt: int
    argv: tuple[str, ...]
    receipt: Path
    started: float
    deadline: float | None
    timeout: float | None
    claimed: bool


@dataclass(frozen=True)
class StatusInvocation:
    task_id: str
    receipt: Path
    started: float
    deadline: float
    timeout: float | None


def pressure_refusal() -> dict[str, object]:
    return {
        "task_id": "e" * 32,
        "root_id": "b" * 32,
        "task_record": "not_created",
        "admission": "unknown",
        "failure": {
            "code": "resource_pressure", "stage": "check-resources",
            "next_action": "retry_later", "retry_after_seconds": 30,
            "reason_code": "host_memory_critical",
        },
        "resource_admission": {"status": "pressure"},
        "error": "fixture resource pressure",
    }


def changed_failure(**changes: object) -> dict[str, object]:
    response = pressure_refusal()
    failure = response["failure"]
    if not isinstance(failure, dict):
        raise AssertionError("fixture failure must be an object")
    response["failure"] = {**failure, **changes}
    return response


class PrivateResourceRetryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-resource-retry-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        owner = DirectoryOwner()
        self.addCleanup(owner.rollback)
        capture_root = owner.create("private capture root", self.root / "capture", self.root)
        old_layout = owner.create("old package root", self.root / "old", self.root)
        self.receipts = self.root / "receipts"
        self.receipts.mkdir()
        self.brief = self.root / "waiting.md"
        self.brief.write_text("ordinary finite fixture turn\n")
        waiters = self.root / "waiters"
        waiters.mkdir()
        self.evidence: dict[str, object] = {}
        self.case = CapacityRequest(
            delegate=self.root / "delegate", state=self.root / "state",
            workspace=self.root, brief=self.brief,
            release_gate=RecordingReleaseGate(self.root / "gate"),
            waiter_directory=waiters, env={"FIXTURE": "resource-retry"},
            old_layout=old_layout.directory, old_layout_owned=(old_layout,),
            client=self.root / "pueue", config=self.root / "pueue.yml",
            parallel_tasks=200, dispatch_workers=4, response_directory=self.receipts,
            evidence=self.evidence, lease_nonce="c" * 32, client_call_log=self.root / "calls",
            upgraded_client_call_log=self.root / "upgraded-calls",
            attempted_task_ids=[], capture_root=capture_root, run_receipt=None,
        )
        self.reset_observations()

    def reset_observations(self):
        self.clock = RetryClock()
        self.clock.on_sleep = self.assert_first_burst_finished
        self.lock = threading.Lock()
        self.invocations: list[DispatchInvocation] = []
        self.script: dict[int, list[dict[str, object] | OSError]] = {}
        self.counts: Counter[int] = Counter()
        self.created: set[str] = set()
        self.initial_finished: set[int] = set()
        self.original_receipts: dict[Path, bytes] = {}
        self.in_flight = 0
        self.peak = 0
        self.first_finished_at = 0.0
        self.first_burst_elapsed = 2.0
        self.barrier: threading.Barrier | None = None
        self.status_ids: list[str] = []
        self.status_invocations: list[StatusInvocation] = []
        self.previous_created_by_thread: dict[int, str] = {}
        self.dispatcher = None

    def created_response(self, index: int) -> dict[str, object]:
        return {
            "task_id": "%032x" % (index + 1), "root_id": "b" * 32,
            "task_record": "created", "admission": "admitted",
        }

    def assert_first_burst_finished(self):
        self.assertEqual(self.initial_finished, set(range(self.case.parallel_tasks)))
        self.assertEqual(self.in_flight, 0)
        self.assertEqual(set(self.case.attempted_task_ids), self.created)

    def run_dispatcher(self):
        with ExitStack() as stack:
            stack.enter_context(patch.object(dispatch_impl, "time", self.clock))
            stack.enter_context(patch.object(admission_impl, "time", self.clock))
            stack.enter_context(patch.object(receipt_impl, "time", self.clock))
            stack.enter_context(patch.object(dispatch_impl, "run_json", side_effect=self.run_json))
            stack.enter_context(patch.object(admission_impl, "run_json", side_effect=self.run_json))
            stack.enter_context(redirect_stdout(io.StringIO()))
            self.dispatcher = dispatch_impl.CapacityDispatcher(
                self.case, self.brief, self.receipts, self.evidence,
            )
            return self.dispatcher.run()

    def run_json(self, delegate: Path, env: dict[str, str], args: list[str],
                 expected: tuple[int, ...] = (0,), timeout: float | None = 45,
                 receipt: Path | None = None, phase: str | None = None, *,
                 capture_root: OwnedDirectory, capture_response: bool = False,
                 deadline: float | None = None) -> dict[str, object]:
        self.assertEqual(delegate, self.case.delegate)
        self.assertEqual(env, self.case.env)
        self.assertIs(capture_root, self.case.capture_root)
        self.assertIsNotNone(receipt)
        if receipt is None:
            raise AssertionError("every command needs its receipt")
        self.assertIsNone(timeout, "control commands must retain an absolute deadline")
        if deadline is None:
            raise AssertionError("control command omitted its absolute deadline")
        self.assertLessEqual(deadline, self.clock.started + 600)
        if args[2] == "status":
            self.assertLessEqual(deadline, self.clock.monotonic() + 45)
            with self.lock:
                self.status_ids.append(args[3])
                self.status_invocations.append(StatusInvocation(
                    args[3], receipt, self.clock.monotonic(), deadline, timeout,
                ))
            response = {"task_id": args[3], "admission": "admitted"}
            receipt.write_text(json.dumps(response))
            return response
        self.assertEqual(args[2], "dispatch")
        self.assertEqual(expected, (0, 1))
        self.assertEqual(deadline, self.clock.started + 600)
        index = int(receipt.name.removeprefix("dispatch-")[:3])
        invocation = self.begin_dispatch(index, args, receipt, deadline, timeout)
        try:
            if self.barrier is not None and invocation.attempt == 0 and index < 4:
                self.barrier.wait(timeout=5)
            return self.dispatch_response(invocation)
        finally:
            self.finish_dispatch(invocation)

    def begin_dispatch(self, index, args, receipt, deadline, timeout):
        with self.lock:
            previous = self.previous_created_by_thread.get(threading.get_ident())
            if previous is not None:
                self.assertIn(previous, self.case.attempted_task_ids,
                              "worker took another command before registering its created task")
            attempt = self.counts[index]
            self.assertLess(attempt, 2, "a logical slot must never make a third dispatch")
            invocation = DispatchInvocation(
                index=index,
                attempt=attempt,
                argv=tuple(args),
                receipt=receipt,
                started=self.clock.monotonic(),
                deadline=deadline,
                timeout=timeout,
                claimed=receipt.exists() and receipt.read_bytes() == b"",
            )
            self.counts[index] += 1
            self.invocations.append(invocation)
            self.in_flight += 1
            self.peak = max(self.peak, self.in_flight)
            return invocation

    def dispatch_response(self, invocation):
        scripted = self.script.get(invocation.index, [])
        response = (scripted[invocation.attempt] if invocation.attempt < len(scripted)
                    else self.created_response(invocation.index))
        if isinstance(response, OSError):
            invocation.receipt.write_text('{"failure_class":"invocation_error"}\n')
            raise response
        invocation.receipt.write_text(json.dumps(response, sort_keys=True) + "\n")
        with self.lock:
            self.original_receipts[invocation.receipt] = invocation.receipt.read_bytes()
            if response.get("task_record") == "created":
                task_id = response["task_id"]
                if not isinstance(task_id, str):
                    raise AssertionError("created fixture task needs a string identity")
                self.created.add(task_id)
                self.previous_created_by_thread[threading.get_ident()] = task_id
        return response

    def finish_dispatch(self, invocation):
        with self.lock:
            self.in_flight -= 1
            if invocation.attempt == 0:
                self.initial_finished.add(invocation.index)
                if len(self.initial_finished) == self.case.parallel_tasks:
                    self.clock.advance(self.first_burst_elapsed)
                    self.first_finished_at = self.clock.monotonic()

    def assert_cleanup_registered(self):
        self.assertEqual(set(self.case.attempted_task_ids), self.created)
        self.assertEqual(len(self.case.attempted_task_ids), len(self.created))
        self.assertNotIn("e" * 32, self.case.attempted_task_ids)

    def assert_no_retry(self):
        self.assertEqual(self.counts, Counter({index: 1 for index in range(self.case.parallel_tasks)}))
        self.assertEqual(self.clock.sleeps, [])
        self.assertEqual(list(self.receipts.glob("*.retry-*.json")), [])
        self.assert_cleanup_registered()

    def assert_receipts_preserved(self):
        for receipt, original in self.original_receipts.items():
            with self.subTest(receipt=receipt.name):
                self.assertEqual(receipt.read_bytes(), original)

    def attach_run_receipt(self):
        with patch.object(receipt_impl, "time", self.clock):
            receipt = receipt_impl.AcceptanceReceipt(
                self.root / "run-receipt.json", self.case.parallel_tasks,
                self.case.dispatch_workers, {},
            )
        self.case = replace(self.case, run_receipt=receipt)
        return receipt

    def write_failure_receipt(self, receipt, error):
        with patch.object(receipt_impl, "time", self.clock):
            receipt.write("FAIL", {"status": "passed"}, error)
        return json.loads(receipt.output.read_text())

    def test_one_pressure_refusal_retries_only_its_slot_after_complete_burst(self):
        refused_index = 17
        self.script[refused_index] = [pressure_refusal()]
        self.barrier = threading.Barrier(4)
        result = self.run_dispatcher()

        self.assertEqual(len(self.invocations), 201)
        self.assertEqual(self.counts[refused_index], 2)
        self.assertEqual(sum(count == 1 for count in self.counts.values()), 199)
        self.assertEqual(self.peak, 4)
        self.assertTrue(all(call.claimed for call in self.invocations))
        original, retry = [call for call in self.invocations if call.index == refused_index]
        self.assertEqual(original.receipt.name, "dispatch-017.json")
        self.assertEqual(retry.receipt.name, "dispatch-017.retry-1.json")
        self.assertEqual(original.argv, retry.argv)
        self.assertEqual(retry.argv, (
            "--root", str(self.case.state), "dispatch", "--provider", "pi:json",
            "--brief", str(self.brief), "--cwd", str(self.case.workspace),
            "--permission", "read-only", "--budget", "30m", "--json",
        ))
        self.assertGreaterEqual(retry.started - self.first_finished_at, 30)
        self.assertGreaterEqual(sum(self.clock.sleeps), 30)
        self.assertEqual(result.task_ids, ["%032x" % (index + 1) for index in range(200)])
        self.assertEqual(len(result.responses), 200)
        self.assertEqual(len(result.expected_labels), 200)
        self.assertEqual(len(self.created), 200)
        self.assert_cleanup_registered()
        self.assert_receipts_preserved()
        self.assertEqual(len(list(self.receipts.glob("dispatch-[0-9][0-9][0-9].json"))), 200)
        self.assertEqual(len(list(self.receipts.glob("*.retry-1.json"))), 1)
        batch = result.dispatch_batch
        for field, expected in {
            "requested_caller_concurrency": 4,
            "observed_peak_in_flight_dispatch_calls": 4,
            "completed_dispatch_call_count": 201, "retry_dispatch_count": 1,
            "resource_refusal_count": 1, "created_task_count": 200,
            "completed_logical_request_count": 200, "admitted_task_count": 200,
        }.items():
            self.assertEqual(batch[field], expected, field)
        self.assertEqual(json.loads((self.receipts / "dispatch-batch.json").read_text()), batch)
        self.assertEqual(self.evidence["dispatch_batch"], batch)
        for call in self.invocations:
            self.assertIsNone(call.timeout)
            self.assertEqual(call.deadline, self.clock.started + 600)

    def test_second_pressure_refusal_fails_without_third_dispatch(self):
        self.script[17] = [pressure_refusal(), pressure_refusal()]
        with self.assertRaises(RuntimeError):
            self.run_dispatcher()
        self.assertEqual(len(self.invocations), 201)
        self.assertEqual(self.counts[17], 2)
        self.assertEqual(len(self.created), 199)
        self.assert_cleanup_registered()
        self.assert_receipts_preserved()
        self.assertTrue((self.receipts / "dispatch-017.json").exists())
        self.assertTrue((self.receipts / "dispatch-017.retry-1.json").exists())

    def test_restart_cleanup_registration_preserves_200_task_cohort_counts(self):
        receipt = self.attach_run_receipt()
        result = self.run_dispatcher()
        dispatcher = self.dispatcher
        if not isinstance(dispatcher, dispatch_impl.CapacityDispatcher):
            raise AssertionError("cohort must retain its cleanup registry owner")
        self.assertEqual(len(result.task_ids), 200)
        self.assert_cleanup_registered()
        self.assertEqual(result.dispatch_batch["created_task_count"], 200)
        self.assertEqual(receipt.counts["created_task_count"], 200)
        original_batch = dict(result.dispatch_batch)
        original_counts = dict(receipt.counts)
        original_attempts = tuple(self.case.attempted_task_ids)
        restart_id = "f" * 32

        registered = dispatcher.remember_attempt(restart_id)
        self.assertEqual(self.case.attempted_task_ids, [*original_attempts, restart_id])
        self.assertEqual(len(self.case.attempted_task_ids), 201)
        self.assertEqual(receipt.counts["created_task_count"], 200)
        self.assertIs(registered, True)
        for task_id in (restart_id, result.task_ids[0]):
            with self.subTest(repeated_id=task_id):
                self.assertIs(dispatcher.remember_attempt(task_id), False)
                self.assertEqual(self.case.attempted_task_ids, [*original_attempts, restart_id])
                self.assertEqual(result.dispatch_batch, original_batch)
                self.assertEqual(receipt.counts, original_counts)
        self.assertEqual(json.loads((self.receipts / "dispatch-batch.json").read_text()),
                         original_batch)
        with patch.object(receipt_impl, "time", self.clock):
            receipt.write("PASS", {"status": "passed"})
        self.assertEqual(json.loads(receipt.output.read_text())["counts"], original_counts)
        self.assertEqual(len(self.invocations), 200)

    def test_four_repeated_refusals_record_all_eight_before_failing(self):
        receipt = self.attach_run_receipt()
        self.barrier = threading.Barrier(4)
        for index in range(4):
            self.script[index] = [pressure_refusal(), pressure_refusal()]
        with self.assertRaises(RuntimeError) as failure:
            self.run_dispatcher()
        recorded = self.write_failure_receipt(receipt, failure.exception)
        self.assertEqual(len(self.invocations), 204)
        self.assertEqual(self.counts, Counter({index: 2 if index < 4 else 1 for index in range(200)}))
        self.assertEqual(self.peak, 4)
        self.assertTrue(all(call.claimed for call in self.invocations))
        self.assertEqual(len(self.created), 196)
        self.assertEqual(set(self.case.attempted_task_ids),
                         {"%032x" % (index + 1) for index in range(4, 200)})
        self.assert_cleanup_registered()
        self.assert_receipts_preserved()
        for field, expected in {
            "dispatch_completed_count": 204, "retry_dispatch_count": 4,
            "resource_refusal_count": 8, "created_task_count": 196,
        }.items():
            self.assertEqual(recorded["counts"][field], expected, field)
        refusals = recorded["resource_pressure_refusals"]
        self.assertEqual(len(refusals), 8)
        self.assertEqual(Counter((item["slot"], item["attempt"]) for item in refusals),
                         Counter((index, attempt) for index in range(4) for attempt in range(2)))
        for item in refusals:
            self.assertEqual(item["retry_after_seconds"], 30)
            self.assertEqual(item["reason_code"], "host_memory_critical")
            elapsed = item["response_elapsed_seconds"]
            if item["attempt"] == 0:
                self.assertGreaterEqual(elapsed, 0)
                self.assertLessEqual(elapsed, self.first_burst_elapsed)
            else:
                self.assertEqual(elapsed, self.first_burst_elapsed + 30)
        self.assertEqual(len(list(self.receipts.glob("*.retry-1.json"))), 4)
        for call in self.invocations:
            if call.attempt == 1:
                self.assertGreaterEqual(call.started - self.first_finished_at, 30)

    def test_first_burst_invocation_error_retains_prior_refusal_evidence(self):
        receipt = self.attach_run_receipt()
        for index in range(4):
            self.script[index] = [pressure_refusal()]
        self.script[199] = [OSError("injected first-burst invocation failure")]
        with self.assertRaisesRegex(OSError, "injected first-burst invocation failure") as failure:
            self.run_dispatcher()
        recorded = self.write_failure_receipt(receipt, failure.exception)
        self.assert_no_retry()
        self.assertEqual(len(self.invocations), 200)
        self.assertEqual(len(self.created), 195)
        self.assertEqual(recorded["counts"]["resource_refusal_count"], 4)
        self.assertEqual(recorded["counts"]["dispatch_completed_count"], 200)
        self.assertEqual(recorded["counts"]["created_task_count"], 195)
        refusals = recorded["resource_pressure_refusals"]
        self.assertEqual(Counter((item["slot"], item["attempt"]) for item in refusals),
                         Counter((index, 0) for index in range(4)))

    def test_created_pressure_response_is_never_redispatched(self):
        response = pressure_refusal()
        response.update(self.created_response(17))
        self.script[17] = [response]
        result = self.run_dispatcher()
        self.assertEqual(len(result.task_ids), 200)
        self.assertEqual(self.status_ids, ["%032x" % 18])
        self.assert_no_retry()

    def test_created_uncertain_submission_observes_same_task_without_retry(self):
        response = self.created_response(17)
        response.update({
            "admission": "unknown",
            "failure": {"code": "task_submission_uncertain", "next_action": "check_status"},
        })
        self.script[17] = [response]
        result = self.run_dispatcher()
        self.assertEqual(len(result.task_ids), 200)
        self.assertEqual(self.status_ids, ["%032x" % 18])
        self.assertEqual(len(self.status_invocations), 1)
        status = self.status_invocations[0]
        self.assertIsNone(status.timeout)
        self.assertEqual(status.deadline, self.first_finished_at + 45)
        self.assert_no_retry()

    def test_uncertain_observation_keeps_absolute_deadline_near_cohort_expiry(self):
        self.case = replace(self.case, parallel_tasks=1)
        self.first_burst_elapsed = 580
        response = self.created_response(0)
        response.update({
            "admission": "unknown",
            "failure": {"code": "task_submission_uncertain", "next_action": "check_status"},
        })
        self.script[0] = [response]
        with patch.object(dispatch_impl, "observe_created_admission",
                          wraps=admission_impl.observe_created_admission) as observe:
            result = self.run_dispatcher()
        self.assertEqual(result.task_ids, ["%032x" % 1])
        observe.assert_called_once()
        self.assertEqual(observe.call_args.kwargs["deadline"], self.clock.started + 600)
        self.assertEqual(len(self.status_invocations), 1)
        status = self.status_invocations[0]
        self.assertEqual(status.task_id, "%032x" % 1)
        self.assertIsNone(status.timeout)
        self.assertEqual(status.deadline, self.clock.started + 600)
        self.assertEqual(status.deadline - status.started, 20)
        self.assert_no_retry()

    def test_unknown_task_record_is_not_retried(self):
        response = pressure_refusal()
        response["task_record"] = "unknown"
        self.script[17] = [response]
        with self.assertRaises(RuntimeError):
            self.run_dispatcher()
        self.assert_no_retry()

    def assert_rejected_response(self, response):
        self.case = replace(self.case, parallel_tasks=1)
        self.script[0] = [response]
        with self.assertRaises(RuntimeError):
            self.run_dispatcher()
        self.assert_no_retry()

    def test_not_created_other_fault_is_not_retried(self):
        self.assert_rejected_response(changed_failure(
            code="supervisor_setup_failed", next_action="stop_and_report",
        ))

    def test_not_created_submission_uncertainty_is_not_retried(self):
        self.assert_rejected_response(changed_failure(
            code="task_submission_uncertain", next_action="check_status",
        ))

    def test_wrong_next_action_is_not_retried(self):
        self.assert_rejected_response(changed_failure(next_action="check_status"))

    def test_invalid_retry_delays_are_not_retried(self):
        for delay in (0, -30, True, False, "30", 31, 30.0, None):
            with self.subTest(delay=delay, kind=type(delay).__name__):
                self.run_rejected_in_fresh_case(changed_failure(retry_after_seconds=delay))

    def run_rejected_in_fresh_case(self, response):
        self.receipts = self.root / ("receipts-case-%03d" % len(list(self.root.iterdir())))
        self.receipts.mkdir()
        self.evidence = {}
        self.case = replace(self.case, response_directory=self.receipts,
                            attempted_task_ids=[], evidence=self.evidence)
        self.reset_observations()
        self.assert_rejected_response(response)

    def test_missing_retry_delay_is_not_retried(self):
        response = pressure_refusal()
        failure = response["failure"]
        self.assertIsInstance(failure, dict)
        if not isinstance(failure, dict):
            raise AssertionError("fixture failure must be an object")
        del failure["retry_after_seconds"]
        self.assert_rejected_response(response)

    def test_wrong_or_malformed_resource_metadata_is_not_retried(self):
        for metadata in ({"status": "unknown"}, {"status": "clear"}, {}, None, [], "pressure"):
            with self.subTest(metadata=metadata):
                response = pressure_refusal()
                response["resource_admission"] = metadata
                self.run_rejected_in_fresh_case(response)

    def test_malformed_failure_metadata_is_not_retried(self):
        for failure in (None, [], "resource_pressure", {}):
            with self.subTest(failure=failure):
                response = pressure_refusal()
                response["failure"] = failure
                self.run_rejected_in_fresh_case(response)

    def test_deadline_expired_after_burst_prevents_retry_command(self):
        self.script[17] = [pressure_refusal()]
        self.first_burst_elapsed = 600
        with self.assertRaises(TimeoutError):
            self.run_dispatcher()
        self.assert_no_retry()
        self.assertEqual(len(self.created), 199)

    def test_insufficient_deadline_for_full_retry_wait_prevents_command(self):
        self.script[17] = [pressure_refusal()]
        self.first_burst_elapsed = 580
        with self.assertRaises(TimeoutError):
            self.run_dispatcher()
        self.assertEqual(len(self.invocations), 200)
        self.assertEqual(self.counts[17], 1)
        self.assertEqual(list(self.receipts.glob("*.retry-*.json")), [])
        self.assert_cleanup_registered()

    def test_deadline_expiring_during_wait_prevents_retry_command(self):
        self.script[17] = [pressure_refusal()]
        self.first_burst_elapsed = 560
        self.clock.overshoot = 41
        with self.assertRaises(TimeoutError):
            self.run_dispatcher()
        self.assertTrue(self.clock.sleeps)
        self.assertEqual(len(self.invocations), 200)
        self.assertEqual(self.counts[17], 1)
        self.assertEqual(list(self.receipts.glob("*.retry-*.json")), [])
        self.assert_cleanup_registered()

    def test_existing_first_receipt_refuses_before_invocation(self):
        self.case = replace(self.case, parallel_tasks=1)
        receipt = self.receipts / "dispatch-000.json"
        original = b"retained first dispatch evidence\n"
        receipt.write_bytes(original)
        with self.assertRaises((FileExistsError, RuntimeError)):
            self.run_dispatcher()
        self.assertEqual(self.invocations, [])
        self.assertEqual(receipt.read_bytes(), original)
        self.assertEqual(self.case.attempted_task_ids, [])

    def test_existing_retry_receipt_refuses_before_retry_invocation(self):
        self.script[17] = [pressure_refusal()]
        receipt = self.receipts / "dispatch-017.retry-1.json"
        original = b"retained retry dispatch evidence\n"
        receipt.write_bytes(original)
        with self.assertRaises((FileExistsError, RuntimeError)):
            self.run_dispatcher()
        self.assertEqual(len(self.invocations), 200)
        self.assertEqual(self.counts[17], 1)
        self.assertEqual(receipt.read_bytes(), original)
        self.assert_cleanup_registered()

    def test_invocation_error_keeps_other_created_tasks_registered(self):
        self.script[199] = [OSError("injected dispatch invocation failure")]
        with self.assertRaisesRegex(OSError, "injected dispatch invocation failure"):
            self.run_dispatcher()
        self.assertEqual(len(self.invocations), 200)
        self.assertEqual(len(self.created), 199)
        self.assert_no_retry()

    def test_retry_invocation_error_keeps_first_burst_cleanup_registered(self):
        self.script[17] = [pressure_refusal(), OSError("injected retry invocation failure")]
        with self.assertRaisesRegex(OSError, "injected retry invocation failure"):
            self.run_dispatcher()
        self.assertEqual(len(self.invocations), 201)
        self.assertEqual(self.counts[17], 2)
        self.assertEqual(len(self.created), 199)
        self.assert_cleanup_registered()
        self.assertEqual(json.loads((self.receipts / "dispatch-017.json").read_text()), pressure_refusal())
