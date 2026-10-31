"""Focused scheduler driver checks; these do not claim service/provider acceptance."""

from contextlib import closing
from dataclasses import replace
import fcntl
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.private_cli import scheduler_acceptance as application
from scripts.delegate_acceptance.private_cli import scheduler_cases as cases
from scripts.delegate_acceptance.private_cli import scheduler_service as native
from scripts.delegate_acceptance.private_cli import scheduler_sequence as sequence
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli.fixture_records import FixtureInstallation
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.delegate_acceptance.private_cli.scheduler_occurrences import (
    MINUTE_NS, Occurrence, read_occurrences, require_occurrences, timestamp_ns,
)


ROOT_ID = "a" * 32
SCHEDULE_ID = "b" * 32
FIRST_ID = "c" * 32
SECOND_ID = "d" * 32


class SchedulerAcceptanceTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="scheduler-unit-")
        self.addCleanup(temporary.cleanup)
        self.parent = Path(temporary.name).resolve(strict=True)
        self.lifetime = FixtureLifetime()
        self.base = self.lifetime.ownership.create("unit fixture", self.parent / "fixture", self.parent).directory
        self.lifetime.base = self.base
        self.lifetime.capture_token = self.lifetime.ownership.create("captures", self.base / "captures", self.base)
        self.addCleanup(self.release_tokens)
        self.state = self.base / "state"
        self.state.mkdir()
        self.lifetime.env = {"HOME": str(self.base / "home"), "PATH": str(self.base / "provider-bin") + ":/usr/bin:/bin"}

    def release_tokens(self):
        if not self.lifetime.capture_token.consumed:
            self.lifetime.ownership.release()

    def service(self, system="Linux"):
        with patch.object(native.platform, "system", return_value=system):
            return native.SchedulerService(self.lifetime, self.state, self.base / "delegate", time.monotonic() + 30)

    def properties(self, service):
        return {"Id": service.unit, "Description": service.unit, "Transient": "yes", "KillMode": "process", "Delegate": "yes", "ExitType": "cgroup",
                "LoadState": "loaded", "InvocationID": "e" * 32, "MainPID": "123", "ActiveState": "active",
                "Result": "success", "ExecMainStatus": "0"}

    def receipt(self):
        return AcceptanceReceipt(self.parent / "receipt.json", 1, 1, {})

    def case(self, service):
        fixture = FixtureInstallation(
            delegate_binary=self.base / "delegate", runner_binary=self.base / "delegate-run",
            upgraded_delegate_binary=self.base / "delegate", upgraded_runner_binary=self.base / "delegate-run",
            pueue_binary=self.base / "pueue", pueued_binary=self.base / "pueued", workspace=self.base / "workspace",
            provider_bin=self.base / "provider-bin", brief=self.base / "brief", upgraded_install=self.base / "install",
            old_daemon_marker=self.base / "old-starts", upgraded_daemon_marker=self.base / "new-starts",
            state=self.state, waiter_directory=self.base / "waiters", lease_nonce="f" * 32,
        )
        return cases.SchedulerCase(fixture, self.lifetime, self.parent, self.receipt(), service, time.monotonic() + 30,
                                   root_id=ROOT_ID, schedule_id=SCHEDULE_ID)

    def database(self, rows=()):
        directory = self.state / ".scheduler"
        directory.mkdir()
        database = directory / "schedules.sqlite"
        # This is a unit-owned discovery fixture, never a production database.
        with closing(sqlite3.connect(database)) as connection, connection:
            connection.execute("CREATE TABLE scheduler_metadata(singleton INTEGER PRIMARY KEY, root_id TEXT)")
            connection.execute("INSERT INTO scheduler_metadata VALUES (1, ?)", (ROOT_ID,))
            connection.execute("CREATE TABLE occurrences(task_id TEXT, schedule_id TEXT, due_unix_nanos INTEGER)")
            connection.executemany("INSERT INTO occurrences VALUES (?, ?, ?)", rows)
        return database

    def admitted_case(self):
        case = self.case(self.service())
        self.lifetime.old_libexec = self.base / "libexec"
        self.lifetime.old_libexec.mkdir()
        directory = self.state / "tasks" / FIRST_ID
        directory.mkdir(parents=True)
        # Presence gates are local; the public Status response below owns the
        # validated admission verdict, not a second task-record interpreter.
        for name in ("meta.json", "supervisor.ref.json"):
            (directory / name).write_text("{}\n")
        case.fixture.old_daemon_marker.touch()
        supervisor = self.state / ".supervisor"
        supervisor.mkdir()
        client, daemon = b"finite client fixture", b"finite daemon fixture"
        client_digest, daemon_digest = hashlib.sha256(client).hexdigest(), hashlib.sha256(daemon).hexdigest()
        pair_digest = hashlib.sha256((client_digest + "\n" + daemon_digest).encode()).hexdigest()
        pair = supervisor / ("pueue-pair-" + pair_digest)
        pair.mkdir()
        for name, content in (("pueue", client), ("pueued", daemon)):
            (pair / name).write_bytes(content)
            (self.lifetime.old_libexec / name).write_bytes(content)
        (supervisor / "pueue-pair.json").write_text(json.dumps({
            "schema_version": 1, "client_sha256": client_digest, "daemon_sha256": daemon_digest,
            "pair_sha256": pair_digest,
        }))
        # Shape observed in service-linux-final-1/003-status: no task_record.
        response = {"schema_version": 1, "command": "status", "root_id": ROOT_ID, "task_id": FIRST_ID,
                    "admission": "admitted", "liveness": "running", "publication": "unknown",
                    "supervisor": {"matched": True, "state": "running", "numeric_task_id": 0}}
        return case, Occurrence(FIRST_ID, SCHEDULE_ID, MINUTE_NS), response

    def test_actual_admitted_status_shape_without_task_record_is_accepted(self):
        case, row, response = self.admitted_case()
        self.assertNotIn("task_record", response)
        with patch.object(case, "command", return_value=response):
            self.assertIs(case.await_admitted(row), response)

    def test_admission_rejects_status_for_a_different_root_or_task(self):
        case, row, response = self.admitted_case()
        for field in ("root_id", "task_id"):
            with (self.subTest(field=field), patch.object(case, "command", return_value={**response, field: SECOND_ID})):
                with self.assertRaisesRegex(RuntimeError, "identity changed"):
                    case.await_admitted(row)

    def test_unadmitted_or_invalid_status_cannot_satisfy_admission(self):
        case, row, response = self.admitted_case()
        case.deadline = time.monotonic()
        for admission in ("unknown", "not-admitted", "created", None):
            with (self.subTest(admission=admission),
                  patch.object(case, "command", return_value={**response, "admission": admission})):
                with self.assertRaisesRegex(RuntimeError, "scheduled task admission"):
                    case.await_admitted(row)

    def test_admitted_status_with_operational_fault_is_rejected(self):
        case, row, response = self.admitted_case()
        for fault in ({"error": "unreadable task evidence"}, {"failure": {"code": "evidence_fault"}}):
            with self.subTest(fault=fault), patch.object(case, "command", return_value={**response, **fault}):
                with self.assertRaisesRegex(RuntimeError, "operational failure"):
                    case.await_admitted(row)

    def test_admitted_status_still_requires_durable_files_and_cold_daemon_evidence(self):
        case, row, response = self.admitted_case()
        case.deadline = time.monotonic()
        directory = self.state / "tasks" / FIRST_ID
        for path in (directory / "meta.json", directory / "supervisor.ref.json", case.fixture.old_daemon_marker):
            with self.subTest(file=path.name), patch.object(case, "command", return_value=response):
                content = path.read_bytes()
                path.unlink()
                try:
                    with self.assertRaises(RuntimeError):
                        case.await_admitted(row)
                finally:
                    path.write_bytes(content)

    def test_admitted_status_does_not_bypass_canonical_supervisor_pair_validation(self):
        case, row, response = self.admitted_case()
        (self.state / ".supervisor" / "pueue-pair.json").write_text("{}\n")
        with patch.object(case, "command", return_value=response):
            with self.assertRaisesRegex(RuntimeError, "invalid identity"):
                case.await_admitted(row)

    def test_systemd_degraded_manager_still_starts_owned_transient_service(self):
        service = self.service()
        (service.directory / "stderr").write_text("scheduler phase=runtime schedule_id= task_id= code=ready\n")
        completed = [subprocess.CompletedProcess([], 1, "degraded\n", ""), subprocess.CompletedProcess([], 0, "", "")]
        with (patch.object(service, "control", side_effect=completed) as control,
              patch.object(service, "properties", side_effect=[{"LoadState": "not-found"}, self.properties(service)]),
              patch.object(native, "owner_released", return_value=False)):
            service.start()
        self.assertTrue(service.started)
        command = control.call_args_list[1].args[0]
        self.assertIn("--property=KillMode=process", command)
        self.assertIn("--property=Delegate=yes", command)
        self.assertIn("--property=ExitType=cgroup", command)
        self.assertIn("--property=TimeoutStopSec=infinity", command)
        self.assertIn("--property=SendSIGKILL=no", command)
        self.assertIn(f"--unit={service.unit}", command)
        self.assertIn("--expand-environment=no", command)
        env_index = command.index("/usr/bin/env")
        self.assertEqual(command[env_index + 1], "-i")
        self.assertIn("HOME=" + self.lifetime.env["HOME"], command)

    def test_restart_uses_separate_owned_service_and_retains_original_for_cleanup(self):
        case = self.case(self.service())
        original = case.service
        with patch.object(original, "stop") as stop, patch.object(native.SchedulerService, "start") as start:
            case.restart()
        stop.assert_called_once()
        start.assert_called_once()
        self.assertEqual(case.previous_services, [original])
        self.assertNotEqual(case.service.directory, original.directory)
        self.assertNotEqual(case.service.unit, original.unit)
        self.assertEqual(case.service.state, original.state)

    def test_retirement_attempts_prior_generation_after_current_failure(self):
        case = self.case(self.service())
        previous = native.SchedulerService(self.lifetime, self.state, self.base / "delegate", time.monotonic() + 30,
                                           generation=1)
        case.previous_services.append(previous)
        failure = RuntimeError("current retirement failed")
        with (patch.object(case.service, "retire", side_effect=failure),
              patch.object(previous, "retire") as retire_previous):
            with self.assertRaises(BaseExceptionGroup) as raised:
                application.retire_case_services(case)
        retire_previous.assert_called_once()
        self.assertEqual(raised.exception.exceptions, (failure,))

    def test_public_history_rejects_unknown_or_malformed_row_kind(self):
        for entry in ({"kind": "future"}, {"kind": "occurrence", "occurrence": []},
                      {"kind": "skip", "skip": {"from": "2026-01-01T00:02:00Z", "to": "2026-01-01T00:01:00Z"}}):
            with self.subTest(entry=entry), self.assertRaises(RuntimeError):
                sequence.history_occurrences({"schedule_history": {"entries": [entry]}})

    def test_execution_evidence_refuses_another_occurrence_and_incomplete_inspection(self):
        row = Occurrence(FIRST_ID, SCHEDULE_ID, MINUTE_NS)
        occurrence = {"task_id": FIRST_ID, "schedule_id": SCHEDULE_ID}
        base = {"occurrence": occurrence, "complete": True, "local_evidence": {"provenance": "validated_local"}}
        for changed in ({**base, "occurrence": {**occurrence, "task_id": SECOND_ID}},
                        {**base, "complete": False}, {**base, "local_evidence": {"provenance": "cached"}}):
            with self.subTest(changed=changed), self.assertRaises(RuntimeError):
                sequence.execution_evidence({"schedule_execution": changed}, row)

    def test_public_sealed_logs_must_match_both_manifest_and_real_bytes(self):
        directory = self.base / "sealed-task"
        (directory / "raw").mkdir(parents=True)
        manifest, descriptors = [], []
        for name, content in (("stdout", b"answer"), ("stderr", b"diagnostic")):
            path = directory / "raw" / name
            path.write_bytes(content)
            digest = hashlib.sha256(content).hexdigest()
            manifest.append({"path": "raw/" + name, "size": len(content), "sha256": digest})
            descriptors.append({"path": str(path), "available": True, "sealed": True,
                                "size": len(content), "sha256": digest})
        (directory / "provider.exit").write_text(json.dumps({"raw_manifest": manifest}))
        sequence.require_public_logs({"raw": descriptors}, directory)
        for fault in ({"sha256": "0" * 64}, {"sealed": False}, {"available": False},
                      {"path": str(directory / "other")}, {"size": 100}):
            changed = [{**descriptors[0], **fault}, descriptors[1]]
            with self.subTest(fault=fault), self.assertRaises(RuntimeError):
                sequence.require_public_logs({"raw": changed}, directory)

    def test_unavailable_manager_is_blocked_before_service_registration(self):
        service = self.service()
        with patch.object(service, "control", return_value=subprocess.CompletedProcess([], 1, "offline\n", "")):
            with self.assertRaisesRegex(AcceptanceBlocked, "systemd-user-manager-unavailable"):
                service.start()
        self.assertFalse(service.attempted)

    def test_existing_unit_is_never_adopted_or_stopped(self):
        service = self.service()
        with (patch.object(service, "control", return_value=subprocess.CompletedProcess([], 0, "running\n", "")),
              patch.object(service, "properties", return_value={"LoadState": "loaded"}),
              patch.object(service, "check") as check):
            with self.assertRaisesRegex(RuntimeError, "already exists"):
                service.start()
            service.stop()
        check.assert_not_called()

    def test_stop_targets_only_same_systemd_invocation_and_requires_clean_exit(self):
        service = self.service()
        service.attempted = service.started = True
        service.invocation = "e" * 32
        running = self.properties(service)
        stopped = {**running, "MainPID": "0", "ActiveState": "inactive"}
        with (patch.object(service, "properties", side_effect=[running, stopped]),
              patch.object(service, "check") as check, patch.object(native, "owner_released", return_value=True)):
            service.stop()
        check.assert_called_once_with(["systemctl", "--user", "--no-block", "stop", service.unit])
        self.assertTrue(service.stopped)

    def test_replaced_unit_refuses_stop(self):
        service = self.service()
        service.attempted = service.started = True
        service.invocation = "f" * 32
        with patch.object(service, "properties", return_value=self.properties(service)), patch.object(service, "check") as check:
            with self.assertRaisesRegex(RuntimeError, "replaced"):
                service.stop()
        check.assert_not_called()
        self.assertFalse(service.stopped)

    def test_unclean_service_exit_cannot_become_a_successful_stop(self):
        service = self.service()
        service.attempted = service.started = True
        stopped = {**self.properties(service), "MainPID": "0", "ActiveState": "failed", "ExecMainStatus": "1"}
        with (patch.object(service, "properties", side_effect=[self.properties(service), stopped]),
              patch.object(service, "check")):
            with self.assertRaisesRegex(RuntimeError, "stop cleanly"):
                service.stop()
        self.assertFalse(service.stopped)

    def test_draining_unit_proves_scheduler_exit_without_claiming_unit_retirement(self):
        service = self.service()
        service.attempted = service.started = True
        service.invocation = "e" * 32
        draining = {**self.properties(service), "MainPID": "0", "ActiveState": "deactivating"}
        with (patch.object(service, "properties", side_effect=[self.properties(service), draining]),
              patch.object(service, "check") as check, patch.object(native, "owner_released", return_value=True)):
            service.stop()
        check.assert_called_once_with(["systemctl", "--user", "--no-block", "stop", service.unit])
        self.assertTrue(service.stopped)

    def test_stop_request_alone_does_not_prove_scheduler_exit(self):
        service = self.service()
        service.invocation = "e" * 32
        with patch.object(service, "properties", return_value=self.properties(service)):
            self.assertFalse(service.linux_scheduler_stopped())

    def test_already_exited_scheduler_is_never_stopped_again(self):
        service = self.service()
        service.attempted = service.started = True
        service.invocation = "e" * 32
        exited = {**self.properties(service), "MainPID": "0"}
        with patch.object(service, "properties", return_value=exited), patch.object(service, "check") as check:
            with self.assertRaisesRegex(RuntimeError, "not live before orderly stop"):
                service.stop()
        check.assert_not_called()
        self.assertFalse(service.stopped)

    def test_launchd_configuration_preserves_children_and_cleans_scheduler_environment(self):
        command = native.scheduler_command(self.base / "with spaces" / "delegate", self.state, self.lifetime.env)
        definition = native.launchd_definition("org.delegate.unit", command, self.base)
        self.assertIs(definition["AbandonProcessGroup"], True)
        self.assertIs(definition["KeepAlive"], False)
        self.assertIs(definition["RunAtLoad"], True)
        self.assertEqual(definition["ProgramArguments"], command)
        self.assertEqual(command[:2], ["/usr/bin/env", "-i"])
        self.assertIn(str(self.base / "with spaces" / "delegate"), command)
        self.assertNotIn("EnvironmentVariables", definition)

    def test_mac_stop_boots_out_exact_owned_job_and_joins_lease(self):
        service = self.service("Darwin")
        service.attempted = service.started = True
        with (patch.object(service, "check") as check, patch.object(service, "launchd_absent", return_value=True),
              patch.object(native, "owner_released", return_value=True) as released):
            service.stop()
        check.assert_called_once_with(["launchctl", "bootout", service.target])
        released.assert_called_once_with(self.state)
        self.assertTrue(service.stopped)

    def test_unknown_bootout_error_is_not_job_absence(self):
        service = self.service("Darwin")
        with patch.object(service, "control", return_value=subprocess.CompletedProcess([], 1, "", "permission denied")):
            with self.assertRaisesRegex(RuntimeError, "absence"):
                service.launchd_absent()

    def test_transport_environment_drops_secrets_and_keeps_manager_coordinates(self):
        with patch.dict(os.environ, {"SECRET_TOKEN": "unit-secret", "DBUS_SESSION_BUS_ADDRESS": "unix:path=unit", "XDG_RUNTIME_DIR": str(self.parent)}, clear=True):
            selected = native.manager_environment()
        self.assertNotIn("SECRET_TOKEN", selected)
        self.assertEqual(selected["DBUS_SESSION_BUS_ADDRESS"], "unix:path=unit")
        self.assertEqual(selected["XDG_RUNTIME_DIR"], str(self.parent))

    def test_scheduler_join_proof_reads_actual_owner_lock(self):
        directory = self.state / ".scheduler"
        directory.mkdir()
        with (directory / "owner.lock").open("xb+") as stream:
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.assertFalse(native.owner_released(self.state))
            fcntl.flock(stream, fcntl.LOCK_UN)
            self.assertTrue(native.owner_released(self.state))

    def test_service_ready_requires_the_post_lease_runtime_diagnostic(self):
        service = self.service()
        self.assertFalse(service.ready())
        output = service.directory / "stderr"
        output.write_text("scheduler phase=claim schedule_id=x task_id=y code=armed\n")
        self.assertFalse(service.ready())
        output.write_text("scheduler phase=runtime schedule_id= task_id= code=ready\n")
        self.assertTrue(service.ready())

    def test_occurrence_discovery_is_read_only_and_ordered(self):
        database = self.database(((SECOND_ID, SCHEDULE_ID, 2 * MINUTE_NS), (FIRST_ID, SCHEDULE_ID, MINUTE_NS)))
        before = database.read_bytes()
        observed = read_occurrences(self.state, ROOT_ID)
        self.assertEqual(observed, (Occurrence(FIRST_ID, SCHEDULE_ID, MINUTE_NS), Occurrence(SECOND_ID, SCHEDULE_ID, 2 * MINUTE_NS)))
        self.assertEqual(database.read_bytes(), before)
        self.assertFalse(database.with_name(database.name + "-journal").exists())

    def test_missing_occurrence_database_is_never_created(self):
        with self.assertRaises(FileNotFoundError):
            read_occurrences(self.state, ROOT_ID)
        self.assertFalse((self.state / ".scheduler").exists())

    def test_discovery_rejects_wrong_root_and_nonminute_due_time(self):
        self.database(((FIRST_ID, SCHEDULE_ID, MINUTE_NS + 1),))
        with self.assertRaisesRegex(RuntimeError, "different root"):
            read_occurrences(self.state, "f" * 32)
        with self.assertRaisesRegex(RuntimeError, "invalid due time"):
            read_occurrences(self.state, ROOT_ID)

    def test_symlinked_occurrence_database_is_refused(self):
        database = self.database()
        moved = database.with_name("other.sqlite")
        database.rename(moved)
        database.symlink_to(moved.name)
        with self.assertRaisesRegex(RuntimeError, "unsafe"):
            read_occurrences(self.state, ROOT_ID)

    def test_read_only_discovery_never_observes_cached_verdict_columns(self):
        database = self.database(((FIRST_ID, SCHEDULE_ID, MINUTE_NS),))
        statements = []
        connect = sqlite3.connect

        def observe(location, **options):
            self.assertTrue(location.endswith("?mode=ro"))
            self.assertIs(options["uri"], True)
            connection = connect(location, **options)
            connection.set_trace_callback(statements.append)
            return connection

        with patch("scripts.delegate_acceptance.private_cli.scheduler_occurrences.sqlite3.connect", side_effect=observe):
            self.assertEqual(read_occurrences(self.state, ROOT_ID)[0].task_id, FIRST_ID)
        self.assertTrue(database.is_file())
        self.assertEqual(len(statements), 2)
        self.assertTrue(all(statement.startswith("SELECT ") for statement in statements))
        self.assertFalse(any("publication" in statement or "release" in statement for statement in statements))

    def test_inventory_checks_reject_duplicate_ids_backlog_and_wrong_schedule(self):
        rows = (Occurrence(FIRST_ID, SCHEDULE_ID, MINUTE_NS), Occurrence(FIRST_ID, SCHEDULE_ID, 2 * MINUTE_NS))
        with self.assertRaisesRegex(RuntimeError, "reused"):
            require_occurrences(rows, SCHEDULE_ID, (MINUTE_NS, 2 * MINUTE_NS))
        with self.assertRaisesRegex(RuntimeError, "inventory"):
            require_occurrences(rows, SCHEDULE_ID, (MINUTE_NS,))
        with self.assertRaisesRegex(RuntimeError, "inventory"):
            require_occurrences(rows[:1], "e" * 32, (MINUTE_NS,))

    def test_discovery_registers_every_id_before_a_wrong_schedule_assertion(self):
        service = self.service()
        self.database(((FIRST_ID, SCHEDULE_ID, MINUTE_NS), (SECOND_ID, "e" * 32, MINUTE_NS)))
        case = self.case(service)
        with self.assertRaisesRegex(RuntimeError, "different schedule"):
            case.occurrences(2)
        self.assertEqual(set(self.lifetime.attempted_task_ids), {FIRST_ID, SECOND_ID})

    def test_finalizer_stops_service_and_rediscovers_before_canonical_cleanup(self):
        service = self.service()
        case = self.case(service)
        events = []
        failure = RuntimeError("case assertion failed")
        args = application.parse_arguments([])
        with (patch.object(service, "stop", side_effect=lambda: events.append("stop")),
              patch.object(case, "discover", side_effect=lambda: events.append("discover")),
              patch.object(application, "finalize_fixture", side_effect=lambda *args: events.append("finalize"))):
            with self.assertRaisesRegex(RuntimeError, "case assertion failed"):
                application.finish_case(args=args, case=case, lifetime=self.lifetime, receipt=case.receipt, failure=failure)
        self.assertEqual(events, ["stop", "discover", "finalize"])

    def test_uncertain_service_stop_retains_directories_without_running_finalizer(self):
        service = self.service()
        case = self.case(service)
        with (patch.object(service, "stop", side_effect=TimeoutError("unknown service closure")),
              patch.object(case, "discover") as discover,
              patch.object(application, "finalize_fixture") as finalize):
            with self.assertRaisesRegex(RuntimeError, "fixture retained"):
                application.finish_case(args=application.parse_arguments([]), case=case, lifetime=self.lifetime,
                                        receipt=case.receipt, failure=None)
        finalize.assert_not_called()
        discover.assert_not_called()
        self.assertTrue(self.base.is_dir())
        recorded = json.loads(case.receipt.output.read_text())
        self.assertEqual(recorded["result"], "FAIL")
        self.assertEqual(recorded["cleanup"]["status"], "retained")

    def test_failed_discovery_after_stop_retains_and_never_shuts_down_supervisor(self):
        service = self.service()
        case = self.case(service)
        with (patch.object(service, "stop"), patch.object(case, "discover", side_effect=RuntimeError("unreadable store")),
              patch.object(application, "shutdown_private_supervisor") as shutdown,
              patch.object(application, "finalize_fixture") as finalize):
            with self.assertRaisesRegex(RuntimeError, "fixture retained"):
                application.finish_case(args=application.parse_arguments([]), case=case, lifetime=self.lifetime,
                                        receipt=case.receipt, failure=None)
        shutdown.assert_not_called()
        finalize.assert_not_called()
        self.assertTrue(self.base.is_dir())

    def test_success_requires_daemon_shutdown_before_unit_retirement_and_directory_cleanup(self):
        service = self.service()
        case = self.case(service)
        self.lifetime.private_config = self.state / ".supervisor" / "pueue.yml"
        self.lifetime.old_libexec = self.base / "libexec"
        events = []
        with (patch.object(service, "stop", side_effect=lambda: events.append("stop")),
              patch.object(case, "discover", side_effect=lambda: events.append("discover")),
              patch.object(application, "shutdown_private_supervisor", side_effect=lambda *args, **kwargs: events.append("daemon")),
              patch.object(service, "retire", side_effect=lambda: events.append("retire")),
              patch.object(application, "finalize_fixture", side_effect=lambda *args: events.append("finalize"))):
            application.finish_case(args=application.parse_arguments([]), case=case, lifetime=self.lifetime,
                                    receipt=case.receipt, failure=None)
        self.assertEqual(events, ["stop", "discover", "daemon", "retire", "finalize"])

    def test_collection_uses_existing_outcome_oracle_and_exact_task_closure(self):
        case = self.case(self.service())
        row = Occurrence(FIRST_ID, SCHEDULE_ID, MINUTE_NS)
        outcome = {"root_id": ROOT_ID, "task_id": FIRST_ID}
        collected = {**outcome, "publication": "committed"}
        closure = {"root_id": ROOT_ID, "tasks": {FIRST_ID: {
            "worker_wait": {"result": "success"}, "termination": {"scope_empty": True},
        }}}
        with (patch.object(case, "await_admitted"), patch.object(case, "command", return_value=collected),
              patch.object(cases, "verify_collected_outcome", return_value=(outcome, cases.EXPECTED_PAYLOAD)) as verify,
              patch.object(cases, "await_managed_lifetime_closure", return_value=closure) as closed):
            case.collect(row)
        verify.assert_called_once_with(collected, self.state / "tasks" / FIRST_ID, "committed")
        self.assertEqual(closed.call_args.args, (self.state, (FIRST_ID,)))
        self.assertEqual(case.receipt.counts["collected_task_count"], 1)

    def test_committed_publication_does_not_replace_worker_or_scope_closure(self):
        case = self.case(self.service())
        row = Occurrence(FIRST_ID, SCHEDULE_ID, MINUTE_NS)
        outcome = {"root_id": ROOT_ID, "task_id": FIRST_ID}
        collected = {**outcome, "publication": "committed"}
        for worker_result, empty in (("wait-error", True), ("success", False)):
            closure = {"root_id": ROOT_ID, "tasks": {FIRST_ID: {
                "worker_wait": {"result": worker_result}, "termination": {"scope_empty": empty},
            }}}
            with (self.subTest(worker_result=worker_result, empty=empty),
                  patch.object(case, "await_admitted"), patch.object(case, "command", return_value=collected),
                  patch.object(cases, "verify_collected_outcome", return_value=(outcome, cases.EXPECTED_PAYLOAD)),
                  patch.object(cases, "await_managed_lifetime_closure", return_value=closure)):
                with self.assertRaisesRegex(RuntimeError, "canonical"):
                    case.collect(row)
        self.assertEqual(case.receipt.counts["collected_task_count"], 0)

    def test_public_timestamp_keeps_nanoseconds_and_utc_semantics(self):
        self.assertEqual(timestamp_ns("1970-01-01T00:01:00.000000001Z"), MINUTE_NS + 1)

    def test_output_refuses_existing_directory_and_preserves_contents(self):
        marker = self.parent / "kept"
        marker.write_text("existing data")
        with self.assertRaises(FileExistsError):
            application.output_directory(self.parent, self.lifetime.old_layout_ownership)
        self.assertEqual(marker.read_text(), "existing data")

    def test_default_paths_use_repository_binaries_without_building(self):
        selected = application.parse_arguments([])
        self.assertEqual(selected.delegate, application.REPOSITORY / "bin" / "delegate")
        self.assertEqual(selected.runner, application.REPOSITORY / "bin" / "delegate-run")
        self.assertEqual(selected.pueue, application.REPOSITORY / "bin" / "test-supervisor" / "pueue")
        self.assertEqual(selected.pueued, application.REPOSITORY / "bin" / "test-supervisor" / "pueued")
        self.assertIsNone(selected.output)
        self.assertEqual(replace(selected, scenario="successive").scenario, "successive")


if __name__ == "__main__":
    unittest.main()
