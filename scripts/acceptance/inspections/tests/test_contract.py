"""Hermetic inspection queue and journal checks for the native acceptance harness."""

from __future__ import annotations

from copy import deepcopy
from pathlib import Path
from types import SimpleNamespace
import tempfile
from typing import TypedDict
import unittest
from unittest.mock import Mock, patch
import sys


from scripts.acceptance.inspections import cli as inspection_acceptance

repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.evidence import digests as evidence_digests
from scripts.acceptance.evidence import encoding as evidence_encoding
from scripts.acceptance.evidence import limits as evidence_limits
from scripts.acceptance.evidence import records as evidence_records
from scripts.acceptance.supervisor import binding as supervisor_binding
from scripts.acceptance.tasks import runtime_metadata as tasks_runtime_metadata
from scripts.acceptance.tasks import specification as tasks_specification

from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.files import read_json
from scripts.acceptance.evidence.files import write_json


ROOT = "a" * 32
TASK = "b" * 32


from scripts.acceptance.inspections.tests.fixtures import InspectionFixture


class RuntimeEvidenceOptions(TypedDict, total=False):
    started_evidence: dict[str, object]
    launch_evidence: tuple[dict[str, object], dict[str, object]]


class InspectionHarnessTests(unittest.TestCase):
    def setUp(self):
        self.fixture = InspectionFixture(self)

    def test_expired_inspection_response_requires_bounded_failure_context(self):
        response: dict[str, object] = {
            "task_id": TASK,
            "root_id": ROOT,
            "task_record": "unknown",
            "admission": "unknown",
            "liveness": "undetermined",
            "publication": "unknown",
            "failure": {
                "code": "provider_preflight_failed",
                "stage": "inspect-provider",
                "next_action": "stop_and_report",
                "inspection": {
                    "class": "admission_deadline_expired",
                    "deadline_state": "expired",
                    "journal_task_id": TASK,
                },
            },
            "error": "Delegate could not complete provider preflight.",
        }

        inspection_acceptance.validate_expired_inspection_response(response, 1, ROOT, TASK)

        for field, value in (
                ("class", "inspection_worker_failed"),
                ("deadline_state", "open"),
                ("journal_task_id", "c" * 32)):
            with self.subTest(field=field):
                changed = deepcopy(response)
                failure = changed["failure"]
                assert isinstance(failure, dict)
                inspection = failure["inspection"]
                assert isinstance(inspection, dict)
                inspection[field] = value
                with self.assertRaisesRegex(
                        AcceptanceFailure, "stable provider-preflight failure envelope"):
                    inspection_acceptance.validate_expired_inspection_response(
                        changed, 1, ROOT, TASK)

    def test_blocker_failure_cleanup_stops_at_finite_observation_deadline(self):
        acceptance = inspection_acceptance.InspectionAcceptance.__new__(
            inspection_acceptance.InspectionAcceptance)
        acceptance.blocker = (7, "bounded-blocker")
        acceptance.ops = self.fixture.ops
        acceptance.daemon = SimpleNamespace(poll=lambda: None)
        acceptance.queue = Mock(return_value={"tasks": {
            "7": {"label": "bounded-blocker", "status": {"Running": {}}},
        }})
        with patch.object(inspection_acceptance, "failure_cleanup_deadline",
                          return_value=10.0), \
                patch.object(inspection_acceptance, "wait_for_failure_cleanup",
                             return_value=False):
            self.assertFalse(acceptance.cleanup_blocker_after_failure())

    def test_expiry_fixtures_outlast_the_fresh_inspection_deadline(self):
        timeout = inspection_acceptance.FRESH_INSPECTION_TIMEOUT_SECONDS
        self.assertGreater(inspection_acceptance.RUNNING_EXPIRY_DELAY_MS, timeout * 1000)
        self.assertGreater(
            inspection_acceptance.ACCEPTANCE_WATCH_SECONDS,
            inspection_acceptance.RUNNING_EXPIRY_DELAY_MS / 1000,
        )
        self.assertEqual(
            inspection_acceptance.BLOCKER_ARGV,
            ["/bin/sleep", str(timeout + inspection_acceptance.EXPIRY_MARGIN_SECONDS)],
        )

    def test_failed_inspection_retains_unexpected_positive_admission(self):
        acceptance = object.__new__(inspection_acceptance.InspectionAcceptance)
        acceptance.base = Path("/workspace")
        acceptance.configure = Mock(return_value=(TASK, Path("/brief"), Path("/case"), Path("/config")))
        acceptance.argv = Mock(return_value=["delegate"])
        acceptance.expected_inspection_binding = Mock(return_value={"binding": "expected"})
        response: dict[str, object] = {
            "task_id": TASK,
            "root_id": ROOT,
            "task_record": "created",
            "admission": "admitted",
            "liveness": "running",
            "publication": "unknown",
            "supervisor": {"matched": True, "numeric_task_id": 7},
        }
        process = SimpleNamespace(result={"exit_code": 0})
        acceptance.ops = self.fixture.ops
        acceptance.ops.dispatch_attempts.clear()

        with patch.object(inspection_acceptance, "parse_json_output", return_value=response), \
                patch.object(acceptance.ops, "direct", return_value=process), \
                patch.object(acceptance.ops, "expect_inspection"), \
                patch.object(acceptance.ops, "record_admission") as record_admission:
            with self.assertRaisesRegex(AcceptanceFailure, "stable provider-preflight failure envelope"):
                acceptance.failed_inspection("running-expiry", 1)

        record_admission.assert_called_once_with("running-expiry", TASK, response)

    def test_failure_cleanup_records_retention_error_and_still_finalizes(self):
        with tempfile.TemporaryDirectory(prefix="inspection-retention-error-") as directory:
            output = Path(directory) / "evidence"
            output.mkdir()
            acceptance = object.__new__(inspection_acceptance.InspectionAcceptance)
            acceptance.output = output
            acceptance.case_records = {}
            acceptance.state = Path(directory) / "state"
            acceptance.config = Path(directory) / "config"
            acceptance.setup = Mock()
            acceptance.success = Mock(side_effect=RuntimeError("primary inspection failure"))
            acceptance.cleanup_blocker_after_failure = Mock(return_value=True)
            acceptance.finalize = Mock(return_value=True)
            acceptance.ops = self.fixture.ops
            acceptance.ops.closed = False

            with patch.object(acceptance.ops, "retain_failure_ownership",
                              side_effect=RuntimeError("retention failed")), \
                    self.assertRaisesRegex(RuntimeError, "primary inspection failure"):
                acceptance.run()

            acceptance.finalize.assert_called_once_with(False)
            cleanup = read_json(output / "failure-cleanup.json")
            self.assertFalse(cleanup["natural_daemon_shutdown"])
            self.assertTrue(cleanup["artifact_finalization_completed"])
            self.assertIn("retention failed", cleanup["failure_ownership_error"])
            self.assertTrue(cleanup["passed_receipt_absent"])

    def test_finalize_removes_closed_fixture_and_raw_process_captures(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            fixture = output / "fixture"
            cases = output / "cases"
            pueue_parent = root / "pueue"
            pueue = pueue_parent / "inspect-run"
            for path in (fixture, cases, output / "processes", pueue):
                path.mkdir(parents=True)
            owner = DirectoryOwner()
            owner.adopt_created("raw process captures", output / "processes", output)
            owner.adopt_created("case evidence", cases, output)
            owner.adopt_created("fixture", fixture, output)
            tokens = owner.transfer()
            acceptance = object.__new__(inspection_acceptance.InspectionAcceptance)
            acceptance.output = output
            acceptance.base = fixture
            acceptance.cases = cases
            acceptance.owned_directories = tokens
            acceptance.raw_process_captures_token = next(
                token for token in tokens if token.directory == output / "processes")
            pueue_owner = DirectoryOwner()
            pueue_owner.adopt_created("private pueue", pueue, pueue_parent)
            acceptance.pueue_directories = pueue_owner.transfer()
            acceptance.pueue_base = pueue
            acceptance.ops = self.fixture.ops
            acceptance.ops.closed = True

            self.assertTrue(acceptance.finalize(True))

            self.assertFalse(fixture.exists())
            self.assertFalse(cases.exists())
            self.assertFalse(pueue.exists())
            self.assertFalse((output / "processes").exists())

    def test_concurrent_response_accepts_only_known_admission_races(self):
        creation_race: dict[str, object] = {
            "task_id": TASK, "root_id": ROOT, "task_record": "unknown",
            "admission": "unknown", "liveness": "undetermined", "publication": "unknown",
            "failure": {"code": "dispatch_failed", "stage": "create-task-record",
                         "next_action": "stop_and_report"},
            "error": "Delegate could not complete the operation.",
        }
        submission_race: dict[str, object] = {
            "task_id": TASK, "root_id": ROOT, "task_record": "created",
            "admission": "unknown", "liveness": "undetermined", "publication": "unknown",
            "failure": {"code": "task_submission_preparation_failed",
                         "stage": "prepare-task-submission", "next_action": "check_status"},
            "error": ("Delegate created the task record but could not prepare it for submission. "
                      "Check its status before taking further action."),
        }
        for response in (creation_race, submission_race):
            with self.subTest(response=response):
                self.assertEqual(
                    inspection_acceptance.validate_concurrent_response(response, 1, TASK), ROOT)

    def test_concurrent_response_rejects_unbounded_failure(self):
        response: dict[str, object] = {
            "task_id": TASK, "root_id": ROOT, "task_record": "unknown",
            "admission": "unknown", "liveness": "undetermined", "publication": "unknown",
            "failure": {"code": "provider_preparation_failed", "stage": "prepare-provider",
                         "next_action": "stop_and_report"},
            "error": "Delegate could not complete provider preflight.",
        }
        with self.assertRaisesRegex(AcceptanceFailure, "known task admission race envelope"):
            inspection_acceptance.validate_concurrent_response(response, 1, TASK)

    def test_observed_pueue_version_accepts_any_bounded_nonempty_text(self):
        for output in ("5.0.0\n", "pueue 5.0.0\n", "future-release-preview\n"):
            with self.subTest(output=output):
                self.assertEqual(
                    supervisor_binding.observed_pueue_version(output, "pueue"), output.strip())

        for output in ("", "\n", "release\x00candidate\n"):
            with self.subTest(output=output):
                with self.assertRaises(AcceptanceFailure):
                    supervisor_binding.observed_pueue_version(output, "pueue")

        with self.assertRaises(AcceptanceFailure):
            supervisor_binding.observed_pueue_version(
                b" " * evidence_limits.MAX_CONTROL_BYTES + b"version", "pueue")

    def test_runtime_capability_digest_encodes_empty_slices_like_go(self):
        definition: dict[str, object] = {
            "revision": "runtime-capability-v1",
            "directory": "/workspace",
            "environment": [],
            "output_limit": 1024,
            "runtime": {"help_args": [], "required_flags": []},
        }
        expected = evidence_digests.sha(evidence_encoding.canonical_go_json({
            "revision": definition["revision"],
            "directory": definition["directory"],
            "environment": None,
            "output_limit": definition["output_limit"],
            "help_args": None,
            "required_flags": None,
        }))
        self.assertEqual(tasks_runtime_metadata.runtime_capability_sha(definition), expected)

    def test_runtime_capability_digest_preserves_go_version_args_order_and_omission(self):
        runtime: dict[str, object] = {"help_args": ["--help"], "required_flags": ["--json"]}
        definition: dict[str, object] = {
            "revision": "runtime-capability-v1", "directory": "/workspace",
            "environment": [], "output_limit": 1024,
            "runtime": runtime,
        }
        # runtimeCapabilityContract in internal/provider/runtime_facts.go:
        # version_args is between output_limit and help_args, with omitempty.
        prefix = (b'{"revision":"runtime-capability-v1","directory":"/workspace",'
                  b'"environment":null,"output_limit":1024,')
        suffix = b'"help_args":["--help"],"required_flags":["--json"]}\n'
        cases = ((None, b''), ([], b''),
                 (["--version"], b'"version_args":["--version"],'),
                 (["version", "--json"], b'"version_args":["version","--json"],'))
        for version_args, field in cases:
            with self.subTest(version_args=version_args):
                runtime["version_args"] = version_args
                self.assertEqual(tasks_runtime_metadata.runtime_capability_sha(definition),
                                 evidence_digests.sha(prefix + field + suffix))
        del runtime["version_args"]
        self.assertEqual(tasks_runtime_metadata.runtime_capability_sha(definition),
                         evidence_digests.sha(prefix + suffix))

    def test_review_target_is_part_of_the_canonical_task_record(self):
        task: dict[str, object] = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "provider": "codex:review", "mode": "read-only",
            "canonical_cwd": "/workspace",
            "requested_config": {
                "permission": "read-only", "budget": "2m0s",
                "review_target": {"kind": "uncommitted"},
            },
            "budget_nanos": 120_000_000_000,
            "brief_sha256": "c" * 64, "brief_length": 31,
        }
        encoded = tasks_specification._canonical_task_bytes(task)
        self.assertIn(b'"review_target":{"kind":"uncommitted"}', encoded)
        self.assertEqual(
            tasks_specification._validate_task_record(task, ROOT, TASK, "review task"),
            evidence_digests.sha(encoded),
        )
        invalid = deepcopy(task)
        requested_config = invalid["requested_config"]
        assert isinstance(requested_config, dict)
        requested_config["review_target"] = {
            "kind": "uncommitted", "value": "main",
        }
        with self.assertRaises(AcceptanceFailure):
            tasks_specification._validate_task_record(invalid, ROOT, TASK, "review task")

    def test_bounded_text_rejects_raw_output_before_trimming(self):
        path = self.fixture.base / "version"
        path.write_bytes(b" " * evidence_limits.MAX_CONTROL_BYTES + b"version")
        with self.assertRaises(AcceptanceFailure):
            evidence_records.bounded_text(path, "version output")

    def test_provider_runtime_metadata_accepts_pending_admission_contract(self):
        meta: dict[str, object] = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }

        self.assertIsNone(tasks_runtime_metadata.validate_provider_runtime_metadata(meta))

    def test_ordinary_task_evidence_accepts_pending_runtime_metadata(self):
        directory = self.fixture.ordinary_only_admission(runtime_pending=True)

        ordinary = self.fixture.ops._durable_ordinary_admission(TASK, None)

        assert ordinary is not None
        self.assertEqual(ordinary["task_id"], TASK)
        self.assertEqual(
            read_json(directory / "meta.json")["provider_version"], "")

    def test_ordinary_task_evidence_accepts_unobserved_version_without_pending_marker(self):
        directory = self.fixture.ordinary_only_admission()
        meta = read_json(directory / "meta.json")
        meta["provider_version"] = ""
        write_json(directory / "meta.json", meta, replace=True)
        self.fixture.rebind_ordinary_metadata(directory)

        ordinary = self.fixture.ops._durable_ordinary_admission(TASK, None)

        assert ordinary is not None
        self.assertEqual(ordinary["task_id"], TASK)

    def test_ordinary_task_evidence_rejects_pending_runtime_digest(self):
        directory = self.fixture.ordinary_only_admission(runtime_pending=True)
        meta = read_json(directory / "meta.json")
        meta["runtime_preflight_sha256"] = "invalid"
        write_json(directory / "meta.json", meta, replace=True)
        self.fixture.rebind_ordinary_metadata(directory)

        with self.assertRaisesRegex(AcceptanceFailure, "preflight digest is invalid"):
            self.fixture.ops._durable_ordinary_admission(TASK, None)

    def test_ordinary_task_evidence_rejects_pending_runtime_version(self):
        directory = self.fixture.ordinary_only_admission(runtime_pending=True)
        meta = read_json(directory / "meta.json")
        meta["provider_version"] = "fixture-v2"
        write_json(directory / "meta.json", meta, replace=True)
        self.fixture.rebind_ordinary_metadata(directory)

        with self.assertRaisesRegex(AcceptanceFailure, "pending version is not empty"):
            self.fixture.ops._durable_ordinary_admission(TASK, None)

    def test_ordinary_task_evidence_rejects_pending_environment_marker(self):
        directory = self.fixture.ordinary_only_admission(runtime_pending=True)
        meta = read_json(directory / "meta.json")
        meta["environment_recorded"] = False
        write_json(directory / "meta.json", meta, replace=True)
        self.fixture.rebind_ordinary_metadata(directory)

        with self.assertRaisesRegex(AcceptanceFailure, "pending environment is not recorded"):
            self.fixture.ops._durable_ordinary_admission(TASK, None)

    def test_provider_runtime_metadata_rejects_invalid_pending_contract(self):
        base: dict[str, object] = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }
        cases = (
            ("missing version", "provider_version", None, True),
            ("null version", "provider_version", None, False),
            ("invalid marker", "runtime_preflight_sha256", "invalid", False),
            ("null marker", "runtime_preflight_sha256", None, False),
            ("observed version", "provider_version", "fixture-v1", False),
            ("missing environment marker", "environment_recorded", None, True),
            ("false environment marker", "environment_recorded", False, False),
        )
        for name, key, value, remove in cases:
            with self.subTest(name=name):
                changed = dict(base)
                if remove:
                    changed.pop(key, None)
                else:
                    changed[key] = value
                with self.assertRaises(AcceptanceFailure):
                    tasks_runtime_metadata.validate_provider_runtime_metadata(changed)

    # The historical test ID is part of the relocation contract.
    def test_provider_runtime_metadata_keeps_historical_version_equality(self):
        """Legacy observed_version calls preserve authoritative historical metadata."""
        meta: dict[str, object] = {
            "provider_executable": "/fixture/provider",
            "provider_version": "fixture-v1",
        }

        tasks_runtime_metadata.validate_provider_runtime_metadata(
            meta, observed_version="fixture-v1")
        self.assertIsNone(tasks_runtime_metadata.validate_provider_runtime_metadata(
            meta, observed_version="fixture-v2"))
        self.assertEqual(meta["provider_version"], "fixture-v1")

    def test_provider_runtime_metadata_requires_a_string_version_but_allows_empty(self):
        meta: dict[str, object] = {"provider_executable": "/fixture/provider", "provider_version": ""}
        self.assertIsNone(tasks_runtime_metadata.validate_provider_runtime_metadata(meta))
        del meta["provider_version"]
        with self.assertRaisesRegex(AcceptanceFailure, "version is invalid"):
            tasks_runtime_metadata.validate_provider_runtime_metadata(meta)
        for version in (None, False, 123, [], {}, " "):
            with self.subTest(version=version), self.assertRaisesRegex(
                    AcceptanceFailure, "version is invalid"):
                tasks_runtime_metadata.validate_provider_runtime_metadata(
                    dict(meta, provider_version=version))

    def test_provider_runtime_metadata_accepts_upgraded_launch_identity(self):
        meta: dict[str, object] = {"provider_executable": "/fixture/provider", "provider_version": "fixture-v1"}
        started: dict[str, object] = {"provider_executable": "/fixture/provider-v2",
                   "provider_version": "fixture-v2", "provider_sha256": "b" * 64}
        expected: dict[str, object] = {"executable": "/fixture/provider-v2",
                    "version": "fixture-v2", "sha256": "b" * 64}
        self.assertEqual(tasks_runtime_metadata.validate_provider_runtime_metadata(
            meta, started_evidence=started), expected)
        self.assertEqual(tasks_runtime_metadata.validate_provider_runtime_metadata(
            meta, launch_evidence=(started, dict(started))), expected)

    def test_provider_runtime_metadata_accepts_unobserved_launch_diagnostics(self):
        meta: dict[str, object] = {"provider_executable": "/fixture/provider", "provider_version": ""}
        for version in (None, "", "fixture-v2"):
            for digest in (None, "", "b" * 64):
                with self.subTest(version=version, digest=digest):
                    started: dict[str, object] = {"provider_executable": "/fixture/provider-v2"}
                    exited = dict(started, provider_version="", provider_sha256="")
                    if version is not None:
                        started["provider_version"] = version
                        exited["provider_version"] = version
                    if digest is not None:
                        started["provider_sha256"] = digest
                        exited["provider_sha256"] = digest
                    expected: dict[str, object] = {"executable": "/fixture/provider-v2", "version": version or ""}
                    if digest is not None:
                        expected["sha256"] = digest
                    self.assertEqual(tasks_runtime_metadata.validate_provider_runtime_metadata(
                        meta, started_evidence=started), expected)
                    self.assertEqual(tasks_runtime_metadata.validate_provider_runtime_metadata(
                        meta, launch_evidence=(started, exited)), expected)

    def test_provider_runtime_metadata_rejects_malformed_present_diagnostics(self):
        meta: dict[str, object] = {"provider_executable": "/fixture/provider", "provider_version": ""}
        for field, values in (
                ("provider_version", (None, False, 123, [], {}, " ")),
                ("provider_sha256", (None, False, 123, [], {}, "invalid", "B" * 64))):
            for value in values:
                invalid: dict[str, object] = {"provider_executable": "/fixture/provider", field: value}
                valid: dict[str, object] = {"provider_executable": "/fixture/provider"}
                cases: tuple[RuntimeEvidenceOptions, ...] = (
                    {"started_evidence": invalid},
                    {"launch_evidence": (invalid, valid)},
                    {"launch_evidence": (valid, invalid)},
                )
                message = "version is invalid" if field == "provider_version" else "digest is invalid"
                for kwargs in cases:
                    with self.subTest(field=field, value=value, kwargs=kwargs):
                        with self.assertRaisesRegex(AcceptanceFailure, message):
                            tasks_runtime_metadata.validate_provider_runtime_metadata(meta, **kwargs)

    def test_provider_runtime_metadata_requires_clean_executable_with_optional_diagnostics(self):
        meta: dict[str, object] = {"provider_executable": "/fixture/provider", "provider_version": ""}
        valid: dict[str, object] = {"provider_executable": "/fixture/provider"}
        for executable in (None, "", "provider", "/fixture//provider", "//fixture/provider"):
            invalid: dict[str, object] = {"provider_executable": executable}
            cases: tuple[RuntimeEvidenceOptions, ...] = (
                {"started_evidence": invalid},
                {"launch_evidence": (invalid, valid)},
                {"launch_evidence": (valid, invalid)},
            )
            for kwargs in cases:
                with self.subTest(executable=executable, kwargs=kwargs):
                    with self.assertRaisesRegex(AcceptanceFailure, "executable is invalid"):
                        tasks_runtime_metadata.validate_provider_runtime_metadata(meta, **kwargs)

    def test_provider_runtime_metadata_rejects_actual_launch_identity_mismatch(self):
        meta: dict[str, object] = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }
        started: dict[str, object] = {
            "provider_executable": "/fixture/provider",
            "provider_version": "fixture-v2",
            "provider_sha256": "b" * 64,
        }
        for field, value in (("provider_executable", "/fixture/provider-v3"),
                             ("provider_version", "fixture-v3"), ("provider_version", "")):
            with self.subTest(field=field, value=value):
                exited = dict(started, **{field: value})
                with self.assertRaisesRegex(AcceptanceFailure, "runtime identities differ"):
                    tasks_runtime_metadata.validate_provider_runtime_metadata(
                        meta, launch_evidence=(started, exited))

    def test_historical_program_sha_remains_data_without_comparison_or_mutation(self):
        meta: dict[str, object] = {"provider_executable": "/fixture/provider", "provider_version": ""}
        started: dict[str, object] = {"provider_executable": "/fixture/provider", "provider_version": "fixture-v2",
                   "provider_sha256": "b" * 64}
        for historical in (None, "", "c" * 64):
            exited: dict[str, object] = {"provider_executable": "/fixture/provider", "provider_version": "fixture-v2"}
            if historical is not None:
                exited["provider_sha256"] = historical
            before = deepcopy((started, exited))
            self.assertEqual(tasks_runtime_metadata.validate_provider_runtime_metadata(
                meta, launch_evidence=(started, exited)), {
                    "executable": "/fixture/provider", "version": "fixture-v2", "sha256": "b" * 64})
            self.assertEqual((started, exited), before)

    def test_provider_runtime_metadata_accepts_started_only_interrupted_identity(self):
        meta: dict[str, object] = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }
        started: dict[str, object] = {
            "provider_executable": "/fixture/provider-v2",
            "provider_version": "fixture-v2",
            "provider_sha256": "b" * 64,
        }

        self.assertEqual(
            tasks_runtime_metadata.validate_provider_runtime_metadata(
                meta, started_evidence=started),
            {"executable": "/fixture/provider-v2",
             "version": "fixture-v2", "sha256": "b" * 64},
        )
        with self.assertRaisesRegex(AcceptanceFailure, "mutually exclusive"):
            tasks_runtime_metadata.validate_provider_runtime_metadata(
                meta, launch_evidence=(started, started), started_evidence=started)

    def test_provider_runtime_evidence_matches_go_identity_contract_for_both_modes(self):
        meta: dict[str, object] = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }
        valid: dict[str, object] = {
            "provider_executable": "/fixture/provider",
            "provider_version": " fixture-v2 ",
            "provider_sha256": "b" * 64,
        }
        for mode in ("started", "sealed"):
            with self.subTest(mode=mode):
                kwargs: RuntimeEvidenceOptions = ({"started_evidence": valid} if mode == "started" else
                          {"launch_evidence": (valid, dict(valid))})
                self.assertEqual(
                    tasks_runtime_metadata.validate_provider_runtime_metadata(meta, **kwargs),
                    {"executable": "/fixture/provider", "version": " fixture-v2 ",
                     "sha256": "b" * 64},
                )
                for name, value in (
                        ("raw path", "/fixture//provider"),
                        ("control", "fixture\nv2"),
                        ("invalid utf8", "fixture\ud800v2"),
                        ("oversized", "v" * (tasks_runtime_metadata.MAX_PROVIDER_RUNTIME_EVIDENCE_VERSION_BYTES + 1))):
                    with self.subTest(case=name):
                        changed = dict(valid)
                        if name == "raw path":
                            changed["provider_executable"] = value
                        else:
                            changed["provider_version"] = value
                        changed_kwargs: RuntimeEvidenceOptions = ({"started_evidence": changed} if mode == "started" else
                                          {"launch_evidence": (changed, dict(changed))})
                        with self.assertRaises(AcceptanceFailure):
                            tasks_runtime_metadata.validate_provider_runtime_metadata(
                                meta, **changed_kwargs)

    def test_provider_runtime_metadata_rejects_noncanonical_executable_path(self):
        for executable in ("/fixture//provider", "//fixture/provider"):
            with self.subTest(executable=executable), self.assertRaisesRegex(
                    AcceptanceFailure, "executable is invalid"):
                tasks_runtime_metadata.validate_provider_runtime_metadata({
                    "provider_executable": executable,
                    "provider_version": "fixture-v1",
                })


if __name__ == "__main__":
    unittest.main()
