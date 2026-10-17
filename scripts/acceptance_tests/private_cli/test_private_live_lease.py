"""Implementation methods for PrivateLiveLeaseTests; collected once by the acceptance_tests discovery."""

import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.delegate_acceptance.directories import operations as directory_owner_impl
from scripts.delegate_acceptance.private_cli import leases as leases_impl


class PrivateLiveLeaseTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-live-lease-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.nonce = "d" * 32

    def start_fifo_writer(self, fifo):
        script = (
            "import os,sys\n"
            "fifo,nonce=sys.argv[1:]\n"
            "try:\n"
            " fd=os.open(fifo,os.O_WRONLY)\n"
            "except FileNotFoundError:\n"
            " sys.exit(0)\n"
            "os.write(fd,(nonce+'\\t'+str(os.getpid())+'\\n').encode())\n"
            "sys.stdin.buffer.read(1)\n"
            "os.close(fd)\n"
        )
        process = subprocess.Popen(
            [sys.executable, "-c", script, str(fifo), self.nonce],
            stdin=subprocess.PIPE,
        )
        self.addCleanup(self.reap, process)
        return process

    def start_fifo_mock_writer_cohort(self, fifos):
        script = (
            "import os,sys,time\n"
            "nonce=sys.argv[1]\n"
            "fds=[]\n"
            "for fifo in sys.argv[2:]:\n"
            " fd=os.open(fifo,os.O_RDWR|os.O_NONBLOCK)\n"
            " fds.append(fd)\n"
            " os.write(fd,(nonce+'\\t'+str(os.getpid())+'\\n').encode())\n"
            "try:\n"
            " time.sleep(30)\n"
            "finally:\n"
            " for fd in fds:\n"
            "  os.close(fd)\n"
        )
        process = subprocess.Popen(
            [sys.executable, "-c", script, self.nonce, *(str(fifo) for fifo in fifos)],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        self.addCleanup(self.reap, process)
        return process

    def test_stale_fifo_without_writer_fails_and_closes_its_descriptor(self):
        fifo = self.root / "stale.fifo"
        os.mkfifo(fifo, 0o600)
        opened = []
        real_open = private_cli.os.open

        def recording_open(*args, **kwargs):
            descriptor = real_open(*args, **kwargs)
            opened.append(descriptor)
            return descriptor

        with patch.object(directory_owner_impl, "_require_fd_cleanup_support"), \
                patch.object(private_cli.os, "open", side_effect=recording_open):
            with self.assertRaisesRegex(RuntimeError, "no live writer"):
                private_cli.LiveFIFOLease.acquire(
                    "a" * 32, 101, fifo, self.nonce, timeout=0.03,
                )
        self.assertEqual(len(opened), 2)
        for descriptor in opened:
            with self.assertRaises(OSError):
                os.fstat(descriptor)

    def test_pi_fixture_holds_nonce_bound_fifo_until_gate_release(self):
        install = self.root / "bin"
        install.mkdir()
        waiters = self.root / "waiters"
        waiters.mkdir()
        gate = self.root / "gate"
        release_gate = private_cli.LockedReleaseGate.acquire(gate)
        self.addCleanup(release_gate.release)
        private_cli.install_pi_fixture(install, gate, waiters, self.nonce)
        process = subprocess.Popen(
            [str(install / "pi"), "--mode", "json"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        self.addCleanup(self.reap, process)
        process.stdin.write(b"DELEGATE_PRIVATE_UPGRADE_WAIT\n")
        process.stdin.close()
        process.stdin = None
        fifo = private_cli.lease_fifo_path(waiters, self.nonce, process.pid)
        for _ in range(200):
            if fifo.exists():
                break
            private_cli.time.sleep(0.01)
        self.assertTrue(fifo.exists(), "fixture did not create its lease FIFO")

        lease = private_cli.LiveFIFOLease.acquire(
            "a" * 32, process.pid, fifo, self.nonce, timeout=2,
        )
        try:
            self.assertIsNone(process.poll())
            lease.assert_live()
            release_gate.release()
            stdout, stderr = process.communicate(timeout=3)
            self.assertEqual(process.returncode, 0, stderr.decode(errors="replace"))
            self.assertIn(b"private supervisor acceptance passed", stdout)
        finally:
            lease.close()
        self.assertFalse(fifo.exists())

    def test_pi_fixture_gate_releases_writer_before_lease_reader_exists(self):
        install = self.root / "bin"
        install.mkdir()
        waiters = self.root / "waiters"
        waiters.mkdir()
        gate = self.root / "gate"
        release_gate = private_cli.LockedReleaseGate.acquire(gate)
        self.addCleanup(release_gate.release)
        private_cli.install_pi_fixture(install, gate, waiters, self.nonce)
        process = subprocess.Popen(
            [str(install / "pi"), "--mode", "json"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        self.addCleanup(self.reap, process)
        process.stdin.write(b"DELEGATE_PRIVATE_UPGRADE_WAIT\n")
        process.stdin.close()
        process.stdin = None
        fifo = private_cli.lease_fifo_path(waiters, self.nonce, process.pid)
        for _ in range(200):
            if fifo.exists():
                break
            private_cli.time.sleep(0.01)
        self.assertTrue(fifo.exists(), "fixture did not create its lease FIFO")

        release_gate.release()
        stdout, stderr = process.communicate(timeout=3)
        self.assertEqual(process.returncode, 0, stderr.decode(errors="replace"))
        self.assertIn(b"private supervisor acceptance passed", stdout)
        self.assertFalse(fifo.exists())

    def test_cohort_removes_owned_fifos_while_original_pins_remain_live(self):
        first_fifo = self.root / "first-owned.fifo"
        second_fifo = self.root / "second-owned.fifo"
        os.mkfifo(first_fifo, 0o600)
        os.mkfifo(second_fifo, 0o600)
        first = self.start_fifo_writer(first_fifo)
        second = self.start_fifo_writer(second_fifo)
        cohort = private_cli.LiveFIFOLeaseCohort.acquire([
            {"task_id": "a" * 32, "pid": first.pid, "path": first_fifo},
            {"task_id": "b" * 32, "pid": second.pid, "path": second_fifo},
        ], self.nonce, timeout=2)
        self.addCleanup(cohort.close)
        leases = tuple(cohort.leases)
        self.assertEqual(len(cohort.parent_pins), 1)
        parent_descriptor = next(iter(cohort.parent_pins.values()))[0]
        self.assertEqual({lease.parent_descriptor for lease in leases}, {parent_descriptor})

        for process in (first, second):
            process.terminate()
            process.wait(timeout=3)
        with cohort:
            removed = cohort.remove_closed_fifos()
            for lease in leases:
                os.fstat(lease.descriptor)
            os.fstat(parent_descriptor)

        self.assertEqual([item["state"] for item in removed], ["removed", "removed"])
        self.assertFalse(first_fifo.exists())
        self.assertFalse(second_fifo.exists())
        self.assertEqual(cohort.leases, [])
        self.assertEqual(cohort.parent_pins, {})
        for lease in leases:
            with self.assertRaises(OSError):
                os.fstat(lease.descriptor)
        with self.assertRaises(OSError):
            os.fstat(parent_descriptor)

    def test_parent_replacement_before_fifo_open_fails_and_closes_parent_pin(self):
        parent = self.root / "waiters"
        parent.mkdir()
        fifo = parent / "lease.fifo"
        os.mkfifo(fifo, 0o600)
        original_parent = self.root / "original-waiters"
        opened_parent = []
        real_open_parent = private_cli._open_parent_directory

        def open_then_replace(path):
            descriptor, identity = real_open_parent(path)
            opened_parent.append(descriptor)
            parent.rename(original_parent)
            parent.mkdir()
            return descriptor, identity

        with patch.object(leases_impl, "_open_parent_directory",
                          side_effect=open_then_replace):
            with self.assertRaisesRegex(RuntimeError, "parent was replaced"):
                private_cli.LiveFIFOLease.acquire(
                    "a" * 32, 101, fifo, self.nonce,
                )

        self.assertEqual(len(opened_parent), 1)
        with self.assertRaises(OSError):
            os.fstat(opened_parent[0])
        self.assertTrue((original_parent / "lease.fifo").exists())
        self.assertFalse((parent / "lease.fifo").exists())

    def test_parent_replacement_between_borrowed_acquisitions_fails_before_second_fifo(self):
        parent = self.root / "waiters"
        parent.mkdir()
        first_fifo = parent / "first.fifo"
        second_fifo = parent / "second.fifo"
        os.mkfifo(first_fifo, 0o600)
        os.mkfifo(second_fifo, 0o600)
        first = self.start_fifo_writer(first_fifo)
        second = self.start_fifo_writer(second_fifo)
        original_parent = self.root / "original-waiters"
        acquired = []
        real_acquire = private_cli.LiveFIFOLease._acquire_with_parent

        def acquire_then_replace(*args, **kwargs):
            lease = real_acquire(*args, **kwargs)
            acquired.append((lease, lease.descriptor, lease.parent_descriptor))
            if len(acquired) == 1:
                parent.rename(original_parent)
                parent.mkdir()
            return lease

        specifications = [
            {"task_id": "a" * 32, "pid": first.pid, "path": first_fifo},
            {"task_id": "b" * 32, "pid": second.pid, "path": second_fifo},
        ]
        with patch.object(private_cli.LiveFIFOLease, "_acquire_with_parent",
                          side_effect=acquire_then_replace):
            with self.assertRaisesRegex(RuntimeError, "parent was replaced"):
                private_cli.LiveFIFOLeaseCohort.acquire(
                    specifications, self.nonce, timeout=2,
                )

        self.assertEqual(len(acquired), 1)
        _first_lease, first_descriptor, parent_descriptor = acquired[0]
        with self.assertRaises(OSError):
            os.fstat(first_descriptor)
        with self.assertRaises(OSError):
            os.fstat(parent_descriptor)
        self.assertTrue((original_parent / "first.fifo").exists())
        self.assertTrue((original_parent / "second.fifo").exists())
        self.assertFalse((parent / "first.fifo").exists())
        self.assertFalse((parent / "second.fifo").exists())

    def test_cohort_pins_one_parent_for_200_mock_handshakes(self):
        waiters = self.root / "waiters"
        waiters.mkdir()
        fifos = [waiters / ("lease-%03d.fifo" % index) for index in range(200)]
        for fifo in fifos:
            os.mkfifo(fifo, 0o600)
        process = self.start_fifo_mock_writer_cohort(fifos)
        specifications = [
            {"task_id": "%032x" % index, "pid": process.pid, "path": fifo}
            for index, fifo in enumerate(fifos)
        ]
        opened_parents = []
        real_open_parent = private_cli._open_parent_directory

        def record_open_parent(path):
            descriptor, identity = real_open_parent(path)
            opened_parents.append(descriptor)
            return descriptor, identity

        with patch.object(leases_impl, "_open_parent_directory",
                          side_effect=record_open_parent):
            cohort = private_cli.LiveFIFOLeaseCohort.acquire(
                specifications, self.nonce, timeout=2,
            )
        leases = tuple(cohort.leases)
        parent_descriptor = next(iter(cohort.parent_pins.values()))[0]
        try:
            self.assertEqual(len(opened_parents), 1)
            self.assertEqual(len(cohort.parent_pins), 1)
            self.assertEqual(len(leases), 200)
            self.assertEqual(
                {lease.parent_descriptor for lease in leases}, {parent_descriptor},
            )
            self.assertEqual(len({lease.descriptor for lease in leases}), 200)
            os.fstat(parent_descriptor)
        finally:
            cohort.close()
        with self.assertRaises(OSError):
            os.fstat(parent_descriptor)
        for lease in leases:
            with self.assertRaises(OSError):
                os.fstat(lease.descriptor)

    def test_cohort_parent_replacement_after_acquisition_fails_live_assertion(self):
        parent = self.root / "waiters"
        parent.mkdir()
        fifo = parent / "lease.fifo"
        os.mkfifo(fifo, 0o600)
        process = self.start_fifo_writer(fifo)
        cohort = private_cli.LiveFIFOLeaseCohort.acquire([
            {"task_id": "a" * 32, "pid": process.pid, "path": fifo},
        ], self.nonce, timeout=2)
        self.addCleanup(cohort.close)
        original_parent = self.root / "original-waiters"
        parent.rename(original_parent)
        parent.mkdir()

        try:
            with self.assertRaisesRegex(RuntimeError, "parent was replaced"):
                cohort.assert_live()
            self.assertTrue((original_parent / "lease.fifo").exists())
            self.assertFalse((parent / "lease.fifo").exists())
        finally:
            cohort.close()

    def test_cohort_parent_replacement_retains_original_fifo(self):
        parent = self.root / "waiters"
        parent.mkdir()
        fifo = parent / "lease.fifo"
        os.mkfifo(fifo, 0o600)
        process = self.start_fifo_writer(fifo)
        cohort = private_cli.LiveFIFOLeaseCohort.acquire([
            {"task_id": "a" * 32, "pid": process.pid, "path": fifo},
        ], self.nonce, timeout=2)
        process.terminate()
        process.wait(timeout=3)
        original_parent = self.root / "original-waiters"
        parent.rename(original_parent)
        parent.mkdir()

        try:
            with self.assertRaisesRegex(RuntimeError, "parent was replaced"):
                cohort.remove_closed_fifos()
            self.assertTrue((original_parent / "lease.fifo").exists())
            self.assertFalse((parent / "lease.fifo").exists())
        finally:
            cohort.close()

    def test_replaced_fifo_identity_fails_and_retains_replacement(self):
        fifo = self.root / "replaced.fifo"
        os.mkfifo(fifo, 0o600)
        process = self.start_fifo_writer(fifo)
        lease = private_cli.LiveFIFOLease.acquire(
            "a" * 32, process.pid, fifo, self.nonce, timeout=2,
        )
        replacement_path = self.root / "replacement.fifo"
        os.mkfifo(replacement_path, 0o600)
        replacement_identity = None
        real_remove = private_cli.remove_exact_owned_fifo

        def replace_after_observation(*args, **kwargs):
            nonlocal replacement_identity
            fifo.unlink()
            replacement_path.rename(fifo)
            replacement_identity = (fifo.stat().st_dev, fifo.stat().st_ino)
            return real_remove(*args, **kwargs)

        try:
            with patch.object(leases_impl, "remove_exact_owned_fifo",
                              side_effect=replace_after_observation):
                with self.assertRaisesRegex(RuntimeError, "was replaced"):
                    lease.remove_closed_fifo()

            self.assertTrue(fifo.exists())
            self.assertEqual(
                (fifo.stat().st_dev, fifo.stat().st_ino), replacement_identity,
            )
        finally:
            lease.close()

    def test_closed_fifo_pin_is_rejected_without_removing_the_entry(self):
        fifo = self.root / "closed.fifo"
        os.mkfifo(fifo, 0o600)
        process = self.start_fifo_writer(fifo)
        lease = private_cli.LiveFIFOLease.acquire(
            "a" * 32, process.pid, fifo, self.nonce, timeout=2,
        )
        parent_descriptor = lease.parent_descriptor
        os.fstat(parent_descriptor)
        lease.close()

        with self.assertRaises(OSError):
            os.fstat(parent_descriptor)

        with self.assertRaisesRegex(RuntimeError, "entry descriptor is closed"):
            lease.remove_closed_fifo()
        self.assertTrue(fifo.exists())

    def test_replaced_fifo_parent_fails_and_retains_both_parent_entries(self):
        parent = self.root / "waiters"
        parent.mkdir()
        fifo = parent / "lease.fifo"
        os.mkfifo(fifo, 0o600)
        process = self.start_fifo_writer(fifo)
        lease = private_cli.LiveFIFOLease.acquire(
            "a" * 32, process.pid, fifo, self.nonce, timeout=2,
        )
        original_parent = self.root / "original-waiters"
        parent.rename(original_parent)
        parent.mkdir()

        try:
            with self.assertRaisesRegex(RuntimeError, "parent was replaced"):
                lease.remove_closed_fifo()

            self.assertTrue(parent.is_dir())
            self.assertTrue((original_parent / "lease.fifo").exists())
        finally:
            lease.close()

    def test_child_death_during_cohort_formation_fails_and_closes_all_fds(self):
        first_fifo = self.root / "first.fifo"
        second_fifo = self.root / "second.fifo"
        os.mkfifo(first_fifo, 0o600)
        os.mkfifo(second_fifo, 0o600)
        script = (
            "import os,sys,time\n"
            "fifo,nonce,delay,hold=sys.argv[1:]\n"
            "fd=os.open(fifo,os.O_WRONLY)\n"
            "time.sleep(float(delay))\n"
            "os.write(fd,(nonce+'\\t'+str(os.getpid())+'\\n').encode())\n"
            "sys.stdin.buffer.read(1) if hold == 'hold' else time.sleep(float(hold))\n"
            "os.close(fd)\n"
        )
        first = subprocess.Popen(
            [sys.executable, "-c", script, str(first_fifo), self.nonce, "0", "hold"],
            stdin=subprocess.PIPE,
        )
        second = subprocess.Popen(
            [sys.executable, "-c", script, str(second_fifo), self.nonce, "0.2", "hold"],
            stdin=subprocess.PIPE,
        )
        self.addCleanup(self.reap, first)
        self.addCleanup(self.reap, second)
        opened = []
        real_open = private_cli.os.open

        def recording_open(*args, **kwargs):
            descriptor = real_open(*args, **kwargs)
            opened.append(descriptor)
            if len(opened) == 3:
                first.stdin.write(b"x")
                first.stdin.flush()
                first.stdin.close()
            return descriptor

        specifications = [
            {"task_id": "a" * 32, "pid": first.pid, "path": first_fifo},
            {"task_id": "b" * 32, "pid": second.pid, "path": second_fifo},
        ]
        with patch.object(directory_owner_impl, "_require_fd_cleanup_support"), \
                patch.object(private_cli.os, "open", side_effect=recording_open):
            with self.assertRaisesRegex(RuntimeError, "died during lease cohort formation"):
                private_cli.LiveFIFOLeaseCohort.acquire(
                    specifications, self.nonce, timeout=2,
                )
        self.assertEqual(len(opened), 3)
        for descriptor in opened:
            with self.assertRaises(OSError):
                os.fstat(descriptor)

    @staticmethod
    def reap(process):
        if process.stdin is not None and not process.stdin.closed:
            process.stdin.close()
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)
