"""Concurrent dispatch and admission observations for one private fixture cohort."""

from scripts.delegate_acceptance.private_cli.configuration import CAPACITY_TASK_BUDGET
from scripts.delegate_acceptance.private_cli.configuration import CAPACITY_ADMISSION_TIMEOUT_SECONDS
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor
import json
from scripts.delegate_acceptance.private_cli.admission import observe_created_admission
from scripts.delegate_acceptance.private_cli.admission import confirmed_resource_refusal, PressureRefusal
from scripts.delegate_acceptance.private_cli.commands import run_json
import threading
import time
from scripts.delegate_acceptance.private_cli.capacity_inputs import CapacityRequest, CapacityDispatchResult


class CapacityDispatcher:
    """One lock owns dispatch-arrival counters for this cohort; task attempts remain caller-owned."""

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
        self.dispatch_started = time.monotonic()
        self.deadline = self.dispatch_started + CAPACITY_ADMISSION_TIMEOUT_SECONDS
        if self.case.run_receipt is not None:
            self.case.run_receipt.update_counts(
                created_task_count=0, retry_dispatch_count=0, resource_refusal_count=0,
            )

    def attempt_receipt(self, index: int, attempt: int) -> Path:
        suffix = "" if attempt == 0 else ".retry-1"
        return self.receipts / ("dispatch-%03d%s.json" % (index, suffix))

    def dispatch_waiting(self, index: int, *, attempt: int = 0):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("capacity dispatch exceeded its cohort admission deadline")
        receipt = self.attempt_receipt(index, attempt)
        # Reserve a distinct attempt before launching; never overwrite prior command evidence.
        with receipt.open("x", encoding="utf-8"):
            pass
        with self.dispatch_lock:
            self.dispatch_in_flight += 1
            self.peak_dispatch_in_flight = max(self.peak_dispatch_in_flight, self.dispatch_in_flight)
            if attempt == 1:
                self.retry_dispatch_count += 1
                if self.case.run_receipt is not None:
                    self.case.run_receipt.update_counts(retry_dispatch_count=self.retry_dispatch_count)
        try:
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
            if refusal is not None:
                self.record_refusal(index, attempt, refusal)
        finally:
            with self.dispatch_lock:
                self.dispatch_in_flight -= 1
                self.dispatch_calls_completed += 1
                if self.case.run_receipt is not None:
                    self.case.run_receipt.update_counts(dispatch_completed_count=self.dispatch_calls_completed)
        return response

    def confirm_admission(self, index_response):
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

    def record_refusal(self, index: int, attempt: int, refusal: PressureRefusal) -> None:
        with self.dispatch_lock:
            self.refusals.append({
                "slot": index, "attempt": attempt, "reason_code": refusal.reason_code,
                "retry_after_seconds": refusal.retry_after_seconds,
                "response_elapsed_seconds": round(max(0.0, time.monotonic() - self.dispatch_started), 6),
            })
            if self.case.run_receipt is not None:
                self.case.run_receipt.update_counts(resource_refusal_count=len(self.refusals))
                self.case.run_receipt.add_details(resource_pressure_refusals=list(self.refusals))

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
        with ThreadPoolExecutor(max_workers=self.case.dispatch_workers) as pool:
            retried = list(pool.map(self.retry_waiting, refused))
        for index, response in zip(refused, retried, strict=True):
            if response.get("task_record") != "created":
                raise RuntimeError("capacity slot %d failed its single resource retry" % index)
            responses[index] = response
        return set(refused)

    def retry_waiting(self, index: int):
        return self.dispatch_waiting(index, attempt=1)

    def run(self):
        if self.case.run_receipt is not None:
            self.case.run_receipt.phase("capacity_dispatch")
        print(
            "Dispatching %d isolated fixture tasks with %d dispatch workers"
            % (self.case.parallel_tasks, self.case.dispatch_workers),
            flush=True,
        )
        with ThreadPoolExecutor(max_workers=self.case.dispatch_workers) as pool:
            responses = list(pool.map(self.dispatch_waiting, range(self.case.parallel_tasks)))
        initial_finished = time.monotonic()
        retried = self.retry_pressure_refusals(responses)
        dispatch_finished = time.monotonic()
        if self.peak_dispatch_in_flight <= 0:
            raise RuntimeError("dispatch batch did not complete its dispatch calls")
        dispatch_elapsed = round(max(0.0, dispatch_finished - self.dispatch_started), 6)
        if self.case.run_receipt is not None:
            self.case.run_receipt.phase("capacity_admission")
        with ThreadPoolExecutor(max_workers=self.case.dispatch_workers) as pool:
            list(pool.map(self.confirm_admission, (
                (index, response, 1 if index in retried else 0)
                for index, response in enumerate(responses)
            )))
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
