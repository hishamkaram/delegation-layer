"""The injected refusal is finite, captured once, and never confused with setup failure."""

import json
import math
import os
import sys
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from scripts.delegate_acceptance.directories.operations import DirectoryOwner, rollback_owned_directories
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli.nested_case import NestedCase
from scripts.delegate_acceptance.private_cli.nested_refusal import (
    RefusedLeaderCase, dispose_retained_root, require_injected_refusal, run_refused_leader,
)
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt, resource_admission_evidence
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_dispatch_evidence


class InnerReceiptFailureTests(unittest.TestCase):
    def test_missing_malformed_or_interrupted_inner_receipt_writes_outer_failure_and_retains_root(self):
        parent = Path(self.enterContext(tempfile.TemporaryDirectory()))
        owner = DirectoryOwner()
        self.addCleanup(lambda: rollback_owned_directories(owner.transfer()))
        base = owner.create("retained acceptance root", parent / ".dl-refusal", parent)
        state = owner.create("retained state", base.directory / "state", base.directory)
        original = RuntimeError("setup private-secret")
        lifetimes = []

        def fail_setup(_args, _nonce, _legacy, lifetime, _responses, _receipt):
            lifetime.ownership = owner
            lifetime.base, lifetime.base_token, lifetime.state_token = base.directory, base, state
            lifetimes.append(lifetime)
            raise original

        for mode in ("missing", "malformed", "interrupted"):
            with self.subTest(mode=mode):
                args = SimpleNamespace(output=parent / (mode + ".json"), pueue=Path(sys.executable),
                                       pueued=Path(sys.executable), native_fixture=Path(sys.executable))
                receipt = AcceptanceReceipt(args.output, 3, 1, {})

                def fail_inner(_args, _failure, _lifetime, inner, _success):
                    if mode == "malformed":
                        inner.output.write_text("{invalid JSON")
                    elif mode == "interrupted":
                        raise InterruptedError("finalizer private-secret")

                with patch("scripts.delegate_acceptance.private_cli.nested_refusal.prepare_fixture", side_effect=fail_setup), \
                        patch("scripts.delegate_acceptance.private_cli.nested_refusal.finalize_fixture", side_effect=fail_inner):
                    with self.assertRaises(AcceptanceFailure) as raised:
                        run_refused_leader(args, parent / "responses", receipt)
                self.assertTrue(args.output.is_file(), "inner read failure must still publish outer FAIL")
                outer = json.loads(args.output.read_text())
                self.assertEqual(outer["result"], "FAIL")
                self.assertEqual(outer["retained_root"], str(base.directory))
                self.assertEqual(outer["cleanup"], {
                    "status": "retained", "private_root": {"status": "retained", "verified": False}})
                self.assertEqual(outer["refused_leader"]["inner"], {})
                self.assertEqual(outer["refused_leader"]["failure_context"]["primary"], "RuntimeError")
                if mode == "interrupted":
                    self.assertEqual(outer["refused_leader"]["failure_context"]["finalization"], "InterruptedError")
                self.assertIs(raised.exception.__cause__, original)
                self.assertNotIn("private-secret", args.output.read_text())
                self.assertEqual(lifetimes[-1].closed_roots, {})
                base.assert_live()
                state.assert_live()

    def test_fresh_read_fault_at_actual_removal_boundary_retains_original_owned_directories(self):
        parent = Path(self.enterContext(tempfile.TemporaryDirectory()))
        owner = DirectoryOwner()
        transferred = []
        self.addCleanup(lambda: rollback_owned_directories(tuple(transferred) if transferred else owner.transfer()))
        base = owner.create("retained acceptance root", parent / ".dl-disposal", parent)
        state = owner.create("retained state", base.directory / "state", base.directory)
        lifetime = FixtureLifetime(ownership=owner, base=base.directory, base_token=base,
                                   state_token=state, private_config=state.directory / ".supervisor" / "pueue.yml")
        warm, leader, root = "a" * 32, "b" * 32, "c" * 32
        lifetime.attempted_task_ids.extend((warm, leader))
        lifetime.managed_state_roots.append((state.directory, lifetime.attempted_task_ids))
        closure = {"root_id": root, "tasks": {warm: {}}}
        write_json(parent / "cleanup-owner-state.json", {"response": {"root_id": root, "supervisor_owner": {
            "action": "stop-owner", "state": "serving_released",
            "startup": {"sequence": 1, "epoch": "d" * 32, "intent_sha256": "e" * 64},
            "process": {"pid": 41, "start_token": "fixture:owner"}}}})
        case = SimpleNamespace(lifetime=lifetime, a=state.directory, configuration={"leader": leader}, receipts=parent,
                               read_closure=Mock(side_effect=[closure, AcceptanceFailure("fresh closure read fault")]),
                               native=Mock(return_value={"present": False, "identity_matches": False}),
                               tokens_match=lambda _observed, _persisted: True)
        before = {"owner": {"pid": 41, "process_start": "fixture:owner"},
                  "daemon": {"pid": 42, "process_start": "fixture:daemon"}}
        original_transfer = owner.transfer

        def transfer():
            entries = original_transfer()
            transferred.extend(entries)
            return entries

        with patch.object(owner, "transfer", side_effect=transfer):
            with self.assertRaisesRegex(AcceptanceFailure, "fresh closure read fault"):
                dispose_retained_root(case, before)
        base.assert_live()
        state.assert_live()
        self.assertFalse(lifetime.disposal.holds(state.directory))


class RefusedLeaderControlTests(unittest.TestCase):
    def setUp(self):
        parent = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.owner = DirectoryOwner()
        self.addCleanup(lambda: rollback_owned_directories(self.owner.transfer()))
        self.state = self.owner.create("state", parent / "state", parent)
        capture = self.owner.create("captures", parent / "captures", parent)
        write_json(self.state.directory / "root.json", {"schema_version": 1, "root_id": "a" * 32})
        self.case = RefusedLeaderCase.__new__(RefusedLeaderCase)
        self.case.a = self.state.directory
        self.case.configuration = {"leader": "b" * 32}
        self.case.injected = False
        self.case.commands = []
        self.case.responses = {}
        self.case.evidence = {}
        self.case.receipts = parent
        self.case.fixture = SimpleNamespace(workspace=parent)
        self.case.lifetime = FixtureLifetime(env=dict(os.environ), capture_token=capture)

    def dispatch(self):
        return self.case.dispatch(self.state.directory, "b" * 32, self.state.directory / "brief", "3m", "leader")

    def test_real_capture_wait_returns_explicit_id_unknown_stop_once_without_task_creation(self):
        # The fixture is an actual finite child of the existing capture owner.
        # Any observation after its non-created reply would escape to this real
        # Delegate boundary and fail the test.
        with patch.object(NestedCase, "command", side_effect=AssertionError("unexpected real task command")):
            with self.assertRaisesRegex(AcceptanceFailure, "never redispatch"):
                self.dispatch()
        require_injected_refusal(self.case)
        captured = json.loads((self.case.receipts / "leader.json").read_text())
        self.assertEqual(captured["exit_code"], 1)
        self.assertEqual(captured["response"]["root_id"], "a" * 32)
        self.assertEqual(captured["response"]["task_record"], "unknown")
        self.assertEqual(captured["response"]["admission"], "unknown")
        self.assertEqual(captured["response"]["liveness"], "undetermined")
        self.assertEqual(captured["response"]["publication"], "unknown")
        self.assertEqual(captured["response"]["error"],
                         "Delegate refused new work because resource pressure was confirmed. Pressure: host_cpu (sustained).")
        self.assertEqual(captured["response"]["failure"], {
            "code": "resource_pressure", "stage": "check-resources",
            "reason_code": "host_cpu_sustained", "next_action": "stop_and_report",
        })
        cpu = {"counter_unit": "ticks" if sys.platform == "linux" else "seconds",
               "busy_threshold": 0.95, "required_streak": 3,
               "intervals": [
                   {"elapsed_ms": 1000, "busy_delta": 95, "total_delta": 100, "busy_fraction": 0.95, "classification": "hot"},
                   {"elapsed_ms": 1002, "busy_delta": 198, "total_delta": 200, "busy_fraction": 0.99, "classification": "hot"},
                   {"elapsed_ms": 1001, "busy_delta": 297, "total_delta": 300, "busy_fraction": 0.99, "classification": "hot"},
               ]}
        resource = {"status": "pressure", "diagnostics": {"host_cpu": cpu}}
        self.assertEqual(captured["response"]["resource_admission"], resource)
        self.assertEqual(self.case.evidence["failed_admission"]["resource_admission"], resource)
        self.assertEqual(self.case.lifetime.attempted_task_ids, ["b" * 32])
        self.assertEqual(self.case.responses, {})
        self.assertEqual(len(self.case.commands), 1)
        self.assertFalse((self.state.directory / "tasks").exists())
        with self.assertRaisesRegex(AcceptanceFailure, "only the original leader dispatch"):
            self.dispatch()

    def test_warm_refusal_and_unrelated_failure_are_never_expected_negative(self):
        with self.assertRaisesRegex(AcceptanceFailure, "never redispatch"):
            self.dispatch()
        original = dict(self.case.evidence["failed_admission"])
        for change in ({"name": "warm-a"}, {"task_record": "not_created"}, {"admission": "not-admitted"},
                       {"task_id": "c" * 32}, {"failure": {"code": "storage_failed"}}, {"resource_admission": {}}):
            with self.subTest(change=change):
                self.case.evidence["failed_admission"] = {**original, **change}
                with self.assertRaisesRegex(AcceptanceFailure, "expected only the injected"):
                    require_injected_refusal(self.case)
        self.case.evidence["failed_admission"] = original
        self.case.injected = False
        with self.assertRaisesRegex(AcceptanceFailure, "expected only the injected"):
            require_injected_refusal(self.case)

    def test_nested_failed_admission_retains_only_allowlisted_cpu_diagnostics(self):
        response = cpu_refusal()
        with patch("scripts.delegate_acceptance.private_cli.nested_refusal.run_json", return_value=response):
            with self.assertRaisesRegex(AcceptanceFailure, "never redispatch"):
                self.dispatch()
        public = self.case.evidence["failed_admission"]["resource_admission"]
        self.assertEqual(public, {"status": "pressure", "diagnostics": {"host_cpu": expected_cpu()}})
        self.assertNotIn("private-secret", json.dumps(self.case.evidence))


class NestedChildAdmissionReceiptTests(unittest.TestCase):
    def setUp(self):
        self.parent = Path(self.enterContext(tempfile.TemporaryDirectory(prefix="nested-child-receipt-")))
        owner = DirectoryOwner()
        self.addCleanup(lambda: rollback_owned_directories(owner.transfer()))
        controls = owner.create("nested controls", self.parent / "controls", self.parent)
        self.case = NestedCase.__new__(NestedCase)
        self.case.controls = controls
        self.case.configuration = {"leader": "b" * 32, "w1": "c" * 32}
        self.case.lifetime = FixtureLifetime()
        self.case.lifetime.attempted_task_ids.append("b" * 32)
        self.case.b_ids = self.case.lifetime.attempted_task_ids
        self.case.responses = {"leader": {"task_id": "b" * 32, "task_record": "created", "admission": "admitted"}}
        self.case.evidence = {"topology": "shared-warm", "milestones": {}}
        self.command = Mock(side_effect=AssertionError("unknown child must not be observed"))
        self.dispatch = Mock(side_effect=AssertionError("unknown child must not be redispatched"))
        self.enterContext(patch.object(self.case, "command", self.command))
        self.enterContext(patch.object(self.case, "dispatch", self.dispatch))
        self.receipt = AcceptanceReceipt(self.parent / "receipt.json", 3, 1, {})
        self.receipt.update_counts(dispatch_completed_count=1, admitted_task_count=1)
        self.case.receipt = self.receipt
        self.receipt.add_details(nested_survival=self.case.evidence)

    def publish_capture(self, capture):
        write_json(self.case.controls.directory / "w1-control-ended.json", {"control": "ended"}, replace=True)
        write_json(self.case.controls.directory / "w1-dispatch.json", capture, replace=True)

    def failed_child(self, capture):
        self.publish_capture(capture)
        before_counts = dict(self.receipt.counts)
        with self.assertRaises(AcceptanceFailure) as raised:
            self.case.nested_response("w1")
        self.assertEqual(str(raised.exception), "nested admission was not proven")
        self.assertEqual(self.case.b_ids, ["b" * 32])
        self.assertEqual(self.case.responses, {"leader": {"task_id": "b" * 32, "task_record": "created", "admission": "admitted"}})
        self.assertEqual(self.receipt.counts, before_counts)
        self.command.assert_not_called()
        self.dispatch.assert_not_called()
        self.assertIn("failed_admission", self.case.evidence)
        self.receipt.write("FAIL", {"status": "retained"}, raised.exception)
        published = json.loads(self.receipt.output.read_text())
        self.assertEqual(published["failure"]["exception_type"], "AcceptanceFailure")
        return published["nested_survival"]["failed_admission"]

    def test_unknown_child_cpu_refusal_retains_only_public_evidence(self):
        response = cpu_refusal()
        response["task_id"] = "c" * 32
        capture = {"exit_code": 1, "response": response, "arguments": ["private-child-canary"],
                   "stdout_tail": "private-child-canary", "stderr_tail": "private-child-canary"}
        actual = self.failed_child(capture)
        self.assertEqual(actual, {
            "name": "w1", "requested_task_id": "c" * 32, "exit_code": 1, "response_present": True,
            "schema_version": 1, "command": "dispatch", "root_id": "a" * 32, "task_id": "c" * 32,
            "task_record": "unknown", "admission": "unknown",
            "failure": {"code": "resource_pressure", "stage": "check-resources",
                        "reason_code": "host_cpu_sustained", "next_action": "stop_and_report"},
            "resource_admission": {"status": "pressure", "diagnostics": {"host_cpu": {
                "counter_unit": "ticks", "busy_threshold": 0.95, "required_streak": 3,
                "intervals": [{"elapsed_ms": 1020, "busy_delta": 98, "total_delta": 100,
                               "busy_fraction": 0.98, "classification": "hot"}]}}},
        })
        serialized = self.receipt.output.read_text()
        self.assertNotIn("private-child-canary", serialized)
        self.assertNotIn("private-secret", serialized)
        self.assertEqual(json.loads((self.case.controls.directory / "w1-dispatch.json").read_text()), capture)

    def test_missing_or_nonobject_response_retains_bounded_exit_and_unknown_markers(self):
        for capture, expected_exit in (({"exit_code": 7}, 7), ({"exit_code": None, "response": []}, None),
                                       ({"exit_code": True, "response": "private-child-canary"}, None),
                                       ({"exit_code": "1"}, None), ({"exit_code": 2**63}, None)):
            with self.subTest(exit_type=type(capture["exit_code"]).__name__):
                self.case.evidence.pop("failed_admission", None)
                actual = self.failed_child({**capture, "arguments": ["private-child-canary"],
                                            "invocation_error": "private-child-canary", "stdout_tail": "private-child-canary"})
                self.assertEqual(actual, {
                    "name": "w1", "requested_task_id": "c" * 32, "exit_code": expected_exit, "response_present": False,
                    "schema_version": None, "command": None, "root_id": None, "task_id": None,
                    "task_record": None, "admission": None, "failure": {},
                })
                self.assertNotIn("private-child-canary", self.receipt.output.read_text())

    def test_malformed_selected_fields_do_not_export_nested_objects(self):
        response = {key: {"private-child-canary": ["private-child-canary"]} for key in (
            "schema_version", "command", "root_id", "task_id", "task_record", "admission")}
        response["failure"] = {key: ["private-child-canary"] for key in (
            "code", "stage", "reason_code", "next_action", "error")}
        actual = self.failed_child({"exit_code": -15, "response": response})
        self.assertEqual(actual, {"name": "w1", "requested_task_id": "c" * 32, "exit_code": -15,
                                  "response_present": True, "failure": {}})
        self.assertNotIn("private-child-canary", self.receipt.output.read_text())

    def test_unexpected_exit_does_not_turn_created_fields_into_admission_authority(self):
        response = {"schema_version": 1, "command": "dispatch", "root_id": "a" * 32,
                    "task_id": "c" * 32, "task_record": "created", "admission": "admitted"}
        actual = self.failed_child({"exit_code": 7, "response": response})
        self.assertEqual(actual, {"name": "w1", "requested_task_id": "c" * 32, "exit_code": 7,
                                  "response_present": True, "failure": {}, **response})

    def test_created_child_keeps_existing_admission_flow_without_failure_evidence(self):
        response = {"schema_version": 1, "command": "dispatch", "root_id": "a" * 32,
                    "task_id": "c" * 32, "task_record": "created", "admission": "admitted"}
        self.publish_capture({"exit_code": 0, "response": response})
        self.assertEqual(self.case.nested_response("w1"), response)
        self.assertEqual(self.case.b_ids, ["b" * 32, "c" * 32])
        self.assertEqual(self.case.responses, {"leader": {"task_id": "b" * 32, "task_record": "created", "admission": "admitted"},
                                               "w1": response})
        self.assertEqual(self.receipt.counts["dispatch_completed_count"], 2)
        self.assertEqual(self.receipt.counts["admitted_task_count"], 2)
        self.assertNotIn("failed_admission", self.case.evidence)
        self.command.assert_not_called()
        self.dispatch.assert_not_called()


def expected_cpu():
    return {"counter_unit": "ticks", "busy_threshold": 0.95, "required_streak": 3,
            "intervals": [{"elapsed_ms": 1020, "busy_delta": 98, "total_delta": 100,
                           "busy_fraction": 0.98, "classification": "hot"}]}


def cpu_refusal():
    diagnostic = expected_cpu()
    diagnostic.update(raw_error="private-secret", pid=1234)
    diagnostic["intervals"][0]["path"] = "private-secret"
    return {"schema_version": 1, "command": "dispatch", "task_id": "b" * 32, "root_id": "a" * 32,
            "task_record": "unknown", "admission": "unknown",
            "failure": {"code": "resource_pressure", "stage": "check-resources",
                        "reason_code": "host_cpu_sustained", "next_action": "stop_and_report"},
            "resource_admission": {"status": "pressure", "raw_error": "private-secret",
                                   "diagnostics": {"host_cpu": diagnostic, "environment": "private-secret"}}}


class HostCPUReceiptTests(unittest.TestCase):
    def test_failed_dispatch_retains_independent_bounded_copy_without_secret_fields(self):
        response = cpu_refusal()
        evidence = default_stop_dispatch_evidence(response)
        expected = {"status": "pressure", "diagnostics": {"host_cpu": expected_cpu()}}
        self.assertEqual(evidence["resource_admission"], expected)
        self.assertNotIn("private-secret", json.dumps(evidence))
        response["resource_admission"]["diagnostics"]["host_cpu"]["intervals"][0]["busy_delta"] = 0
        self.assertEqual(evidence["resource_admission"], expected)

    def test_unavailable_interval_keeps_reason_and_missing_numbers_absent(self):
        response = cpu_refusal()
        interval = {"elapsed_ms": 1000, "classification": "unavailable", "unavailable_reason": "counter_reset"}
        response["resource_admission"]["diagnostics"]["host_cpu"]["intervals"] = [interval]
        evidence = resource_admission_evidence(response)
        self.assertEqual(evidence, {"status": "pressure", "diagnostics": {
            "host_cpu": {**expected_cpu(), "intervals": [interval]}}})

    def test_native_seconds_and_fractional_deltas_are_preserved(self):
        response = cpu_refusal()
        host = response["resource_admission"]["diagnostics"]["host_cpu"]
        host["counter_unit"] = "seconds"
        host["intervals"][0].update(busy_delta=0.98, total_delta=1)
        expected = {**expected_cpu(), "counter_unit": "seconds", "intervals": [
            {"elapsed_ms": 1020, "busy_delta": 0.98, "total_delta": 1, "busy_fraction": 0.98,
             "classification": "hot"}]}
        self.assertEqual(resource_admission_evidence(response), {"status": "pressure", "diagnostics": {"host_cpu": expected}})

    def test_malformed_unbounded_or_nonfinite_optional_evidence_is_omitted(self):
        for changed in ({"counter_unit": "private-secret"}, {"busy_threshold": math.nan},
                        {"required_streak": True}, {"intervals": [expected_cpu()["intervals"][0]] * 4},
                        {"intervals": [{"elapsed_ms": 1, "classification": "hot", "busy_delta": math.inf}]},
                        {"intervals": [{"elapsed_ms": 1, "classification": "hot"}]},
                        {"intervals": [{"elapsed_ms": -1, "classification": "unavailable", "unavailable_reason": "deadline"}]},
                        {"intervals": [{"elapsed_ms": 1, "classification": "hot", "busy_fraction": 1.1}]},
                        {"intervals": [{"elapsed_ms": 1, "classification": "unavailable",
                                        "unavailable_reason": "private-secret"}]}):
            with self.subTest(changed=changed):
                response = cpu_refusal()
                response["resource_admission"]["diagnostics"]["host_cpu"].update(changed)
                self.assertEqual(resource_admission_evidence(response), {"status": "pressure"})

    def test_clear_unknown_non_cpu_and_joined_failure_preserve_status_only(self):
        for status, failure in (("clear", {}), ("unknown", {}),
                                ("pressure", {"code": "resource_pressure", "reason_code": "host_memory_critical"}),
                                ("pressure", {"code": "admission_failed", "reason_code": "host_cpu_sustained"})):
            with self.subTest(status=status, failure=failure):
                response = cpu_refusal()
                response["resource_admission"]["status"], response["failure"] = status, failure
                self.assertEqual(resource_admission_evidence(response), {"status": status})
