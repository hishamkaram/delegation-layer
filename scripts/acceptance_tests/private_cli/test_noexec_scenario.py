"""Noexec Delegate state: fixture placement, ownership cleanup, scenario entry, and the external case flow.

These tests run the real harness code over fake binaries and mocked edges. They prove harness
wiring and policy only; whether a public CLI executes correctly on a real noexec mount is
native acceptance evidence.
"""

from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from scripts.acceptance_tests.resources import DirectoryPinOwner
from scripts.acceptance_tests.private_cli.fixtures import ready_path
from scripts.delegate_acceptance.private_cli import application, handoff as handoff_impl
from scripts.delegate_acceptance.private_cli import fixture_setup, scenario_entry
from scripts.delegate_acceptance.private_cli import handoff_cleanup as handoff_cleanup_impl
from scripts.delegate_acceptance.private_cli.configuration import (
    EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
    EXTERNAL_NOEXEC_STATE_SCENARIO,
)
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.finalization import finish_root_cleanup
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli.handoff_observation import ObservedHandoffTask
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt

TASK = "1" * 32
INSPECTION = "3" * 32
OTHER = "4" * 32
# Shape of the live Linux noexec status: committed publication before any admission observation.
TERMINAL_STATUS = {
    "schema_version": 1, "command": "status", "root_id": "r" * 32, "task_id": TASK, "admission": "unknown",
    "liveness": "undetermined", "publication": "committed",
    "outcome": {"verdict": "committed", "payload": {"basename": "result.txt", "length": 36}},
}
PROBE_EVIDENCE = {"control": "executed", "state_container": "execution-refused"}


class FixtureCase(unittest.TestCase):
    """A fake home, caller-owned noexec parent and fake binaries around the real fixture setup."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-noexec-fixture-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.home = self.root / "home"
        self.parent = self.root / "caller-noexec"
        self.home.mkdir()
        self.parent.mkdir()
        self.sentinel = self.parent / "caller-owned.txt"
        self.sentinel.write_text("owned by the caller\n")
        self.binaries = {}
        for name in ("delegate", "runner", "pueue", "pueued"):
            binary = self.root / ("fake-" + name)
            binary.write_text("#!/bin/sh\nexit 0\n")
            binary.chmod(0o755)
            self.binaries[name] = binary
        self.receipt = AcceptanceReceipt(self.root / "receipt.json", 1, 1, {})
        self.lifetime = FixtureLifetime(DirectoryPinOwner(self), DirectoryPinOwner(self))

    def arguments(self, scenario, parent):
        return SimpleNamespace(
            scenario=scenario, noexec_state_parent=parent, upgraded_delegate=None, upgraded_runner=None,
            **self.binaries,
        )

    def prepare(self, scenario, parent=None, probe=None):
        probe = probe or Mock(return_value=dict(PROBE_EVIDENCE))
        with patch.object(Path, "home", return_value=self.home), \
                patch.object(fixture_setup, "require_execution_refused", probe):
            fixture = fixture_setup.prepare_fixture(
                self.arguments(scenario, parent), "n" * 32, False, self.lifetime, self.root / "responses",
                self.receipt,
            )
        return fixture, probe


class NoexecFixtureTests(FixtureCase):
    def test_default_scenarios_keep_state_under_the_acceptance_root_without_a_temporary_override(self):
        fixture, probe = self.prepare(EXTERNAL_INSPECTION_HANDOFF_SCENARIO)
        base = ready_path(self.lifetime.base)
        self.assertEqual(fixture.state, base / "state")
        self.assertEqual(self.lifetime.private_config, base / "state" / ".supervisor" / "pueue.yml")
        self.assertNotIn("TMPDIR", self.lifetime.env)
        self.assertFalse((base / "tmp").exists())
        self.assertNotIn("noexec_state", self.receipt.details)
        probe.assert_not_called()
        self.assertEqual(list(self.parent.iterdir()), [self.sentinel])

    def test_capacity_scenarios_still_register_their_state_for_managed_closure(self):
        fixture, _ = self.prepare("capacity")
        self.assertEqual(self.lifetime.managed_state_roots, [(fixture.state, self.lifetime.attempted_task_ids)])

    def test_noexec_scenario_puts_only_the_state_root_in_the_caller_parent(self):
        fixture, probe = self.prepare(EXTERNAL_NOEXEC_STATE_SCENARIO, self.parent)
        base = ready_path(self.lifetime.base)
        container = self.parent / base.name
        self.assertEqual(fixture.state, container / "state")
        self.assertEqual(sorted(entry.name for entry in container.iterdir()), ["state"])
        self.assertEqual(set(self.parent.iterdir()), {self.sentinel, container})
        self.assertEqual(self.lifetime.private_config, container / "state" / ".supervisor" / "pueue.yml")
        for tool in (fixture.workspace, fixture.provider_bin, fixture.waiter_directory, self.lifetime.old_install,
                     self.lifetime.old_libexec, self.lifetime.upgraded_libexec, self.lifetime.gate):
            tool = ready_path(tool)
            self.assertTrue(tool.is_relative_to(base))
            self.assertFalse(tool.is_relative_to(container))
        self.assertEqual(self.lifetime.env["TMPDIR"], str(base / "tmp"))
        self.assertEqual(self.lifetime.env["HOME"], str(base / "home"))
        self.assertTrue((base / "tmp").is_dir())
        probe.assert_called_once_with(container, base / "tmp")
        self.assertEqual(self.lifetime.managed_state_roots, [])
        self.assertEqual(
            self.receipt.details["noexec_state"], {"state_container": str(container), "probe": PROBE_EVIDENCE}
        )

    def test_the_caller_parent_is_canonicalized_through_an_alias(self):
        alias = self.root / "alias"
        alias.symlink_to(self.parent)
        fixture, _ = self.prepare(EXTERNAL_NOEXEC_STATE_SCENARIO, alias)
        self.assertEqual(fixture.state.parent.parent, self.parent)

    def test_success_cleanup_removes_only_the_container_and_acceptance_root(self):
        self.prepare(EXTERNAL_NOEXEC_STATE_SCENARIO, self.parent)
        base = ready_path(self.lifetime.base)
        container = self.parent / base.name
        success, root_cleanup = finish_root_cleanup([], None, self.lifetime, True)
        self.assertTrue(success)
        self.assertEqual(root_cleanup, {"status": "removed", "verified": True})
        self.assertFalse(base.exists())
        self.assertFalse(container.exists())
        self.assertEqual(list(self.parent.iterdir()), [self.sentinel])
        self.assertEqual(self.sentinel.read_text(), "owned by the caller\n")

    def test_a_later_failure_retains_the_named_container_and_never_touches_the_caller_parent(self):
        self.prepare(EXTERNAL_NOEXEC_STATE_SCENARIO, self.parent)
        container = self.parent / ready_path(self.lifetime.base).name
        (container / "state" / "evidence.txt").write_text("diagnostic\n")
        success, root_cleanup = finish_root_cleanup([], RuntimeError("later failure"), self.lifetime, False)
        self.assertFalse(success)
        self.assertEqual(root_cleanup, {"status": "retained", "verified": True})
        self.assertEqual(self.receipt.details["noexec_state"]["state_container"], str(container))
        self.assertEqual(self.receipt.details["noexec_state"]["probe"], PROBE_EVIDENCE)
        self.assertEqual((container / "state" / "evidence.txt").read_text(), "diagnostic\n")
        self.assertEqual(set(self.parent.iterdir()), {self.sentinel, container})
        self.assertEqual(self.sentinel.read_text(), "owned by the caller\n")

    def test_a_blocked_probe_retains_the_container_with_a_diagnostic_receipt(self):
        probe = Mock(side_effect=AcceptanceBlocked("noexec-state-parent-executable"))
        with self.assertRaises(AcceptanceBlocked) as blocked:
            self.prepare(EXTERNAL_NOEXEC_STATE_SCENARIO, self.parent, probe)
        self.assertEqual(blocked.exception.code, "noexec-state-parent-executable")
        container = self.parent / ready_path(self.lifetime.base).name
        self.assertTrue(container.is_dir())
        self.assertEqual(self.receipt.details["noexec_state"], {"state_container": str(container)})
        success, root_cleanup = finish_root_cleanup([], blocked.exception, self.lifetime, False)
        self.assertFalse(success)
        self.assertEqual(root_cleanup, {"status": "retained", "verified": True})
        self.assertTrue(container.is_dir())
        self.assertTrue(ready_path(self.lifetime.base).is_dir())

    def test_a_missing_parent_blocks_before_creating_anything(self):
        with self.assertRaises(AcceptanceBlocked) as blocked:
            self.prepare(EXTERNAL_NOEXEC_STATE_SCENARIO, self.root / "absent")
        self.assertEqual(blocked.exception.code, "noexec-state-parent-unavailable")
        self.assertFalse((ready_path(self.lifetime.base) / "tmp").exists())
        self.assertEqual(list(self.parent.iterdir()), [self.sentinel])


class NoexecScenarioEntryTests(FixtureCase):
    def run_scenario(self, outcome=None):
        fixture, _ = self.prepare(EXTERNAL_NOEXEC_STATE_SCENARIO, self.parent)
        calls = []

        def case(**inputs):
            inputs["attempted_task_ids"].append(TASK)
            calls.append(inputs)
            return outcome if outcome is not None else {"case": inputs["case_label"]}

        with patch.object(scenario_entry, "run_external_noexec_state_acceptance", side_effect=case):
            scenario_entry.run_noexec_state_scenario(
                "n" * 32, self.lifetime, self.root / "responses", self.receipt, fixture
            )
        return fixture, calls

    def test_one_external_case_runs_against_the_noexec_state_with_the_captured_temporary_root(self):
        fixture, calls = self.run_scenario()
        self.assertEqual(len(calls), 1)
        inputs = calls[0]
        self.assertEqual(inputs["state"], fixture.state)
        self.assertEqual((inputs["case_label"], inputs["handoff_action"]),
                         (EXTERNAL_NOEXEC_STATE_SCENARIO, EXTERNAL_NOEXEC_STATE_SCENARIO))
        self.assertEqual(inputs["env"]["TMPDIR"], str(ready_path(self.lifetime.base) / "tmp"))
        self.assertEqual(inputs["private_config"], self.lifetime.private_config)
        self.assertTrue((self.root / "responses" / ("external-handoff-" + EXTERNAL_NOEXEC_STATE_SCENARIO)).is_dir())
        self.assertEqual(self.receipt.current_phase, "external_noexec_state")

    def test_the_case_state_and_task_ids_are_registered_for_cleanup_closure(self):
        fixture, _ = self.run_scenario()
        self.assertEqual(self.lifetime.managed_state_roots, [(fixture.state, [TASK])])
        self.assertEqual(self.lifetime.handoff_cleanup_bindings[0]["state"], fixture.state)
        self.assertEqual(self.lifetime.attempted_task_ids, [TASK])
        self.assertEqual(len(self.receipt.details["checks"]), 6)

    def test_main_dispatches_the_scenario_through_the_one_fixture_owner_and_finalizer(self):
        output = self.root / "dispatch" / "receipt.json"
        argv = [
            "acceptance-private-cli", "--delegate", "/b/delegate", "--runner", "/b/delegate-run",
            "--pueue", "/b/pueue", "--pueued", "/b/pueued", "--output", str(output),
            "--scenario", EXTERNAL_NOEXEC_STATE_SCENARIO, "--parallel-tasks", "1",
            "--noexec-state-parent", str(self.parent),
        ]
        fixture = object()
        with patch.object(sys, "argv", argv), \
                patch.object(application, "prepare_fixture", return_value=fixture) as prepare, \
                patch.object(application, "run_noexec_state_scenario") as scenario, \
                patch.object(application, "prepare_initial_observations", side_effect=AssertionError("capacity path")), \
                patch.object(application, "finalize_fixture") as finalize:
            application.main()
        self.assertEqual(prepare.call_args.args[0].noexec_state_parent, self.parent)
        scenario.assert_called_once()
        self.assertIs(scenario.call_args.args[-1], fixture)
        self.assertTrue(finalize.call_args.args[-1])


class Gate:
    def __init__(self, order):
        self.order = order

    def release(self):
        self.order.append("release")


class Cohort:
    def __init__(self):
        self.closed = 0

    def remove_closed_fifos(self):
        return {"removed": 1}

    def close(self):
        self.closed += 1


class NoexecCaseFlowTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-noexec-case-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        owner = DirectoryPinOwner(self)
        self.external_root = owner.create("external", self.root / "external-supervisor", self.root)
        self.process_root = owner.create("processes", self.root / "external-supervisor-processes", self.root)
        self.capture_root = owner.create("capture", self.root / "capture", self.root)
        self.state = self.root / "noexec" / "state"
        self.execution = self.root / "tmp"
        self.waiters = self.root / "waiters"
        self.responses = self.root / "responses"
        for directory in (self.state, self.execution, self.waiters, self.responses):
            directory.mkdir(parents=True)
        self.receipt = AcceptanceReceipt(self.root / "receipt.json", 1, 1, {})
        self.cases = {}
        self.task_ids = []
        self.order = []
        self.collect_calls = []
        self.status_calls = []
        self.dispatch_root = "r" * 32
        self.shutdowns = []
        self.closure_calls = []

    def registration(self):
        return {"created_at": "r", "scope_kind": "process-group", "assurance": "fixture"}

    def closures(self):
        return {
            "tasks": {TASK: {
                "intent": {"deadline": "d", "created_at": "c"},
                "registration": self.registration(),
                "worker_wait": {"child_pid": 1, "result": "success", "exit_code": 0},
                "termination": {"cause": "completed", "scope_empty": True},
                "launches": [{"pid": 1}],
            }},
            "inspections": {INSPECTION: {"registration": self.registration()}},
        }

    def dispatch_response(self):
        response = {"task_id": TASK, "task_record": "created", "admission": "admitted"}
        if self.dispatch_root is not ...:
            response["root_id"] = self.dispatch_root
        return response

    def start_task(self, case, evidence, resources, supervisor_control, task_commands):
        resources.release_gate = Gate(self.order)
        resources.old_cohort = Cohort()
        resources.delegate_control_started = True
        case.attempted_task_ids.append(TASK)
        meta = self.state / "meta.json"
        meta.write_bytes(b"{}")
        task = ObservedHandoffTask(
            task_id=TASK, response=self.dispatch_response(),
            specifications=[], metadata_path=meta, metadata=b"{}", metadata_sha256="m" * 64,
            label="delegate:" + "r" * 32 + ":" + TASK, row={}, snapshot_name="snapshot.json", wrapper_calls=[],
        )
        return task, 4242

    def run_case(self, closures, verdict="committed", env=None, status=None):
        def closure(state, task_ids, **_options):
            self.order.append("closure")
            self.closure_calls.append((state, tuple(task_ids)))
            return closures

        def collect(_delegate, _env, command, **options):
            if "status" in command:
                self.order.append("status")
                self.status_calls.append((command, options))
                if status is not None:
                    return status
                return dict(TERMINAL_STATUS)
            self.order.append("collect")
            self.collect_calls.append(command)
            return {"outcome": {"verdict": verdict}}

        with patch.object(handoff_impl, "_start_external_task", side_effect=self.start_task), \
                patch.object(handoff_impl, "prove_lifetime_owner_cohort", return_value={"proof": "fixture"}), \
                patch.object(handoff_impl, "await_managed_lifetime_closure", side_effect=closure), \
                patch.object(handoff_cleanup_impl, "await_managed_lifetime_closure", side_effect=closure), \
                patch.object(handoff_impl, "run_json", side_effect=collect), \
                patch.object(handoff_impl.ExternalSupervisorControl, "shutdown_external",
                             side_effect=lambda: self.shutdowns.append(True)):
            return handoff_impl.run_external_noexec_state_acceptance(
                delegate=Path("/fixture/delegate"), client=Path("/fixture/pueue"), pueued=Path("/fixture/pueued"),
                state=self.state, workspace=self.root, gate=self.root / "gate", waiter_directory=self.waiters,
                private_config=self.state / ".supervisor" / "pueue.yml",
                env={"TMPDIR": str(self.execution)} if env is None else env,
                response_directory=self.responses, provider_fixture=self.root / "pi", lease_nonce="n" * 32,
                run_receipt=self.receipt, attempted_task_ids=self.task_ids, external_root_token=self.external_root,
                external_process_root_token=self.process_root, capture_root=self.capture_root,
                client_call_log=self.root / "old-client-starts-calls",
                handoff_action=EXTERNAL_NOEXEC_STATE_SCENARIO, case_label=EXTERNAL_NOEXEC_STATE_SCENARIO,
                handoff_cases=self.cases,
            )

    def test_success_proves_unplaced_registrations_after_closure_and_before_collection(self):
        evidence = self.run_case(self.closures())
        self.assertEqual(self.order[:4], ["release", "closure", "status", "collect"])
        self.assertEqual(evidence["scenario"], EXTERNAL_NOEXEC_STATE_SCENARIO)
        self.assertEqual(evidence["requested_task_count"], 1)
        self.assertEqual(evidence["execution_registrations"], {"tasks": [TASK], "inspections": [INSPECTION]})
        self.assertNotIn("custody_placement", evidence)
        self.assertNotIn("private", evidence["supervisors"])
        self.assertNotIn("private_daemon", evidence["cleanup"])
        self.assertEqual(evidence["tasks"]["external"]["closure"]["task_id"], TASK)
        self.assertEqual(evidence["tasks"]["external"]["collection"], "external-binding")
        self.assertEqual(evidence["cleanup"]["fixture_waiter_entries"], 0)
        self.assertEqual(self.closure_calls[0], (self.state, (TASK,)))
        self.assertEqual(self.collect_calls[0][:5], ["--root", str(self.state), "--pueue-config",
                                                      str(self.root / "external-supervisor" / "pueue.yml"), "collect"])
        self.assertIn(TASK, self.collect_calls[0])
        status_command, status_options = self.status_calls[0]
        self.assertEqual(status_command, [
            "--root", str(self.state), "--pueue-config", str(self.root / "external-supervisor" / "pueue.yml"),
            "status", TASK, "--json",
        ])
        self.assertEqual(status_options["receipt"], self.responses / "status-external.json")
        self.assertEqual(status_options["phase"], "external-task-status")
        self.assertEqual(evidence["tasks"]["external"]["status"], {
            "task_id": TASK, "root_id": "r" * 32, "admission": "unknown", "liveness": "undetermined",
            "publication": "committed",
        })
        self.assertEqual(self.receipt.counts["collected_task_count"], 1)
        self.assertIs(self.receipt.details["external_noexec_state"][EXTERNAL_NOEXEC_STATE_SCENARIO], evidence)
        self.assertNotIn("external_inspection_handoff", self.receipt.details)
        self.assertEqual(self.receipt.safe_stage, "external-noexec-state-proved")
        self.assertTrue(self.shutdowns)

    def test_a_placement_field_fails_before_collection_and_still_cleans_up(self):
        for kind, identity in (("tasks", TASK), ("inspections", INSPECTION)):
            with self.subTest(kind=kind):
                self.setUp()
                closures = self.closures()
                closures[kind][identity]["registration"]["custody_placement"] = None
                with self.assertRaisesRegex(RuntimeError, "lifetime registration contains custody placement"):
                    self.run_case(closures)
                self.assertNotIn("collect", self.order)
                self.assertEqual(self.order.count("release"), 1)
                self.assertEqual(len(self.closure_calls), 2)
                recorded = self.receipt.details["external_noexec_state"][EXTERNAL_NOEXEC_STATE_SCENARIO]
                self.assertIn("closure", recorded["tasks"]["external"])
                self.assertNotIn("execution_registrations", recorded)

    def test_no_registered_inspection_fails_before_status_and_collection(self):
        for rows in ({}, {INSPECTION: {"registration": None}}):
            with self.subTest(rows=rows):
                self.setUp()
                closures = self.closures()
                closures["inspections"] = rows
                with self.assertRaisesRegex(RuntimeError, "no inspections lifetime registration"):
                    self.run_case(closures)
                self.assertEqual(self.status_calls, [])
                self.assertEqual(self.collect_calls, [])
                self.assertEqual(self.order, ["release", "closure", "closure"])
                recorded = self.receipt.details["external_noexec_state"][EXTERNAL_NOEXEC_STATE_SCENARIO]
                self.assertEqual(recorded["cleanup"]["fixture_waiter_entries"], 0)
                self.assertNotIn("errors", recorded["cleanup"])

    def test_unplaced_registrations_never_substitute_for_canonical_closure(self):
        closures = self.closures()
        closures["tasks"][TASK]["termination"]["scope_empty"] = False
        with self.assertRaisesRegex(RuntimeError, "did not prove an empty native scope"):
            self.run_case(closures)
        self.assertEqual(self.status_calls, [])
        self.assertEqual(self.collect_calls, [])
        recorded = self.receipt.details["external_noexec_state"][EXTERNAL_NOEXEC_STATE_SCENARIO]
        self.assertNotIn("execution_registrations", recorded)

    def test_an_unusable_public_status_prevents_a_pass_before_collection(self):
        missing_root = {key: value for key, value in TERMINAL_STATUS.items() if key != "root_id"}
        for name, status in {
            "other task": {**TERMINAL_STATUS, "task_id": OTHER},
            "other root": {**TERMINAL_STATUS, "root_id": "x" * 32},
            "missing root": missing_root,
            "null root": {**TERMINAL_STATUS, "root_id": None},
            "empty root": {**TERMINAL_STATUS, "root_id": ""},
            "wrong command": {**TERMINAL_STATUS, "command": "collect"},
            "missing command": {key: value for key, value in TERMINAL_STATUS.items() if key != "command"},
            "failure": {**TERMINAL_STATUS, "failure": {"code": "x"}},
            "error": {**TERMINAL_STATUS, "error": "boom"},
            "malformed admission": {**TERMINAL_STATUS, "admission": 7},
        }.items():
            with self.subTest(name=name):
                self.setUp()
                with self.assertRaisesRegex(RuntimeError, "external status"):
                    self.run_case(self.closures(), status=status)
                self.assertNotIn("collect", self.order)
                self.assertEqual(len(self.status_calls), 1)
                recorded = self.receipt.details["external_noexec_state"][EXTERNAL_NOEXEC_STATE_SCENARIO]
                self.assertIn("execution_registrations", recorded)
                self.assertNotIn("status", recorded["tasks"]["external"])

    def test_terminal_unknown_admission_and_pending_observations_are_preserved_not_refused(self):
        pending = {"schema_version": 1, "command": "status", "root_id": "r" * 32, "task_id": TASK,
                   "admission": "unknown", "pending": {"kind": "supervisor_observation"}}
        for name, status, expected in (
            ("admitted running", {**TERMINAL_STATUS, "admission": "admitted", "liveness": "running",
                                  "publication": "none"},
             {"admission": "admitted", "liveness": "running", "publication": "none"}),
            ("pending", pending, {"admission": "unknown", "pending": "supervisor_observation"}),
        ):
            with self.subTest(name=name):
                self.setUp()
                evidence = self.run_case(self.closures(), status=status)
                self.assertEqual(
                    evidence["tasks"]["external"]["status"], {"task_id": TASK, "root_id": "r" * 32, **expected}
                )
                self.assertEqual(self.order[2:4], ["status", "collect"])

    def test_the_status_check_needs_the_known_dispatch_root_id(self):
        for name, root in {"missing": ..., "null": None, "empty": "", "number": 7}.items():
            with self.subTest(name=name):
                self.setUp()
                self.dispatch_root = root
                with self.assertRaisesRegex(RuntimeError, "needs the dispatch response root id"):
                    self.run_case(self.closures())
                self.assertNotIn("collect", self.order)

    def test_an_uncommitted_collection_is_a_failure_after_the_registration_proof(self):
        with self.assertRaisesRegex(RuntimeError, "did not publish a committed outcome"):
            self.run_case(self.closures(), verdict="failed")
        recorded = self.receipt.details["external_noexec_state"][EXTERNAL_NOEXEC_STATE_SCENARIO]
        self.assertIn("execution_registrations", recorded)
        self.assertNotIn("collection", recorded["tasks"]["external"])

    def test_the_case_requires_the_captured_execution_temporary_root(self):
        with self.assertRaisesRegex(RuntimeError, "omitted the execution TMPDIR"):
            self.run_case(self.closures(), env={})
        self.assertEqual(self.order, [])


if __name__ == "__main__":
    unittest.main()
