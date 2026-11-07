"""Model discovery accepts Delegate's typed operational failure without echoing it."""

from argparse import Namespace

import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from scripts import acceptance_native as entry
from scripts.acceptance_tests.test_foundation_owners import failing_lstat
from scripts.acceptance_tests.native.support import CapturedCommand, fixture_list, fixture_record, fixture_text
from scripts.delegate_acceptance.native import (
    acceptance as gate,
    provider_policy,
)


PROVIDER = "pi:json"
JOURNAL = "a" * 32
MODELS_ROOT, INSPECTION, ORDINARY = "b" * 32, "d" * 32, "c" * 32
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

    def test_summary_contains_only_the_independent_public_projection(self):
        for status, reason in (("available", "ok"), ("partial", "partial_catalog"), *FAILURE_STATUSES):
            with self.subTest(status=status):
                fields = ({"failure": failure(recovery_command=RECOVERY), "error": ERROR_TEXT}
                          if (status, reason) in FAILURE_STATUSES else {})
                summary = self.validate(response(status, reason, **fields))
                self.assertEqual(summary, {
                    "status": status, "reason_code": reason, "complete": status == "available",
                    "source": "pi --list-models" if status in {"available", "partial"} else "none",
                    "model_count": 1 if status in {"available", "partial"} else 0,
                    "effort_count": None,
                })
                self.assertNotIn(ERROR_TEXT, json.dumps(summary))
                self.assertNotIn(RECOVERY, json.dumps(summary))


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


class DiscoveryAcceptance(gate.NativeAcceptance):
    """The native owner with explicit references to its injected command mocks."""

    command: CapturedCommand
    processes_mock: Mock
    dispatch_mock: Mock


class DiscoveryExitConsumerTests(unittest.TestCase):
    """The real discovery, scenario and failure-receipt owners; only the CLI is stubbed."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-model-exit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.processes = 0

    def operation(self, value: dict[str, object], exit_code: object, *,
                  discover_only: bool) -> DiscoveryAcceptance:
        self.processes += 1
        directory = self.root / f"process-{self.processes}"
        directory.mkdir()
        (directory / "stdout").write_text(json.dumps(value) + "\n")
        output, workspace = directory / "output", directory / "workspace"
        output.mkdir()
        workspace.mkdir()
        operation = object.__new__(DiscoveryAcceptance)
        operation.provider, operation.mode, operation.scenario = PROVIDER, "read-only", "read-only"
        operation.args = Namespace(model=None, effort=None)
        operation.discover_models_only = discover_only
        operation.delegate, operation.config = Path("/delegate"), Path("/pueue.yml")
        operation.base, operation.output, operation.workspace = directory, output, workspace
        operation.models_state = directory / "models-state"
        # Discovery binds its owned root and helper before launch; the constructor provides both.
        operation.models_state.mkdir()
        operation.provider_executable = directory / "provider"
        operation.provider_executable.write_bytes(b"provider fixture\n")
        operation.command = CapturedCommand(directory, {"exit_code": exit_code})
        operation.processes_mock = Mock()
        operation.processes_mock.run.return_value = operation.command
        process_patch = patch.object(operation, "processes", operation.processes_mock, create=True)
        process_patch.start()
        self.addCleanup(process_patch.stop)
        operation.runner, operation.pueue, operation.state = (
            directory / "runner", directory / "pueue", directory / "state")
        operation.ops = gate.NativeAcceptanceTaskOps(operation)
        operation.setup = Mock()
        operation.closed, operation.lifetime_closure = False, None
        operation.shutdown = Mock(side_effect=lambda: setattr(operation, "closed", True))
        operation.native_mode_receipt = Mock(return_value=None)
        operation.success_candidate = None
        operation.brief = Mock(return_value=workspace / "brief")
        operation.dispatch_mock = Mock(side_effect=AssertionError("paid task must not start"))
        operation.dispatch = operation.dispatch_mock
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
                operation.dispatch_mock.assert_not_called()

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
                operation.dispatch_mock.assert_not_called()

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
                self.assertEqual(set(fixture_record(receipt["model_discovery"])),
                                 {"status", "reason_code", "complete", "model_count", "effort_count"})
                for text in (ERROR_TEXT, RECOVERY):
                    self.assertNotIn(text, data)
                operation.dispatch_mock.assert_not_called()

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
                operation.dispatch_mock.assert_not_called()

    def test_pi_advisory_unavailable_readiness_still_proceeds(self):
        operation = self.operation(response("unavailable", "interface_unavailable"), 2, discover_only=False)
        operation.require_default_model_prerequisite()
        self.assertEqual(operation.model_discovery_exit_code, 2)
        operation.processes_mock.run.assert_called_once()
        # An operation that journaled nothing records no inspection identity.
        self.assertIsNone(operation.model_discovery_task_id)

    # A Pi default-model prerequisite whose stubbed CLI journals its one
    # inspection, as Go does. The real entry, finalizer and diagnostics owners
    # also run; only the ordinary admission and the supervisor shutdown are
    # stubbed. The finalizer persists diagnostics before shutdown, so the
    # inspection identity must already be recorded by then.
    def journal_one_inspection(self, models: Path) -> None:
        # Diagnostics describe the journal's files without decoding them.
        gate.write_json(models / "root.json", {"schema_version": 1, "root_id": MODELS_ROOT})
        journal = models / "inspections" / INSPECTION
        journal.mkdir(parents=True)
        gate.write_json(journal / "request.json", {"schema_version": 1, "task_id": INSPECTION})
        gate.write_json(journal / "result.json", {"schema_version": 1, "reason": "eligible"})

    def journaled(self, value: dict[str, object], exit_code: object, *, journal=None,
                  raised: BaseException | None = None) -> tuple[DiscoveryAcceptance, Path]:
        """The models command calls ``journal`` with its root, then returns or raises ``raised``."""
        operation = self.operation(value, exit_code, discover_only=False)
        process = operation.command
        write_journal = self.journal_one_inspection if journal is None else journal

        def models_command(*_args: object, **_kwargs: object) -> CapturedCommand:
            write_journal(operation.models_state)
            if raised is not None:
                raise raised
            return process

        operation.processes_mock.run.side_effect = models_command
        operation.processes_mock.capture_root = None
        operation.state = self.root / "ordinary-state"
        operation.state.mkdir(exist_ok=True)
        operation.tasks, operation.numeric_ids = {}, {}
        operation.owned_directories = ()
        operation.launch_environment = Mock(return_value={})
        return operation, operation.models_state / "inspections" / INSPECTION

    def admit_failed_ordinary_task(self, operation: DiscoveryAcceptance) -> None:
        def admit(name: str, *_args: object, **_kwargs: object) -> None:
            task = operation.state / "tasks" / ORDINARY
            task.mkdir(parents=True)
            (task / "task.json").write_bytes(b'{"task":"fixture"}\n')
            (task / "meta.json").write_bytes(b'{"meta":"fixture"}\n')
            operation.tasks[name] = ORDINARY
            operation.numeric_ids[ORDINARY] = 7
            raise gate.AcceptanceFailure("fresh provider task failed")

        operation.dispatch_mock = Mock(side_effect=admit)
        operation.dispatch = operation.dispatch_mock

    def failed_main(self, operation: DiscoveryAcceptance) -> dict[str, object]:
        """Run the entry's failure path; return the diagnostics persisted before shutdown."""
        frozen: dict[str, object] = {}

        def safe_shutdown() -> None:
            frozen.update(json.loads((operation.output / "failure-diagnostics.json").read_text()))

        operation.safe_shutdown = Mock(side_effect=safe_shutdown)
        with patch.object(entry, "choose_output", return_value=operation.output), \
                patch.object(entry, "NativeAcceptance", return_value=operation):
            self.assertEqual(entry.main(["--provider", PROVIDER, "--permission", "read-only",
                                         "--output", str(operation.output)]), 1)
        self.assertTrue(frozen, "diagnostics were not persisted before shutdown")
        self.assertIs(fixture_record(frozen["failure"])["discover_models"], False)
        return frozen

    def process_failure(self) -> RuntimeError:
        # The acceptance's Processes registry keeps its default RuntimeError failure type.
        return RuntimeError(f"unexpected exit 3 for {self.root / 'processes' / 'model-discovery'}")

    def assertProcessErrorReported(self, operation: DiscoveryAcceptance, diagnostics: dict[str, object],
                                   primary: BaseException) -> dict[str, object]:
        receipt = json.loads((operation.output / "failure.json").read_text())
        self.assertEqual((receipt["status"], receipt["error"]), ("failed", str(primary)))
        self.assertEqual(fixture_record(fixture_record(diagnostics["failure"])["error"])["bytes"], len(str(primary).encode()))
        operation.dispatch_mock.assert_not_called()
        return receipt

    def assertInspectionBound(self, diagnostics: dict[str, object], journal: Path) -> None:
        self.assertIn("model_discovery_inspection", diagnostics)
        inspection = fixture_record(diagnostics["model_discovery_inspection"])
        self.assertEqual((inspection["journal_task_id"], inspection["journal"]), (INSPECTION, "present"))
        for name in ("request.json", "result.json"):
            self.assertEqual(fixture_record(fixture_record(inspection["files"])[name])["sha256"], gate.digest(journal / name))

    def test_ordinary_failure_after_discovery_binds_the_recorded_inspection(self):
        operation, journal = self.journaled(response("available", "ok"), 0)
        self.admit_failed_ordinary_task(operation)
        diagnostics = self.failed_main(operation)
        self.assertInspectionBound(diagnostics, journal)
        self.assertEqual(diagnostics["task_ids"], [ORDINARY, INSPECTION])
        self.assertEqual([fixture_record(task)["task_id"] for task in fixture_list(diagnostics["tasks"])], [ORDINARY])
        self.assertEqual((operation.models_root_id, operation.model_discovery_task_id), (MODELS_ROOT, INSPECTION))

    def test_response_validation_failure_keeps_the_journaled_inspection_attributable(self):
        operation, journal = self.journaled(response("available", "ok"), 2)
        diagnostics = self.failed_main(operation)
        self.assertInspectionBound(diagnostics, journal)
        self.assertEqual(diagnostics["task_ids"], [INSPECTION])
        self.assertIsNone(operation.model_discovery_exit_code)
        operation.dispatch_mock.assert_not_called()


    def test_process_failure_after_journaling_binds_the_inspection_and_keeps_its_error(self):
        primary = self.process_failure()
        operation, journal = self.journaled(response("available", "ok"), 0, raised=primary)
        diagnostics = self.failed_main(operation)
        self.assertInspectionBound(diagnostics, journal)
        self.assertEqual(diagnostics["task_ids"], [INSPECTION])
        receipt = self.assertProcessErrorReported(operation, diagnostics, primary)
        self.assertNotIn("direct cause", fixture_text(receipt["traceback"]))

    def test_process_failure_without_a_journal_records_no_inspection(self):
        primary = self.process_failure()
        operation, _ = self.journaled(response("available", "ok"), 0, journal=lambda _models: None,
                                      raised=primary)
        diagnostics = self.failed_main(operation)
        self.assertNotIn("model_discovery_inspection", diagnostics)
        self.assertEqual(diagnostics["task_ids"], [])
        self.assertEqual((operation.models_root_id, operation.model_discovery_task_id), (None, None))
        receipt = self.assertProcessErrorReported(operation, diagnostics, primary)
        self.assertNotIn("direct cause", fixture_text(receipt["traceback"]))

    def test_process_failure_with_an_invalid_journal_keeps_its_error_and_names_the_identity_cause(self):
        def two_inspections(models: Path) -> None:
            self.journal_one_inspection(models)
            (models / "inspections" / ("e" * 32)).mkdir()

        def regular_file(models: Path) -> None:
            gate.write_json(models / "root.json", {"schema_version": 1, "root_id": MODELS_ROOT})
            (models / "inspections").write_bytes(b"")

        def dangling_inspections(models: Path) -> None:
            gate.write_json(models / "root.json", {"schema_version": 1, "root_id": MODELS_ROOT})
            (models / "inspections").symlink_to(models / "absent", target_is_directory=True)

        def dangling_root(models: Path) -> None:
            (models / "root.json").symlink_to(models / "absent.json")

        def duplicate_root_keys(models: Path) -> None:
            (models / "root.json").write_bytes(
                ('{"root_id": "%s", "root_id": "%s"}' % (MODELS_ROOT, "e" * 32)).encode())

        def oversized_root(models: Path) -> None:
            (models / "root.json").write_bytes(b'{"pad": "' + b"x" * (1 << 20) + b'"}')

        # The bounded JSON reader refuses duplicates and oversize with its plain RuntimeError.
        for label, journal, cause_type, cause in (
                ("ambiguous", two_inspections, gate.AcceptanceFailure, "inspection journal coverage is not exact"),
                ("regular file", regular_file, gate.AcceptanceFailure, "inspection journal is invalid"),
                ("dangling inspections", dangling_inspections, gate.AcceptanceFailure,
                 "inspection journal is invalid"),
                ("dangling root", dangling_root, gate.AcceptanceFailure, "root record is invalid"),
                ("duplicate root keys", duplicate_root_keys, RuntimeError, "duplicate evidence key"),
                ("oversized root", oversized_root, RuntimeError, "JSON evidence exceeds bound")):
            with self.subTest(journal=label):
                primary = self.process_failure()
                direct, _ = self.journaled(response("available", "ok"), 0, journal=journal, raised=primary)
                with self.assertRaises(RuntimeError) as raised:
                    direct.discover_models()
                self.assertIs(raised.exception, primary)
                self.assertIs(type(primary.__cause__), cause_type)
                self.assertIn(cause, str(primary.__cause__))
                self.assertIsNone(direct.model_discovery_task_id)

                primary = self.process_failure()
                operation, _ = self.journaled(response("available", "ok"), 0, journal=journal, raised=primary)
                diagnostics = self.failed_main(operation)
                self.assertNotIn("model_discovery_inspection", diagnostics)
                self.assertEqual(diagnostics["task_ids"], [])
                self.assertIsNone(operation.model_discovery_task_id)
                receipt = self.assertProcessErrorReported(operation, diagnostics, primary)
                # The entry's traceback projects the identity fault as the process error's explicit cause.
                self.assertIn("direct cause", fixture_text(receipt["traceback"]))
                self.assertIn(cause, fixture_text(receipt["traceback"]))


    def test_process_failure_with_an_uninspectable_inventory_keeps_its_error_and_names_the_fault(self):
        def root_only(models: Path) -> None:
            gate.write_json(models / "root.json", {"schema_version": 1, "root_id": MODELS_ROOT})

        # An unreadable parent denies even an entry the command never created.
        for label, journal, name in (("present root", None, "root.json"),
                                     ("unlisted inspections", root_only, "inspections")):
            with self.subTest(inventory=label):
                denied = PermissionError(13, "injected permission denied")
                primary = self.process_failure()
                direct, _ = self.journaled(response("available", "ok"), 0, journal=journal, raised=primary)
                with failing_lstat(direct.models_state / name, denied), self.assertRaises(RuntimeError) as raised:
                    direct.discover_models()
                self.assertIs(raised.exception, primary)
                self.assertIs(primary.__cause__, denied)
                self.assertIsNone(direct.model_discovery_task_id)

                primary = self.process_failure()
                operation, _ = self.journaled(response("available", "ok"), 0, journal=journal, raised=primary)
                with failing_lstat(operation.models_state / name, denied):
                    diagnostics = self.failed_main(operation)
                self.assertNotIn("model_discovery_inspection", diagnostics)
                self.assertEqual(diagnostics["task_ids"], [])
                receipt = self.assertProcessErrorReported(operation, diagnostics, primary)
                self.assertIn("direct cause", fixture_text(receipt["traceback"]))
                self.assertIn("injected permission denied", fixture_text(receipt["traceback"]))


class PiDiscoveryUnavailableTests(unittest.TestCase):
    def test_pi_discovery_unavailable_stops_before_any_dispatch(self):
        with tempfile.TemporaryDirectory(prefix="native-pi-discovery-unavailable-") as directory:
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = PROVIDER
            operation.mode = "read-only"
            operation.scenario = "read-only"
            operation.args = Namespace(model=None)
            operation.discover_models_only = False
            operation.setup = Mock()
            operation.workspace = Path(directory)
            operation.brief = Mock(return_value=Path(directory) / "brief")
            dispatch = Mock(side_effect=AssertionError("paid task must not start"))
            operation.dispatch = dispatch
            operation.discover_models = Mock()
            operation.model_discovery = response(
                "unavailable", "discovery_unavailable", failure=failure(), error=ERROR_TEXT)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "operational failure") as raised:
                operation.run()
            self.assertNotIsInstance(raised.exception, gate.BlockedFailure)
            self.assertNotIn(ERROR_TEXT, str(raised.exception))
            operation.discover_models.assert_called_once()
            dispatch.assert_not_called()


if __name__ == "__main__":
    unittest.main()
