"""Negative oracles for the native nested-survival scenario."""

import tempfile
import json
import threading
import time
import fcntl
import os
import sys
import unittest
import io
from contextlib import redirect_stderr
from functools import partial
from copy import deepcopy
from pathlib import Path
from unittest.mock import patch, Mock
from types import SimpleNamespace

from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.testing.invalid_calls import invoke_invalid
from scripts.acceptance.private_cli.nested.policy import require_backend
from scripts.acceptance.private_cli.nested.policy import require_challenge
from scripts.acceptance.private_cli.nested.policy import require_descendant_absent
from scripts.acceptance.private_cli.nested.policy import require_setup_margin
from scripts.acceptance.private_cli.nested.policy import require_worker_deadline
from scripts.acceptance.private_cli.nested.policy import queue_ended_evidence
from scripts.acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.acceptance.private_cli.finalization import release_fixture_workers
from scripts.acceptance.private_cli.capture import borrow_capture_root
from scripts.acceptance.private_cli.capture import run_captured_text_command
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.directories.operations import rollback_owned_directories
from scripts.acceptance.evidence.files import write_json
from scripts.acceptance.private_cli.nested.observation import await_record
from scripts.acceptance.private_cli.nested.cleanup import cleanup_case
from scripts.acceptance.private_cli.nested.cleanup import fence_creator
from scripts.acceptance.evidence.records import _read_control_record
from scripts.acceptance.private_cli.leases import DisposalLeases
from scripts.acceptance.private_cli.leases import ordinary_lifetime_owner_live
from scripts.acceptance.private_cli.nested.fixture import wait_gate
from scripts.acceptance.private_cli.nested.fixture import dispatch_worker
from scripts.acceptance.private_cli.nested.case import NestedCase
from scripts.acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.acceptance.processes.contracts import BoundedCommandResult
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.contracts import CommandFailure
from scripts.acceptance.testing.record_fixtures import object_at
from scripts.acceptance.private_cli.arguments import parse_arguments


class NestedGuardArgumentTests(unittest.TestCase):
    def parse(self, *extra, scenario="nested-survival"):
        arguments = ["acceptance-private-cli", "--pueue", sys.executable, "--pueued", sys.executable,
                     "--output", "/unused-nested-parser-receipt.json"]
        if scenario is not None:
            arguments.extend(["--scenario", scenario, "--nested-topology", "shared-warm",
                              "--expected-backend", "process-group", "--native-fixture", sys.executable,
                              "--parallel-tasks", "3", "--dispatch-workers", "1", "--runtime-only"])
        with patch.object(sys, "argv", [*arguments, *extra]):
            return parse_arguments()[0]

    def test_only_explicit_false_disables_nested_cpu_admission(self):
        self.assertIs(self.parse("--nested-cpu-guard", "false").nested_cpu_guard, False)
        self.assertIs(self.parse().nested_cpu_guard, True)
        self.assertIs(self.parse("--nested-cpu-guard", "true").nested_cpu_guard, True)

    def test_refusal_scenario_accepts_policy_but_other_scenarios_reject_every_explicit_selection(self):
        self.assertIs(self.parse("--nested-cpu-guard", "false", scenario="nested-refused-leader").nested_cpu_guard, False)
        self.assertIs(self.parse(scenario=None).nested_cpu_guard, True)
        for value, scenario in (("0", "nested-survival"), ("true", None), ("false", None)):
            with self.subTest(value=value, scenario=scenario), redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as caught:
                    self.parse("--nested-cpu-guard", value, scenario=scenario)
                self.assertEqual(caught.exception.code, 2)


class NestedAdmissionPolicyTests(unittest.TestCase):
    def make_case(self, enabled):
        parent = Path(self.enterContext(tempfile.TemporaryDirectory(prefix="nested-policy-")))
        owner = DirectoryOwner()
        self.addCleanup(lambda: rollback_owned_directories(owner.transfer()))
        base = owner.create("nested policy fixture", parent / "fixture", parent)
        state = owner.create("nested policy state", base.directory / "state", base.directory)
        capture = owner.create("nested policy captures", base.directory / "captures", base.directory)
        workspace = owner.create("nested policy workspace", base.directory / "workspace", base.directory)
        lifetime = FixtureLifetime(ownership=owner, base=base.directory, base_token=base, state_token=state,
                                   capture_token=capture, old_install=base.directory / "installed",
                                   old_libexec=base.directory / "libexec")
        args = SimpleNamespace(nested_topology="other-warm", expected_backend="process-group",
                               native_fixture=Path(sys.executable), nested_cpu_guard=enabled)
        fixture = SimpleNamespace(state=state.directory, workspace=workspace.directory,
                                  waiter_directory=base.directory / "waiters", lease_nonce="policy-case")
        receipt = AcceptanceReceipt(parent / "receipt.json", 3, 1, {})
        return NestedCase(args, lifetime, fixture, parent, receipt)

    def assert_request(self, invoke, owner_path, expected_flags):
        fault = RuntimeError("no task may start in this test")
        with patch(owner_path, side_effect=fault) as command:
            with self.assertRaises(RuntimeError) as caught:
                invoke()
        self.assertIs(caught.exception, fault)
        command.assert_called_once()
        arguments = command.call_args.args[2]
        self.assertEqual([argument for argument in arguments if argument.startswith("--cpu-guard")], expected_flags)
        self.assertFalse(any(argument.startswith("--memory-guard") for argument in arguments))

    def test_parent_and_worker_requests_read_the_same_serialized_policy_before_admission(self):
        for enabled in (True, False):
            with self.subTest(enabled=enabled):
                case = self.make_case(enabled)
                configuration = json.loads((case.controls.directory / "configuration.json").read_text())
                self.assertIs(configuration["cpu_guard"], enabled)
                self.assertEqual(case.lifetime.attempted_task_ids, [])
                case.receipt.write("FAIL", {"status": "not-required"}, RuntimeError("provider-free test"))
                evidence = json.loads(case.receipt.output.read_text())["nested_survival"]
                self.assertEqual(evidence["admission_guards"], {
                    "cpu": "enabled" if enabled else "disabled-by-caller", "memory": "enabled"})
                self.assertEqual(evidence["admission_guard_scope"], "positive-fixture-admissions")
                if enabled:
                    self.assertNotIn("admission_guard_limit", evidence)
                else:
                    self.assertEqual(evidence["admission_guard_limit"],
                                     "CPU admission disabled by caller; host load source and fixture-descendant closure are unproven.")
                flags = [] if enabled else ["--cpu-guard=false"]
                for name, state, task_id in (("warm-a", case.a, "a" * 32), ("warm-b", case.b, "b" * 32),
                                             ("leader", case.a, configuration["leader"])):
                    with self.subTest(parent=name):
                        self.assert_request(partial(case.dispatch, state, task_id, Path(configuration["leader_brief"]), "3m", name),
                                            "scripts.acceptance.private_cli.nested.case.run_json", flags)
                for role in ("w1", "w2"):
                    with self.subTest(worker=role):
                        self.assert_request(partial(dispatch_worker, configuration, None, role),
                                            "scripts.acceptance.private_cli.nested.fixture.run_json", flags)

    def test_worker_requires_present_exact_boolean_before_intent_or_invocation(self):
        case = self.make_case(True)
        configuration = json.loads((case.controls.directory / "configuration.json").read_text())
        for invalid in ({}, {"cpu_guard": None}, {"cpu_guard": 0}, {"cpu_guard": "false"}):
            with self.subTest(invalid=invalid):
                changed = {key: value for key, value in configuration.items() if key != "cpu_guard"}
                changed.update(invalid)
                with patch("scripts.acceptance.private_cli.nested.fixture.run_json") as command:
                    with self.assertRaisesRegex(AcceptanceFailure, "present boolean"):
                        dispatch_worker(changed, None, "w1")
                command.assert_not_called()
                self.assertFalse((case.controls.directory / "w1-intent.json").exists())
                self.assertFalse((case.controls.directory / "w1-control-ended.json").exists())


class NestedSurvivalOracleTests(unittest.TestCase):
    def test_text_capture_passes_absolute_deadline_without_renewing_it(self):
        captured = SimpleNamespace(args=["native-fixture"], returncode=0, stdout=b"ok", stderr=b"")
        token = object()
        with patch("scripts.acceptance.private_cli.capture.run_captured_command", return_value=captured) as owner:
            response = run_captured_text_command(["native-fixture"], {}, None, capture_root=token, deadline=123.5)
        owner.assert_called_once_with(["native-fixture"], {}, None, capture_root=token, deadline=123.5)
        self.assertEqual(response.stdout, "ok")

    def test_record_wait_distinguishes_absent_from_malformed_existing(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "reply.json"
            entered = threading.Event()

            def publish():
                entered.wait(timeout=1)
                write_json(path, {"nonce": "fresh"})

            writer = threading.Thread(target=publish)
            writer.start()
            try:
                entered.set()
                self.assertEqual(await_record(path, time.monotonic() + 1), {"nonce": "fresh"})
            finally:
                entered.set()
                writer.join(timeout=1)
                self.assertFalse(writer.is_alive())
            path.write_text("not JSON")
            with self.assertRaises(AcceptanceFailure):
                await_record(path, time.monotonic() + 1)

    def test_fenced_leader_acknowledges_but_stays_alive_until_child_cleanup_release(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fence = root / "fence"
            fence.touch()
            finished = threading.Event()
            results = []

            def observe(_configuration, _capture, pid, output):
                write_json(output, {"pid": pid, "present": True, "process_start": "linux:11"})

            def leader():
                try:
                    results.append(wait_gate(root / "paused", fence, time.time_ns() + 2_000_000_000,
                                              {"nonce": "case-nonce"}, None))
                finally:
                    finished.set()

            with patch("scripts.acceptance.private_cli.nested.fixture.native_observation", side_effect=observe):
                worker = threading.Thread(target=leader)
                worker.start()
                try:
                    acknowledgement = await_record(root / "fenced.json", time.monotonic() + 1)
                    self.assertEqual(acknowledgement["nonce"], "case-nonce")
                    self.assertFalse(finished.is_set())
                finally:
                    (root / "leader-release").touch()
                    worker.join(timeout=1)
                    self.assertFalse(worker.is_alive())
            self.assertEqual(results, [False])

    def assert_fenced_dispatch(self, *, after_first):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            configuration = {"controls": directory, "nonce": "case-nonce", "delegate": "delegate", "cpu_guard": True,
                             "worker_state": "state", "workspace": directory, "w1_brief": "one.md",
                             "w2_brief": "two.md", "w1": "a" * 32, "w2": "b" * 32}
            results, failures = [], []

            def observe(_configuration, _capture, pid, output):
                write_json(output, {"pid": pid, "present": True, "process_start": "fixture-start"})

            def complete_first(*args, **kwargs):
                (root / "fence").touch()

            def creator():
                try:
                    if after_first:
                        results.append(dispatch_worker(configuration, None, "w1"))
                    results.append(dispatch_worker(configuration, None, "w2" if after_first else "w1"))
                except BaseException as error:
                    failures.append(error)

            if not after_first:
                (root / "fence").touch()
            with patch("scripts.acceptance.private_cli.nested.fixture.native_observation", side_effect=observe), \
                    patch("scripts.acceptance.private_cli.nested.fixture.run_json",
                          side_effect=complete_first) as invoke:
                worker = threading.Thread(target=creator)
                worker.start()
                try:
                    ack = await_record(root / "fenced.json", time.monotonic() + 2)
                    self.assertEqual((ack["pid"], ack["process_start"], ack["nonce"]),
                                     (os.getpid(), "fixture-start", "case-nonce"))
                    self.assertEqual(invoke.call_count, int(after_first))
                    self.assertFalse((root / (("w2" if after_first else "w1") + "-intent.json")).exists())
                finally:
                    (root / "leader-release").touch()
                    worker.join(timeout=2)
                    self.assertFalse(worker.is_alive())
            self.assertEqual(failures, [])
            self.assertEqual(results, [True, False] if after_first else [False])

    def test_fence_before_first_dispatch_refuses_invocation_and_binds_ack(self):
        self.assert_fenced_dispatch(after_first=False)

    def test_fence_between_worker_dispatches_refuses_second_and_binds_ack(self):
        self.assert_fenced_dispatch(after_first=True)

    def test_repeated_fence_keeps_original_request_for_live_acknowledged_creator(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            controls = root / "controls"
            controls.mkdir()
            task = root / "tasks" / ("a" * 32)
            task.mkdir(parents=True)
            (task / "provider.start").touch()
            response = {"task_id": "a" * 32, "root_id": "b" * 32}
            case = SimpleNamespace(controls=SimpleNamespace(directory=controls), a=root,
                                   configuration={"nonce": "same-case"}, responses={"leader": response},
                                   match_launch=Mock(return_value={"pid": 123}))
            ack = {"pid": 123, "process_start": "native-start", "nonce": "same-case", "observed_at_unix_ns": 150}
            write_json(controls / "fenced.json", ack)
            with patch("scripts.acceptance.private_cli.nested.cleanup.time.time_ns", side_effect=[100, 200]) as clock:
                fence_creator(case, time.monotonic() + 1)
                original = (controls / "fence").read_bytes()
                fence_creator(case, time.monotonic() + 1)
            clock.assert_called_once()
            self.assertEqual((controls / "fence").read_bytes(), original)
            self.assertEqual(_read_control_record(controls / "fence", "request")["requested_at_unix_ns"], 100)

    def test_guardian_owner_probe_never_creates_a_lock_or_claims_a_busy_owner_free(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "owner.lock"
            with self.assertRaises(FileNotFoundError):
                ordinary_lifetime_owner_live(path)
            with path.open("xb") as owner:
                fcntl.flock(owner, fcntl.LOCK_EX | fcntl.LOCK_NB)
                self.assertTrue(ordinary_lifetime_owner_live(path))
            self.assertFalse(ordinary_lifetime_owner_live(path))

    def test_removed_row_only_passes_with_independent_native_closure_and_admission(self):
        receipt = {"root_id": "a" * 32, "task_id": "b" * 32, "numeric_task_id": 0,
                   "label": "delegate:" + "a" * 32 + ":" + "b" * 32}
        closed = {"scope_disposed": True, "owner_released": True, "termination": {"scope_empty": True}}
        self.assertEqual(queue_ended_evidence({"tasks": {}}, receipt, closed)["state"],
                         "removed-after-canonical-native-closure")
        with self.assertRaises(AcceptanceFailure):
            queue_ended_evidence({"tasks": {}}, receipt, {**closed, "owner_released": False})
        row = {"id": 0, "label": receipt["label"], "status": {"Running": {}}}
        self.assertIsNone(queue_ended_evidence({"tasks": {"0": row}}, receipt, closed))
        row["status"] = {"Done": {}}
        self.assertEqual(queue_ended_evidence({"tasks": {"0": row}}, receipt, closed)["state"], "Done")
        row["id"] = 1
        with self.assertRaises(AcceptanceFailure):
            queue_ended_evidence({"tasks": {"1": row}}, receipt, closed)

    def test_nonce_rejects_old_response_and_pid_reuse(self):
        launch = {"pid": 123, "process_start": "linux:11"}
        reply = {"nonce": "fresh", **launch, "observed_at_unix_ns": 101}
        require_challenge(reply, "fresh", launch, 100, 200, same_identity=True)
        for change in ({"nonce": "old"}, {"process_start": "linux:12"}, {"pid": True},
                       {"observed_at_unix_ns": 99}, {"observed_at_unix_ns": 201}):
            with self.subTest(change=change), self.assertRaises(AcceptanceFailure):
                require_challenge({**reply, **change}, "fresh", launch, 100, 200,
                                  same_identity=False if "process_start" in change else True)

    def test_backend_does_not_accept_cooperative_as_exact(self):
        require_backend({"scope_kind": "cgroup-v2", "assurance": "exact"}, "cgroup-v2")
        with self.assertRaises(AcceptanceFailure):
            require_backend({"scope_kind": "process-group", "assurance": "cooperative"}, "cgroup-v2")

    def test_margin_is_not_an_automatic_retry_or_budget_extension(self):
        require_setup_margin(31_000_000_000, 1_000_000_000)
        with self.assertRaisesRegex(AcceptanceFailure, "setup_inconclusive"):
            require_setup_margin(31_000_000_000, 1_000_000_001)

    def test_descendant_self_exit_cannot_pass_containment(self):
        before = {"pid": 123, "process_start": "linux:11", "present": True}
        after = {"pid": 123, "present": False}
        require_descendant_absent(before, after, 101, 100, False, same_identity=False)
        for live, observed, released in ((before, 101, False), (after, 101, True),
                                         (after, 60_000_000_100, False)):
            with self.subTest(observed=observed, released=released), self.assertRaises(AcceptanceFailure):
                require_descendant_absent(before, live, observed, 100, released, same_identity=live is before)

    def test_queued_admission_time_is_not_execution_start(self):
        intent = {"created_at": "2026-11-02T12:00:00Z", "deadline": "2026-11-02T12:09:59Z"}
        require_worker_deadline(intent, 0, 0)
        for deadline in ("2026-11-02T12:01:00Z", "2026-11-02T12:10:01Z"):
            with self.subTest(deadline=deadline), self.assertRaises(AcceptanceFailure):
                require_worker_deadline({**intent, "deadline": deadline}, 0, 0)

    def test_cleanup_hook_precedes_readiness_and_shares_deadline(self):
        lifetime = FixtureLifetime()
        events = []
        disposal = DisposalLeases()
        self.addCleanup(disposal.release_all)
        lifetime.prepare_cleanup = lambda deadline, borrowed: events.append(("prepare", deadline, borrowed))
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config"
            config.touch()
            lifetime.private_config = config
            lifetime.managed_state_roots.append((Path(directory), ["a" * 32]))
            with patch("scripts.acceptance.private_cli.finalization.await_primary_readiness",
                       side_effect=lambda *args, **kwargs: events.append(("readiness", args[3]))):
                _, ready, deadline, failed = release_fixture_workers(SimpleNamespace(scenario="nested-survival"),
                                                                     [], lifetime, False, disposal=disposal)
        self.assertTrue(ready)
        self.assertFalse(failed)
        self.assertEqual(events, [("prepare", deadline, disposal), ("readiness", deadline)])

    def test_failed_cleanup_hook_blocks_parent_readiness(self):
        lifetime = FixtureLifetime()
        lifetime.prepare_cleanup = lambda deadline, disposal: (_ for _ in ()).throw(RuntimeError("child unresolved"))
        errors = []
        _, ready, _, failed = release_fixture_workers(SimpleNamespace(scenario="nested-survival"), errors, lifetime, False)
        self.assertFalse(ready)
        self.assertTrue(failed)
        self.assertIn("child unresolved", str(errors[0]))

    def test_no_admission_forgets_only_empty_owned_roots_without_controls(self):
        with tempfile.TemporaryDirectory() as directory:
            a, b = Path(directory) / "a", Path(directory) / "b"
            a.mkdir()
            b.mkdir()
            lifetime = invoke_invalid(FixtureLifetime, managed_state_roots=[(a, []), (b, [])],
                                      supervisor_cleanups=[{"state": a}, {"state": b}])
            case = SimpleNamespace(a=a, b=b, b_ids=[], lifetime=lifetime, command=Mock(), evidence={"cleanup": {}})
            cleanup_case(case, time.monotonic() + 1, None)
            case.command.assert_not_called()
            self.assertEqual(lifetime.managed_state_roots, [])
            self.assertEqual(lifetime.supervisor_cleanups, [])
            self.assertEqual(case.evidence["cleanup"]["status"], "no-task-or-supervisor-created")
            (a / "unknown").touch()
            with self.assertRaisesRegex(AcceptanceFailure, "unknown state"):
                cleanup_case(case, time.monotonic() + 1, None)

    def test_cleanup_closes_child_before_parent_and_shared_root_once(self):
        for shared in (False, True):
            with self.subTest(shared=shared), tempfile.TemporaryDirectory() as directory:
                a = Path(directory) / "a"
                a.mkdir()
                b = a if shared else Path(directory) / "b"
                b.mkdir(exist_ok=True)
                tasks = ["setup"]
                child_tasks = tasks if shared else ["child-setup"]
                lifetime = FixtureLifetime(attempted_task_ids=tasks)
                events = []
                case = SimpleNamespace(a=a, b=b, b_ids=child_tasks, lifetime=lifetime, responses={},
                                       configuration={"leader": "leader", "w1": "w1", "w2": "w2"},
                                       release=lambda role: events.append(("release", role)),
                                       controls=SimpleNamespace(directory=Path(directory)), evidence={"cleanup": {}})
                disposal = object()
                deadline = time.monotonic() + 1

                def close(_case, state, task_ids, observed_deadline, borrowed):
                    self.assertIs(borrowed, disposal)
                    self.assertEqual(observed_deadline, deadline)
                    self.assertEqual(task_ids, tasks if state == a else child_tasks)
                    events.append(("close", state))

                with patch("scripts.acceptance.private_cli.nested.cleanup.fence_creator"), \
                        patch("scripts.acceptance.private_cli.nested.cleanup.reconcile_children"), \
                        patch("scripts.acceptance.private_cli.nested.cleanup.cancel_actor"), \
                        patch("scripts.acceptance.private_cli.nested.cleanup.cancel_owned_setup"), \
                        patch("scripts.acceptance.private_cli.nested.cleanup.close_root", side_effect=close):
                    cleanup_case(case, deadline, disposal)
                self.assertEqual([state for event, state in events if event == "close"], [a] if shared else [b, a])
                if not shared:
                    self.assertLess(events.index(("close", b)), events.index(("release", "leader")))
                self.assertEqual(case.evidence["cleanup"]["status"], "canonical-closure-proven")

    def test_capture_borrow_fences_replaced_directory_without_taking_deletion(self):
        with tempfile.TemporaryDirectory() as directory:
            owner = DirectoryOwner()
            token = owner.create("captures", Path(directory) / "captures", Path(directory))
            try:
                with borrow_capture_root(token.directory, token.identity) as borrowed:
                    borrowed.assert_live()
                self.assertTrue(borrowed.consumed)
                token.assert_live()
                with self.assertRaisesRegex(RuntimeError, "identity changed"):
                    with borrow_capture_root(token.directory, (0, 0)):
                        self.fail("changed capture pin accepted")
            finally:
                rollback_owned_directories(owner.transfer())


class NestedAdmissionReceiptTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.task_id = "a" * 32
        self.case = NestedCase.__new__(NestedCase)
        self.case.a = self.root
        self.case.b_ids = []
        self.case.lifetime = FixtureLifetime(old_install=self.root / "installed")
        self.case.fixture = SimpleNamespace(workspace=self.root)
        self.case.responses = {}
        self.case.configuration = {"cpu_guard": True}
        self.case.evidence = {}
        self.case.receipts = self.root
        self.receipt = AcceptanceReceipt(self.root / "receipt.json", 3, 1, {})
        self.case.receipt = self.receipt
        self.receipt.add_details(nested_survival=self.case.evidence)

    def test_failed_admission_keeps_only_public_diagnostics_through_cleanup_failure(self):
        failure = {"code": "resource_pressure", "stage": "resource-admission",
                   "reason_code": "memory", "next_action": "retry_later", "private": "private-secret"}
        public_failure = {key: failure[key] for key in ("code", "stage", "reason_code", "next_action")}
        for record, returned_id in (("not_created", self.task_id), ("unknown", self.task_id),
                                    ("created", "b" * 32), ("created", None)):
            with self.subTest(record=record, returned_id=returned_id):
                self.case.evidence.clear()
                self.case.lifetime.attempted_task_ids.clear()
                response = {"schema_version": 1, "command": "dispatch", "root_id": "c" * 32,
                            "task_id": returned_id, "task_record": record, "admission": "not-admitted",
                            "failure": failure, "error": "private-secret",
                            "raw": {"stdout_tail": "private-secret", "stderr_tail": "private-secret"},
                            "provider": {"session": "private-secret"}}
                with patch.object(NestedCase, "command", return_value=response) as command:
                    with self.assertRaisesRegex(AcceptanceFailure, "never redispatch"):
                        self.case.dispatch(self.root, self.task_id, self.root / "brief", "3m", "leader")
                self.assertEqual(command.call_count, 1)
                self.assertEqual(self.case.lifetime.attempted_task_ids, [self.task_id])
                self.assertEqual(self.case.responses, {})
                self.assertIn("failed_admission", self.case.evidence)
                expected = {key: response[key] for key in
                            ("schema_version", "command", "root_id", "task_id", "task_record", "admission")}
                expected.update(name="leader", requested_task_id=self.task_id, failure=public_failure)
                self.receipt.write("FAIL", {"status": "failed"}, RuntimeError("cleanup failed"))
                receipt_text = self.receipt.output.read_text()
                published = json.loads(receipt_text)
                self.assertEqual(published["nested_survival"]["failed_admission"], expected)
                self.assertEqual(published["failure"]["code"], "cleanup_failed")
                self.assertNotIn("private-secret", receipt_text)

    def test_created_admission_keeps_status_observation_and_no_failure_receipt(self):
        response = {"schema_version": 1, "command": "dispatch", "root_id": "c" * 32,
                    "task_id": self.task_id, "task_record": "created", "admission": "admitted"}
        with patch.object(NestedCase, "command", return_value=response) as command, \
                patch("scripts.acceptance.private_cli.nested.case.observe_created_admission") as observe:
            result = self.case.dispatch(self.root, self.task_id, self.root / "brief", "3m", "leader")
        self.assertEqual(result, response)
        self.assertEqual(command.call_count, 2)
        observe.assert_called_once()
        self.assertNotIn("failed_admission", self.case.evidence)
        self.assertEqual(self.receipt.counts["dispatch_completed_count"], 1)


class NestedBootstrapCleanupTests(unittest.TestCase):
    """Bootstrap refusal wiring; the native port separately proves store semantics."""

    def setUp(self):
        self.parent = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.owner = DirectoryOwner()
        self.transferred = None
        self.addCleanup(self.close_owned)
        self.base = self.owner.create("bootstrap fixture", self.parent / ".dl-bootstrap", self.parent)
        self.state = self.owner.create("primary state", self.base.directory / "state", self.base.directory)
        captures = self.owner.create("captures", self.base.directory / "captures", self.base.directory)
        workspace = self.owner.create("workspace", self.base.directory / "workspace", self.base.directory)
        self.receipts = self.parent / "receipts"
        self.receipts.mkdir(mode=0o700)
        lifetime = FixtureLifetime(ownership=self.owner, base=self.base.directory, base_token=self.base,
                                   state_token=self.state, capture_token=captures,
                                   private_config=self.state.directory / ".supervisor" / "pueue.yml",
                                   old_install=self.base.directory / "installed",
                                   old_libexec=self.base.directory / "libexec")
        self.receipt = AcceptanceReceipt(self.receipts / "receipt.json", 3, 1, {})
        self.args = SimpleNamespace(nested_topology="other-warm", expected_backend="process-group", nested_cpu_guard=True,
                                    native_fixture=Path(sys.executable))
        fixture = SimpleNamespace(state=self.state.directory, workspace=workspace.directory,
                                  waiter_directory=self.base.directory / "waiters", lease_nonce="fixture-nonce")
        self.case = NestedCase(self.args, lifetime, fixture, self.receipts, self.receipt)
        self.task_id = "c" * 32
        self.calls = []
        self.failure = {"code": "resource_pressure", "stage": "check-resources",
                        "reason_code": "host_cpu_sustained", "next_action": "stop_and_report"}
        self.native_observations = []
        self.native = self.enterContext(patch(
            "scripts.acceptance.private_cli.nested.observation.run_captured_text_command",
            side_effect=self.observe_entry))
        self.enterContext(patch("scripts.acceptance.private_cli.nested.case.NestedCase.command",
                                side_effect=self.command))

    def close_owned(self):
        entries = self.owner.transfer() if self.transferred is None else self.transferred
        rollback_owned_directories(entry for entry in entries if not entry.consumed)

    def root_id(self, state):
        return "a" * 32 if state == self.case.a else "b" * 32

    def initialize(self, state):
        if not (state / "root.json").exists():
            write_json(state / "root.json", {"schema_version": 1, "root_id": self.root_id(state),
                                             "created_at": "2026-11-07T00:00:00Z"})

    def command(self, state, arguments, name, **_options):
        self.calls.append((state, arguments, name))
        if arguments[0] == "dispatch":
            self.initialize(state)
            return {"schema_version": 1, "command": "dispatch", "root_id": self.root_id(state),
                    "task_id": self.task_id, "task_record": "unknown", "admission": "unknown",
                    "failure": self.failure}
        self.assertEqual(arguments, ["supervisor", "stop-owner"])
        return {"root_id": self.root_id(state), "supervisor_owner": {"action": "stop-owner",
                                                                    "state": "not_initialized"}}

    def observe_entry(self, _command, environment, _timeout, *, deadline, capture_root):
        self.assertIs(capture_root, self.case.lifetime.capture_token)
        self.assertLessEqual(deadline, self.case.cleanup_deadline)
        state = Path(environment["DELEGATE_NESTED_FIXTURE_ROOT"])
        token = self.state if state == self.case.a else self.case.b_token
        assert token is not None, "native observation must borrow the original root token"
        # The fake reflects concrete fixture entry changes; native Go tests
        # and the bounded real-port proof own safe rooted-read semantics.
        proof = {"root_id": self.root_id(state), "task_id": self.task_id,
                 "entry_exists": os.path.lexists(state / "tasks" / self.task_id),
                 "root_device": token.identity[0], "root_inode": token.identity[1]}
        self.native_observations.append(proof)
        return SimpleNamespace(returncode=0, stdout="NESTED_TASK_OBSERVATION\t" + json.dumps(proof) + "\n", stderr="")

    def refuse(self, name="warm-a"):
        state = self.case.b if name == "warm-b" else self.case.a
        if name == "warm-b":
            self.initialize(self.case.a)
        elif name == "leader":
            self.prepare_warm_record()
            leader = self.case.configuration["leader"]
            if not isinstance(leader, str):
                self.fail("configured fixture leader must be a task ID")
            self.task_id = leader
        with self.assertRaisesRegex(AcceptanceFailure, "never redispatch") as rejected:
            self.case.dispatch(state, self.task_id, self.parent / "brief.md", "2m", name)
        return state, rejected.exception

    def prepare_warm_record(self):
        self.initialize(self.case.a)
        self.warm_task_id = "d" * 32
        (self.case.a / "tasks").mkdir(mode=0o700, exist_ok=True)
        directory = self.case.a / "tasks" / self.warm_task_id
        directory.mkdir(mode=0o700)
        identity = {"schema_version": 1, "root_id": self.root_id(self.case.a), "task_id": self.warm_task_id}
        # Minimal Python closure projections, not complete prepared-task
        # records. No handoff or lifetime journal claims that a worker launched.
        write_json(directory / "task.json", identity)
        write_json(directory / "meta.json", {**identity, "runner_ownership": "managed"})
        write_json(directory / "outcome.json", {**identity, "verdict": "rejected"})
        self.case.lifetime.attempted_task_ids.append(self.warm_task_id)

    def assert_refusal_cleanup(self, name):
        from scripts.acceptance.private_cli.finalization import finalize_fixture
        state, primary = self.refuse(name)
        raw = self.case.lifetime.attempted_task_ids if state == self.case.a else self.case.b_ids
        attempted = list(raw)
        original = json.loads(json.dumps(self.case.evidence["failed_admission"]))
        self.receipt.add_details(failure_phase="nested_setup")
        with patch("scripts.acceptance.private_cli.nested.cleanup.shutdown_custody_roots",
                   side_effect=lambda observed, _env: (observed,)), \
                patch("scripts.acceptance.private_cli.supervisor.shutdown_custody_roots",
                      side_effect=lambda observed, _env: (observed,)):
            finalize_fixture(SimpleNamespace(scenario="nested-survival", pueue=Path(sys.executable)), primary, self.case.lifetime,
                             self.receipt, False)
        published = json.loads(self.receipt.output.read_text())
        self.assertEqual(published["result"], "FAIL")
        self.assertEqual(published["failure"], {"code": "nested-setup-failed", "exception_type": "AcceptanceFailure",
                                                 "phase": "nested_setup"})
        self.assertEqual(published["nested_survival"]["failed_admission"], original)
        self.assertEqual(original["task_record"], "unknown")
        self.assertEqual(original["failure"], self.failure)
        self.assertEqual(published["nested_survival"]["cleanup"]["status"], "canonical-closure-proven")
        self.assertEqual(published["cleanup"]["status"], "retained")
        self.assertEqual(raw, attempted)
        self.assertTrue(self.base.directory.is_dir())
        self.assertEqual(sum(arguments[0] == "dispatch" for _, arguments, _ in self.calls), 1)
        self.assertTrue(all(arguments[0] != "cancel" for _, arguments, _ in self.calls))
        self.assertIn(state, self.case.lifetime.closed_roots)
        self.assertGreaterEqual(len(self.native_observations), 3)

    def test_warm_a_refusal_closes_without_reclassifying_primary_failure_or_creating_b(self):
        self.assert_refusal_cleanup("warm-a")
        self.assertFalse(self.case.b.exists())

    def completed_cpu_refusal(self, state, arguments, name, **options):
        response = self.command(state, arguments, name, **options)
        if arguments[0] == "dispatch":
            response.update(liveness="undetermined", publication="unknown",
                            resource_admission={"status": "pressure"})
            write_json(self.receipts / (name + ".json"), {
                "exit_code": 1, "failure_class": "nonzero_exit", "response": response,
            })
        return response

    def test_cpu_refusal_snapshot_is_bounded_redacted_and_keeps_primary_cleanup(self):
        listing = b"220 /private/build-canary/go\n100 /private/compile\n4 /private/secret-name\n"
        with patch("scripts.acceptance.private_cli.nested.case.NestedCase.command",
                   side_effect=self.completed_cpu_refusal), \
                patch("scripts.acceptance.private_cli.nested.case.run_bounded_command",
                      return_value=BoundedCommandResult(0, listing)) as snapshot_owner:
            self.assert_refusal_cleanup("warm-a")
        published = json.loads(self.receipt.output.read_text())
        snapshot = published["nested_survival"]["failed_admission"]["host_cpu_snapshot"]
        snapshot_owner.assert_called_once_with(
            ["/bin/ps", "-A", "-o", "pcpu=,comm="], {"LC_ALL": "C"},
            cwd=self.case.fixture.workspace, capture=True, timeout=5, max_output=256 * 1024,
        )
        self.assertEqual(snapshot["status"], "observed")
        self.assertEqual(snapshot["listed_process_count"], {"go": 1, "compile": 1, "other": 1})
        self.assertEqual(snapshot["top_cpu"], [
            {"role": "go", "native_cpu_percent": 220.0},
            {"role": "compile", "native_cpu_percent": 100.0},
            {"role": "other", "native_cpu_percent": 4.0},
        ])
        self.assertFalse(snapshot["admission_intervals_matched"])
        self.assertFalse(snapshot["causal_attribution"])
        self.assertGreaterEqual(snapshot["capture_started_offset_ms"], 0)
        self.assertGreaterEqual(snapshot["capture_completed_offset_ms"], snapshot["capture_started_offset_ms"])
        encoded = json.dumps(snapshot)
        for private in ("build-canary", "secret-name", "/", "argv", "environment", "pid"):
            self.assertNotIn(private, encoded)

    def test_cpu_snapshot_interruption_remains_secondary_to_refusal_and_cleanup(self):
        with patch("scripts.acceptance.private_cli.nested.case.NestedCase.command",
                   side_effect=self.completed_cpu_refusal), \
                patch("scripts.acceptance.private_cli.nested.case.run_bounded_command",
                      side_effect=KeyboardInterrupt("private-canary")) as snapshot_owner:
            self.assert_refusal_cleanup("warm-a")
        snapshot_owner.assert_called_once()
        published = json.loads(self.receipt.output.read_text())
        snapshot = published["nested_survival"]["failed_admission"]["host_cpu_snapshot"]
        self.assertEqual(snapshot["status"], "unavailable")
        self.assertEqual(snapshot["reason"], "capture_unavailable")
        self.assertNotIn("private-canary", json.dumps(published))

    def test_cpu_snapshot_unproven_wait_retains_failure_and_unverified_resource_evidence(self):
        with patch("scripts.acceptance.private_cli.nested.case.NestedCase.command",
                   side_effect=self.completed_cpu_refusal), \
                patch("scripts.acceptance.private_cli.nested.case.run_bounded_command",
                      side_effect=BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)):
            self.assert_refusal_cleanup("warm-a")
        published = json.loads(self.receipt.output.read_text())
        snapshot = published["nested_survival"]["failed_admission"]["host_cpu_snapshot"]
        self.assertTrue(snapshot["closure_unproven"])
        self.assertEqual(snapshot["status"], "unavailable")
        self.assertEqual(published["result"], "FAIL")
        self.assertEqual(published["cleanup"]["status"], "retained")

    def test_cpu_snapshot_capture_and_projection_faults_are_secondary_evidence(self):
        response = self.completed_cpu_refusal(self.case.a, ["dispatch"], "probe")
        capture = json.loads((self.receipts / "probe.json").read_text())
        cases = (
            (OSError("private-canary"), None, "capture_unavailable"),
            (BoundedCommandError(CommandFailure.TIMEOUT), None, "capture_unavailable"),
            (BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT), None, "capture_unavailable"),
            (SystemExit(9), None, "capture_unavailable"),
            (None, BoundedCommandResult(1, b""), "native_completion_unavailable"),
            (None, BoundedCommandResult(0, b"nan private-canary\n"), "invalid_output"),
            (None, BoundedCommandResult(0, b"\xff"), "invalid_output"),
        )
        for fault, result, reason in cases:
            with self.subTest(fault=type(fault).__name__, reason=reason), \
                    patch("scripts.acceptance.private_cli.nested.case.run_bounded_command",
                          side_effect=fault, return_value=result) as snapshot_owner:
                self.case._record_failed_admission("probe", self.task_id, response, capture=capture)
            snapshot_owner.assert_called_once()
            projected = object_at(self.case.evidence, "failed_admission")
            self.assertEqual(projected["failure"], self.failure)
            snapshot = object_at(projected, "host_cpu_snapshot")
            self.assertEqual(snapshot["status"], "unavailable")
            self.assertEqual(snapshot["reason"], reason)
            self.assertEqual(snapshot.get("closure_unproven", False),
                             isinstance(fault, BoundedCommandError) and fault.reason == CommandFailure.TERMINATION_TIMEOUT)
            self.assertNotIn("private-canary", json.dumps(projected))

    def test_cpu_snapshot_requires_exact_response_and_completed_matching_control(self):
        response = self.completed_cpu_refusal(self.case.a, ["dispatch"], "probe")
        capture = json.loads((self.receipts / "probe.json").read_text())
        for key, value in (("schema_version", True), ("task_id", "d" * 32), ("admission", "admitted"),
                           ("resource_admission", {"status": "unknown"}),
                           ("failure", {**self.failure, "reason_code": "host_memory_critical"})):
            changed = {**response, key: value}
            with self.subTest(key=key), \
                    patch("scripts.acceptance.private_cli.nested.case.run_bounded_command") as snapshot_owner:
                self.case._record_failed_admission("probe", self.task_id, changed, capture=capture)
            snapshot_owner.assert_not_called()
            self.assertNotIn("host_cpu_snapshot", self.case.evidence["failed_admission"])
        for key, value in (("exit_code", True), ("exit_code", 0), ("failure_class", "invocation_error"),
                           ("invocation_error", "private-canary"), ("natural_wait", False),
                           ("response", {**response, "root_id": "d" * 32}),
                           ("response", {**response, "schema_version": True})):
            changed_capture = {**deepcopy(capture), key: value}
            with self.subTest(key=key, value=value), \
                    patch("scripts.acceptance.private_cli.nested.case.run_bounded_command") as snapshot_owner:
                self.case._record_failed_admission("probe", self.task_id, response, capture=changed_capture)
            snapshot_owner.assert_not_called()
            projected = self.case.evidence["failed_admission"]
            self.assertEqual(object_at(projected, "host_cpu_snapshot")["reason"], "control_completion_unavailable")
            self.assertNotIn("private-canary", json.dumps(projected))

    def test_warm_b_refusal_uses_its_immediately_precreated_original_token(self):
        self.assertFalse(self.case.b.exists())
        self.assert_refusal_cleanup("warm-b")
        token = self.case.b_token
        assert token is not None, "warm B must retain its original root token"
        token.assert_live()
        self.assertEqual(token.directory, self.case.b)

    def test_refused_direct_leader_filters_only_itself_and_preserves_warm_history(self):
        state, _primary = self.refuse("leader")
        raw = [self.warm_task_id, self.case.configuration["leader"]]
        self.assertEqual(self.case.lifetime.attempted_task_ids, raw)
        self.case.cleanup_deadline = time.monotonic() + 3
        fence_creator(self.case, self.case.cleanup_deadline)
        self.case.creator_fenced = True
        self.assertEqual(self.case.cleanup_task_ids(state, raw), (self.warm_task_id,))
        self.assertEqual(self.case.lifetime.attempted_task_ids, raw)
        self.assertEqual(len(self.native_observations), 1)
        self.assertEqual(self.native_observations[0], {
            "root_id": self.root_id(state), "task_id": self.case.configuration["leader"],
            "entry_exists": False, "root_device": self.state.identity[0], "root_inode": self.state.identity[1]})

    def test_direct_leader_refusal_closes_without_observing_or_cancelling_absent_leader(self):
        observer = Mock(side_effect=AssertionError("absent leader must not reach public status"))
        with patch("scripts.acceptance.private_cli.nested.cleanup.public_status_observer", return_value=observer):
            self.assert_refusal_cleanup("leader")
        observer.assert_not_called()
        self.assertEqual(self.case.lifetime.attempted_task_ids, [self.warm_task_id, self.case.configuration["leader"]])
        self.assertTrue((self.case.a / "tasks" / self.warm_task_id / "task.json").is_file())

    def test_direct_leader_requires_exact_fenced_unreconciled_childless_attempt(self):
        state, _primary = self.refuse("leader")
        self.case.cleanup_deadline = time.monotonic() + 3
        raw = list(self.case.lifetime.attempted_task_ids)
        failed = dict(object_at(self.case.evidence, "failed_admission"))
        for fault in ("unfenced", "wrong-state", "wrong-configured-leader", "wrong-returned-id", "not-last",
                      "created", "reconciled", "response-key-without-value", "w1-in-b", "w2-in-a"):
            with self.subTest(fault=fault):
                self.case.creator_fenced = fault != "unfenced"
                self.case.lifetime.attempted_task_ids[:] = raw
                self.case.b_ids.clear()
                self.case.responses.clear()
                self.case.evidence["failed_admission"] = dict(failed)
                leader = self.case.configuration["leader"]
                selected = state
                if fault == "wrong-state":
                    selected = self.case.b
                elif fault == "wrong-configured-leader":
                    self.case.configuration["leader"] = "e" * 32
                elif fault == "wrong-returned-id":
                    self.case.evidence["failed_admission"]["task_id"] = "e" * 32
                elif fault == "not-last":
                    self.case.lifetime.attempted_task_ids.append("e" * 32)
                elif fault == "created":
                    self.case.evidence["failed_admission"]["task_record"] = "created"
                elif fault in {"reconciled", "response-key-without-value"}:
                    self.case.responses["leader"] = {"task_id": leader} if fault == "reconciled" else None
                elif fault == "w1-in-b":
                    self.case.b_ids.append(self.case.configuration["w1"])
                elif fault == "w2-in-a":
                    self.case.lifetime.attempted_task_ids.insert(0, self.case.configuration["w2"])
                try:
                    requested = tuple(self.case.lifetime.attempted_task_ids)
                    self.assertEqual(self.case.cleanup_task_ids(selected, requested), requested)
                    self.native.assert_not_called()
                finally:
                    self.case.configuration["leader"] = leader

    def test_direct_leader_reappearance_blocks_actual_removal_even_with_filtered_input(self):
        from scripts.acceptance.private_cli.root_cleanup import remove_private_acceptance_root
        state, _primary = self.refuse("leader")
        self.case.cleanup_deadline = time.monotonic() + 3
        fence_creator(self.case, self.case.cleanup_deadline)
        self.case.creator_fenced = True
        self.assertEqual(self.case.cleanup_task_ids(state, self.case.lifetime.attempted_task_ids), (self.warm_task_id,))
        self.assertEqual(self.case.read_closure(state, ())["tasks"], {})
        (state / "tasks" / self.task_id).mkdir(mode=0o700)
        self.transferred = self.owner.transfer()
        with DisposalLeases() as disposal, self.assertRaisesRegex(AcceptanceFailure, "entry is present"):
            remove_private_acceptance_root(self.base, self.transferred,
                                           managed_state_roots=((state, ()),), disposal=disposal,
                                           deadline=self.case.cleanup_deadline, reader=self.case.read_closure)
        self.assertTrue(self.base.directory.is_dir())
        self.assertEqual(self.case.lifetime.attempted_task_ids, [self.warm_task_id, self.task_id])
        self.assertTrue(self.native_observations[-1]["entry_exists"])

    def test_cold_root_remains_absent_after_primary_bootstrap_refusal(self):
        self.base = self.owner.create("cold fixture", self.parent / ".dl-cold", self.parent)
        self.state = self.owner.create("cold primary state", self.base.directory / "state", self.base.directory)
        captures = self.owner.create("cold captures", self.base.directory / "captures", self.base.directory)
        lifetime = FixtureLifetime(ownership=self.owner, base=self.base.directory, base_token=self.base,
                                   state_token=self.state, capture_token=captures,
                                   private_config=self.state.directory / ".supervisor" / "pueue.yml",
                                   old_install=self.base.directory / "installed",
                                   old_libexec=self.base.directory / "libexec")
        args = SimpleNamespace(nested_topology="other-cold", expected_backend="process-group", nested_cpu_guard=True,
                               native_fixture=Path(sys.executable))
        fixture = SimpleNamespace(state=self.state.directory, workspace=self.parent,
                                  waiter_directory=self.base.directory / "waiters", lease_nonce="cold-nonce")
        self.case = NestedCase(args, lifetime, fixture, self.receipts, self.receipt)
        self.assertFalse(self.case.b.exists())
        self.assert_refusal_cleanup("warm-a")
        self.assertFalse(self.case.b.exists())
        self.assertIsNone(self.case.b_token)

    def test_already_filtered_ids_reprove_raw_candidate_and_reappearance_retains(self):
        state, _primary = self.refuse()
        self.case.cleanup_deadline = time.monotonic() + 3
        fence_creator(self.case, self.case.cleanup_deadline)
        self.case.creator_fenced = True
        self.assertEqual(self.case.cleanup_task_ids(state, [self.task_id]), ())
        self.assertEqual(self.case.read_closure(state, ())["tasks"], {})
        (state / "tasks").mkdir(mode=0o700)
        (state / "tasks" / self.task_id).mkdir(mode=0o700)
        with self.assertRaisesRegex(AcceptanceFailure, "entry is present"):
            self.case.read_closure(state, ())
        self.assertEqual(len(self.native_observations), 3)
        self.assertTrue(self.base.directory.is_dir())

    def test_finalizer_raw_attempts_are_filtered_at_closure_reader(self):
        state, _primary = self.refuse()
        self.case.cleanup_deadline = time.monotonic() + 3
        fence_creator(self.case, self.case.cleanup_deadline)
        self.case.creator_fenced = True
        self.assertEqual(self.case.read_closure(state, self.case.lifetime.attempted_task_ids)["tasks"], {})
        self.assertEqual(self.case.lifetime.attempted_task_ids, [self.task_id])
        self.assertEqual(len(self.native_observations), 1)

    def test_root_removal_rechecks_raw_refused_id_after_filtered_read(self):
        from scripts.acceptance.private_cli.root_cleanup import remove_private_acceptance_root
        state, _primary = self.refuse()
        self.case.cleanup_deadline = time.monotonic() + 3
        fence_creator(self.case, self.case.cleanup_deadline)
        self.case.creator_fenced = True
        self.assertEqual(self.case.cleanup_task_ids(state, [self.task_id]), ())
        self.assertEqual(self.case.read_closure(state, ())["tasks"], {})
        (state / "tasks").mkdir(mode=0o700)
        (state / "tasks" / self.task_id).mkdir(mode=0o700)
        self.transferred = self.owner.transfer()
        with DisposalLeases() as disposal, self.assertRaisesRegex(AcceptanceFailure, "entry is present"):
            remove_private_acceptance_root(self.base, self.transferred,
                                           managed_state_roots=((state, ()),), disposal=disposal,
                                           deadline=self.case.cleanup_deadline, reader=self.case.read_closure)
        self.assertTrue(self.base.directory.is_dir())
        self.assertEqual(self.case.lifetime.attempted_task_ids, [self.task_id])
        self.assertTrue(self.native_observations[-1]["entry_exists"])

    def test_expired_read_and_native_fault_or_changed_pin_cannot_exclude_attempt(self):
        state, _primary = self.refuse()
        self.case.creator_fenced = True
        self.case.cleanup_deadline = time.monotonic() - 1
        with self.assertRaisesRegex(AcceptanceFailure, "deadline expired"):
            self.case.cleanup_task_ids(state, [self.task_id])
        self.native.assert_not_called()
        self.case.cleanup_deadline = time.monotonic() + 3
        for fault in (SimpleNamespace(returncode=1, stdout="", stderr="read/close fault"),
                      SimpleNamespace(returncode=0, stdout="NESTED_TASK_OBSERVATION\t" + json.dumps({
                          "root_id": self.root_id(state), "task_id": self.task_id, "entry_exists": False,
                          "root_device": self.state.identity[0], "root_inode": self.state.identity[1] + 1}), stderr="")):
            with self.subTest(fault=fault), patch(
                    "scripts.acceptance.private_cli.nested.observation.run_captured_text_command",
                    return_value=fault), self.assertRaises(AcceptanceFailure):
                self.case.cleanup_task_ids(state, [self.task_id])
        self.assertEqual(self.case.lifetime.attempted_task_ids, [self.task_id])

    def test_replaced_original_root_is_rejected_before_native_observation(self):
        state, _primary = self.refuse()
        self.case.creator_fenced = True
        self.case.cleanup_deadline = time.monotonic() + 3
        retained = state.with_name("original-state")
        state.rename(retained)
        state.mkdir(mode=0o700)
        try:
            with self.assertRaisesRegex(RuntimeError, "replaced"):
                self.case.cleanup_task_ids(state, [self.task_id])
            self.native.assert_not_called()
        finally:
            state.rmdir()
            retained.rename(state)

    def test_unfenced_leader_attempted_or_exception_path_remains_strict(self):
        from scripts.acceptance.private_cli.nested.cleanup import cancel_owned_setup
        state, _primary = self.refuse()
        self.case.cleanup_deadline = time.monotonic() + 3
        with self.assertRaisesRegex(AcceptanceFailure, "owned setup task is not a regular record"):
            cancel_owned_setup(self.case, state, [self.task_id], self.case.cleanup_deadline)
        self.case.creator_fenced = True
        self.case.lifetime.attempted_task_ids.append(self.case.configuration["leader"])
        with self.assertRaisesRegex(AcceptanceFailure, "owned setup task is not a regular record"):
            cancel_owned_setup(self.case, state, [self.task_id], self.case.cleanup_deadline)
        self.case.lifetime.attempted_task_ids.pop()
        del self.case.evidence["failed_admission"]
        self.case.lifetime.attempted_task_ids.clear()
        with patch.object(NestedCase, "command", side_effect=TimeoutError("bootstrap control unresolved")), \
                self.assertRaisesRegex(TimeoutError, "control unresolved"):
            self.case.dispatch(state, self.task_id, self.parent / "brief.md", "2m", "warm-a")
        with self.assertRaisesRegex(AcceptanceFailure, "owned setup task is not a regular record"):
            cancel_owned_setup(self.case, state, [self.task_id], self.case.cleanup_deadline)
        self.native.assert_not_called()

    def test_created_setup_missing_its_record_never_uses_absence_exception(self):
        from scripts.acceptance.private_cli.nested.cleanup import cancel_owned_setup
        state, _primary = self.refuse()
        self.case.creator_fenced = True
        self.case.cleanup_deadline = time.monotonic() + 3
        object_at(self.case.evidence, "failed_admission")["task_record"] = "created"
        with self.assertRaisesRegex(AcceptanceFailure, "owned setup task is not a regular record"):
            cancel_owned_setup(self.case, state, [self.task_id], self.case.cleanup_deadline)
        self.native.assert_not_called()
