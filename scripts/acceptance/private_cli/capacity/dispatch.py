"""Concurrent dispatch and admission observations for one private fixture cohort."""

from scripts.acceptance.private_cli.configuration import CAPACITY_TASK_BUDGET
from scripts.acceptance.private_cli.configuration import CAPACITY_ADMISSION_TIMEOUT_SECONDS
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor, as_completed
from collections.abc import Iterable
from enum import Enum
import json
from scripts.acceptance.private_cli.admission import observe_created_admission
from scripts.acceptance.private_cli.admission import confirmed_resource_refusal
from scripts.acceptance.private_cli.admission import PressureRefusal
from scripts.acceptance.private_cli.commands import run_json
import threading
import time
from scripts.acceptance.private_cli.capacity.inputs import CapacityRequest
from scripts.acceptance.private_cli.capacity.inputs import CapacityDispatchResult
from scripts.acceptance.evidence.cpu_snapshot import CPU_SNAPSHOT_ARGV, CPU_SNAPSHOT_BYTES
from scripts.acceptance.evidence.cpu_snapshot import CPU_SNAPSHOT_TIMEOUT_SECONDS, cpu_snapshot_rows
from scripts.acceptance.processes.contracts import BoundedCommandError, command_closure_unproven
from scripts.acceptance.processes.operations import run_bounded_command


class _DispatchDenied(Enum):
    FENCED = "fenced"


class CapacityDispatcher:
    """One lock owns grants, first failure and counters; granted calls retain their created-task evidence."""

    def __init__(self, case: CapacityRequest, waiting_brief: Path, receipts: Path, evidence: dict[str, object]):
        self.case = case
        self.waiting_brief = waiting_brief
        self.receipts = receipts
        self.evidence = evidence
        self.dispatch_lock = threading.Lock()
        self.dispatch_in_flight = 0
        self.peak_dispatch_in_flight = 0
        self.dispatch_calls_completed = 0
        self.admitted_task_count = 0
        self.created_task_count = 0
        self.retry_dispatch_count = 0
        self.refusals: list[dict[str, int | float | str]] = []
        self.failures: list[BaseException] = []
        self.cpu_snapshot_claimed = False
        self.dispatch_started = time.monotonic()
        self.deadline = self.dispatch_started + CAPACITY_ADMISSION_TIMEOUT_SECONDS
        if self.case.run_receipt is not None:
            self.case.run_receipt.update_counts(
                created_task_count=0, retry_dispatch_count=0, resource_refusal_count=0,
            )

    def attempt_receipt(self, index: int, attempt: int) -> Path:
        suffix = "" if attempt == 0 else ".retry-1"
        return self.receipts / ("dispatch-%03d%s.json" % (index, suffix))

    def dispatch_waiting(self, index: int, *, attempt: int = 0) -> dict[str, object] | _DispatchDenied:
        # A grant precedes all command preparation, including its empty receipt.
        with self.dispatch_lock:
            if self.failures:
                return _DispatchDenied.FENCED
            self.dispatch_in_flight += 1
            self.peak_dispatch_in_flight = max(self.peak_dispatch_in_flight, self.dispatch_in_flight)
        invoked = False
        try:
            if self.deadline - time.monotonic() <= 0:
                raise TimeoutError("capacity dispatch exceeded its cohort admission deadline")
            receipt = self.attempt_receipt(index, attempt)
            with receipt.open("x", encoding="utf-8"):
                pass
            with self.dispatch_lock:
                if attempt == 1:
                    self.retry_dispatch_count += 1
                    if self.case.run_receipt is not None:
                        self.case.run_receipt.update_counts(retry_dispatch_count=self.retry_dispatch_count)
            invoked = True
            response = run_json(
                self.case.delegate,
                self.case.env,
                [
                    "--root",
                    str(self.case.state),
                    "dispatch",
                    "--provider",
                    "pi:json",
                    "--brief",
                    str(self.waiting_brief),
                    "--cwd",
                    str(self.case.workspace),
                    "--permission",
                    "read-only",
                    "--budget",
                    CAPACITY_TASK_BUDGET,
                    "--json",
                ],
                expected=(0, 1),
                timeout=None,
                deadline=self.deadline,
                receipt=receipt,
                phase="managed-runner",
                capture_root=self.case.capture_root,
            )
            if not isinstance(response, dict):
                raise RuntimeError("capacity dispatch returned a non-object response")
            if response.get("task_record") == "created":
                task_id = response.get("task_id")
                if not isinstance(task_id, str) or not task_id:
                    raise RuntimeError("created dispatch response omitted its task identity")
                if not self.remember_attempt(task_id):
                    raise RuntimeError("capacity dispatch reused a created task identity")
                with self.dispatch_lock:
                    self.created_task_count += 1
                    if self.case.run_receipt is not None:
                        self.case.run_receipt.update_counts(created_task_count=self.created_task_count)
            refusal = confirmed_resource_refusal(response)
            fatal = None
            if response.get("task_record") != "created" and (attempt == 1 or refusal is None):
                fatal = RuntimeError("capacity slot %d %s" % (
                    index, "failed its single resource retry" if attempt == 1
                    else "was not created; no retry is authorized"))
            capture = False
            if refusal is not None:
                capture = self.record_refusal(index, attempt, refusal, fatal)
            elif fatal is not None:
                self.record_failure(fatal)
            # A terminal refusal seals grants before the diagnostic's bounded I/O.
            if capture:
                self.capture_cpu_snapshot()
            if fatal is not None:
                raise fatal
            return response
        except BaseException as error:
            self.record_failure(error)
            raise
        finally:
            with self.dispatch_lock:
                self.dispatch_in_flight -= 1
                if invoked:
                    self.dispatch_calls_completed += 1
                    if self.case.run_receipt is not None:
                        self.case.run_receipt.update_counts(dispatch_completed_count=self.dispatch_calls_completed)

    def record_failure(self, error: BaseException) -> None:
        with self.dispatch_lock:
            if not any(previous is error for previous in self.failures):
                self.failures.append(error)

    def raise_failure(self) -> None:
        if not self.failures:
            return
        primary, *other = self.failures
        if other:
            causes = ([primary.__cause__] if primary.__cause__ is not None else []) + other
            primary.__cause__ = BaseExceptionGroup("additional capacity dispatch failures", causes)
        raise primary

    def dispatch_batch(self, indices: Iterable[int], *, attempt: int = 0) -> list[dict[str, object]]:
        indices = list(indices)
        responses: dict[int, dict[str, object]] = {}
        with ThreadPoolExecutor(max_workers=self.case.dispatch_workers) as pool:
            futures = {}
            try:
                for index in indices:
                    with self.dispatch_lock:
                        if self.failures:
                            break
                    futures[pool.submit(self.dispatch_waiting, index, attempt=attempt)] = index
                for future in as_completed(futures):
                    if future.cancelled():
                        continue
                    response = future.result()
                    if response is not _DispatchDenied.FENCED:
                        responses[futures[future]] = response
                    with self.dispatch_lock:
                        failed = bool(self.failures)
                    if failed:
                        pool.shutdown(wait=True, cancel_futures=True)
                        break
            except BaseException as error:
                # This includes main-thread interrupts, before executor __exit__ waits.
                self.record_failure(error)
                pool.shutdown(wait=True, cancel_futures=True)
        self.raise_failure()
        return [responses[index] for index in indices]

    def confirm_admission(self, index_response):
        try:
            index, response, attempt = index_response
            remaining = self.deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("capacity admission exceeded its cohort deadline")
            observe_created_admission(
                self.case.delegate,
                self.case.state,
                self.case.env,
                response,
                self.attempt_receipt(index, attempt),
                timeout=min(60, remaining),
                deadline=self.deadline,
                phase="managed-runner-observation",
                capture_root=self.case.capture_root,
            )
            with self.dispatch_lock:
                self.admitted_task_count += 1
                if self.case.run_receipt is not None:
                    self.case.run_receipt.update_counts(admitted_task_count=self.admitted_task_count)
        except BaseException as error:
            self.record_failure(error)
            raise

    def record_refusal(self, index: int, attempt: int, refusal: PressureRefusal,
                       fatal: BaseException | None) -> bool:
        with self.dispatch_lock:
            self.refusals.append({
                "slot": index, "attempt": attempt, "reason_code": refusal.reason_code,
                "retry_after_seconds": refusal.retry_after_seconds,
                "response_elapsed_seconds": round(max(0.0, time.monotonic() - self.dispatch_started), 6),
            })
            if self.case.run_receipt is not None:
                self.case.run_receipt.update_counts(resource_refusal_count=len(self.refusals))
                self.case.run_receipt.add_details(resource_pressure_refusals=list(self.refusals))
            if fatal is not None:
                self.failures.append(fatal)
            if refusal.reason_code == "host_cpu_sustained" and not self.cpu_snapshot_claimed:
                self.cpu_snapshot_claimed = True
                return True
        return False

    def capture_cpu_snapshot(self) -> None:
        snapshot: dict[str, object] = {
            "method": "native_ps", "timing": "after_first_host_cpu_refusal",
            "basis": "native_platform_historical_percent", "admission_intervals_matched": False,
            "causal_attribution": False, "status": "unavailable", "reason": "capture_unavailable",
        }
        fatal = None
        try:
            result = run_bounded_command(list(CPU_SNAPSHOT_ARGV), {**self.case.env, "LC_ALL": "C"},
                max_output=CPU_SNAPSHOT_BYTES, capture=True, timeout=CPU_SNAPSHOT_TIMEOUT_SECONDS,
                cwd=self.case.workspace)
            snapshot["native_wait"] = True
            if result.exit_code != 0:
                snapshot["reason"] = "native_exit_nonzero"
            else:
                snapshot["reason"] = "invalid_output"
                snapshot.update(cpu_snapshot_rows(result.output.decode("utf-8")), status="observed")
                del snapshot["reason"]
        except BaseException as error:
            if isinstance(error, BoundedCommandError):
                snapshot["reason"] = error.reason.value
                if error.exit_code is not None:
                    snapshot["native_wait"] = True
            if command_closure_unproven(error) or isinstance(error, (KeyboardInterrupt, SystemExit)):
                snapshot["closure_unproven"] = command_closure_unproven(error)
                self.record_failure(error)
                fatal = error
        self.evidence["host_cpu_snapshot"] = snapshot
        if self.case.run_receipt is not None:
            self.case.run_receipt.add_details(host_cpu_snapshot=snapshot)
        if fatal is not None:
            raise fatal

    def retry_pressure_refusals(self, responses) -> set[int]:
        refused: dict[int, PressureRefusal] = {}
        for index, response in enumerate(responses):
            refusal = confirmed_resource_refusal(response)
            if refusal is not None:
                refused[index] = refusal
            elif response.get("task_record") != "created":
                raise RuntimeError("capacity slot %d was not created; no retry is authorized" % index)
        if not refused:
            return set()
        # The burst has finished, so this waits at least the advice for every refusal.
        delay = max(refusal.retry_after_seconds for refusal in refused.values())
        if self.deadline - time.monotonic() <= delay:
            raise TimeoutError("capacity pressure wait cannot fit within its cohort deadline")
        if self.case.run_receipt is not None:
            self.case.run_receipt.phase("capacity_pressure_wait")
        time.sleep(delay)
        if time.monotonic() >= self.deadline:
            raise TimeoutError("capacity pressure wait exceeded its cohort deadline")
        if self.case.run_receipt is not None:
            self.case.run_receipt.phase("capacity_retry_dispatch")
        retried = self.dispatch_batch(refused, attempt=1)
        for index, response in zip(refused, retried, strict=True):
            if response.get("task_record") != "created":
                raise RuntimeError("capacity slot %d failed its single resource retry" % index)
            responses[index] = response
        return set(refused)

    def run(self):
        if self.case.run_receipt is not None:
            self.case.run_receipt.phase("capacity_dispatch")
        print(
            "Dispatching %d isolated fixture tasks with %d dispatch workers"
            % (self.case.parallel_tasks, self.case.dispatch_workers),
            flush=True,
        )
        responses = self.dispatch_batch(range(self.case.parallel_tasks))
        initial_finished = time.monotonic()
        retried = self.retry_pressure_refusals(responses)
        dispatch_finished = time.monotonic()
        if self.peak_dispatch_in_flight <= 0:
            raise RuntimeError("dispatch batch did not complete its dispatch calls")
        dispatch_elapsed = round(max(0.0, dispatch_finished - self.dispatch_started), 6)
        if self.case.run_receipt is not None:
            self.case.run_receipt.phase("capacity_admission")
        with ThreadPoolExecutor(max_workers=self.case.dispatch_workers) as pool:
            try:
                futures = [pool.submit(self.confirm_admission, (index, response, 1 if index in retried else 0))
                           for index, response in enumerate(responses)]
                for future in as_completed(futures):
                    future.result()
            except BaseException as error:
                self.record_failure(error)
                pool.shutdown(wait=True, cancel_futures=True)
        self.raise_failure()
        admission_elapsed = round(max(0.0, time.monotonic() - self.dispatch_started), 6)
        return self.batch_result(responses, initial_finished, dispatch_elapsed, admission_elapsed)

    def batch_result(self, responses, initial_finished, dispatch_elapsed, admission_elapsed):
        task_ids = [response["task_id"] for response in responses]
        if len(task_ids) != len(set(task_ids)):
            raise RuntimeError("dispatch batch returned duplicate task identities")
        if any((not response.get("root_id") for response in responses)):
            raise RuntimeError("dispatch batch response omitted its supervisor root identity")
        expected_labels = {"delegate:" + response["root_id"] + ":" + response["task_id"] for response in responses}
        if len(expected_labels) != self.case.parallel_tasks:
            raise RuntimeError("dispatch batch did not produce one unique task label per request")
        dispatch_batch = {
            "requested_caller_concurrency": self.case.dispatch_workers,
            "observed_peak_in_flight_dispatch_calls": self.peak_dispatch_in_flight,
            "completed_dispatch_call_count": self.dispatch_calls_completed,
            "completed_logical_request_count": len(responses),
            "retry_dispatch_count": self.retry_dispatch_count,
            "resource_refusal_count": len(self.refusals),
            "created_task_count": self.created_task_count,
            "initial_dispatch_elapsed_seconds": round(max(0.0, initial_finished - self.dispatch_started), 6),
            "dispatch_elapsed_seconds": dispatch_elapsed,
            "admission_elapsed_seconds": admission_elapsed,
            "admitted_task_count": self.admitted_task_count,
        }
        if (
            dispatch_batch["completed_dispatch_call_count"] != self.case.parallel_tasks + self.retry_dispatch_count
            or self.retry_dispatch_count != len(self.refusals)
            or self.created_task_count != self.case.parallel_tasks
            or len(responses) != self.case.parallel_tasks
            or dispatch_batch["admitted_task_count"] != self.case.parallel_tasks
            or self.peak_dispatch_in_flight > self.case.dispatch_workers
        ):
            raise RuntimeError("dispatch batch did not admit every requested fixture task: %r" % dispatch_batch)
        (self.receipts / "dispatch-batch.json").write_text(json.dumps(dispatch_batch, sort_keys=True) + "\n")
        self.evidence["dispatch_batch"] = dispatch_batch
        return CapacityDispatchResult(responses, task_ids, expected_labels, dispatch_batch)

    def remember_attempt(self, task_id: str) -> bool:
        with self.dispatch_lock:
            if task_id in self.case.attempted_task_ids:
                return False
            self.case.attempted_task_ids.append(task_id)
            return True
