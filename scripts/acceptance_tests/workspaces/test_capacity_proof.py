"""Fault tests for complete workspace-capacity receipt evidence."""

from __future__ import annotations

import hashlib
from pathlib import Path
import unittest

from scripts.delegate_acceptance.workspaces.scenarios.evidence import ReceiptFinalizer, _compact
from scripts.delegate_acceptance.workspaces.scenarios.policy import validate_capacity_lifetime_proof


ROOT_ID = "f" * 32


def _sha(index: int) -> str:
    return f"{index:064x}"


def _lifetime_records(identity: str, index: int) -> dict[str, object]:
    owner_sha = _sha(index + 1)
    registration_sha = _sha(index + 1001)
    scope_plan = "scope-plan-" + identity
    scope_id = "scope-id-" + identity
    return {
        "intent": {
            "root_id": ROOT_ID,
            "task_id": identity,
            "owner_sha256": owner_sha,
            "scope_plan": scope_plan,
        },
        "registration": {
            "root_id": ROOT_ID,
            "task_id": identity,
            "owner_sha256": owner_sha,
            "scope_kind": "cgroup-v2",
            "assurance": "exact",
            "scope_id": scope_id,
        },
        "worker": {
            "root_id": ROOT_ID,
            "task_id": identity,
            "registration_sha256": registration_sha,
        },
        "worker_wait": {
            "root_id": ROOT_ID,
            "task_id": identity,
            "registration_sha256": registration_sha,
        },
        "termination": {
            "root_id": ROOT_ID,
            "task_id": identity,
            "registration_sha256": registration_sha,
        },
        "launches": [{
            "root_id": ROOT_ID,
            "task_id": identity,
            "registration_sha256": registration_sha,
            "sequence": 1,
        }],
    }


def _capacity_evidence(count: int) -> dict[str, object]:
    task_ids = [f"{index:032x}" for index in range(1, count + 1)]
    lease_records = []
    closure_tasks = {}
    workspace_operations = {}
    settled = []
    released = []
    for index, task_id in enumerate(task_ids):
        lifetime = _lifetime_records(task_id, index)
        intent = lifetime["intent"]
        registration = lifetime["registration"]
        worker = lifetime["worker"]
        scope_plan = intent["scope_plan"]
        scope_id = registration["scope_id"]
        lease_records.append({
            "root_id": ROOT_ID,
            "task_id": task_id,
            "owner_sha256": intent["owner_sha256"],
            "registration_sha256": worker["registration_sha256"],
            "scope_kind": registration["scope_kind"],
            "assurance": registration["assurance"],
            "scope_plan_sha256": hashlib.sha256(scope_plan.encode()).hexdigest(),
            "scope_id_sha256": hashlib.sha256(scope_id.encode()).hexdigest(),
            "owner_lock": {"device": 1, "inode": index + 1},
        })
        closure_tasks[task_id] = lifetime
        operation_id = f"{10_000 + index:032x}"
        workspace_operations[operation_id] = {
            "mode": "terminated",
            "request_sha256": _sha(index + 2001),
            "managed_lifetime": _lifetime_records(operation_id, index + 3000),
        }
        settled.append({
            "task_id": task_id,
            "status": "done",
            "collection_status": "collected",
        })
        released.append({
            "task_id": task_id,
            "release": {"release_state": "released", "release_operation": operation_id},
            "forget": {"forget_state": "forgotten", "forget_operation": operation_id},
        })
    lease_proof = {
        "started_unix_ns": 200,
        "ended_unix_ns": 300,
        "duration_ns": 100,
        "lease_probe": "exclusive-flock-contended",
        "counted_task_count": count,
        "counted_tasks": lease_records,
    }
    overlap = {
        "started_unix_ns": 100,
        "ended_unix_ns": 400,
        "duration_ns": 300,
        "task_ids": list(task_ids),
        "lease_cohort_asserted_live_before_and_after": True,
        "live_lease_members": [
            {"task_id": task_id, "pid": index + 5000}
            for index, task_id in enumerate(task_ids)
        ],
        "owner_lock_probe_task_ids": list(task_ids),
    }
    closure = {
        "root_id": ROOT_ID,
        "tasks": closure_tasks,
        "workspace": workspace_operations,
    }
    return {
        "task_ids": task_ids,
        "lease_proof": lease_proof,
        "overlap": overlap,
        "closure": closure,
        "settled": settled,
        "released": released,
    }


def _validate(evidence: dict[str, object]) -> None:
    validate_capacity_lifetime_proof(
        evidence["task_ids"], evidence["lease_proof"], evidence["overlap"],
        evidence["closure"], evidence["settled"], evidence["released"],
    )


class CapacityProofTests(unittest.TestCase):
    def test_retains_every_task_and_shared_interval_in_bounded_receipt(self) -> None:
        evidence = _capacity_evidence(200)
        _validate(evidence)
        finalizer = ReceiptFinalizer(Path("receipt.json"), [], {})
        native_proof = {
            "scenario": "capacity",
            "counted_task_count": 200,
            "lifetime_owner_proof": evidence["lease_proof"],
            "shared_live_overlap": evidence["overlap"],
            "lifetime_closure": evidence["closure"],
            "settled_tasks": evidence["settled"],
            "released_workspaces": evidence["released"],
        }
        capacity_result = {
            "capacity_lifetime_proof": {
                "lifetime_owner_proof": evidence["lease_proof"],
                "shared_live_overlap": evidence["overlap"],
                "lifetime_closure": evidence["closure"],
            },
            "settled_tasks": evidence["settled"],
            "released_workspaces": evidence["released"],
        }
        finalizer.set_details(native_proofs=[native_proof], capacity=capacity_result)

        payload = finalizer._payload("passed", "fixture", {"authorized": True})
        recorded = payload["details"]
        recorded_proof = recorded["native_proofs"][0]
        self.assertEqual(len(recorded_proof["lifetime_owner_proof"]["counted_tasks"]), 200)
        self.assertEqual(
            {item["task_id"] for item in recorded_proof["shared_live_overlap"]["live_lease_members"]},
            set(evidence["task_ids"]),
        )
        self.assertEqual(set(recorded_proof["lifetime_closure"]["tasks"]), set(evidence["task_ids"]))
        self.assertEqual(len(recorded_proof["lifetime_closure"]["workspace"]), 200)
        self.assertEqual(
            recorded_proof["shared_live_overlap"]["started_unix_ns"], 100,
        )

    def test_rejects_a_missing_counted_task_owner_record(self) -> None:
        evidence = _capacity_evidence(2)
        evidence["lease_proof"]["counted_tasks"].pop()
        with self.assertRaisesRegex(RuntimeError, "lease proof task set is incomplete"):
            _validate(evidence)

    def test_rejects_a_missing_counted_task_closure(self) -> None:
        evidence = _capacity_evidence(2)
        evidence["closure"]["tasks"].pop(evidence["task_ids"][-1])
        with self.assertRaisesRegex(RuntimeError, "closure task set is incomplete"):
            _validate(evidence)

    def test_rejects_an_incomplete_shared_live_lease_interval(self) -> None:
        evidence = _capacity_evidence(2)
        evidence["overlap"]["live_lease_members"].pop()
        with self.assertRaisesRegex(RuntimeError, "live lease cohort is incomplete"):
            _validate(evidence)

    def test_rejects_owner_proof_detached_from_native_scope_records(self) -> None:
        evidence = _capacity_evidence(2)
        evidence["lease_proof"]["counted_tasks"][0]["scope_id_sha256"] = _sha(9999)
        with self.assertRaisesRegex(RuntimeError, "differs from its closed task proof"):
            _validate(evidence)

    def test_rejects_overlap_interval_that_does_not_enclose_owner_probe(self) -> None:
        evidence = _capacity_evidence(2)
        evidence["overlap"]["ended_unix_ns"] = 250
        with self.assertRaisesRegex(RuntimeError, "shared live-overlap interval is incomplete"):
            _validate(evidence)

    def test_rejects_compacted_closure_markers_and_overbound_nesting(self) -> None:
        evidence = _capacity_evidence(1)
        evidence["closure"]["tasks"][evidence["task_ids"][0]]["worker_wait"]["nested"] = (
            "<depth-limit>"
        )
        with self.assertRaisesRegex(RuntimeError, "compacted evidence"):
            _validate(evidence)

        nested: object = "leaf"
        for _ in range(70):
            nested = [nested]
        with self.assertRaisesRegex(RuntimeError, "receipt nesting exceeds bound"):
            _compact(nested)

    def test_rejects_missing_per_task_owner_lock_probe(self) -> None:
        evidence = _capacity_evidence(2)
        evidence["overlap"]["owner_lock_probe_task_ids"].pop()
        with self.assertRaisesRegex(RuntimeError, "owner-lock probes"):
            _validate(evidence)


if __name__ == "__main__":
    unittest.main()
