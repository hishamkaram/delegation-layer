"""Closure-port checks; Go production-store tests own native absence semantics."""

import unittest
import fcntl
import os
import time
from types import SimpleNamespace
from unittest.mock import patch
from unittest.mock import Mock

from scripts.acceptance_tests.inspection.fixtures import InspectionFixture, ROOT, TASK
from scripts.delegate_acceptance.evidence.files import digest, write_json
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.private_cli.nested_observation import fixture_launch, observe_native, compare_native_tokens, await_actor_closure
from scripts.delegate_acceptance.private_cli.leases import DisposalLeases, DISPOSAL_LOCK_ORDER
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
            self.assertEqual(result["tasks"][TASK], {"disposition": "never-launched", "registration": None,
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
