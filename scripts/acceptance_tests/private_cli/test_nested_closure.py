"""Closure-port checks; Go production-store tests own native absence semantics."""

import unittest
from contextlib import ExitStack
import fcntl
import os
import time
from types import SimpleNamespace
from unittest.mock import patch
from unittest.mock import Mock

from scripts.acceptance_tests.inspection.fixtures import InspectionFixture, ROOT, TASK
from scripts.acceptance_tests.record_fixtures import object_at
from scripts.delegate_acceptance.evidence.files import digest, write_json
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.private_cli.nested_observation import fixture_launch, observe_native, compare_native_tokens, await_actor_closure
from scripts.delegate_acceptance.private_cli.leases import DisposalLeases, DISPOSAL_LOCK_ORDER
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime, SupervisorCleanup
from scripts.delegate_acceptance.private_cli.nested_case import NestedCase
from scripts.delegate_acceptance.private_cli import nested_cleanup
from scripts.delegate_acceptance.private_cli import nested_observation
from scripts.delegate_acceptance.private_cli.supervisor import run_when_disposal_ready, await_managed_lifetime_closure_until
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.lifetime.closure import validate_managed_lifetime_closure


class NestedClosurePortTests(unittest.TestCase):
    def setUp(self):
        self.fixture = InspectionFixture(self)
        self.fixture.inspection_records(managed_exit_proof=True)
        self.directory = self.fixture.durable_ordinary_admission(runner_ownership="managed")
        stops = self.directory / "stop"
        stops.mkdir()
        self.request = stops / ("c" * 32 + ".request.json")
        write_json(self.request, {"schema_version": 1, "root_id": ROOT, "task_id": TASK,
                                 "request_id": "c" * 32, "cause": "user",
                                 "spec_sha256": digest(self.directory / "task.json")})
        self.proof = {"root_id": ROOT, "task_id": TASK, "never_started": True,
                      "refusal_sha256": digest(self.request)}

    def test_default_reader_stays_strict_and_explicit_owner_port_is_repeated(self):
        with self.assertRaises(AcceptanceFailure):
            validate_managed_lifetime_closure(self.fixture.state)
        owner = Mock(return_value=self.proof)
        for _ in range(2):
            result = validate_managed_lifetime_closure(self.fixture.state, prove_unlaunched=owner)
            self.assertEqual(object_at(result, "tasks")[TASK], {"disposition": "never-launched", "registration": None,
                                                    "refusal_sha256": self.proof["refusal_sha256"]})
        self.assertEqual(owner.call_count, 2)
        owner.assert_called_with(self.fixture.state, ROOT, TASK)

    def test_wrong_identity_false_proof_or_changed_refusal_cannot_pass(self):
        for change in ({"root_id": "d" * 32}, {"task_id": "d" * 32}, {"never_started": False},
                       {"refusal_sha256": "d" * 64}, {"refusal_sha256": "not-a-digest"}):
            with self.subTest(change=change), self.assertRaises(AcceptanceFailure):
                validate_managed_lifetime_closure(self.fixture.state,
                                                  prove_unlaunched=Mock(return_value={**self.proof, **change}))

    def test_past_proof_cannot_hide_new_execution_or_replaced_request(self):
        owner = Mock(return_value=self.proof)
        validate_managed_lifetime_closure(self.fixture.state, prove_unlaunched=owner)
        start = self.directory / "provider.start"
        start.write_bytes(b"consumed")
        with self.assertRaises(AcceptanceFailure):
            validate_managed_lifetime_closure(self.fixture.state, prove_unlaunched=owner)
        start.unlink()
        self.request.write_bytes(b"{}")
        with self.assertRaises(AcceptanceFailure):
            validate_managed_lifetime_closure(self.fixture.state, prove_unlaunched=owner)

    def test_present_native_journal_never_falls_back_to_unlaunched_port(self):
        lifetime = self.directory / "lifetime"
        lifetime.mkdir()
        write_json(lifetime / "intent.json", {})
        owner = Mock(return_value=self.proof)
        with self.assertRaises(AcceptanceFailure):
            validate_managed_lifetime_closure(self.fixture.state, prove_unlaunched=owner)
        owner.assert_not_called()

    def test_fixture_identity_selects_one_registered_command_among_preflight_and_later_launches(self):
        lifetime = self.fixture.managed_ordinary_lifetime(self.directory)
        first = _read_control_record(lifetime / "00000000000000000001.launch.json", "first launch")
        provider = {**first, "sequence": 2, "pid": 3002, "process_start": "provider-start"}
        later = {**first, "sequence": 3, "pid": 3003, "process_start": "later-start"}
        write_json(lifetime / "00000000000000000002.launch.json", provider)
        write_json(lifetime / "00000000000000000003.launch.json", later)
        response = {"root_id": ROOT, "task_id": TASK}
        identity = {"pid": 3002, "process_start": "provider-start"}
        tokens = Mock(side_effect=lambda observed, persisted: (observed, persisted) == ("provider-start", "provider-start"))
        self.assertEqual(fixture_launch(self.fixture.state, response, identity, tokens_match=tokens), provider)
        for changed in ({"pid": True}, {"process_start": "reused-start"}, {"pid": 9999}):
            with self.subTest(changed=changed), self.assertRaises(AcceptanceFailure):
                fixture_launch(self.fixture.state, response, {**identity, **changed}, tokens_match=tokens)
        write_json(lifetime / "00000000000000000003.launch.json", {**provider, "sequence": 3}, replace=True)
        with self.assertRaises(AcceptanceFailure):
            fixture_launch(self.fixture.state, response, identity, tokens_match=tokens)
        write_json(lifetime / "00000000000000000003.launch.json", {**later, "root_id": "f" * 32}, replace=True)
        with self.assertRaises(AcceptanceFailure):
            fixture_launch(self.fixture.state, response, identity, tokens_match=tokens)

    def test_fresh_callback_revalidates_under_real_disposal_leases_and_fault_retains(self):
        supervisor = self.fixture.state / ".supervisor"
        supervisor.mkdir(mode=0o700)
        for name in DISPOSAL_LOCK_ORDER:
            (supervisor / name).touch(mode=0o600)
        observations = []
        with DisposalLeases() as disposal:
            def proof(state, root, task):
                held = disposal.holds(state)
                observations.append(held)
                if held:
                    for name in DISPOSAL_LOCK_ORDER:
                        descriptor = os.open(supervisor / name, os.O_RDWR | os.O_NOFOLLOW)
                        try:
                            with self.assertRaises(BlockingIOError):
                                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                        finally:
                            os.close(descriptor)
                return self.proof

            def reader(state, tasks):
                return validate_managed_lifetime_closure(state, tasks, prove_unlaunched=proof)

            actions = []
            run_when_disposal_ready(self.fixture.state, (TASK,), (self.fixture.state,), time.monotonic() + 2,
                                    lambda closure: actions.append(closure), disposal=disposal, reader=reader)
            self.assertEqual(observations, [False, True])
            self.assertEqual(len(actions), 1)
            with patch("scripts.delegate_acceptance.private_cli.supervisor.read_managed_closure",
                       side_effect=RuntimeError("native helper close failed")):
                with self.assertRaisesRegex(RuntimeError, "native helper close failed"):
                    run_when_disposal_ready(self.fixture.state, (TASK,), (self.fixture.state,), time.monotonic() + 2,
                                            lambda closure: actions.append(closure), disposal=disposal, reader=reader)
            self.assertTrue(self.fixture.state.is_dir())
            self.assertEqual(len(actions), 1)

    def test_native_ports_forward_expected_token_and_reject_nonboolean_verdict(self):
        configuration = {"controls": str(self.fixture.base)}

        def observe(_configuration, _capture, pid, output, **options):
            self.assertEqual(options["DELEGATE_NESTED_FIXTURE_EXPECTED_TOKEN"], "persisted")
            write_json(output, {"pid": pid, "present": True, "process_start": "observed", "identity_matches": True})

        with patch("scripts.delegate_acceptance.private_cli.nested_observation.native_observation", side_effect=observe):
            result = observe_native(configuration, None, 123, expected_start="persisted")
        self.assertIs(result["identity_matches"], True)

        def malformed(_configuration, _capture, pid, output, **options):
            write_json(output, {"pid": pid, "present": True, "identity_matches": 1})

        with patch("scripts.delegate_acceptance.private_cli.nested_observation.native_observation", side_effect=malformed):
            with self.assertRaises(AcceptanceFailure):
                observe_native(configuration, None, 123, expected_start="persisted")
            with self.assertRaises(AcceptanceFailure):
                compare_native_tokens(configuration, None, "observed", "persisted")

    def test_historical_comparison_helper_failure_never_becomes_a_false_verdict(self):
        configuration = {"controls": str(self.fixture.base), "native_fixture": "/fixture"}
        failed = SimpleNamespace(returncode=17, stderr="unknown nested fixture mode", stdout="")
        with patch("scripts.delegate_acceptance.private_cli.nested_fixture.run_captured_text_command",
                   return_value=failed) as invoke, \
                patch("scripts.delegate_acceptance.private_cli.nested_observation._read_control_record") as read:
            with self.assertRaisesRegex(AcceptanceFailure, "native nested fixture observation failed"):
                compare_native_tokens(configuration, None, "observed", "persisted")
        read.assert_not_called()
        self.assertEqual(invoke.call_args.args[0], ["/fixture", "-test.run=^TestNestedSurvivalFixture$"])
        self.assertEqual(invoke.call_args.args[1]["DELEGATE_NESTED_FIXTURE_MODE"], "compare-identity")

    def test_closure_cannot_treat_changed_representation_of_live_identity_as_absent(self):
        lifetime = self.fixture.managed_ordinary_lifetime(self.directory)
        (lifetime / "owner.lock").touch(mode=0o600)
        case = SimpleNamespace(configuration={}, lifetime=SimpleNamespace(capture_token=None),
                               controls=SimpleNamespace(directory=self.fixture.base),
                               native=Mock(return_value={"present": True, "identity_matches": True,
                                                         "process_start": "representation-after-exec"}))
        response = {"root_id": ROOT, "task_id": TASK}
        with self.assertRaisesRegex(TimeoutError, "native task disposal"):
            await_actor_closure(case, self.fixture.state, response, time.monotonic() + 0.06)
        self.assertEqual(case.native.call_args.kwargs["expected_start"], "fixture-ordinary-worker")
        case.native.return_value = {"present": True, "identity_matches": False}

        def disposed(_configuration, _capture, _pid, output, **options):
            self.assertEqual(options["mode"], "scope")
            write_json(output, {"persistent_scope_present": False})

        with patch("scripts.delegate_acceptance.private_cli.nested_observation.native_observation", side_effect=disposed):
            closed = await_actor_closure(case, self.fixture.state, response, time.monotonic() + 1)
        self.assertTrue(closed["owner_released"])
        self.assertTrue(closed["scope_disposed"])

    def test_busy_absence_owner_retries_only_until_callers_deadline(self):
        owner = Mock(return_value={**self.proof, "never_started": False})

        def reader(state, tasks):
            return validate_managed_lifetime_closure(state, tasks, prove_unlaunched=owner)

        deadline = time.monotonic() + 0.06
        with self.assertRaisesRegex(RuntimeError, "closure remained incomplete"):
            await_managed_lifetime_closure_until(self.fixture.state, (TASK,), deadline, reader=reader)
        self.assertGreaterEqual(owner.call_count, 2)
        self.assertLess(time.monotonic(), deadline + 1)
        self.assertTrue(self.fixture.state.is_dir())

    def test_helper_failure_is_not_an_absence_certificate(self):
        owner = Mock(side_effect=RuntimeError("helper close failed"))
        with self.assertRaisesRegex(RuntimeError, "helper close failed"):
            validate_managed_lifetime_closure(self.fixture.state, prove_unlaunched=owner)


class NestedOwnerCleanupTests(unittest.TestCase):
    def setUp(self):
        self.fixture = InspectionFixture(self)
        self.state = self.fixture.state
        self.config = self.state / ".supervisor" / "pueue.yml"
        self.config.parent.mkdir(mode=0o700)
        self.config.write_text("shared: {}\n")
        for name in DISPOSAL_LOCK_ORDER:
            (self.config.parent / name).touch(mode=0o600)
        self.pending: SupervisorCleanup = {"client": self.fixture.pueue, "config": self.config, "env": {}, "state": self.state, "task_ids": []}
        self.lifetime = FixtureLifetime(supervisor_cleanups=[self.pending], managed_state_roots=[(self.state, [])])
        self.reply: dict[str, object] = {"root_id": ROOT, "supervisor_owner": {
            "action": "stop-owner", "state": "serving_released", "process_closure": "pending",
            "startup": {"sequence": 1, "epoch": "c" * 32, "intent_sha256": "d" * 64},
            "process": {"pid": 31415, "start_token": "selected-resident-start"}}}
        self.case = SimpleNamespace(configuration={"delegate": str(self.fixture.base / "delegate")},
                                    lifetime=self.lifetime, command=Mock(return_value=self.reply),
                                    native=Mock(return_value={"present": False, "identity_matches": False}),
                                    read_closure=Mock())
        self.disposal = DisposalLeases()
        self.addCleanup(self.disposal.release_all)
        self.readiness = self.patch("await_observed_disposal_readiness", return_value={"root_id": ROOT, "tasks": {}})
        self.patch("shutdown_custody_roots", return_value=(self.state,))
        self.patch("public_status_observer", return_value=Mock())
        self.selected_client = self.patch("bound_private_client", return_value=(self.fixture.pueue, {}))

        def shutdown(*_args, **options):
            self.disposal.hold(self.state, time.monotonic() + options["timeout"])
            return {"root_id": ROOT, "tasks": {}}

        self.shutdown = self.patch("shutdown_private_supervisor", side_effect=shutdown)

    def patch(self, name, *, target=nested_cleanup, **options):
        patcher = patch.object(target, name, **options)
        self.addCleanup(patcher.stop)
        return patcher.start()

    def close(self, deadline=None):
        deadline = time.monotonic() + 1 if deadline is None else deadline
        nested_cleanup.close_root(self.case, self.state, [], deadline, self.disposal)
        return deadline

    def assert_retained(self):
        self.assertNotIn(self.state, self.lifetime.closed_roots)
        self.assertEqual(self.lifetime.supervisor_cleanups, [self.pending])
        self.assertTrue(self.config.is_file())

    def closed_unlaunched_case(self):
        self.fixture.inspection_records(managed_exit_proof=True)
        directory = self.fixture.durable_ordinary_admission(runner_ownership="managed")
        lifetime = directory / "lifetime"
        lifetime.mkdir(exist_ok=True)
        (lifetime / "owner.lock").touch(mode=0o600)
        stops = directory / "stop"
        stops.mkdir()
        refusal = stops / ("c" * 32 + ".request.json")
        write_json(refusal, {"schema_version": 1, "root_id": ROOT, "task_id": TASK,
                             "request_id": "c" * 32, "cause": "user",
                             "spec_sha256": digest(directory / "task.json")})
        proof = {"root_id": ROOT, "task_id": TASK, "never_started": True,
                 "refusal_sha256": digest(refusal)}
        meta = _read_control_record(directory / "meta.json", "task metadata")
        write_json(self.config.parent / "supervisor.identity.json", {"binding": meta["supervisor_config"]})
        case = NestedCase.__new__(NestedCase)
        case.configuration = {**self.case.configuration, "observer_environment": {}}
        case.lifetime, case.command, case.native = self.lifetime, self.case.command, self.case.native
        case.cleanup_deadline = time.monotonic() + 2
        stopped = False

        def queue(*_args, **_options):
            if stopped:
                raise RuntimeError("dead daemon queue transport used")
            return {"tasks": {}, "groups": {}}

        def shutdown(*_args, **_options):
            nonlocal stopped
            self.disposal.hold(self.state, case.cleanup_deadline)
            closure = case.read_closure(self.state, (TASK,))
            stopped = True
            return closure

        self.readiness.side_effect = lambda *_args, **_options: case.read_closure(self.state, (TASK,))
        self.shutdown.side_effect = shutdown
        self.patch("bound_private_client", target=nested_observation, return_value=(self.fixture.pueue, {}))
        snapshot = self.patch("pueue_snapshot", target=nested_observation, side_effect=queue)
        observe = self.patch("observe_task_store", target=nested_observation, return_value=proof)
        nested_cleanup.close_root(case, self.state, (TASK,), case.cleanup_deadline, self.disposal)
        self.assertTrue(stopped)
        self.assertIn(self.state, self.lifetime.closed_roots)
        return case, proof, observe, snapshot

    def test_closed_root_rechecks_fresh_unlaunched_proof_without_dead_queue_transport(self):
        case, proof, observe, snapshot = self.closed_unlaunched_case()
        before = snapshot.call_count
        observe.reset_mock()
        closure = case.read_closure(self.state, (TASK,))
        self.assertEqual(object_at(closure, "tasks", TASK)["refusal_sha256"], proof["refusal_sha256"])
        observe.assert_called_once()
        self.assertEqual(snapshot.call_count, before)

    def test_closed_root_rejects_other_authority_changed_proof_and_released_lease(self):
        case, proof, observe, _snapshot = self.closed_unlaunched_case()
        closed = self.lifetime.closed_roots[self.state]
        if closed is None:
            self.fail("the closed root did not retain its authorizing closure")
        authorized = closed["closure"]
        previous = object_at(authorized, "tasks", TASK)
        for change in ({"root_id": "d" * 32}, {"tasks": {}},
                       {"tasks": {TASK: {**previous, "disposition": "closed"}}},
                       {"tasks": {TASK: {**previous, "refusal_sha256": "invalid"}}}):
            with self.subTest(change=change):
                self.lifetime.closed_roots[self.state] = {**closed, "closure": {**authorized, **change}}
                observe.reset_mock()
                with self.assertRaises(AcceptanceFailure):
                    case.read_closure(self.state, (TASK,))
                observe.assert_not_called()
        self.lifetime.closed_roots[self.state] = closed
        for change in ({"root_id": "d" * 32}, {"task_id": "d" * 32}, {"never_started": False},
                       {"refusal_sha256": "d" * 64}):
            with self.subTest(proof_change=change):
                observe.return_value = {**proof, **change}
                with self.assertRaises(AcceptanceFailure):
                    case.read_closure(self.state, (TASK,))
        observe.side_effect = RuntimeError("fresh task observer failed")
        with self.assertRaisesRegex(RuntimeError, "fresh task observer failed"):
            case.read_closure(self.state, (TASK,))

        def release_during_observation(*_args, **_options):
            self.disposal.release(self.state)
            return proof

        observe.side_effect = release_during_observation
        with self.assertRaisesRegex(RuntimeError, "disposal lease is incomplete"):
            case.read_closure(self.state, (TASK,))
        observe.reset_mock(side_effect=True)
        self.disposal.hold(self.state, time.monotonic() + 1)
        with self.assertRaisesRegex(RuntimeError, "disposal lease is incomplete"):
            case.read_closure(self.state, (TASK,))
        observe.assert_not_called()

    def test_one_stop_then_daemon_shutdown_then_exact_waiter_absence_under_same_deadline(self):
        events = []
        deadline = time.monotonic() + 1
        self.case.command.side_effect = lambda *_args, **_options: events.append("stop") or self.reply

        def shutdown(_client, _config, _env, **options):
            self.assertIs(options["disposal"], self.disposal)
            self.assertGreater(options["timeout"], 0)
            self.assertLessEqual(options["timeout"], deadline - started)
            self.disposal.hold(self.state, deadline)
            self.assert_retained()
            events.append("daemon")
            return {"root_id": ROOT, "tasks": {}}

        def native(pid, *, expected_start, deadline):
            self.assertEqual((pid, expected_start, deadline), (31415, "selected-resident-start", absolute_deadline))
            self.assertTrue(self.disposal.holds(self.state))
            self.assert_retained()
            events.append("native")
            return {"present": len(events) == 3, "identity_matches": True}

        self.shutdown.side_effect = shutdown
        self.case.native.side_effect = native
        absolute_deadline = deadline
        started = time.monotonic()
        self.close(deadline)
        self.assertEqual(events, ["stop", "daemon", "native", "native"])
        self.case.command.assert_called_once_with(self.state, ["supervisor", "stop-owner"], "cleanup-owner-state",
                                                  expected=(0, 1, 3), deadline=deadline)
        self.assertIn(self.state, self.lifetime.closed_roots)
        self.assertEqual(self.lifetime.supervisor_cleanups, [])
        self.close(deadline)
        self.assertEqual(self.case.command.call_count, 1)
        self.assertEqual(self.shutdown.call_count, 1)

    def test_reused_pid_proves_only_selected_incarnation_absent(self):
        self.case.native.return_value = {"present": True, "identity_matches": False}
        deadline = self.close()
        self.case.native.assert_called_once_with(31415, expected_start="selected-resident-start", deadline=deadline)
        self.assertIn(self.state, self.lifetime.closed_roots)

    def test_busy_foreign_or_malformed_stop_never_shuts_down_daemon(self):
        owner = object_at(self.reply, "supervisor_owner")
        replies = [
            {**self.reply, "root_id": "f" * 32}, {**self.reply, "error": "control fault"},
            *({**self.reply, "supervisor_owner": {**owner, "state": state}} for state in ("busy", "unknown")),
            *({**self.reply, "supervisor_owner": {**owner, "startup": startup}}
              for startup in (None, {**object_at(owner, "startup"), "sequence": True}, {**object_at(owner, "startup"), "epoch": "foreign"},
                              {**object_at(owner, "startup"), "intent_sha256": "malformed"})),
            *({**self.reply, "supervisor_owner": {**owner, "process": process}}
              for process in (None, {"pid": True, "start_token": "selected"}, {"pid": 0, "start_token": "selected"},
                              {"pid": 31415, "start_token": ""}, {"pid": 31415, "start_token": "bad\x00token"})),
        ]
        for reply in replies:
            with self.subTest(reply=reply):
                self.case.command.return_value = reply
                with self.assertRaises(AcceptanceFailure):
                    self.close()
                self.assert_retained()
                self.shutdown.assert_not_called()
                self.case.native.assert_not_called()
        self.assertEqual(self.case.command.call_count, len(replies))

    def test_stop_transport_error_never_tears_down_daemon_or_retries(self):
        self.case.command.side_effect = RuntimeError("stop transport failed")
        with self.assertRaisesRegex(RuntimeError, "stop transport failed"):
            self.close()
        self.case.command.assert_called_once()
        self.shutdown.assert_not_called()
        self.case.native.assert_not_called()
        self.assert_retained()

    def test_daemon_shutdown_error_retains_entry_before_any_waiter_claim(self):
        self.shutdown.side_effect = RuntimeError("daemon closure unknown")
        with self.assertRaisesRegex(RuntimeError, "daemon closure unknown"):
            self.close()
        self.case.command.assert_called_once()
        self.case.native.assert_not_called()
        self.assert_retained()

    def test_live_unknown_or_failed_native_observation_retains_entry_without_another_stop(self):
        for observation in ({"present": True, "identity_matches": True},
                            {"present": True, "identity_matches": None}, RuntimeError("native observer fault")):
            with self.subTest(observation=observation):
                self.case.command.reset_mock()
                self.case.native.side_effect = observation if isinstance(observation, RuntimeError) else None
                self.case.native.return_value = observation
                with self.assertRaises((TimeoutError, AcceptanceFailure, RuntimeError)):
                    self.close(time.monotonic() + 0.03)
                self.case.command.assert_called_once()
                self.assert_retained()

    def test_pending_custody_never_stops_owner_or_counts_cleanup_complete(self):
        self.readiness.side_effect = TimeoutError("custody release pending")
        with self.assertRaisesRegex(TimeoutError, "custody release pending"):
            self.close()
        self.case.command.assert_not_called()
        self.shutdown.assert_not_called()
        self.assert_retained()

    def test_not_initialized_only_passes_without_daemon_config(self):
        self.config.unlink()
        self.case.command.return_value = {"root_id": ROOT, "supervisor_owner": {"action": "stop-owner", "state": "not_initialized"}}
        self.close()
        self.selected_client.assert_not_called()
        self.shutdown.assert_not_called()
        self.case.native.assert_not_called()
        self.assertIn(self.state, self.lifetime.closed_roots)
        self.assertEqual(self.lifetime.supervisor_cleanups, [])

    def test_not_initialized_with_daemon_config_retains_without_teardown(self):
        self.case.command.return_value = {"root_id": ROOT, "supervisor_owner": {"action": "stop-owner", "state": "not_initialized"}}
        with self.assertRaisesRegex(AcceptanceFailure, "private daemon config remains"):
            self.close()
        self.shutdown.assert_not_called()
        self.assert_retained()

    def test_expired_deadline_does_not_renew_daemon_shutdown_budget(self):
        deadline = time.monotonic() - 1
        with self.assertRaisesRegex(TimeoutError, "shutdown observation expired"):
            self.close(deadline)
        self.shutdown.assert_not_called()
        self.case.command.assert_called_once_with(self.state, ["supervisor", "stop-owner"], "cleanup-owner-state",
                                                  expected=(0, 1, 3), deadline=deadline)
        self.assert_retained()

    def test_cleanup_propagates_owner_failure_and_closes_its_existing_actor_leases(self):
        failure = RuntimeError("selected owner closure failed")
        case = object.__new__(NestedCase)
        case.leases = ExitStack()
        with patch.object(case.leases, "close") as close, \
                patch.object(nested_cleanup, "cleanup_case", side_effect=failure) as cleanup:
            with self.assertRaisesRegex(RuntimeError, "selected owner closure failed") as observed:
                NestedCase.cleanup(case, 123.5, self.disposal)
        self.assertIs(observed.exception, failure)
        self.assertEqual(case.cleanup_deadline, 123.5)
        cleanup.assert_called_once_with(case, 123.5, self.disposal)
        close.assert_called_once()

    def test_cleanup_preserves_owner_failure_when_actor_lease_close_also_fails(self):
        failure = RuntimeError("selected owner closure failed")
        case = object.__new__(NestedCase)
        case.leases = ExitStack()
        with patch.object(case.leases, "close", side_effect=RuntimeError("actor lease close failed")), \
                patch.object(nested_cleanup, "cleanup_case", side_effect=failure):
            with self.assertRaisesRegex(RuntimeError, "selected owner closure failed.*actor lease close failed") as observed:
                NestedCase.cleanup(case, 123.5, self.disposal)
        self.assertIs(observed.exception.__cause__, failure)
