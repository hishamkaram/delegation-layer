"""Implementation methods for PrivateGuardianEvidenceTests; collected once by the acceptance package discovery."""

import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import sys


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.evidence import encoding as evidence_encoding
from scripts.acceptance.lifetime import identity as lifetime_identity
from scripts.acceptance.private_cli.capacity import ownership as private_cli_capacity_ownership
from scripts.acceptance.private_cli.capacity import records as private_cli_capacity_records
from scripts.acceptance.private_cli import leases as private_cli_leases


class PrivateGuardianEvidenceTests(unittest.TestCase):
    def test_lease_candidates_require_entry_publication_without_claiming_safety(self):
        missing = {"path": self.waiters / "not-published.fifo"}
        present = {"path": self.waiters / "published.fifo"}
        unsafe = {"path": self.waiters / "regular-file"}
        os.mkfifo(present["path"], 0o600)
        unsafe["path"].touch()
        self.assertEqual(
            private_cli_capacity_ownership.lease_candidate_specifications([missing, present, unsafe]),
            [present, unsafe],
        )
        with self.assertRaisesRegex(RuntimeError, "lease is unsafe"):
            private_cli_leases.LiveFIFOLease.acquire("a" * 32, 102, unsafe["path"], "c" * 32)

    def test_lease_candidate_read_errors_remain_explicit(self):
        with patch.object(Path, "lstat", side_effect=PermissionError("entry unreadable")):
            with self.assertRaisesRegex(PermissionError, "entry unreadable"):
                private_cli_capacity_ownership.lease_candidate_specifications([{"path": self.waiters / "fifo"}])

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
        owner_digest = lifetime_identity.ordinary_lifetime_owner_digest(
            task_digest, meta_digest,
        )
        intent = {
            **common, "boot_id": "boot-token", "owner_kind": "task",
            "owner_sha256": owner_digest, "scope_kind": "process-group",
            "assurance": "cooperative", "scope_plan": "process-group-plan",
            "deadline": "2026-09-15T00:00:30Z",
        }
        registration = {
            **common, "boot_id": "boot-token", "owner_kind": "task",
            "owner_sha256": owner_digest, "scope_kind": "process-group",
            "assurance": "cooperative", "scope_id": "process-group-scope",
            "deadline": "2026-09-15T00:00:30Z",
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
                "started_at": "2026-09-15T00:00:05Z",
            }
            (self.lifetime / ("%020d.launch.json" % sequence)).write_text(
                json.dumps(launch) + "\n"
            )

    def test_active_guardian_requires_consistent_nonterminal_launch_evidence(self):
        self.assertEqual(
            private_cli_capacity_records.capacity_lease_specifications(
                self.state, [self.response], self.waiters, self.lease_nonce,
            ), []
        )
        self.write_active_records(launch_count=3)
        specifications = private_cli_capacity_records.capacity_lease_specifications(
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
        self.assertEqual(specifications[0]["deadline"], "2026-09-15T00:00:30Z")
        self.assertEqual(
            specifications[0]["owner_lock"], self.lifetime / "owner.lock",
        )
        (self.lifetime / "terminated.json").write_text("{}\n")
        with self.assertRaisesRegex(RuntimeError, "terminated before"):
            private_cli_capacity_records.capacity_lease_specifications(
                self.state, [self.response], self.waiters, self.lease_nonce,
            )

    def test_active_guardian_rejects_changed_identity(self):
        self.write_active_records()
        launch_path = self.lifetime / "00000000000000000001.launch.json"
        launch = json.loads(launch_path.read_text())
        launch["task_id"] = "c" * 32
        launch_path.write_text(json.dumps(launch) + "\n")
        with self.assertRaisesRegex(RuntimeError, "changed identity"):
            private_cli_capacity_records.capacity_lease_specifications(
                self.state, [self.response], self.waiters, self.lease_nonce,
            )

    def test_stale_pid_marker_is_not_a_capacity_lease(self):
        self.write_active_records()
        stale_marker = self.waiters / "102"
        stale_marker.touch()

        specifications = private_cli_capacity_records.capacity_lease_specifications(
            self.state, [self.response], self.waiters, self.lease_nonce,
        )

        self.assertEqual(len(specifications), 1)
        self.assertNotEqual(specifications[0]["path"], stale_marker)
        self.assertFalse(specifications[0]["path"].exists())

    def capacity_specifications(self):
        return private_cli_capacity_records.capacity_lease_specifications(
            self.state, [self.response], self.waiters, self.lease_nonce,
        )

    def test_capacity_accepts_launch_65_under_the_shared_count(self):
        self.write_active_records(launch_count=65)
        specifications = self.capacity_specifications()
        self.assertEqual([specification["pid"] for specification in specifications], [166])

    def test_capacity_refuses_launch_4097_before_record_reads(self):
        self.write_active_records()
        for sequence in range(2, 4098):
            (self.lifetime / ("%020d.launch.json" % sequence)).touch()
        with patch(
            "scripts.acceptance.private_cli.capacity.records._read_control_record",
            side_effect=AssertionError("launch record was read"),
        ) as read:
            with self.assertRaisesRegex(RuntimeError, "invalid lifetime launch set"):
                self.capacity_specifications()
        read.assert_not_called()

    def test_capacity_refuses_gapped_or_malformed_launch_names(self):
        self.write_active_records(launch_count=2)
        refused = self.lifetime / "00000000000000000002.launch.json"
        for name in ("1.launch.json", "00000000000000000000.launch.json",
                     "00000000000000004097.launch.json", "00000000000000000003.launch.json"):
            with self.subTest(name=name):
                refused = refused.rename(self.lifetime / name)
                with patch(
                    "scripts.acceptance.private_cli.capacity.records._read_control_record",
                    side_effect=AssertionError("launch record was read"),
                ) as read:
                    with self.assertRaisesRegex(RuntimeError, "invalid lifetime launch set"):
                        self.capacity_specifications()
                read.assert_not_called()

    def test_capacity_enforces_the_shared_canonical_byte_budget(self):
        self.write_active_records(launch_count=2)
        launches = [
            json.loads((self.lifetime / ("%020d.launch.json" % sequence)).read_text())
            for sequence in (1, 2)
        ]
        exact = sum(len(evidence_encoding.canonical_go_json(launch)) for launch in launches)
        target = "scripts.acceptance.private_cli.capacity.records.MAX_LIFETIME_LAUNCH_BYTES"
        with patch(target, exact):
            self.assertEqual(len(self.capacity_specifications()), 1)
        with patch(target, exact - 1):
            with self.assertRaisesRegex(RuntimeError, "invalid lifetime launch set"):
                self.capacity_specifications()

    def test_capacity_refuses_a_launch_whose_escaping_exceeds_the_record_bound(self):
        self.write_active_records()
        path = self.lifetime / "00000000000000000001.launch.json"
        launch = json.loads(path.read_text())
        for process_start, refused in (("<" * 180000, True), ("x" * 180000, False)):
            with self.subTest(refused=refused):
                launch["process_start"] = process_start
                path.write_text(json.dumps(launch, separators=(",", ":")))
                if refused:
                    with self.assertRaisesRegex(RuntimeError, "invalid lifetime launch set"):
                        self.capacity_specifications()
                else:
                    self.assertEqual(len(self.capacity_specifications()), 1)
