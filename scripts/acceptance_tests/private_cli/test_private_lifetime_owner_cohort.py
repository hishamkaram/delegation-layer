"""Implementation methods for PrivateLifetimeOwnerCohortTests; collected once by the acceptance_tests discovery."""

from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from scripts import acceptance_private_cli as private_cli


class PrivateLifetimeOwnerCohortTests(unittest.TestCase):
    class Cohort:
        def __init__(self):
            self.assertions = 0

        def assert_live(self):
            self.assertions += 1

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-owner-cohort-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.lock_path = self.root / "owner.lock"
        self.lock_path.touch(mode=0o600)

    def specification(self):
        return {
            "root_id": "f" * 32,
            "task_id": "a" * 32,
            "owner_lock": self.lock_path,
            "owner_sha256": "b" * 64,
            "registration_sha256": "c" * 64,
            "scope_kind": "process-group",
            "assurance": "cooperative",
            "scope_plan_sha256": "d" * 64,
            "scope_id_sha256": "e" * 64,
        }

    @staticmethod
    def stop_holder(holder):
        if holder.poll() is None:
            holder.terminate()
        try:
            holder.wait(timeout=3)
        except subprocess.TimeoutExpired:
            holder.kill()
            holder.wait(timeout=3)

    def start_holder(self):
        ready = self.root / "ready"
        holder = subprocess.Popen([
            sys.executable, "-c",
            "import fcntl,pathlib,sys,time; "
            "stream=open(sys.argv[1],'r+'); "
            "fcntl.flock(stream.fileno(),fcntl.LOCK_EX); "
            "pathlib.Path(sys.argv[2]).write_text('ready'); time.sleep(30)",
            str(self.lock_path), str(ready),
        ])
        self.addCleanup(self.stop_holder, holder)
        deadline = time.monotonic() + 5
        while not ready.is_file():
            if holder.poll() is not None:
                self.fail("lifetime-owner lock holder exited before readiness")
            if time.monotonic() >= deadline:
                self.fail("lifetime-owner lock holder did not become ready")
            time.sleep(0.01)
        return holder

    def test_live_owner_lock_binds_native_scope_evidence(self):
        self.start_holder()
        cohort = self.Cohort()

        proof = private_cli.prove_lifetime_owner_cohort(
            [self.specification()], cohort,
        )

        self.assertGreaterEqual(proof["started_unix_ns"], 1)
        self.assertGreaterEqual(proof["ended_unix_ns"], proof["started_unix_ns"])
        self.assertGreaterEqual(proof["duration_ns"], 0)
        self.assertEqual(proof["lease_probe"], "exclusive-flock-contended")
        self.assertEqual(proof["counted_task_count"], 1)
        self.assertGreaterEqual(cohort.assertions, 2)
        self.assertEqual(proof["counted_tasks"], [{
            "root_id": "f" * 32,
            "task_id": "a" * 32,
            "owner_sha256": "b" * 64,
            "registration_sha256": "c" * 64,
            "scope_kind": "process-group",
            "assurance": "cooperative",
            "scope_plan_sha256": "d" * 64,
            "scope_id_sha256": "e" * 64,
            "owner_lock": {
                "device": self.lock_path.stat().st_dev,
                "inode": self.lock_path.stat().st_ino,
            },
        }])

    def test_unlocked_owner_file_cannot_prove_live_ownership(self):
        with self.assertRaisesRegex(RuntimeError, "no live lifetime owner"):
            private_cli.prove_lifetime_owner_cohort(
                [self.specification()], self.Cohort(),
            )
