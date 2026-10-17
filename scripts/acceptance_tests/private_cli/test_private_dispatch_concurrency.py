"""Implementation methods for PrivateDispatchConcurrencyTests; collected once by the acceptance_tests discovery."""

from concurrent.futures import ThreadPoolExecutor
from contextlib import redirect_stdout
import io
import json
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.delegate_acceptance.private_cli import capacity as capacity_impl
from scripts.delegate_acceptance.private_cli import capacity_dispatch as dispatch_impl
from scripts.delegate_acceptance.private_cli import admission as admission_impl
from scripts.acceptance_tests.private_cli.fixtures import (
    RecordingReleaseGate, _release_live_directory_tokens,
)


class PrivateDispatchConcurrencyTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-dispatch-proof-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.state = self.root / "state"
        self.brief = self.root / "brief.md"
        self.brief.write_text("ordinary fixture turn\n")
        self.gate = self.root / "gate"
        self.release_gate = RecordingReleaseGate(self.gate)
        self.waiters = self.root / "waiters"
        self.waiters.mkdir()
        self.old_layout = self.root / "old"
        self.old_layout.mkdir()
        owner = private_cli.DirectoryOwner()
        owner.adopt_created("old package root", self.old_layout, self.root)
        self.old_layout_owned = owner.transfer()
        self.addCleanup(self.release_old_layout)
        self.total = 200
        self.task_ids = ["%032x" % (index + 1) for index in range(self.total)]
        self.lease_candidates = [
            self.waiters / ("candidate-%03d.fifo" % index)
            for index in range(self.total)
        ]
        for candidate in self.lease_candidates:
            candidate.touch()
        self.restart_id = "f" * 32
        self.lock = threading.Lock()
        self.dispatched = set()
        self.observed = set()
        self.collected = set()
        self.restarts = 0
        self.snapshots = 0
        self.all_running = False
        self.shutdown = False
        self.status_bytes = 1024 * 1024 + 1
        self.worker_counts = []
        self.evidence = {}
        self.client_call_log = self.root / "client-calls"
        self.status_snapshot_owners = 1
        self.dispatch_burst_barrier = threading.Barrier(2)
        self.upgraded_liveness = "running"
        self.upgraded_error = ""
        self.upgraded_status = ""
        self.upgraded_admission = "admitted"
        self.duplicate_capacity_label = False
        self.cohort_closed = False
        self.owner_proof_count = self.total
        self.failed_dispatch_index = None
        self.attempted_task_ids = []
        self.capture_owner = private_cli.DirectoryOwner()
        self.capture_root = self.capture_owner.create(
            "private control capture root", self.root / "capture-root", self.root,
        )
        self.addCleanup(self.capture_owner.rollback)

    def release_old_layout(self):
        _release_live_directory_tokens(self.old_layout_owned)

    def test_orchestration_records_all_200_capacity_members_before_release(self):
        # This unit isolates receipt/orchestration logic. Live FIFO handshakes,
        # owner-lock contention, and 200 native workers are separate executable
        # tests and the mandatory private CLI acceptance gate.
        task_ids, status_bytes = self.run_upgrade()
        self.assertEqual(self.worker_counts, [2, 2, 16, 16])
        self.assertEqual(task_ids, [*self.task_ids, self.restart_id])
        self.assertEqual(status_bytes, self.status_bytes)
        self.assertEqual(self.dispatched, set(range(self.total)))
        self.assertEqual(self.observed, set(self.task_ids[:16]))
        self.assertEqual(self.collected, set(self.task_ids))
        self.assertEqual(self.snapshots, 4)
        self.assertEqual(self.restarts, 1)
        dispatch_batch = self.evidence["dispatch_batch"]
        self.assertEqual(dispatch_batch["requested_caller_concurrency"], 2)
        self.assertEqual(dispatch_batch["observed_peak_in_flight_dispatch_calls"], 2)
        self.assertEqual(dispatch_batch["completed_dispatch_call_count"], self.total)
        self.assertEqual(dispatch_batch["admitted_task_count"], self.total)
        self.assertGreaterEqual(dispatch_batch["dispatch_elapsed_seconds"], 0)
        self.assertGreaterEqual(
            dispatch_batch["admission_elapsed_seconds"],
            dispatch_batch["dispatch_elapsed_seconds"],
        )
        capacity_proof = self.evidence["capacity_proof"]
        self.assertEqual(capacity_proof["requested_task_count"], self.total)
        self.assertEqual(capacity_proof["observed_running_task_count"], self.total)
        self.assertEqual(capacity_proof["peak_observed_running_task_count"], self.total)
        self.assertEqual(capacity_proof["observed_lease_candidate_count"], self.total)
        self.assertEqual(capacity_proof["peak_observed_lease_candidate_count"], self.total)
        self.assertEqual(capacity_proof["observed_live_lease_count"], self.total)
        self.assertEqual(
            capacity_proof["observed_lifetime_owner_lease_count"], self.total,
        )
        self.assertEqual(capacity_proof["scope_kind_counts"], {"process-group": self.total})
        self.assertEqual(capacity_proof["assurance_counts"], {"cooperative": self.total})
        self.assertEqual(
            len(capacity_proof["lifetime_owner_overlap"]["counted_tasks"]), self.total,
        )
        self.assertEqual(
            capacity_proof["lifetime_owner_overlap"]["counted_task_count"], self.total,
        )
        self.assertEqual(
            capacity_proof["lifetime_owner_overlap"]["lease_probe"],
            "exclusive-flock-contended",
        )
        self.assertTrue(capacity_proof["queue_confirmed_while_leases_held"])
        self.assertTrue(capacity_proof["proof_established_before_gate"])
        self.assertTrue(capacity_proof["gate_opened_after_proof"])
        self.assertTrue(self.cohort_closed)
        self.assertEqual(self.evidence["status_coalescing"], {
            "concurrent_status_call_count": 16,
            "pueue_status_snapshot_count": 1,
            "shared_status_call_count": 15,
        })
        receipts = list((self.root / "dispatch-responses").glob("dispatch-[0-9][0-9][0-9].json"))
        self.assertEqual(len(receipts), self.total)
        saved_ids = {json.loads(path.read_text())["task_id"] for path in receipts}
        self.assertEqual(saved_ids, set(self.task_ids))
        self.assertEqual(
            json.loads((self.root / "dispatch-responses" / "dispatch-batch.json").read_text()),
            dispatch_batch,
        )
        self.assertEqual(
            json.loads((self.root / "dispatch-responses" / "capacity-proof.json").read_text()),
            capacity_proof,
        )
        self.assertEqual(
            json.loads((self.root / "dispatch-responses" / "status-coalescing.json").read_text()),
            self.evidence["status_coalescing"],
        )
        self.assertTrue(self.gate.exists())
        self.assertFalse(self.old_layout.exists())

    def test_small_status_cannot_pass_the_200_task_proof(self):
        self.status_bytes = 1024 * 1024
        with self.assertRaisesRegex(RuntimeError, "former 1 MiB status limit"):
            self.run_upgrade()
        self.assertEqual(self.worker_counts, [2, 2])
        self.assertEqual(len(self.dispatched), self.total)
        self.assertEqual(self.observed, set())
        self.assertEqual(self.collected, set())
        self.assertEqual(self.restarts, 0)
        self.assertFalse(self.gate.exists())
        self.assertTrue(self.old_layout.exists())

    def test_admitted_but_unobserved_task_cannot_pass_upgrade(self):
        self.upgraded_liveness = "undetermined"
        with self.assertRaisesRegex(RuntimeError, "did not observe its gated task running"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())
        self.assertEqual(self.restarts, 0)

    def test_admitted_running_with_diagnostic_cannot_pass_upgrade(self):
        self.upgraded_error = "pueue command remains in flight"
        with self.assertRaisesRegex(RuntimeError, "did not observe its gated task running"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_timed_out_running_task_cannot_pass_upgrade(self):
        self.upgraded_status = "timed_out"
        with self.assertRaisesRegex(RuntimeError, "did not observe its gated task running"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_status_without_admitted_admission_cannot_pass_upgrade(self):
        self.upgraded_admission = "unknown"
        with self.assertRaisesRegex(RuntimeError, "did not observe its gated task running"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_duplicate_capacity_queue_label_fails_before_gate_opens(self):
        self.duplicate_capacity_label = True
        with self.assertRaisesRegex(RuntimeError, "duplicate task label"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.observed, set())
        self.assertEqual(self.collected, set())
        self.assertTrue(self.old_layout.exists())

    def test_multiple_scheduled_status_cohorts_still_prove_coalescing(self):
        self.status_snapshot_owners = 2
        self.run_upgrade()
        self.assertEqual(self.evidence["status_coalescing"], {
            "concurrent_status_call_count": 16,
            "pueue_status_snapshot_count": 2,
            "shared_status_call_count": 14,
        })

    def test_uncoalesced_status_burst_fails_acceptance(self):
        self.status_snapshot_owners = 16
        with self.assertRaisesRegex(RuntimeError, "did not coalesce queue snapshots"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_missing_status_snapshot_fails_acceptance(self):
        self.status_snapshot_owners = 0
        with self.assertRaisesRegex(RuntimeError, "did not coalesce queue snapshots"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_incomplete_lifetime_owner_cohort_cannot_open_gate(self):
        self.owner_proof_count = self.total - 1
        with self.assertRaisesRegex(RuntimeError, "did not bind every admitted task"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_partial_dispatch_failure_retains_every_created_task_identity_once(self):
        self.failed_dispatch_index = 10

        with self.assertRaisesRegex(RuntimeError, "injected dispatch failure"):
            self.run_upgrade()

        expected = {self.task_ids[index] for index in self.dispatched}
        self.assertGreater(len(expected), 0)
        self.assertNotIn(self.task_ids[self.failed_dispatch_index], expected)
        self.assertEqual(set(self.attempted_task_ids), expected)
        self.assertEqual(len(self.attempted_task_ids), len(expected))
        self.assertFalse(self.gate.exists())

    def run_upgrade(self):
        def pool(max_workers):
            self.worker_counts.append(max_workers)
            return ThreadPoolExecutor(max_workers=max_workers)

        with patch.object(capacity_impl, "ThreadPoolExecutor", side_effect=pool), \
                patch.object(dispatch_impl, "ThreadPoolExecutor", side_effect=pool), \
                patch.object(capacity_impl, "run_json", side_effect=self.run_json), \
                patch.object(dispatch_impl, "run_json", side_effect=self.run_json), \
                patch.object(admission_impl, "run_json", side_effect=self.run_json), \
                patch.object(capacity_impl, "pueue_snapshot", side_effect=self.snapshot), \
                patch.object(capacity_impl, "capacity_lease_specifications",
                             side_effect=self.lease_specifications), \
                patch.object(private_cli.LiveFIFOLeaseCohort, "acquire",
                             side_effect=self.acquire_cohort), \
                patch.object(capacity_impl, "prove_lifetime_owner_cohort",
                             side_effect=self.prove_owner_cohort), \
                patch.object(capacity_impl, "shutdown_private_supervisor", side_effect=self.stop), \
                patch.object(private_cli.time, "sleep"), redirect_stdout(io.StringIO()):
            return private_cli.verify_current_installation_upgrade(
                delegate=Path("/fixture/delegate"), state=self.state, workspace=self.root, brief=self.brief,
                release_gate=self.release_gate,
                waiter_directory=self.waiters, env={},
                old_layout=self.old_layout, old_layout_owned=self.old_layout_owned,
                client=Path("/fixture/pueue"), config=Path("/fixture/pueue.yml"),
                parallel_tasks=self.total, dispatch_workers=2, response_directory=self.root / "dispatch-responses",
                evidence=self.evidence,
                lease_nonce="e" * 32,
                client_call_log=self.client_call_log,
                attempted_task_ids=self.attempted_task_ids,
                capture_root=self.capture_root,
            )

    def lease_specifications(self, state, responses, waiter_directory, nonce):
        self.assertEqual(state, self.state)
        self.assertEqual(waiter_directory, self.waiters)
        self.assertEqual(len(responses), self.total)
        self.assertEqual(nonce, "e" * 32)
        return [
            {
                "task_id": task_id,
                "root_id": "b" * 32,
                "pid": index + 1,
                "path": self.lease_candidates[index],
                "owner_lock": self.root / (task_id + ".lock"),
                "owner_sha256": "%064x" % (index + 1),
                "registration_sha256": "%064x" % (index + 201),
                "scope_kind": "process-group",
                "assurance": "cooperative",
                "scope_plan_sha256": "%064x" % (index + 401),
                "scope_id_sha256": "%064x" % (index + 601),
            }
            for index, task_id in enumerate(self.task_ids)
        ]

    def prove_owner_cohort(self, specifications, cohort):
        self.assertEqual(len(specifications), self.total)
        cohort.assert_live()
        counted = specifications[:self.owner_proof_count]
        return {
            "started_unix_ns": 1,
            "ended_unix_ns": 3,
            "duration_ns": 2,
            "lease_probe": "exclusive-flock-contended",
            "counted_task_count": len(counted),
            "counted_tasks": [{
                "root_id": specification["root_id"],
                "task_id": specification["task_id"],
                "owner_sha256": specification["owner_sha256"],
                "registration_sha256": specification["registration_sha256"],
                "scope_kind": specification["scope_kind"],
                "assurance": specification["assurance"],
                "scope_plan_sha256": specification["scope_plan_sha256"],
                "scope_id_sha256": specification["scope_id_sha256"],
                "owner_lock": {"device": 1, "inode": index + 1},
            } for index, specification in enumerate(counted)],
        }

    def acquire_cohort(self, specifications, nonce):
        self.assertEqual(len(specifications), self.total)
        self.assertEqual(nonce, "e" * 32)
        owner = self

        class Cohort:
            def __init__(self):
                self.leases = [object()] * owner.total

            def assert_live(self):
                return None

            def close(self):
                owner.cohort_closed = True
                self.leases = []

        return Cohort()

    def run_json(self, _delegate, _env, args, receipt=None, **_options):
        self.assertNotIn("--dispatch-workers", args)
        command = args[2]
        if command == "dispatch":
            response = self.dispatch(args, receipt)
        elif command == "status":
            task_id = args[3]
            upgraded = receipt is not None and receipt.name.startswith("status-after-upgrade-")
            if upgraded:
                self.assertTrue(self.all_running)
                self.assertFalse(self.gate.exists())
                self.assertFalse(self.old_layout.exists())
                with self.lock:
                    self.assertNotIn(task_id, self.observed)
                    self.observed.add(task_id)
                    if len(self.observed) <= self.status_snapshot_owners:
                        with self.client_call_log.open("a") as calls:
                            calls.write("managed-runner-observation\tclient\t"
                                        "-c /fixture/pueue.yml status --json\n")
            response = {"task_id": task_id,
                        "admission": self.upgraded_admission if upgraded else "admitted",
                        "liveness": "running", "supervisor": {
                            "matched": True,
                            "state": self.upgraded_liveness if upgraded else "running"}}
            if upgraded and self.upgraded_error:
                response["error"] = self.upgraded_error
            if upgraded and self.upgraded_status:
                response["status"] = self.upgraded_status
        else:
            self.assertEqual(command, "collect")
            self.assertTrue(self.gate.exists())
            self.assertEqual(self.observed, set(self.task_ids[:16]))
            if args[3] != self.restart_id:
                with self.lock:
                    self.assertNotIn(args[3], self.collected)
                    self.collected.add(args[3])
            response = {"outcome": {"verdict": "committed"}}
        if receipt is not None:
            receipt.write_text(json.dumps(response))
        return response

    def dispatch(self, args, receipt):
        if receipt is not None and receipt.stem == "dispatch-restarted":
            self.assertTrue(self.shutdown)
            self.assertTrue(self.gate.exists())
            self.assertEqual(self.collected, set(self.task_ids))
            self.restarts += 1
            task_id = self.restart_id
        else:
            self.assertFalse(self.gate.exists())
            self.assertEqual(args[args.index("--budget") + 1], private_cli.CAPACITY_TASK_BUDGET)
            waiting_brief = Path(args[args.index("--brief") + 1])
            self.assertEqual(waiting_brief.read_text(), "DELEGATE_PRIVATE_UPGRADE_WAIT\n")
            index = int(receipt.stem.removeprefix("dispatch-"))
            if index == self.failed_dispatch_index:
                raise RuntimeError("injected dispatch failure")
            with self.lock:
                self.assertNotIn(index, self.dispatched, "dispatch response was retried")
                self.dispatched.add(index)
                wait_for_burst = len(self.dispatched) <= 2
            if wait_for_burst:
                self.dispatch_burst_barrier.wait(timeout=5)
            task_id = self.task_ids[index]
        return {"task_id": task_id, "root_id": "b" * 32,
                "task_record": "created", "admission": "admitted"}

    def snapshot(self, _client, _config, _env, metrics, phase=None, **_options):
        self.assertEqual(len(self.dispatched), self.total)
        self.assertFalse(self.gate.exists())
        self.assertTrue(self.old_layout.exists())
        self.snapshots += 1
        self.all_running = self.snapshots > 1
        metrics["status_bytes"] = self.status_bytes
        tasks = {
            str(index): {
                "label": "delegate:" + "b" * 32 + ":" + task_id,
                "status": {"Running" if self.all_running or index < self.total - 1 else "Queued": {}},
            }
            for index, task_id in enumerate(self.task_ids)
        }
        if self.duplicate_capacity_label:
            tasks["duplicate"] = {
                "label": "delegate:" + "b" * 32 + ":" + self.task_ids[0],
                "status": {"Running": {}},
            }
        return {"tasks": tasks}

    def stop(self, _client, _config, _env, **_kwargs):
        self.assertEqual(self.collected, set(self.task_ids))
        self.assertEqual(_kwargs["state"], self.state)
        self.assertEqual(set(_kwargs["task_ids"]), set(self.task_ids))
        self.shutdown = True
