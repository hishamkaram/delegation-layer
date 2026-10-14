"""A shutdown acknowledgement must not satisfy the restart precondition."""

import json
import shutil
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import acceptance_private_cli as private_cli
import acceptance_provider_common as provider_common


ROOT_ID = "a" * 32
TASK_ID = "b" * 32


def write_record(path, value):
    path.write_bytes(provider_common.canonical_go_json(value))


class ManagedLifetimeFixture:
    """Write the durable ordinary-task records consumed by the shared oracle."""

    def __init__(self, parent):
        self.state = Path(parent) / "state"
        self.state.mkdir()
        write_record(self.state / "root.json", {
            "schema_version": 1, "root_id": ROOT_ID,
            "created_at": "2026-10-13T00:00:00Z",
        })
        self.task = self.state / "tasks" / TASK_ID
        self.task.mkdir(parents=True)

    def write_task(self, *, wait_result="success", include_wait=True,
                   termination_cause="completed", scope_empty=True,
                   outcome="committed"):
        task_path = self.task / "task.json"
        meta_path = self.task / "meta.json"
        write_record(task_path, {"task": "ordinary fixture"})
        write_record(meta_path, {"schema_version": 1, "runner_ownership": "managed"})
        write_record(self.task / "outcome.json", {"verdict": outcome})
        write_record(self.task / "queue-row.json", {
            "status": {outcome: {}}, "label": f"delegate:{ROOT_ID}:{TASK_ID}",
        })

        lifetime = self.task / "lifetime"
        lifetime.mkdir(exist_ok=True)
        owner_sha = provider_common.ordinary_lifetime_owner_digest(
            provider_common.digest(task_path), provider_common.digest(meta_path))
        common = {"schema_version": 1, "root_id": ROOT_ID, "task_id": TASK_ID}
        write_record(lifetime / "intent.json", {
            **common, "owner_kind": "task", "owner_sha256": owner_sha,
            "scope_kind": "process-group", "assurance": "cooperative",
            "scope_plan": "fixture-plan", "boot_id": "fixture-boot",
            "deadline": "2026-10-13T00:01:00Z",
            "created_at": "2026-10-13T00:00:04Z",
        })
        registration = {
            **common, "owner_kind": "task", "owner_sha256": owner_sha,
            "scope_kind": "process-group", "assurance": "cooperative",
            "scope_id": "fixture-scope", "boot_id": "fixture-boot",
            "deadline": "2026-10-13T00:01:00Z",
            "created_at": "2026-10-13T00:00:05Z",
        }
        write_record(lifetime / "registration.json", registration)
        registration_sha = provider_common.digest(lifetime / "registration.json")
        worker = {
            **common, "registration_sha256": registration_sha, "pid": 2001,
            "process_start": "fixture-worker", "boot_id": "fixture-boot",
            "started_at": "2026-10-13T00:00:06Z",
        }
        write_record(lifetime / "worker.json", worker)
        worker_sha = provider_common.digest(lifetime / "worker.json")
        launch = {
            **common, "registration_sha256": registration_sha, "sequence": 1,
            "pid": 2002, "process_start": "fixture-provider",
            "started_at": "2026-10-13T00:00:07Z",
        }
        write_record(lifetime / "00000000000000000001.launch.json", launch)
        launch_set_sha = provider_common.sha(provider_common.canonical_go_json([launch]))
        write_record(lifetime / "terminated.json", {
            **common, "registration_sha256": registration_sha, "launch_count": 1,
            "launch_set_sha256": launch_set_sha, "cause": termination_cause,
            "scope_empty": scope_empty, "observed_at": "2026-10-13T00:00:08Z",
        })
        wait_path = lifetime / "worker-wait.json"
        if include_wait:
            wait = {
                **common, "registration_sha256": registration_sha,
                "worker_sha256": worker_sha, "child_pid": 2001,
                "result": wait_result, "observed_at": "2026-10-13T00:00:09Z",
            }
            if wait_result == "success":
                wait["exit_code"] = 0
            elif wait_result == "exit-error":
                wait["exit_code"] = 17
            write_record(wait_path, wait)
        else:
            wait_path.unlink(missing_ok=True)

    def write_not_started(self):
        self.write_task(wait_result="not-started", include_wait=True)
        lifetime = self.task / "lifetime"
        (lifetime / "worker.json").unlink()
        (lifetime / "00000000000000000001.launch.json").unlink()
        registration_sha = provider_common.digest(lifetime / "registration.json")
        write_record(lifetime / "terminated.json", {
            "schema_version": 1, "root_id": ROOT_ID, "task_id": TASK_ID,
            "registration_sha256": registration_sha, "launch_count": 0,
            "launch_set_sha256": provider_common.sha(
                provider_common.canonical_go_json([])),
            "cause": "completed", "scope_empty": True,
            "observed_at": "2026-10-13T00:00:08Z",
        })
        write_record(lifetime / "worker-wait.json", {
            "schema_version": 1, "root_id": ROOT_ID, "task_id": TASK_ID,
            "registration_sha256": registration_sha, "child_pid": 0,
            "result": "not-started", "observed_at": "2026-10-13T00:00:09Z",
        })


class PrivateShutdownTests(unittest.TestCase):
    def test_shutdown_evidence_accepts_owned_compact_native_socket(self):
        with tempfile.TemporaryDirectory(prefix="dls-", dir="/tmp") as directory:
            parent = Path(directory).resolve()
            socket_limit = 103 if private_cli.platform.system() == "Darwin" else 107
            target_base_length = socket_limit - 2
            padding = target_base_length - len(private_cli.os.fsencode(parent)) - 2 - len(".supervisor")
            self.assertGreater(padding, 0)
            supervisor = parent / ("s" * padding) / ".supervisor"
            runtime = supervisor / "run"
            runtime.mkdir(parents=True)
            self.assertEqual(len(private_cli.os.fsencode(supervisor)), target_base_length)
            endpoint = supervisor / "s"
            self.assertLessEqual(len(private_cli.os.fsencode(endpoint)), socket_limit)
            self.assertGreater(len(private_cli.os.fsencode(runtime / "pueue.sock")), socket_limit)
            (runtime / "pueue.pid").write_text("999999\n", encoding="utf-8")
            config = supervisor / "pueue.yml"
            config.write_text(
                "shared:\n"
                f"  runtime_directory: {json.dumps(str(runtime))}\n"
                f"  unix_socket_path: {json.dumps(str(endpoint))}\n"
                f"  pid_path: {json.dumps(str(runtime / 'pueue.pid'))}\n",
                encoding="utf-8",
            )
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
                listener.bind(str(endpoint))
                listener.listen()
                info = endpoint.stat()
                self.assertEqual(
                    private_cli.private_shutdown_evidence(config),
                    (999999, endpoint, (info.st_dev, info.st_ino)),
                )

    def test_waits_for_managed_lifetime_closure_after_gate_release(self):
        evidence = {"root_id": ROOT_ID, "tasks": {}, "inspections": {}}
        with patch.object(
                private_cli, "authorize_managed_lifetime_closure",
                side_effect=[provider_common.AcceptanceFailure("pending"), evidence],
        ) as authorize, patch.object(
                private_cli.time, "monotonic", side_effect=[0, 0, 0],
        ), patch.object(private_cli.time, "sleep") as sleep:
            result = private_cli.await_managed_lifetime_closure(
                Path("/fixture/state"), (TASK_ID,), timeout=1,
            )
        self.assertEqual(result, evidence)
        self.assertEqual(authorize.call_count, 2)
        sleep.assert_called_once()

    def test_managed_lifetime_closure_timeout_preserves_last_failure(self):
        pending = provider_common.AcceptanceFailure("pending evidence")
        with patch.object(
                private_cli, "authorize_managed_lifetime_closure", side_effect=pending,
        ), patch.object(private_cli.time, "monotonic", side_effect=[0, 2]):
            with self.assertRaisesRegex(
                    RuntimeError, "managed lifetime closure remained incomplete",
            ) as raised:
                private_cli.await_managed_lifetime_closure(
                    Path("/fixture/state"), (TASK_ID,), timeout=1,
                )
        self.assertIs(raised.exception.__cause__, pending)

    def test_waits_for_both_process_and_endpoint_after_one_ack(self):
        with patch.object(private_cli, "private_shutdown_evidence", return_value=(123, Path("/fixture/socket"), (1, 2))), \
                patch.object(private_cli, "private_daemon_process_gone", side_effect=[False, False, True, True]) as gone, \
                patch.object(private_cli, "private_shutdown_endpoint_closed", side_effect=[False, False, True]) as closed, \
                patch.object(private_cli, "pueue_command") as command, \
                patch.object(private_cli, "authorize_managed_lifetime_closure"), \
                patch.object(private_cli.time, "sleep"):
            private_cli.shutdown_private_supervisor(
                "client", "config", {}, timeout=5, state=Path("/fixture/state"))
        self.assertEqual(gone.call_count, 4)
        self.assertEqual(closed.call_count, 3)
        command.assert_called_once()
        self.assertEqual(command.call_args.args, ("client", "config", {}, ["shutdown"]))

    def test_ack_with_live_process_expires(self):
        with patch.object(private_cli, "private_shutdown_evidence", return_value=(123, Path("/fixture/socket"), (1, 2))), \
                patch.object(private_cli, "private_daemon_process_gone", return_value=False), \
                patch.object(private_cli, "private_shutdown_endpoint_closed", return_value=True), \
                patch.object(private_cli, "pueue_command") as command, \
                patch.object(private_cli, "authorize_managed_lifetime_closure"), \
                patch.object(private_cli.time, "monotonic", side_effect=[0, 0, 2]):
            with self.assertRaises(TimeoutError):
                private_cli.shutdown_private_supervisor(
                    "client", "config", {}, timeout=1, state=Path("/fixture/state"))
        command.assert_called_once()

    def test_observed_pid_reuse_cannot_prove_shutdown(self):
        with patch.object(private_cli, "private_shutdown_evidence", return_value=(123, Path("/fixture/socket"), (1, 2))), \
                patch.object(private_cli, "private_daemon_process_gone", side_effect=[False, True, False]), \
                patch.object(private_cli, "private_shutdown_endpoint_closed", return_value=False), \
                patch.object(private_cli, "pueue_command"), \
                patch.object(private_cli, "authorize_managed_lifetime_closure"), \
                patch.object(private_cli.time, "sleep"):
            with self.assertRaisesRegex(RuntimeError, "PID identity changed"):
                private_cli.shutdown_private_supervisor(
                    "client", "config", {}, state=Path("/fixture/state"))

    def test_real_child_is_not_gone_until_reaped(self):
        child = subprocess.Popen([sys.executable, "-c", "import sys; sys.stdin.buffer.read(1)"], stdin=subprocess.PIPE)
        deadline = private_cli.time.monotonic() + 10
        try:
            self.assertFalse(private_cli.private_daemon_process_gone(child.pid, deadline))
            child.stdin.close()
            while True:
                status = subprocess.run(["/bin/ps", "-p", str(child.pid), "-o", "stat="],
                                        capture_output=True, text=True,
                                        timeout=private_cli.shutdown_remaining(deadline), check=False)
                if status.returncode == 0 and status.stdout.strip().startswith("Z"):
                    break
                private_cli.time.sleep(0.01)
            self.assertFalse(private_cli.private_daemon_process_gone(child.pid, deadline))
        finally:
            if not child.stdin.closed:
                child.stdin.close()
            child.wait(timeout=10)
        self.assertTrue(private_cli.private_daemon_process_gone(child.pid, deadline))

    def test_unobservable_process_is_not_exit_proof(self):
        with patch.object(private_cli.os, "getpgid", side_effect=PermissionError("unobservable")):
            with self.assertRaises(PermissionError):
                private_cli.private_daemon_process_gone(123, private_cli.time.monotonic() + 5)

    def test_failed_presence_probe_is_not_exit_proof(self):
        failed = subprocess.CompletedProcess([], 1, stdout="", stderr="ps: malloc failed")
        with patch.object(private_cli.os, "getpgid", side_effect=ProcessLookupError()), \
                patch.object(private_cli.subprocess, "run", return_value=failed):
            with self.assertRaisesRegex(RuntimeError, "PID presence is uncertain"):
                private_cli.private_daemon_process_gone(123, private_cli.time.monotonic() + 5)

    def test_transient_socket_timeout_does_not_prove_closure(self):
        with tempfile.TemporaryDirectory(prefix="dls-", dir="/tmp") as directory:
            endpoint = Path(directory) / "socket"
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
                listener.bind(str(endpoint))
                listener.listen()
                info = endpoint.stat()
                identity = (info.st_dev, info.st_ino)
                with patch.object(private_cli, "private_shutdown_evidence", return_value=(123, endpoint, identity)), \
                        patch.object(private_cli, "private_daemon_process_gone", side_effect=[False, False, True]), \
                        patch.object(private_cli, "pueue_command") as command, \
                        patch.object(private_cli, "authorize_managed_lifetime_closure"), \
                        patch.object(private_cli.socket, "socket") as connection, \
                        patch.object(private_cli.time, "sleep"):
                    connection.return_value.__enter__.return_value.connect.side_effect = [TimeoutError(), ConnectionRefusedError(private_cli.errno.ECONNREFUSED, "closed")]
                    private_cli.shutdown_private_supervisor(
                        "client", "config", {}, state=Path("/fixture/state"))
                command.assert_called_once()

    def test_real_endpoint_must_stop_listening(self):
        with tempfile.TemporaryDirectory(prefix="dls-", dir="/tmp") as directory:
            endpoint = Path(directory) / "socket"
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
                listener.bind(str(endpoint))
                listener.listen()
                info = endpoint.stat()
                identity = (info.st_dev, info.st_ino)
                deadline = private_cli.time.monotonic() + 5
                self.assertFalse(private_cli.private_shutdown_endpoint_closed(endpoint, identity, deadline))
            self.assertTrue(private_cli.private_shutdown_endpoint_closed(endpoint, identity, deadline))
            endpoint.unlink()
            self.assertTrue(private_cli.private_shutdown_endpoint_closed(endpoint, identity, deadline))
            endpoint.write_text("replacement")
            with self.assertRaisesRegex(RuntimeError, "socket identity changed"):
                private_cli.private_shutdown_endpoint_closed(endpoint, identity, deadline)


class PrivateLifetimeAuthorizationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-private-lifetime-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.case_number = 0

    def fixture(self):
        self.case_number += 1
        parent = self.root / ("case-%d" % self.case_number)
        parent.mkdir()
        return ManagedLifetimeFixture(parent)

    def assert_shutdown_refused(self, state):
        with patch.object(private_cli, "pueue_command") as command, \
                patch.object(private_cli, "private_shutdown_evidence") as evidence:
            with self.assertRaises(provider_common.AcceptanceFailure):
                private_cli.shutdown_private_supervisor(
                    "client", "config", {}, state=state)
        command.assert_not_called()
        evidence.assert_not_called()

    def test_terminal_success_failed_and_killed_rows_require_lifetime_closure(self):
        for queue_result in ("Success", "Failed", "Killed"):
            with self.subTest(queue_result=queue_result):
                fixture = self.fixture()
                fixture.write_task(include_wait=False, outcome=queue_result)
                self.assert_shutdown_refused(fixture.state)

    def test_complete_wait_with_bad_or_missing_termination_refuses_shutdown(self):
        for mutation in ("missing", "registration", "launch-set", "scope-empty"):
            with self.subTest(mutation=mutation):
                fixture = self.fixture()
                fixture.write_task()
                termination_path = fixture.task / "lifetime" / "terminated.json"
                if mutation == "missing":
                    termination_path.unlink()
                else:
                    termination = json.loads(termination_path.read_text())
                    if mutation == "registration":
                        termination["registration_sha256"] = "0" * 64
                    elif mutation == "launch-set":
                        termination["launch_set_sha256"] = provider_common.sha(
                            provider_common.canonical_go_json([]))
                    else:
                        termination["scope_empty"] = False
                    write_record(termination_path, termination)
                self.assert_shutdown_refused(fixture.state)

    def test_committed_outcome_before_worker_wait_refuses_root_cleanup(self):
        fixture = self.fixture()
        fixture.write_task(include_wait=False, outcome="Success")
        parent = self.root / "parent"
        owned = parent / (private_cli.PRIVATE_ACCEPTANCE_PREFIX + "lifetime")
        parent.mkdir()
        owned.mkdir()
        owner = private_cli.DirectoryOwner()
        token = owner.adopt_created("private root", owned, parent)
        entries = owner.transfer()
        try:
            with patch.object(private_cli, "rollback_owned_directories") as rollback:
                with self.assertRaises(provider_common.AcceptanceFailure):
                    private_cli.remove_private_acceptance_root(
                        token, entries, managed_state_roots=(fixture.state,))
            rollback.assert_not_called()
            self.assertTrue(owned.is_dir())
        finally:
            private_cli.rollback_owned_directories(entries)

    def test_complete_closure_not_started_and_previous_boot_are_authorized(self):
        fixture = self.fixture()
        fixture.write_task()
        closure = private_cli.authorize_managed_lifetime_closure(fixture.state)
        self.assertEqual(set(closure["tasks"]), {TASK_ID})
        self.assertEqual(closure["tasks"][TASK_ID]["worker_wait"]["result"], "success")
        self.assertEqual(closure["inspections"], {})

        fixture.write_not_started()
        closure = private_cli.authorize_managed_lifetime_closure(fixture.state)
        self.assertEqual(closure["tasks"][TASK_ID]["worker_wait"]["result"], "not-started")

        fixture.write_task(include_wait=False, termination_cause="previous-boot")
        closure = private_cli.authorize_managed_lifetime_closure(fixture.state)
        self.assertIsNone(closure["tasks"][TASK_ID]["worker_wait"])

        fixture.write_task()
        meta_path = fixture.task / "meta.json"
        meta = json.loads(meta_path.read_text())
        meta["runner_ownership"] = "custom"
        write_record(meta_path, meta)
        shutil.rmtree(fixture.task / "lifetime")
        self.assertEqual(private_cli.authorize_managed_lifetime_closure(fixture.state), {
            "root_id": ROOT_ID, "tasks": {}, "inspections": {},
        })

    def test_lifetime_scan_survives_removed_queue_row_and_lost_dispatch_response(self):
        fixture = self.fixture()
        fixture.write_task()
        (fixture.task / "queue-row.json").unlink()
        self.assertEqual(
            set(private_cli.authorize_managed_lifetime_closure(fixture.state)["tasks"]),
            {TASK_ID},
        )


if __name__ == "__main__":
    unittest.main()
