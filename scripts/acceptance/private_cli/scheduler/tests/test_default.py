"""Default-root driver contracts; no native scheduler or provider is launched."""

from contextlib import redirect_stderr, redirect_stdout
from dataclasses import replace
import io
import json
from pathlib import Path
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
import sys


repository_root = str(Path(__file__).resolve().parents[5])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.private_cli.scheduler import acceptance as application
from scripts.acceptance.private_cli.scheduler import cases as cases
from scripts.acceptance.private_cli.scheduler import default as default
from scripts.acceptance.private_cli.scheduler import host as foreground
from scripts.acceptance.private_cli import packaged_defaults as packaged_defaults
from scripts.acceptance.private_cli.configuration import PRIVATE_ACCEPTANCE_PREFIX
from scripts.acceptance.private_cli.errors import AcceptanceBlocked
from scripts.acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.acceptance.private_cli.fixture_records import FixtureInstallation
from scripts.acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.acceptance.private_cli.scheduler.occurrences import MINUTE_NS
from scripts.acceptance.private_cli.scheduler.occurrences import Occurrence


ROOT_ID = "a" * 32
SCHEDULE_ID = "b" * 32
TASK_ID = "c" * 32


class SchedulerDefaultTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="scheduler-default-unit-")
        self.addCleanup(temporary.cleanup)
        self.parent = Path(temporary.name).resolve(strict=True)
        self.lifetime = FixtureLifetime()
        self.lifetime.base_token = self.lifetime.ownership.create(
            "unit fixture profile", self.parent / (PRIVATE_ACCEPTANCE_PREFIX + "default"), self.parent,
        )
        self.base = self.lifetime.base_token.directory
        self.lifetime.base = self.base
        self.lifetime.capture_token = self.lifetime.ownership.create("captures", self.base / "captures", self.base)
        self.addCleanup(self.release_tokens)
        home = self.lifetime.ownership.create("home", self.base / "home", self.base).directory
        workspace = self.lifetime.ownership.create("workspace", self.base / "workspace", self.base).directory
        state = self.lifetime.ownership.create("original state", self.base / "state", self.base).directory
        self.lifetime.env = {"HOME": str(home), "XDG_CONFIG_HOME": str(self.parent / "unowned-config"),
                             "PATH": "/usr/bin:/bin"}
        self.lifetime.old_install = self.base
        self.lifetime.old_libexec = self.base / "libexec"
        self.lifetime.managed_state_roots.append((state, self.lifetime.attempted_task_ids))
        self.lifetime.private_config = state / ".supervisor" / "pueue.yml"
        self.fixture = FixtureInstallation(
            delegate_binary=self.base / "delegate", runner_binary=self.base / "delegate-run",
            upgraded_delegate_binary=self.base / "delegate", upgraded_runner_binary=self.base / "delegate-run",
            pueue_binary=self.base / "pueue", pueued_binary=self.base / "pueued", workspace=workspace,
            provider_bin=self.base / "provider-bin", brief=self.base / "brief", upgraded_install=self.base / "install",
            old_daemon_marker=self.base / "old-starts", upgraded_daemon_marker=self.base / "new-starts",
            state=state, waiter_directory=self.base / "waiters", lease_nonce="f" * 32,
        )
        self.receipt = AcceptanceReceipt(self.parent / "receipt.json", 1, 1, {})

    def release_tokens(self):
        token = self.lifetime.base_token
        if token is None:
            raise AssertionError("fixture base ownership token is absent")
        if not token.consumed:
            self.lifetime.ownership.release()

    def bound_fixture(self, system="Linux"):
        with patch.object(packaged_defaults.platform, "system", return_value=system):
            return default.bind_default_fixture(self.fixture, self.lifetime)

    def case(self, *, use_default=True):
        fixture = self.bound_fixture() if use_default else self.fixture
        host = foreground.SchedulerHost(self.lifetime, fixture.state, self.base / "delegate",
                                        time.monotonic() + 30, default_root=use_default)
        return cases.SchedulerCase(fixture, self.lifetime, self.parent, self.receipt, host,
                                   time.monotonic() + 30, default_root=use_default)

    def test_arguments_select_only_managed_host_without_changing_existing_defaults(self):
        args = application.parse_arguments(["--scenario", "fresh-default"])
        self.assertEqual((args.scenario, args.hosting), ("fresh-default", "managed-host"))
        self.assertEqual(application.parse_arguments([]).scenario, "all")
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            application.parse_arguments(["--scenario", "fresh-default", "--hosting", "service"])
        with patch.object(application, "prepare_fixture") as prepare:
            with self.assertRaisesRegex(AcceptanceBlocked, "requires-managed-host"):
                application.run_case(replace(args, hosting="service"), self.parent, "fresh-default")
        prepare.assert_not_called()

    def test_main_all_selects_fresh_default_only_for_managed_host(self):
        for arguments, expected in (
            ([], ("successive", "overlap-stop", "fresh-default")),
            (["--scenario", "all", "--hosting", "service"], ("successive", "overlap-stop")),
            (["--scenario", "fresh-default"], ("fresh-default",)),
            (["--scenario", "successive"], ("successive",)),
            (["--scenario", "overlap-stop", "--hosting", "service"], ("overlap-stop",)),
        ):
            with (self.subTest(arguments=arguments), redirect_stdout(io.StringIO()),
                  patch.object(application, "output_directory", return_value=self.parent),
                  patch.object(application, "run_case") as run):
                self.assertEqual(application.main(arguments), 0)
            self.assertEqual(tuple(call.args[2] for call in run.call_args_list), expected)

    def test_native_paths_reset_ambient_xdg_and_preserve_existing_owners(self):
        home = self.base / "home"
        supplemental_ids = [TASK_ID]
        supplemental = (self.base / "other-state", supplemental_ids)
        self.lifetime.managed_state_roots.append(supplemental)
        ownership, token = self.lifetime.ownership, self.lifetime.base_token
        for system, suffix in (("Linux", Path(".config/delegation-layer")),
                               ("Darwin", Path("Library/Application Support/delegation-layer"))):
            with self.subTest(system=system):
                self.lifetime.managed_state_roots[0] = (self.fixture.state, self.lifetime.attempted_task_ids)
                fixture = self.bound_fixture(system)
                self.assertEqual(fixture.state, home / suffix)
                self.assertFalse(fixture.state.exists())
                self.assertFalse((fixture.state / "root.json").exists())
                self.assertEqual(self.lifetime.env["XDG_CONFIG_HOME"], str(home / ".config"))
                self.assertEqual(self.lifetime.private_config, fixture.state / ".supervisor/pueue.yml")
                self.assertEqual(self.lifetime.managed_state_roots[0][0], fixture.state)
                self.assertIs(self.lifetime.managed_state_roots[0][1], self.lifetime.attempted_task_ids)
                self.assertIs(self.lifetime.managed_state_roots[1], supplemental)
                self.assertIs(self.lifetime.ownership, ownership)
                self.assertIs(self.lifetime.base_token, token)
                self.assertTrue(self.fixture.state.is_dir())
        self.assertFalse((self.parent / "unowned-config").exists())

    def test_binding_refuses_existing_or_symlinked_default_before_launch(self):
        state = self.base / "home/.config/delegation-layer"
        state.parent.mkdir()
        state.mkdir()
        with self.assertRaisesRegex(RuntimeError, "not fresh"):
            self.bound_fixture()
        state.rmdir()
        state.symlink_to(self.parent / "missing")
        with self.assertRaisesRegex(RuntimeError, "not fresh"):
            self.bound_fixture()
        self.assertEqual(self.lifetime.managed_state_roots[0][0], self.fixture.state)

    def test_binding_refuses_unowned_home_and_different_primary_cleanup(self):
        self.lifetime.env["HOME"] = str(self.parent)
        with self.assertRaisesRegex(RuntimeError, "outside the owned fixture"):
            self.bound_fixture()
        self.lifetime.env["HOME"] = str(self.base / "home")
        self.lifetime.managed_state_roots[0] = (self.base / "other", [])
        with self.assertRaisesRegex(RuntimeError, "different primary cleanup owner"):
            self.bound_fixture()

    def test_host_omits_root_and_uses_existing_base_before_default_storage_exists(self):
        case = self.case()
        host = case.service
        response = Mock()
        response.json.return_value = {"root_id": ROOT_ID, "scheduler": {"state": "ready", "ready": True}}
        with (patch.object(host.processes, "start") as start,
              patch.object(foreground, "wait_until"), patch.object(host.processes, "run", return_value=response) as run):
            host.start()
            self.assertEqual(host.command("status"), {"state": "ready", "ready": True})
        self.assertEqual(start.call_args.args[1:3], ([host.delegate, "scheduler", "serve"], self.base))
        self.assertIs(start.call_args.kwargs["start_new_session"], True)
        self.assertEqual(run.call_args.args[1:3], ([host.delegate, "scheduler", "status", "--json"], self.base))
        self.assertFalse(case.fixture.state.exists())

    def test_case_ordinary_argv_runs_in_other_cwd_and_rejects_different_root(self):
        case = self.case()
        case.root_id = ROOT_ID
        response = Mock()
        response.json.return_value = {"root_id": ROOT_ID, "schedules": []}
        with (patch.object(case.service.processes, "run", return_value=response) as run,
              patch.object(cases, "run_json") as legacy):
            self.assertEqual(default.schedules(case), [])
            self.assertEqual(run.call_args.args[1:3],
                             ([case.delegate, "schedule", "list", "--json"], self.fixture.workspace))
            legacy.assert_not_called()
            response.json.return_value = {"root_id": TASK_ID, "schedules": []}
            with self.assertRaisesRegex(RuntimeError, "changed root identity"):
                default.schedules(case)
        self.assertNotEqual(case.fixture.workspace, case.service.directory)

    def test_existing_case_keeps_explicit_root_client_boundary(self):
        case = self.case(use_default=False)
        self.assertEqual(case.service.prefix, ["--root", str(self.fixture.state)])
        self.assertEqual(case.service.directory, self.fixture.state)
        with patch.object(cases, "run_json", return_value={"schedules": []}) as command:
            default.schedules(case)
        self.assertEqual(command.call_args.args[2], ["--root", str(self.fixture.state), "schedule", "list", "--json"])

    def test_fresh_scenario_serves_first_and_deletes_before_validating_one_task(self):
        case = self.case()
        row = Occurrence(TASK_ID, SCHEDULE_ID, MINUTE_NS)
        events = []
        definition = {}

        def command(_name, arguments, expected=(0,)):
            family, action = arguments[:2]
            events.append(action)
            if family == "scheduler":
                return {"root_id": ROOT_ID, "scheduler": {"state": "ready", "ready": True}}
            if action == "list":
                return {"schedules": [dict(definition)] if definition else []}
            if action == "create":
                self.assertEqual(arguments[arguments.index("--id") + 1], SCHEDULE_ID)
                self.assertEqual(arguments[arguments.index("--cwd") + 1], str(case.fixture.workspace))
                self.assertEqual(arguments[arguments.index("--provider") + 1], "pi:json")
                definition.update(id=SCHEDULE_ID, state="enabled", created_at="1970-01-01T00:00:10Z")
                return {"schedule": {**definition, "saved": True}, "scheduler": {"ready": True}}
            if action == "delete":
                definition["state"] = "deleted"
            if action == "history":
                return {"schedule_history": {"entries": [{"kind": "occurrence", "occurrence": {
                    "task_id": TASK_ID, "due": "1970-01-01T00:01:00Z"}}]}}
            return {"schedule": dict(definition)}

        def start():
            self.assertFalse(case.fixture.state.exists())
            self.assertFalse(definition)
            events.append("serve")

        with (patch.object(case.service, "start", side_effect=start),
              patch.object(case, "command", side_effect=command),
              patch.object(case, "discover", side_effect=[(), (row,)]),
              patch.object(case, "occurrences", return_value=(row,)),
              patch.object(case, "collect", side_effect=lambda item: events.append("collect")) as collect,
              patch.object(default, "await_release") as release,
              patch.object(default, "require_public_result") as public,
              patch.object(default.time, "time_ns", return_value=15_000_000_000),
              patch.object(default.uuid, "uuid4", return_value=SimpleNamespace(hex=SCHEDULE_ID))):
            default.run_fresh_default(case)
        self.assertEqual(events[0], "serve")
        self.assertLess(events.index("delete"), events.index("collect"))
        self.assertEqual(events.count("create"), 1)
        collect.assert_called_once_with(row)
        release.assert_called_once_with(case, row)
        public.assert_called_once_with(case, row, "committed")
        self.assertEqual(self.receipt.details["actual_minute_ticks"], 1)

    def test_delete_targets_only_registered_id_and_confirms_deleted_state(self):
        case = self.case()
        case.schedule_id = SCHEDULE_ID
        with patch.object(case, "command", side_effect=[
            {"schedules": [{"id": SCHEDULE_ID, "state": "enabled"}]},
            {"schedule": {"id": SCHEDULE_ID}}, {"schedule": {"id": SCHEDULE_ID, "state": "deleted"}},
        ]) as command:
            default.delete_default_schedule(case)
        self.assertEqual(command.call_args_list[1].args[1], ["schedule", "delete", SCHEDULE_ID])
        with patch.object(case, "command", return_value={"schedules": [{"id": TASK_ID, "state": "enabled"}]}) as command:
            default.delete_default_schedule(case)
        self.assertEqual(command.call_count, 1)

    def test_lost_create_response_deletes_then_joins_and_registers_every_discovered_task(self):
        case = self.case()
        case.root_id, case.schedule_id = ROOT_ID, SCHEDULE_ID
        case.deadline = time.monotonic() - 1
        failure = RuntimeError("create response lost")
        events = []

        def finalized(_args, observed_failure, lifetime, _receipt, success):
            self.assertIs(observed_failure, failure)
            self.assertFalse(success)
            self.assertEqual(lifetime.attempted_task_ids, [TASK_ID, "d" * 32])
            self.assertEqual(lifetime.managed_state_roots[0][0], case.fixture.state)
            events.append("finalize")

        with (patch.object(application, "delete_default_schedule", side_effect=lambda item: events.append("delete")),
              patch.object(case.service, "stop", side_effect=lambda: events.append("stop")),
              patch.object(cases, "read_occurrences", return_value=(
                  Occurrence(TASK_ID, SCHEDULE_ID, MINUTE_NS), Occurrence("d" * 32, SCHEDULE_ID, 2 * MINUTE_NS))),
              patch.object(application, "finalize_fixture", side_effect=finalized)):
            with self.assertRaisesRegex(RuntimeError, "create response lost"):
                application.finish_case(args=application.parse_arguments([]), case=case, lifetime=self.lifetime,
                                        receipt=self.receipt, failure=failure)
        self.assertEqual(events, ["delete", "stop", "finalize"])
        self.assertTrue(self.base.is_dir())

    def test_delete_failure_still_stops_host_and_finalizes_with_both_faults(self):
        case = self.case()
        original, deletion = RuntimeError("case failed"), RuntimeError("delete failed")
        events = []
        with (patch.object(application, "delete_default_schedule", side_effect=deletion),
              patch.object(case.service, "stop", side_effect=lambda: events.append("stop")),
              patch.object(case, "discover", side_effect=lambda: events.append("discover")),
              patch.object(application, "finalize_fixture") as finalize):
            with self.assertRaises(BaseExceptionGroup) as raised:
                application.finish_case(args=application.parse_arguments([]), case=case, lifetime=self.lifetime,
                                        receipt=self.receipt, failure=original)
        self.assertEqual(raised.exception.exceptions, (original, deletion))
        self.assertEqual(events, ["stop", "discover"])
        self.assertIs(finalize.call_args.args[1], raised.exception)
        self.assertIs(finalize.call_args.args[4], False)
        self.assertTrue(self.base.is_dir())

    def test_success_uses_existing_finalizer_to_remove_only_owned_profile(self):
        case = self.case()
        case.fixture.state.mkdir(parents=True, mode=0o700)
        root_record = case.fixture.state / "root.json"
        root_record.write_text(json.dumps({"schema_version": 1, "root_id": ROOT_ID,
                                           "created_at": "2026-11-02T00:00:00Z"}))
        root_record.chmod(0o600)
        sibling = self.parent / "user-data"
        sibling.write_text("preserve")
        with (patch.object(case.service, "stop"),
              patch.object(application, "shutdown_terminal_private_supervisor") as shutdown):
            application.finish_case(args=application.parse_arguments([]), case=case, lifetime=self.lifetime,
                                    receipt=self.receipt, failure=None)
        self.assertFalse(self.base.exists())
        self.assertEqual(sibling.read_text(), "preserve")
        self.assertEqual(shutdown.call_args.kwargs["state"], case.fixture.state)
        receipt = json.loads(self.receipt.output.read_text())
        self.assertEqual(receipt["result"], "PASS")
        self.assertEqual(receipt["cleanup"]["private_root"], {"status": "removed", "verified": True})


if __name__ == "__main__":
    unittest.main()
