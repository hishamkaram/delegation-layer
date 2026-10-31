"""Failure-only disposal status receipts that tie a timed-out public status call to its direct CLI process.

A small local script stands in for the Delegate CLI. These tests prove the
acceptance evidence mapping only; they are not native supervisor evidence.
"""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

from scripts.acceptance_tests.private_cli.fixtures import set_up_private_capture_root
from scripts.delegate_acceptance.private_cli import capture
from scripts.delegate_acceptance.private_cli import disposal_observer


TASK_ID = "a" * 32
OTHER_TASK_ID = "d" * 32
ROOT_ID = "b" * 32
# Long enough for the script to write its streams, short enough to keep the suite fast.
OBSERVATION_SECONDS = 2.0
# How long a test waits for its workers before it reports them late; cleanup still joins them.
WORKER_WATCHDOG_SECONDS = OBSERVATION_SECONDS * 10
POST_RELEASE_WORK_SECONDS = 0.2
STATUS_SCRIPT = """\
import json, os, sys, time
task = sys.argv[4]
if os.environ["FAKE_STATUS_MODE"] == "ready":
    sys.stdout.write(json.dumps({"task_id": task, "root_id": %r}))
    sys.exit(0)
os.write(1, json.dumps({"pid": os.getpid(), "task": task}).encode())
os.write(2, ("waiting on " + task).encode())
time.sleep(30)
""" % ROOT_ID


class StartedWorkers:
    """Test-owned threads and the barriers they wait on.

    Construct it after the capture root: its cleanup is registered before any
    thread starts, so it runs before the capture owner's rollback. The cleanup
    releases every barrier and joins every thread that actually started. A
    watchdog only reports a late worker; it never abandons one. Each worker's
    own work is bounded by its observation deadline and the capture owner's reap.
    """

    def __init__(self, test):
        self.test = test
        self.started = []
        self.barriers = []
        test.addCleanup(self.release_and_join)

    def barrier(self):
        held = threading.Event()
        self.barriers.append(held)
        return held

    def start(self, target, *args):
        worker = threading.Thread(target=target, args=args)
        worker.start()
        self.started.append(worker)
        return worker

    def join(self, watchdog=WORKER_WATCHDOG_SECONDS):
        deadline = time.monotonic() + watchdog
        for worker in self.started:
            worker.join(max(0.0, deadline - time.monotonic()))
        late = [worker.name for worker in self.started if worker.is_alive()]
        self.test.assertEqual(late, [], "workers outlived their watchdog")

    def release_and_join(self):
        for held in self.barriers:
            held.set()
        for worker in self.started:
            worker.join()


class StartedWorkersTests(unittest.TestCase):
    def setUp(self):
        set_up_private_capture_root(self)

    def test_cleanup_releases_and_joins_a_late_or_partial_start_before_capture_rollback(self):
        live_after_release = []

        def capture_rollback_follows():
            self.assertEqual([worker.is_alive() for worker in workers.started], [False],
                             "a started worker was still alive when the capture root was about to roll back")
            self.assertEqual(live_after_release, [True])

        # Registered first, so it runs after the workers' cleanup and immediately before the capture rollback.
        self.addCleanup(capture_rollback_follows)
        workers = StartedWorkers(self)
        held = workers.barrier()

        def wait_then_use_capture_root():
            held.wait()
            # Work after the release, so a cleanup that skipped the join would let the rollback overtake it.
            time.sleep(POST_RELEASE_WORK_SECONDS)
            self.capture_root.assert_live()
            live_after_release.append(True)

        workers.start(wait_then_use_capture_root)
        with patch.object(threading.Thread, "start", autospec=True,
                          side_effect=RuntimeError("can't start new thread")):
            with self.assertRaisesRegex(RuntimeError, "can't start new thread"):
                workers.start(wait_then_use_capture_root)
        self.assertEqual(len(workers.started), 1, "only the started prefix is owned")
        with self.assertRaisesRegex(AssertionError, "workers outlived their watchdog"):
            workers.join(watchdog=0.05)
        self.assertTrue(workers.started[0].is_alive(), "the late worker is still owned, not abandoned")


class DisposalObserverReceiptTests(unittest.TestCase):
    def setUp(self):
        set_up_private_capture_root(self)
        temporary = tempfile.TemporaryDirectory(prefix="delegate-observer-cli-")
        self.addCleanup(temporary.cleanup)
        self.delegate = Path(temporary.name) / "delegate"
        self.delegate.write_text("#!" + sys.executable + "\n" + STATUS_SCRIPT)
        self.delegate.chmod(0o700)
        self.state = Path(temporary.name) / "state"

    def observer(self, mode):
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "FAKE_STATUS_MODE": mode}
        return disposal_observer.public_status_observer(self.delegate, self.state, env, capture_root=self.capture_root)

    def receipts(self):
        return sorted(self.capture_root.directory.glob("disposal-status-*.json"))

    def timed_out(self, observe, task_id):
        with self.assertRaises(subprocess.TimeoutExpired) as raised:
            observe(task_id, time.monotonic() + OBSERVATION_SECONDS)
        return raised.exception

    def only_receipt(self, task_id):
        matching = [path for path in self.receipts() if path.name.startswith("disposal-status-" + task_id + "-")]
        self.assertEqual(len(matching), 1, "expected exactly one timeout receipt for task " + task_id)
        return matching[0]

    def assert_receipt_maps_the_call(self, error, task_id):
        receipt_path = self.only_receipt(task_id)
        self.assertIn(str(receipt_path), "\n".join(getattr(error, "__notes__", ())))
        receipt = json.loads(receipt_path.read_text())
        argv = ["--root", str(self.state), "status", task_id, "--json"]
        self.assertEqual(receipt["arguments"], argv)
        self.assertEqual(receipt["command"], [str(self.delegate), *argv])
        self.assertEqual(receipt["failure_class"], "invocation_error")
        self.assertEqual(receipt["invocation_error"], str(error))
        stdout_path, stderr_path = (Path(path) for path in receipt["capture_paths"])
        self.assertEqual({stdout_path.parent, stderr_path.parent}, {self.capture_root.directory})
        # The script prints its own PID, so the receipt's direct PID is checked against the process itself.
        printed = json.loads(stdout_path.read_bytes())
        self.assertEqual(printed, {"pid": receipt["direct_process"]["pid"], "task": task_id})
        self.assertEqual(receipt["direct_process"]["provenance"], "subprocess.Popen.pid")
        for name, path in (("stdout", stdout_path), ("stderr", stderr_path)):
            raw = path.read_bytes()
            self.assertEqual(receipt[name + "_bytes"], len(raw))
            self.assertEqual(receipt[name + "_sha256"], hashlib.sha256(raw).hexdigest())
        self.assertEqual(stderr_path.read_bytes(), ("waiting on " + task_id).encode())
        return receipt

    def test_a_timed_out_status_call_maps_its_task_and_argv_to_its_pid_and_streams(self):
        error = self.timed_out(self.observer("hang"), TASK_ID)
        self.assertEqual(error.cmd, [str(self.delegate), "--root", str(self.state), "status", TASK_ID, "--json"])
        receipt = self.assert_receipt_maps_the_call(error, TASK_ID)
        self.assertEqual(error.output, Path(receipt["capture_paths"][0]).read_bytes())

    def test_concurrent_timeouts_keep_distinct_receipts_pids_and_streams(self):
        observe = self.observer("hang")
        errors = {}
        workers = StartedWorkers(self)

        def run(task_id):
            try:
                observe(task_id, time.monotonic() + OBSERVATION_SECONDS)
            except subprocess.TimeoutExpired as error:
                errors[task_id] = error

        for task_id in (TASK_ID, OTHER_TASK_ID):
            workers.start(run, task_id)
        workers.join()
        self.assertEqual(set(errors), {TASK_ID, OTHER_TASK_ID})
        receipts = [self.assert_receipt_maps_the_call(errors[task_id], task_id) for task_id in (TASK_ID, OTHER_TASK_ID)]
        self.assertNotEqual(receipts[0]["direct_process"]["pid"], receipts[1]["direct_process"]["pid"])
        self.assertTrue(set(receipts[0]["capture_paths"]).isdisjoint(receipts[1]["capture_paths"]))

    def test_a_capture_read_failure_keeps_the_timeout_its_cause_paths_and_pid(self):
        with patch.object(capture, "_read_capture", side_effect=OSError("capture read failed")):
            error = self.timed_out(self.observer("hang"), TASK_ID)
        self.assertIsInstance(error.__cause__, OSError)
        receipt = json.loads(self.only_receipt(TASK_ID).read_text())
        self.assertEqual(receipt["capture_failure"]["cause"], {"type": "OSError", "message": "capture read failed"})
        self.assertEqual(receipt["capture_failure"]["retained_paths"], receipt["capture_paths"])
        stdout_path = Path(receipt["capture_paths"][0])
        self.assertEqual(json.loads(stdout_path.read_bytes())["pid"], receipt["direct_process"]["pid"])
        self.assertEqual(receipt["stdout_bytes"], 0)

    def test_a_prelaunch_deadline_refusal_launches_nothing_and_records_no_process(self):
        with patch.object(capture.subprocess, "Popen", side_effect=AssertionError("launched")) as popen, \
                self.assertRaisesRegex(TimeoutError, "expired before launch") as raised:
            self.observer("hang")(TASK_ID, time.monotonic() - 1)
        popen.assert_not_called()
        self.assertFalse(hasattr(raised.exception, "process_pid"))
        self.assertEqual(self.receipts(), [])

    def test_a_receipt_write_failure_never_masks_the_original_timeout(self):
        # create=True keeps this seam inert, not an import error, before the observer records receipts.
        with patch.object(disposal_observer, "record_invocation_error", create=True,
                          side_effect=OSError("receipt disk full")):
            error = self.timed_out(self.observer("hang"), TASK_ID)
        self.assertEqual(error.cmd[-3:], ["status", TASK_ID, "--json"])
        self.assertEqual(json.loads(error.output)["pid"], getattr(error, "process_pid", None))
        notes = "\n".join(getattr(error, "__notes__", ()))
        self.assertRegex(notes, "disposal status timeout receipt was not written to .*disposal-status-%s-" % TASK_ID)
        self.assertIn("OSError: receipt disk full", notes)
        self.assertEqual(self.receipts(), [])

    def test_a_receipt_name_allocation_fault_keeps_the_same_timeout_primary(self):
        timeout = subprocess.TimeoutExpired(["delegate", "status", TASK_ID], 1.0)
        escaped = None
        with patch.object(disposal_observer, "run_json", side_effect=timeout), \
                patch.object(disposal_observer.uuid, "uuid4", side_effect=OSError("no entropy")):
            try:
                self.observer("hang")(TASK_ID, time.monotonic() + OBSERVATION_SECONDS)
            except BaseException as error:
                escaped = error
        self.assertIs(escaped, timeout)
        notes = "\n".join(getattr(escaped, "__notes__", ()))
        self.assertIn("disposal status timeout receipt was not written", notes)
        self.assertIn("OSError: no entropy", notes)
        self.assertEqual(list(self.capture_root.directory.iterdir()), [])

    def test_a_task_id_unfit_for_a_receipt_name_is_noted_and_writes_nothing(self):
        error = self.timed_out(self.observer("hang"), "../escape")
        self.assertIn("task id is not 32 lowercase hex characters", "\n".join(getattr(error, "__notes__", ())))
        self.assertEqual(self.receipts(), [])
        # Only this call's two capture files exist; nothing was written beside or beneath them.
        self.assertEqual(sorted(path.suffix for path in self.capture_root.directory.iterdir()), [".stderr", ".stdout"])

    def test_a_successful_status_call_writes_no_receipt(self):
        self.assertEqual(self.observer("ready")(TASK_ID, time.monotonic() + OBSERVATION_SECONDS * 5), ROOT_ID)
        self.assertEqual(self.receipts(), [])
        self.assertEqual(len(tuple(self.capture_root.directory.iterdir())), 2)


if __name__ == "__main__":
    unittest.main()
