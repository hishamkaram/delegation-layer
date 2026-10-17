"""Concurrent dispatch and admission observations for one private fixture cohort."""

from scripts.delegate_acceptance.private_cli.configuration import CAPACITY_TASK_BUDGET
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor
import json
from scripts.delegate_acceptance.private_cli.admission import observe_created_admission
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
        self.dispatch_started = time.monotonic()

    def dispatch_waiting(self, index):
        receipt = self.receipts / ("dispatch-%03d.json" % index)
        with self.dispatch_lock:
            self.dispatch_in_flight += 1
            self.peak_dispatch_in_flight = max(self.peak_dispatch_in_flight, self.dispatch_in_flight)
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
                timeout=600,
                receipt=receipt,
                phase="managed-runner",
                capture_root=self.case.capture_root,
            )
            if response.get("task_record") == "created":
                task_id = response.get("task_id")
                if not isinstance(task_id, str) or not task_id:
                    raise RuntimeError("created dispatch response omitted its task identity")
                self.remember_attempt(task_id)
        finally:
            with self.dispatch_lock:
                self.dispatch_in_flight -= 1
                self.dispatch_calls_completed += 1
                if self.case.run_receipt is not None:
                    self.case.run_receipt.update_counts(dispatch_completed_count=self.dispatch_calls_completed)
        return response

    def confirm_admission(self, index_response):
        index, response = index_response
        observe_created_admission(
            self.case.delegate,
            self.case.state,
            self.case.env,
            response,
            self.receipts / ("dispatch-%03d.json" % index),
            phase="managed-runner-observation",
            capture_root=self.case.capture_root,
        )
        with self.dispatch_lock:
            self.admitted_task_count += 1
            if self.case.run_receipt is not None:
                self.case.run_receipt.update_counts(admitted_task_count=self.admitted_task_count)

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
        dispatch_finished = time.monotonic()
        if self.peak_dispatch_in_flight <= 0:
            raise RuntimeError("dispatch batch did not complete its dispatch calls")
        dispatch_elapsed = round(max(0.0, dispatch_finished - self.dispatch_started), 6)
        if self.case.run_receipt is not None:
            self.case.run_receipt.phase("capacity_admission")
        with ThreadPoolExecutor(max_workers=self.case.dispatch_workers) as pool:
            list(pool.map(self.confirm_admission, enumerate(responses)))
        admission_elapsed = round(max(0.0, time.monotonic() - self.dispatch_started), 6)
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
            "dispatch_elapsed_seconds": dispatch_elapsed,
            "admission_elapsed_seconds": admission_elapsed,
            "admitted_task_count": self.admitted_task_count,
        }
        if (
            dispatch_batch["completed_dispatch_call_count"] != self.case.parallel_tasks
            or dispatch_batch["admitted_task_count"] != self.case.parallel_tasks
        ):
            raise RuntimeError("dispatch batch did not admit every requested fixture task: %r" % dispatch_batch)
        (self.receipts / "dispatch-batch.json").write_text(json.dumps(dispatch_batch, sort_keys=True) + "\n")
        self.evidence["dispatch_batch"] = dispatch_batch
        return CapacityDispatchResult(responses, task_ids, expected_labels, dispatch_batch)

    def remember_attempt(self, task_id: str) -> None:
        with self.dispatch_lock:
            if task_id not in self.case.attempted_task_ids:
                self.case.attempted_task_ids.append(task_id)
