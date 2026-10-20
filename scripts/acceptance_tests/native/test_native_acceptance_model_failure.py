"""Model discovery accepts Delegate's typed operational failure without echoing it."""

import ast
import inspect
import json
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from scripts import acceptance_native as entry
from scripts.delegate_acceptance.native import (
    acceptance as gate,
    provider_policy,
)


PROVIDER = "pi:json"
JOURNAL = "a" * 32
# Distinct sentinels prove the sanitized summary never carries diagnostic text.
ERROR_TEXT = "Delegate could not start the provider SENTINEL-ERROR."
RECOVERY = "delegate supervisor migrate --root <same-state-root> --json SENTINEL-RECOVERY"


def response(status: str, reason: str, **fields) -> dict[str, object]:
    listed = status in {"available", "partial"}
    return {
        "schema_version": 1, "command": "models", "provider": PROVIDER,
        "observed_at": "2026-10-20T10:20:30.123Z", "status": status, "reason_code": reason,
        "complete": status == "available", "source": "pi --list-models" if listed else "none",
        "efforts": None, "models": [{"id": "local/model", "efforts": None}] if listed else [],
        **fields,
    }


def failure(**fields) -> dict[str, object]:
    return {"code": "provider_unavailable", "stage": "select-provider",
            "next_action": "stop_and_report", **fields}


FAILURE_STATUSES = (("failed", "discovery_failed"), ("unavailable", "discovery_unavailable"),
                    ("blocked", "supervisor_upgrade_required"))


class ModelDiscoveryFailureContractTests(unittest.TestCase):
    def validate(self, value: dict[str, object]) -> dict[str, object]:
        return gate.validate_model_discovery_response(value, PROVIDER)

    def test_every_status_validates_with_and_without_diagnostics(self):
        for status, reason in (("available", "ok"), ("partial", "partial_catalog"), *FAILURE_STATUSES):
            with self.subTest(status=status):
                self.assertEqual(self.validate(response(status, reason))["status"], status)
        diagnostics = (
            failure(),
            failure(reason_code="pueue_version_unsupported", recovery_command=RECOVERY),
            failure(inspection={"class": "inspection_failed", "deadline_state": "unknown"}),
            failure(code="supervisor_upgrade_required", next_action="run_supervisor_migrate",
                    recovery_command=RECOVERY,
                    inspection={"class": "admission_deadline_expired", "deadline_state": "expired",
                                "journal_task_id": JOURNAL}),
        )
        for status, reason in FAILURE_STATUSES:
            for index, value in enumerate(diagnostics):
                with self.subTest(status=status, diagnostic=index):
                    plain = self.validate(response(status, reason))
                    summary = self.validate(response(status, reason, failure=value, error=ERROR_TEXT))
                    self.assertEqual(summary, plain)
                    rendered = json.dumps(summary)
                    for text in (ERROR_TEXT, RECOVERY, value["stage"], value["next_action"]):
                        self.assertNotIn(text, rendered)

    def test_malformed_failure_projection_is_rejected(self):
        inspection = {"class": "inspection_failed", "deadline_state": "unknown"}
        cases = {
            "failure-not-object": ["provider_unavailable"],
            "failure-null": None,
            "missing-code": {"stage": "select-provider", "next_action": "stop_and_report"},
            "missing-stage": {"code": "provider_unavailable", "next_action": "stop_and_report"},
            "missing-next-action": {"code": "provider_unavailable", "stage": "select-provider"},
            "empty-code": failure(code=""),
            "integer-stage": failure(stage=7),
            "null-next-action": failure(next_action=None),
            "empty-reason": failure(reason_code=""),
            "integer-recovery": failure(recovery_command=1),
            "unknown-field": failure(cause="raw provider stderr"),
            "inspection-not-object": failure(inspection="inspection_failed"),
            "inspection-missing-class": failure(inspection={"deadline_state": "unknown"}),
            "inspection-missing-deadline": failure(inspection={"class": "inspection_failed"}),
            "inspection-empty-class": failure(inspection=dict(inspection, **{"class": ""})),
            "inspection-unknown-field": failure(inspection=dict(inspection, path="/state/root")),
            "journal-short": failure(inspection=dict(inspection, journal_task_id="a" * 31)),
            "journal-uppercase": failure(inspection=dict(inspection, journal_task_id="A" * 32)),
            "journal-integer": failure(inspection=dict(inspection, journal_task_id=1)),
        }
        for label, value in cases.items():
            with self.subTest(case=label), self.assertRaises(gate.AcceptanceFailure):
                self.validate(response("failed", "discovery_failed", failure=value, error=ERROR_TEXT))
        for label, error in (("empty", ""), ("integer", 1), ("null", None)):
            with self.subTest(error=label), self.assertRaises(gate.AcceptanceFailure):
                self.validate(response("failed", "discovery_failed", failure=failure(), error=error))

    def test_failure_and_error_are_paired_and_only_on_failure_statuses(self):
        for label, fields in (("failure-only", {"failure": failure()}), ("error-only", {"error": ERROR_TEXT})):
            for status, reason in FAILURE_STATUSES:
                with self.subTest(case=label, status=status), \
                        self.assertRaisesRegex(gate.AcceptanceFailure, "appear together"):
                    self.validate(response(status, reason, **fields))
        for status, reason in (("available", "ok"), ("partial", "partial_catalog")):
            with self.subTest(status=status), \
                    self.assertRaisesRegex(gate.AcceptanceFailure, "failed, unavailable or blocked"):
                self.validate(response(status, reason, failure=failure(), error=ERROR_TEXT))
        with self.assertRaisesRegex(gate.AcceptanceFailure, "unknown or missing fields"):
            self.validate(response("failed", "discovery_failed", failure=failure(), error=ERROR_TEXT,
                                   diagnostics=[]))

    def test_summary_statically_excludes_the_diagnostic_fields(self):
        tree = ast.parse(inspect.getsource(provider_policy.validate_model_discovery_response))
        returns = [node.value for node in ast.walk(tree) if isinstance(node, ast.Return)]
        self.assertEqual(len(returns), 1)
        summary = returns[0]
        self.assertIsInstance(summary, ast.Dict)
        self.assertEqual({key.value for key in summary.keys},
                         {"status", "reason_code", "complete", "source", "model_count", "effort_count"})
        constants = {node.value for node in ast.walk(summary) if isinstance(node, ast.Constant)}
        names = {node.id for node in ast.walk(summary) if isinstance(node, ast.Name)}
        self.assertFalse({"failure", "error"} & (constants | names))


class ObservedExitPolicyTests(unittest.TestCase):
    def test_allowed_exits_keep_the_verdict_and_admit_typed_invalid_requests(self):
        allowed = provider_policy.model_discovery_allowed_exit_codes
        self.assertEqual(allowed(response("failed", "discovery_failed")), {1})
        self.assertEqual(allowed(response("failed", "discovery_failed", failure=failure(), error=ERROR_TEXT)),
                         {1, 2})
        for status, reason in (("blocked", "supervisor_upgrade_required"),
                               ("unavailable", "discovery_unavailable")):
            with self.subTest(status=status):
                self.assertEqual(allowed(response(status, reason)), {2})
                self.assertEqual(allowed(response(status, reason, failure=failure(), error=ERROR_TEXT)), {2})
        for status, reason in (("available", "ok"), ("partial", "partial_catalog")):
            with self.subTest(status=status):
                self.assertEqual(allowed(response(status, reason)), {0})
        with self.assertRaises(gate.AcceptanceFailure):
            allowed(response("failed", "discovery_failed", failure=failure(cause="raw"), error=ERROR_TEXT))
        with self.assertRaises(gate.AcceptanceFailure):
            allowed(response("failed", "discovery_failed", failure=failure()))


class DiscoveryExitConsumerTests(unittest.TestCase):
    """The real discovery, scenario and failure-receipt owners; only the CLI is stubbed."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-model-exit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.processes = 0

    def operation(self, value: dict[str, object], exit_code: object, *,
                  discover_only: bool) -> gate.NativeAcceptance:
        self.processes += 1
        directory = self.root / f"process-{self.processes}"
        directory.mkdir()
        (directory / "stdout").write_text(json.dumps(value) + "\n")
        output, workspace = directory / "output", directory / "workspace"
        output.mkdir()
        workspace.mkdir()
        operation = object.__new__(gate.NativeAcceptance)
        operation.provider, operation.mode, operation.scenario = PROVIDER, "read-only", "read-only"
        operation.args = SimpleNamespace(model=None, effort=None)
        operation.discover_models_only = discover_only
        operation.delegate, operation.config = Path("/delegate"), Path("/pueue.yml")
        operation.ops = SimpleNamespace(managed_runner=True)
        operation.base, operation.output, operation.workspace = directory, output, workspace
        operation.models_state = directory / "models-state"
        operation.processes = SimpleNamespace(run=Mock(return_value=SimpleNamespace(
            directory=directory, result={"exit_code": exit_code})))
        operation.setup = Mock()
        operation.closed, operation.lifetime_closure = False, None
        operation.shutdown = lambda: setattr(operation, "closed", True)
        operation.native_mode_receipt = lambda: None
        operation.success_candidate = None
        operation.brief = Mock(return_value=workspace / "brief")
        operation.dispatch = Mock(side_effect=AssertionError("paid task must not start"))
        return operation

    def test_failed_status_owns_the_verdict_while_the_observed_exit_is_recorded(self):
        typed = {"failure": failure(code="invalid_request", stage="validate-request",
                                    next_action="correct_request"), "error": ERROR_TEXT}
        for label, fields, observed in (("provider", {}, 1), ("typed-provider", typed, 1),
                                        ("invalid-request", typed, 2)):
            with self.subTest(case=label):
                operation = self.operation(response("failed", "discovery_failed", **fields), observed,
                                           discover_only=True)
                # A negative catalog belongs to the entry's failure path, never the success shutdown.
                operation.shutdown = Mock(side_effect=AssertionError("success shutdown on a negative catalog"))
                self.assertEqual(operation.run(), 1)
                self.assertEqual(operation.model_discovery_exit_code, observed)
                self.assertIsNone(operation.success_receipt)
                operation.dispatch.assert_not_called()

    def test_wrong_status_and_exit_pairs_are_refused(self):
        typed = {"failure": failure(), "error": ERROR_TEXT}
        for status, reason, fields, observed in (
                ("failed", "discovery_failed", {}, 0), ("failed", "discovery_failed", typed, 0),
                ("failed", "discovery_failed", {}, 2), ("failed", "discovery_failed", typed, True),
                ("blocked", "supervisor_upgrade_required", typed, 1),
                ("unavailable", "discovery_unavailable", typed, 0),
                ("unavailable", "interface_unavailable", {}, 1),
                ("available", "ok", {}, 2), ("partial", "partial_catalog", {}, 1)):
            with self.subTest(status=status, fields=bool(fields), observed=observed):
                operation = self.operation(response(status, reason, **fields), observed, discover_only=True)
                with self.assertRaisesRegex(gate.AcceptanceFailure, "exit status does not match"):
                    operation.run()
                self.assertIsNone(operation.model_discovery_exit_code)
                operation.dispatch.assert_not_called()

    def test_main_failure_receipt_keeps_the_observed_exit_and_status_verdict(self):
        typed = {"failure": failure(code="supervisor_upgrade_required", next_action="run_supervisor_migrate",
                                    recovery_command=RECOVERY), "error": ERROR_TEXT}
        for status, reason, observed, code, verdict in (
                ("failed", "discovery_failed", 2, 1, "failed"),
                ("blocked", "supervisor_upgrade_required", 2, 2, "BLOCKED")):
            with self.subTest(status=status):
                operation = self.operation(response(status, reason, **typed), observed, discover_only=True)
                operation.finalize = Mock(return_value=True)
                with patch.object(entry, "choose_output", return_value=operation.output), \
                        patch.object(entry, "NativeAcceptance", return_value=operation):
                    self.assertEqual(entry.main([
                        "--provider", PROVIDER, "--permission", "read-only", "--discover-models",
                        "--output", str(operation.output)]), code)
                operation.finalize.assert_called_once_with(False, blocked=code == 2)
                self.assertFalse((operation.output / "success.json").exists())
                data = (operation.output / "failure.json").read_text()
                receipt = json.loads(data)
                self.assertEqual((receipt["status"], receipt["discovery_exit_code"]), (verdict, observed))
                self.assertEqual(set(receipt["model_discovery"]),
                                 {"status", "reason_code", "complete", "model_count", "effort_count"})
                for text in (ERROR_TEXT, RECOVERY):
                    self.assertNotIn(text, data)
                operation.dispatch.assert_not_called()

    def test_pi_typed_operational_failures_stop_before_dispatch(self):
        for status, reason, observed, expected in (
                ("unavailable", "discovery_unavailable", 2, "operational failure"),
                ("failed", "discovery_failed", 2, "default-model discovery failed")):
            with self.subTest(status=status):
                operation = self.operation(response(status, reason, failure=failure(), error=ERROR_TEXT),
                                           observed, discover_only=False)
                with self.assertRaisesRegex(gate.AcceptanceFailure, expected) as raised:
                    operation.run()
                self.assertNotIsInstance(raised.exception, gate.BlockedFailure)
                self.assertNotIn(ERROR_TEXT, str(raised.exception))
                self.assertEqual(operation.model_discovery_exit_code, observed)
                operation.dispatch.assert_not_called()

    def test_pi_advisory_unavailable_readiness_still_proceeds(self):
        operation = self.operation(response("unavailable", "interface_unavailable"), 2, discover_only=False)
        operation.require_default_model_prerequisite()
        self.assertEqual(operation.model_discovery_exit_code, 2)
        operation.processes.run.assert_called_once()


class PiDiscoveryUnavailableTests(unittest.TestCase):
    def test_pi_discovery_unavailable_stops_before_any_dispatch(self):
        with tempfile.TemporaryDirectory(prefix="native-pi-discovery-unavailable-") as directory:
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = PROVIDER
            operation.mode = "read-only"
            operation.scenario = "read-only"
            operation.args = SimpleNamespace(model=None)
            operation.discover_models_only = False
            operation.setup = Mock()
            operation.workspace = Path(directory)
            operation.brief = Mock(return_value=Path(directory) / "brief")
            operation.dispatch = Mock(side_effect=AssertionError("paid task must not start"))
            operation.discover_models = Mock()
            operation.model_discovery = response(
                "unavailable", "discovery_unavailable", failure=failure(), error=ERROR_TEXT)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "operational failure") as raised:
                operation.run()
            self.assertNotIsInstance(raised.exception, gate.BlockedFailure)
            self.assertNotIn(ERROR_TEXT, str(raised.exception))
            operation.discover_models.assert_called_once()
            operation.dispatch.assert_not_called()


if __name__ == "__main__":
    unittest.main()
