"""Implementation methods for PrivateGuardianEvidenceTests; collected once by the acceptance_tests discovery."""

import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli


class PrivateGuardianEvidenceTests(unittest.TestCase):
    def test_lease_candidates_require_entry_publication_without_claiming_safety(self):
        missing = {"path": self.waiters / "not-published.fifo"}
        present = {"path": self.waiters / "published.fifo"}
        unsafe = {"path": self.waiters / "regular-file"}
        os.mkfifo(present["path"], 0o600)
        unsafe["path"].touch()
        self.assertEqual(
            private_cli.lease_candidate_specifications([missing, present, unsafe]),
            [present, unsafe],
        )
        with self.assertRaisesRegex(RuntimeError, "lease is unsafe"):
            private_cli.LiveFIFOLease.acquire("a" * 32, 102, unsafe["path"], "c" * 32)

    def test_lease_candidate_read_errors_remain_explicit(self):
        with patch.object(Path, "lstat", side_effect=PermissionError("entry unreadable")):
            with self.assertRaisesRegex(PermissionError, "entry unreadable"):
                private_cli.lease_candidate_specifications([{"path": self.waiters / "fifo"}])

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-guardian-proof-")
        self.addCleanup(temporary.cleanup)
        self.state = Path(temporary.name) / "state"
        self.root_id = "b" * 32
        self.task_id = "a" * 32
        self.response = {"root_id": self.root_id, "task_id": self.task_id}
        self.lease_nonce = "c" * 32
        self.lifetime = self.state / "tasks" / self.task_id / "lifetime"
        self.lifetime.mkdir(parents=True)
        self.waiters = self.state.parent / "waiters"
        self.waiters.mkdir()

    def write_active_records(self, launch_count=1):
        common = {"schema_version": 1, "root_id": self.root_id, "task_id": self.task_id}
        task_bytes = b'{"schema_version":1,"task":"capacity"}\n'
        task_digest = hashlib.sha256(task_bytes).hexdigest()
        meta_bytes = b'{"schema_version":1,"runner_ownership":"managed"}\n'
        meta_digest = hashlib.sha256(meta_bytes).hexdigest()
        owner_digest = private_cli.ordinary_lifetime_owner_digest(
            task_digest, meta_digest,
        )
        intent = {
            **common, "boot_id": "boot-token", "owner_kind": "task",
            "owner_sha256": owner_digest, "scope_kind": "process-group",
            "assurance": "cooperative", "scope_plan": "process-group-plan",
        }
        registration = {
            **common, "boot_id": "boot-token", "owner_kind": "task",
            "owner_sha256": owner_digest, "scope_kind": "process-group",
            "assurance": "cooperative", "scope_id": "process-group-scope",
        }
        registration_bytes = (json.dumps(registration, sort_keys=True) + "\n").encode()
        registration_digest = hashlib.sha256(registration_bytes).hexdigest()
        worker = {
            **common, "boot_id": "boot-token", "registration_sha256": registration_digest,
            "pid": 101, "process_start": "worker-start",
        }
        (self.lifetime.parent / "task.json").write_bytes(task_bytes)
        (self.lifetime.parent / "meta.json").write_bytes(meta_bytes)
        (self.lifetime / "intent.json").write_text(json.dumps(intent) + "\n")
        (self.lifetime / "registration.json").write_bytes(registration_bytes)
        (self.lifetime / "worker.json").write_text(json.dumps(worker) + "\n")
        (self.lifetime / "owner.lock").touch()
        for sequence in range(1, launch_count + 1):
            launch = {
                **common, "registration_sha256": registration_digest, "sequence": sequence,
                "pid": 101 + sequence, "process_start": "provider-start-%d" % sequence,
            }
            (self.lifetime / ("%020d.launch.json" % sequence)).write_text(
                json.dumps(launch) + "\n"
            )

    def test_active_guardian_requires_consistent_nonterminal_launch_evidence(self):
        self.assertEqual(
            private_cli.capacity_lease_specifications(
                self.state, [self.response], self.waiters, self.lease_nonce,
            ), []
        )
        self.write_active_records(launch_count=3)
        specifications = private_cli.capacity_lease_specifications(
            self.state, [self.response], self.waiters, self.lease_nonce,
        )
        self.assertEqual(len(specifications), 1)
        self.assertEqual(specifications[0]["pid"], 104)
        self.assertEqual(
            specifications[0]["path"],
            self.waiters / (self.lease_nonce + "-104.fifo"),
        )
        self.assertEqual(specifications[0]["scope_kind"], "process-group")
        self.assertEqual(specifications[0]["assurance"], "cooperative")
        self.assertEqual(
            specifications[0]["owner_lock"], self.lifetime / "owner.lock",
        )
        (self.lifetime / "terminated.json").write_text("{}\n")
        with self.assertRaisesRegex(RuntimeError, "terminated before"):
            private_cli.capacity_lease_specifications(
                self.state, [self.response], self.waiters, self.lease_nonce,
            )

    def test_active_guardian_rejects_changed_identity(self):
        self.write_active_records()
        launch_path = self.lifetime / "00000000000000000001.launch.json"
        launch = json.loads(launch_path.read_text())
        launch["task_id"] = "c" * 32
        launch_path.write_text(json.dumps(launch) + "\n")
        with self.assertRaisesRegex(RuntimeError, "changed identity"):
            private_cli.capacity_lease_specifications(
                self.state, [self.response], self.waiters, self.lease_nonce,
            )

    def test_stale_pid_marker_is_not_a_capacity_lease(self):
        self.write_active_records()
        stale_marker = self.waiters / "102"
        stale_marker.touch()

        specifications = private_cli.capacity_lease_specifications(
            self.state, [self.response], self.waiters, self.lease_nonce,
        )

        self.assertEqual(len(specifications), 1)
        self.assertNotEqual(specifications[0]["path"], stale_marker)
        self.assertFalse(specifications[0]["path"].exists())
