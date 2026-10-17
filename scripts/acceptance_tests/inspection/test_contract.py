"""Hermetic inspection queue and journal checks for the native acceptance harness."""

from __future__ import annotations

from copy import deepcopy
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import Mock, patch

from scripts import acceptance_inspection as inspection_acceptance
from scripts import acceptance_provider_common as provider_common
from scripts.acceptance_provider_common import AcceptanceFailure
from scripts.acceptance_supervisor_common import DirectoryOwner, read_json, write_json


ROOT = "a" * 32
TASK = "b" * 32


from .fixtures import InspectionFixture


class InspectionHarnessTests(unittest.TestCase):
    def setUp(self):
        self.fixture = InspectionFixture(self)

    def test_expired_inspection_response_requires_bounded_failure_context(self):
        response = {
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
                changed["failure"]["inspection"][field] = value
                with self.assertRaisesRegex(
                        AcceptanceFailure, "stable provider-preflight failure envelope"):
                    inspection_acceptance.validate_expired_inspection_response(
                        changed, 1, ROOT, TASK)

    def test_blocker_failure_cleanup_stops_at_finite_observation_deadline(self):
        acceptance = inspection_acceptance.InspectionAcceptance.__new__(
            inspection_acceptance.InspectionAcceptance)
        acceptance.blocker = (7, "bounded-blocker")
        acceptance.ops = SimpleNamespace(root_id=ROOT)
        acceptance.daemon = SimpleNamespace(poll=lambda: None)
        acceptance.queue = lambda: {"tasks": {
            "7": {"label": "bounded-blocker", "status": {"Running": {}}},
        }}
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
        response = {
            "task_id": TASK,
            "root_id": ROOT,
            "task_record": "created",
            "admission": "admitted",
            "liveness": "running",
            "publication": "unknown",
            "supervisor": {"matched": True, "numeric_task_id": 7},
        }
        process = SimpleNamespace(result={"exit_code": 0})
        acceptance.ops = SimpleNamespace(
            dispatch_attempts=set(),
            direct=Mock(return_value=process),
            expect_inspection=Mock(),
            record_admission=Mock(),
            root_id=ROOT,
            watch_seconds=150,
        )

        with patch.object(inspection_acceptance, "parse_json_output", return_value=response):
            with self.assertRaisesRegex(AcceptanceFailure, "stable provider-preflight failure envelope"):
                acceptance.failed_inspection("running-expiry", 1)

        acceptance.ops.record_admission.assert_called_once_with("running-expiry", TASK, response)

    def test_failure_cleanup_records_retention_error_and_still_finalizes(self):
        with tempfile.TemporaryDirectory(prefix="inspection-retention-error-") as directory:
            output = Path(directory) / "evidence"
            output.mkdir()
            acceptance = object.__new__(inspection_acceptance.InspectionAcceptance)
            acceptance.output = output
            acceptance.case_records = {}
            acceptance.state = Path(directory) / "state"
            acceptance.config = Path(directory) / "config"
            acceptance.setup = lambda: None

            def fail_success():
                raise RuntimeError("primary inspection failure")

            acceptance.success = fail_success
            acceptance.cleanup_blocker_after_failure = Mock(return_value=True)
            acceptance.finalize = Mock(return_value=True)
            acceptance.ops = SimpleNamespace(
                closed=False,
                retain_failure_ownership=Mock(side_effect=RuntimeError("retention failed")),
            )

            with self.assertRaisesRegex(RuntimeError, "primary inspection failure"):
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
            acceptance.ops = SimpleNamespace(closed=True)

            self.assertTrue(acceptance.finalize(True))

            self.assertFalse(fixture.exists())
            self.assertFalse(cases.exists())
            self.assertFalse(pueue.exists())
            self.assertFalse((output / "processes").exists())

    def test_concurrent_response_accepts_only_known_admission_races(self):
        creation_race = {
            "task_id": TASK, "root_id": ROOT, "task_record": "unknown",
            "admission": "unknown", "liveness": "undetermined", "publication": "unknown",
            "failure": {"code": "dispatch_failed", "stage": "create-task-record",
                         "next_action": "stop_and_report"},
            "error": "Delegate could not complete dispatch.",
        }
        submission_race = {
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
        response = {
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
                    provider_common.observed_pueue_version(output, "pueue"), output.strip())

        for output in ("", "\n", "release\x00candidate\n"):
            with self.subTest(output=output):
                with self.assertRaises(AcceptanceFailure):
                    provider_common.observed_pueue_version(output, "pueue")

        with self.assertRaises(AcceptanceFailure):
            provider_common.observed_pueue_version(
                b" " * provider_common.MAX_CONTROL_BYTES + b"version", "pueue")

    def test_runtime_capability_digest_encodes_empty_slices_like_go(self):
        definition = {
            "revision": "runtime-capability-v1",
            "directory": "/workspace",
            "environment": [],
            "output_limit": 1024,
            "runtime": {"help_args": [], "required_flags": []},
        }
        expected = provider_common.sha(provider_common.canonical_go_json({
            "revision": definition["revision"],
            "directory": definition["directory"],
            "environment": None,
            "output_limit": definition["output_limit"],
            "help_args": None,
            "required_flags": None,
        }))
        self.assertEqual(provider_common.runtime_capability_sha(definition), expected)

    def test_review_target_is_part_of_the_canonical_task_record(self):
        task = {
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
        encoded = provider_common._canonical_task_bytes(task)
        self.assertIn(b'"review_target":{"kind":"uncommitted"}', encoded)
        self.assertEqual(
            provider_common._validate_task_record(task, ROOT, TASK, "review task"),
            provider_common.sha(encoded),
        )
        invalid = deepcopy(task)
        invalid["requested_config"]["review_target"] = {
            "kind": "uncommitted", "value": "main",
        }
        with self.assertRaises(AcceptanceFailure):
            provider_common._validate_task_record(invalid, ROOT, TASK, "review task")

    def test_bounded_text_rejects_raw_output_before_trimming(self):
        path = self.fixture.base / "version"
        path.write_bytes(b" " * provider_common.MAX_CONTROL_BYTES + b"version")
        with self.assertRaises(AcceptanceFailure):
            provider_common.bounded_text(path, "version output")

    def test_provider_runtime_metadata_accepts_pending_admission_contract(self):
        meta = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }

        self.assertIsNone(provider_common.validate_provider_runtime_metadata(meta))

    def test_ordinary_task_evidence_accepts_pending_runtime_metadata(self):
        directory = self.fixture.ordinary_only_admission(runtime_pending=True)

        ordinary = self.fixture.ops._durable_ordinary_admission(TASK, None)

        self.assertEqual(ordinary["task_id"], TASK)
        self.assertEqual(
            read_json(directory / "meta.json")["provider_version"], "")

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
        base = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }
        cases = (
            ("missing marker", "runtime_preflight_sha256", None, True),
            ("invalid marker", "runtime_preflight_sha256", "invalid", False),
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
                    provider_common.validate_provider_runtime_metadata(changed)

    def test_provider_runtime_metadata_keeps_historical_version_equality(self):
        meta = {
            "provider_executable": "/fixture/provider",
            "provider_version": "fixture-v1",
        }

        provider_common.validate_provider_runtime_metadata(
            meta, observed_version="fixture-v1")
        with self.assertRaisesRegex(AcceptanceFailure, "does not match observed runtime"):
            provider_common.validate_provider_runtime_metadata(
                meta, observed_version="fixture-v2")

    def test_provider_runtime_metadata_rejects_actual_launch_identity_mismatch(self):
        meta = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }
        started = {
            "provider_executable": "/fixture/provider",
            "provider_version": "fixture-v2",
            "provider_sha256": "b" * 64,
        }
        exited = dict(started, provider_version="fixture-v3")

        with self.assertRaisesRegex(AcceptanceFailure, "runtime identities differ"):
            provider_common.validate_provider_runtime_metadata(
                meta, launch_evidence=(started, exited))

    def test_provider_runtime_metadata_accepts_started_only_interrupted_identity(self):
        meta = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }
        started = {
            "provider_executable": "/fixture/provider-v2",
            "provider_version": "fixture-v2",
            "provider_sha256": "b" * 64,
        }

        self.assertEqual(
            provider_common.validate_provider_runtime_metadata(
                meta, started_evidence=started),
            {"executable": "/fixture/provider-v2",
             "version": "fixture-v2", "sha256": "b" * 64},
        )
        with self.assertRaisesRegex(AcceptanceFailure, "mutually exclusive"):
            provider_common.validate_provider_runtime_metadata(
                meta, launch_evidence=(started, started), started_evidence=started)

    def test_provider_runtime_evidence_matches_go_identity_contract_for_both_modes(self):
        meta = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }
        valid = {
            "provider_executable": "/fixture/provider",
            "provider_version": " fixture-v2 ",
            "provider_sha256": "b" * 64,
        }
        for mode in ("started", "sealed"):
            with self.subTest(mode=mode):
                kwargs = ({"started_evidence": valid} if mode == "started" else
                          {"launch_evidence": (valid, dict(valid))})
                self.assertEqual(
                    provider_common.validate_provider_runtime_metadata(meta, **kwargs),
                    {"executable": "/fixture/provider", "version": " fixture-v2 ",
                     "sha256": "b" * 64},
                )
                for name, value in (
                        ("raw path", "/fixture//provider"),
                        ("control", "fixture\nv2"),
                        ("invalid utf8", "fixture\ud800v2"),
                        ("oversized", "v" * (provider_common.MAX_PROVIDER_RUNTIME_EVIDENCE_VERSION_BYTES + 1))):
                    with self.subTest(case=name):
                        changed = dict(valid)
                        if name == "raw path":
                            changed["provider_executable"] = value
                        else:
                            changed["provider_version"] = value
                        changed_kwargs = ({"started_evidence": changed} if mode == "started" else
                                          {"launch_evidence": (changed, dict(changed))})
                        with self.assertRaises(AcceptanceFailure):
                            provider_common.validate_provider_runtime_metadata(
                                meta, **changed_kwargs)

    def test_provider_runtime_metadata_rejects_noncanonical_executable_path(self):
        for executable in ("/fixture//provider", "//fixture/provider"):
            with self.subTest(executable=executable), self.assertRaisesRegex(
                    AcceptanceFailure, "executable is invalid"):
                provider_common.validate_provider_runtime_metadata({
                    "provider_executable": executable,
                    "provider_version": "fixture-v1",
                })


if __name__ == "__main__":
    unittest.main()
