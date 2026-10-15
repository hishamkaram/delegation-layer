"""Hermetic inspection queue and journal checks for the native acceptance harness."""

from __future__ import annotations

from copy import deepcopy
from pathlib import Path
from types import SimpleNamespace
import shlex
import shutil
import tempfile
import unittest
from unittest.mock import Mock, patch

import acceptance_inspection as inspection_acceptance
import acceptance_provider_common as provider_common
import acceptance_supervisor_common as supervisor_common
from acceptance_provider_common import AcceptanceFailure, NativeTaskOps, _inspection_group
from acceptance_supervisor_common import DirectoryOwner, digest, read_json, write_json


ROOT = "a" * 32
TASK = "b" * 32


class InspectionHarnessTests(unittest.TestCase):
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
        path = self.base / "version"
        path.write_bytes(b" " * provider_common.MAX_CONTROL_BYTES + b"version")
        with self.assertRaises(AcceptanceFailure):
            provider_common.bounded_text(path, "version output")

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.base = Path(self.temporary.name).resolve(strict=True)
        self.state = self.base / "state"
        self.state_parent = self.base / "state-parent"
        self.output = self.base / "output"
        for directory in (self.state, self.state_parent, self.output):
            directory.mkdir(mode=0o700)

        self.runner = self.base / "delegate-run"
        self.pueue = self.base / "pueue"
        self.config = self.base / "pueue.yml"
        self.runner.write_bytes(b"runner")
        self.pueue.write_bytes(b"pueue")
        self.config.write_bytes(b"config")
        self.runner.chmod(0o700)
        self.pueue.chmod(0o700)
        self.config.chmod(0o600)

        self.ops = NativeTaskOps(SimpleNamespace(), self.base / "delegate", self.runner,
                                 self.pueue, self.state_parent, self.state, self.output)
        self.ops.bind_supervisor(self.config)
        self.ops.root_id = ROOT
        self.ops.tasks = {"fresh": TASK}
        self.ops.labels = {"fresh": f"delegate:{ROOT}:{TASK}"}
        self.ops.numbers = {"fresh": 7}
        self.ops.dispatch_attempts = {TASK}
        write_json(self.state / "root.json", {
            "schema_version": 1, "root_id": ROOT, "created_at": "2026-09-15T00:00:00Z",
        })

    def tearDown(self):
        self.temporary.cleanup()

    def inspection_records(self, result: str | None = "eligible",
                           include_ordinary: bool = True,
                           revision: str = "inspection-v1",
                           legacy_model_discovery: bool = False,
                           legacy_admission: bool = False,
                           managed_exit_proof: bool = False,
                           custom_inspection: bool = False) -> Path:
        # Declare the expectation before restoring the seeded dispatch
        # attempt, matching the production pre-dispatch ordering.
        self.ops.dispatch_attempts.discard(TASK)
        directory = self.state / "inspections" / TASK
        directory.mkdir(mode=0o700, parents=True)
        standalone = revision == "model-discovery-v1"
        legacy = standalone and legacy_model_discovery
        brief = b"x" * 31
        task_record = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "provider": "fixture:test", "mode": "read-only",
            "canonical_cwd": str(self.base),
            "requested_config": {"permission": "read-only",
                                  "budget": "1m0s" if legacy else ("5m0s" if standalone else "1m0s")},
            "budget_nanos": 60_000_000_000 if legacy else (300_000_000_000 if standalone else 60_000_000_000),
            "brief_sha256": provider_common.sha(brief), "brief_length": len(brief),
        }
        if include_ordinary:
            ordinary = self.state / "tasks" / TASK
            ordinary.mkdir(mode=0o700, parents=True)
            task_bytes = provider_common._canonical_task_bytes(task_record)
            (ordinary / "task.json").write_bytes(task_bytes)
            (ordinary / "task.json").chmod(0o600)
            (ordinary / "brief.md").write_bytes(brief)
            (ordinary / "brief.md").chmod(0o600)
        request = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "task": task_record,
            "task_sha256": provider_common.sha(provider_common._canonical_task_bytes(task_record)),
            "binding": {
                "definition_revision": revision,
                "definition_sha256": "d" * 64,
                "helper_executable": str(self.base / "helper"),
                "helper_sha256": "e" * 64,
                "worker_executable": str(self.runner),
                "worker_sha256": digest(self.runner),
                "supervisor": provider_common.supervisor_binding(
                    self.pueue, self.config, self.config.parent, digest(self.config),
                    "pueue 4.0.4"),
            },
            "group": _inspection_group(ROOT),
            "label": f"{_inspection_group(ROOT)}-{TASK}",
            "created_at": "2026-09-15T00:00:00Z",
            "deadline": (("2026-09-15T00:01:00Z" if legacy else "2026-09-15T00:05:00Z") if standalone
                         else ("2026-09-15T00:00:20Z" if legacy_admission
                               else "2026-09-15T00:01:00Z")),
        }
        if managed_exit_proof:
            request["binding"].update({
                "runner_ownership": "managed",
                "worker_exit_proof": "guardian-wait-v1",
            })
        elif custom_inspection:
            request["binding"]["runner_ownership"] = "custom"
        write_json(directory / "request.json", request)
        self.ops.expect_inspection(TASK, request["binding"])
        self.ops.dispatch_attempts.add(TASK)
        request_sha = digest(directory / "request.json")
        write_json(directory / "receipt.json", {
            "schema_version": 1, "request_sha256": request_sha, "numeric_task_id": 42,
        })
        write_json(directory / "submission.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "created_at": "2026-09-15T00:00:01Z",
        })
        if result is not None:
            write_json(directory / "start.json", {
                "schema_version": 1, "request_sha256": request_sha,
                "created_at": "2026-09-15T00:00:02Z",
            })
            write_json(directory / "result.json", {
                "schema_version": 1, "request_sha256": request_sha, "reason": result,
                "facts": {"eligible": True} if result == "eligible" else {},
            })
            result_sha = digest(directory / "result.json")
            write_json(directory / "completion.json", {
                "schema_version": 1, "request_sha256": request_sha,
                "result_sha256": result_sha,
                "completed_at": "2026-09-15T00:00:02Z",
                "native_exit": "successful-exit" if result == "eligible" else "unavailable",
            })
            if result == "eligible" and not managed_exit_proof:
                write_json(directory / "worker-observation.json", {
                    "schema_version": 1, "request_sha256": request_sha,
                    "completion_sha256": digest(directory / "completion.json"),
                    "numeric_task_id": 42, "state": "succeeded",
                })
            if managed_exit_proof:
                self.managed_inspection_lifetime(directory, request, request_sha, result)
        return directory

    @staticmethod
    def managed_inspection_lifetime(directory: Path, request: dict[str, object],
                                    request_sha: str, result: str) -> None:
        registration = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "owner_kind": "inspection", "owner_sha256": request_sha,
            "scope_kind": "process-group", "assurance": "cooperative",
            "scope_id": "pg:fixture", "boot_id": "fixture-boot",
            "deadline": request["deadline"], "created_at": "2026-09-15T00:00:03Z",
        }
        write_json(directory / "lifetime-intent.json", {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "owner_kind": "inspection", "owner_sha256": request_sha,
            "scope_kind": "process-group", "assurance": "cooperative",
            "scope_plan": "pg:fixture", "boot_id": "fixture-boot",
            "deadline": request["deadline"], "created_at": "2026-09-15T00:00:02Z",
        })
        write_json(directory / "lifetime-registration.json", registration)
        registration_sha = digest(directory / "lifetime-registration.json")
        worker = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "registration_sha256": registration_sha, "pid": 1001,
            "process_start": "fixture-worker-start", "boot_id": "fixture-boot",
            "started_at": "2026-09-15T00:00:04Z",
        }
        write_json(directory / "lifetime-worker.json", worker)
        worker_sha = digest(directory / "lifetime-worker.json")
        launch = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "registration_sha256": registration_sha, "sequence": 1,
            "pid": 1002, "process_start": "fixture-launch-start", "pgid": 1002,
            "started_at": "2026-09-15T00:00:05Z",
        }
        write_json(directory / "lifetime-launch-00000000000000000001.json", launch)
        launch_set_sha = provider_common.sha(
            provider_common.canonical_go_json([launch]))
        write_json(directory / "lifetime-termination.json", {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "registration_sha256": registration_sha, "launch_count": 1,
            "launch_set_sha256": launch_set_sha, "cause": "completed",
            "scope_empty": True, "observed_at": "2026-09-15T00:00:06Z",
        })
        wait_result = "success" if result == "eligible" else "exit-error"
        wait_exit = 0 if result == "eligible" else 1
        write_json(directory / "lifetime-worker-wait.json", {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "registration_sha256": registration_sha, "worker_sha256": worker_sha,
            "child_pid": 1001, "result": wait_result, "exit_code": wait_exit,
            "observed_at": "2026-09-15T00:00:07Z",
        })

    def durable_ordinary_admission(self, numeric_task_id: int = 7,
                                   runner_ownership: str | None = None,
                                   runtime_pending: bool = False) -> Path:
        directory = self.state / "tasks" / TASK
        request = read_json(self.state / "inspections" / TASK / "request.json")
        supervisor = request["binding"]["supervisor"]
        task_record = read_json(directory / "task.json")
        spec_sha256 = digest(directory / "task.json")
        meta_path = directory / "meta.json"
        meta = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "spec_sha256": spec_sha256,
            "requested_config": task_record["requested_config"],
            "effective_config": {
                "containment": "fixture-local", "approval": "never", "digest": "b" * 64,
            },
            "containment": "fixture-local", "approval": "never",
            "provider_executable": str(self.base / "provider"),
            "provider_version": "fixture-v1", "publisher_build": "fixture-build",
            "publisher_version": "1",
            "predicate": {
                "adapter": task_record["provider"], "mode": task_record["mode"],
                "version": "fixture-v1", "sha256": "d" * 64,
            },
            "supervisor_config": supervisor, "created_at": "2026-09-15T00:00:02Z",
        }
        if runner_ownership is not None:
            self.assertIn(runner_ownership, {"managed", "custom"})
            meta.update({
                "runner_executable": str(self.runner),
                "runner_ownership": runner_ownership,
            })
        if runtime_pending:
            meta.update({
                "provider_version": "",
                "runtime_preflight_sha256": "a" * 64,
                "environment_recorded": True,
            })
        if runner_ownership == "managed":
            self.ops.managed_runner = True
        write_json(meta_path, meta)
        label = f"delegate:{ROOT}:{TASK}"
        meta_sha256 = digest(meta_path)
        write_json(directory / "submit.json", {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "spec_sha256": spec_sha256, "meta_sha256": meta_sha256,
            "label": label, "supervisor": supervisor,
            "created_at": "2026-09-15T00:00:03Z",
        })
        write_json(directory / "supervisor.ref.json", {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "spec_sha256": spec_sha256, "meta_sha256": meta_sha256,
            "numeric_task_id": numeric_task_id, "label": label, **supervisor,
        })
        return directory

    @staticmethod
    def managed_ordinary_lifetime(
            directory: Path, wait_result: str = "success", with_worker: bool = True,
            include_wait: bool = True, termination_cause: str = "completed",
            scope_kind: str = "process-group") -> Path:
        lifetime = directory / "lifetime"
        lifetime.mkdir(mode=0o700)
        assurance = "exact" if scope_kind == "cgroup-v2" else "cooperative"
        owner_sha = provider_common.ordinary_lifetime_owner_digest(
            digest(directory / "task.json"), digest(directory / "meta.json"))
        registration = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "owner_kind": "task", "owner_sha256": owner_sha,
            "scope_kind": scope_kind, "assurance": assurance,
            "scope_id": "scope:ordinary-fixture", "boot_id": "fixture-boot",
            "deadline": "2026-09-15T00:01:00Z", "created_at": "2026-09-15T00:00:05Z",
        }
        write_json(lifetime / "intent.json", {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "owner_kind": "task", "owner_sha256": owner_sha,
            "scope_kind": scope_kind, "assurance": assurance,
            "scope_plan": "plan:ordinary-fixture", "boot_id": "fixture-boot",
            "deadline": "2026-09-15T00:01:00Z", "created_at": "2026-09-15T00:00:04Z",
        })
        write_json(lifetime / "registration.json", registration)
        registration_sha = digest(lifetime / "registration.json")

        worker = None
        worker_sha = ""
        if with_worker:
            worker = {
                "schema_version": 1, "root_id": ROOT, "task_id": TASK,
                "registration_sha256": registration_sha, "pid": 2001,
                "process_start": "fixture-ordinary-worker", "boot_id": "fixture-boot",
                "started_at": "2026-09-15T00:00:06Z",
            }
            write_json(lifetime / "worker.json", worker)
            worker_sha = digest(lifetime / "worker.json")

        launches = []
        if with_worker:
            launch = {
                "schema_version": 1, "root_id": ROOT, "task_id": TASK,
                "registration_sha256": registration_sha, "sequence": 1,
                "pid": 2002, "process_start": "fixture-ordinary-launch", "pgid": 2002,
                "started_at": "2026-09-15T00:00:07Z",
            }
            write_json(lifetime / "00000000000000000001.launch.json", launch)
            launches.append(launch)
        write_json(lifetime / "terminated.json", {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "registration_sha256": registration_sha, "launch_count": len(launches),
            "launch_set_sha256": provider_common.sha(
                provider_common.canonical_go_json(launches)),
            "cause": termination_cause, "scope_empty": True,
            "observed_at": "2026-09-15T00:00:08Z",
        })

        if include_wait:
            wait = {
                "schema_version": 1, "root_id": ROOT, "task_id": TASK,
                "registration_sha256": registration_sha,
                "child_pid": (worker["pid"] if worker is not None else
                              (0 if wait_result == "not-started" else 2001)),
                "result": wait_result, "observed_at": "2026-09-15T00:00:09Z",
            }
            if worker is not None:
                wait["worker_sha256"] = worker_sha
            if wait_result == "success":
                wait["exit_code"] = 0
            elif wait_result == "exit-error":
                wait["exit_code"] = 17
            elif wait_result == "wait-error":
                wait["exit_code"] = -1
            write_json(lifetime / "worker-wait.json", wait)
        return lifetime

    def ordinary_only_admission(self, runner_ownership: str | None = None,
                                runtime_pending: bool = False) -> Path:
        self.inspection_records()
        directory = self.durable_ordinary_admission(
            runner_ownership=runner_ownership, runtime_pending=runtime_pending)
        shutil.rmtree(self.state / "inspections")
        self.ops.inspection_attempts.clear()
        self.ops.inspection_bindings.clear()
        return directory

    @staticmethod
    def rebind_ordinary_metadata(directory: Path) -> None:
        meta_sha256 = digest(directory / "meta.json")
        for name in ("submit.json", "supervisor.ref.json"):
            record = read_json(directory / name)
            record["meta_sha256"] = meta_sha256
            write_json(directory / name, record, replace=True)

    @staticmethod
    def refresh_request_links(directory: Path) -> None:
        request_sha = digest(directory / "request.json")
        for name in ("receipt.json", "submission.json", "start.json"):
            path = directory / name
            if path.exists():
                record = read_json(path)
                record["request_sha256"] = request_sha
                write_json(path, record, replace=True)

    def ordinary_row(self, result: object = "Success", name: str = "fresh",
                     task: str = TASK, number: int = 7) -> dict[str, object]:
        argv = [str(self.runner)]
        if self.ops.managed_runner:
            argv.extend(["--guardian", "--supervised"])
        argv.extend(["--root", str(self.state), task])
        command = shlex.join(argv)
        return {
            "id": number, "label": self.ops.labels[name], "group": "default",
            "original_command": command, "command": command, "path": str(self.state),
            "status": {"Done": {"result": result}},
        }

    def inspection_row(self, result: object = "Success") -> dict[str, object]:
        command = shlex.join([str(self.runner), "--inspection", "--guardian", "--supervised",
                              "--root", str(self.state), TASK])
        return {
            "id": 42, "label": f"{_inspection_group(ROOT)}-{TASK}",
            "group": _inspection_group(ROOT), "original_command": command,
            "command": command, "path": str(self.state_parent),
            "status": {"Done": {"result": result}},
        }

    def status(self, inspection: dict[str, object] | None = None,
               ordinary: dict[str, object] | None = None,
               include_ordinary: bool = True):
        rows = {}
        if include_ordinary:
            rows["7"] = ordinary or self.ordinary_row()
        if inspection is not None:
            rows["42"] = inspection
        return {"tasks": rows, "groups": {
            "default": {"status": "Running", "parallel_tasks": 1},
            _inspection_group(ROOT): {"status": "Running", "parallel_tasks": 1},
        }}

    def ordinary_only_status(self, result: object = "Success") -> dict[str, object]:
        return {
            "tasks": {"7": self.ordinary_row(result)},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }

    def static_refusal(self, exit_code: int = 2,
                       response: dict[str, object] | None = None) -> None:
        self.state.joinpath(".maintenance.lock").write_bytes(b"")
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.dispatch_attempts = {TASK}
        self.ops.inspection_attempts = {TASK}
        self.ops.dispatch_observations = [{
            "name": "static-refusal",
            "task_id": TASK,
            "exit_code": exit_code,
            "natural_wait": True,
            "response": response or {
                "schema_version": 1, "command": "dispatch", "root_id": ROOT,
                "task_id": TASK, "admission": "unknown", "liveness": "undetermined",
                "publication": "unknown",
                "error": "unsupported-effective-config: static native profile refusal",
            },
            "receipt_written": True,
        }]

    def admitted_response(self) -> dict[str, object]:
        return {
            "schema_version": 1, "command": "dispatch", "root_id": ROOT,
            "task_id": TASK, "admission": "admitted", "liveness": "running",
            "publication": "unknown",
            "supervisor": {"matched": True, "state": "Queued", "numeric_task_id": 7},
        }

    def test_dispatch_rejection_code_keeps_positive_admission_for_cleanup(self):
        directory = self.base / "dispatch-rejected"
        directory.mkdir(mode=0o700)
        response = self.admitted_response()
        write_json(directory / "stdout", response)
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 1, "natural_wait": True})
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}
        self.ops.dispatch_attempts = set()
        self.ops.processes = SimpleNamespace(run=lambda *args, **kwargs: process)

        with self.assertRaisesRegex(AcceptanceFailure, "did not exit successfully"):
            self.ops.dispatch("rejected", TASK, ["delegate"])

        self.assertEqual(self.ops.tasks, {"rejected": TASK})
        self.assertEqual(self.ops.numbers, {"rejected": 7})
        self.assertTrue(self.ops.dispatch_observations[0]["receipt_written"])

    def test_dispatch_receipt_failure_retains_positive_admission(self):
        directory = self.base / "dispatch-receipt-failure"
        directory.mkdir(mode=0o700)
        response = self.admitted_response()
        write_json(directory / "stdout", response)
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 0, "natural_wait": True})
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}
        self.ops.dispatch_attempts = set()
        self.ops.processes = SimpleNamespace(run=lambda *args, **kwargs: process)

        def fail_attempt_receipt(path, value, replace=False):
            if Path(path).name == "receipt-failure-dispatch-attempt.json":
                raise OSError("injected dispatch-attempt receipt failure")
            return supervisor_common.write_json(path, value, replace)

        with patch.object(provider_common, "write_json", side_effect=fail_attempt_receipt):
            with self.assertRaisesRegex(OSError, "dispatch-attempt receipt failure"):
                self.ops.dispatch("receipt-failure", TASK, ["delegate"])

        self.assertEqual(self.ops.tasks, {"receipt-failure": TASK})
        self.assertFalse(self.ops.dispatch_observations[0]["receipt_written"])
        self.assertTrue((self.output / "receipt-failure-dispatch.json").is_file())

    def test_dispatch_records_static_refusal_before_admission_validation(self):
        directory = self.base / "dispatch"
        directory.mkdir(mode=0o700)
        response = {
            "schema_version": 1, "command": "dispatch", "root_id": ROOT,
            "task_id": TASK, "admission": "unknown", "liveness": "undetermined",
            "publication": "unknown",
            "error": "unsupported-effective-config: static native profile refusal",
        }
        write_json(directory / "stdout", response)
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 2, "natural_wait": True})
        calls = []
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.dispatch_attempts = set()
        self.ops.inspection_attempts = {TASK}
        self.ops.processes = SimpleNamespace(
            run=lambda *args, **kwargs: calls.append((args, kwargs)) or process)

        with self.assertRaisesRegex(AcceptanceFailure, "was not admitted"):
            self.ops.dispatch("static-refusal", TASK, ["delegate"])

        self.assertEqual(calls[0][1]["expected"], None)
        self.assertEqual(len(self.ops.dispatch_observations), 1)
        observation = self.ops.dispatch_observations[0]
        self.assertEqual(observation["exit_code"], 2)
        self.assertTrue(observation["receipt_written"])
        self.assertEqual(read_json(self.output / "static-refusal-dispatch-attempt.json"), {
            **observation, "receipt_written": True,
        })

    def test_dispatch_rejects_duplicate_before_starting_process(self):
        process = Mock()
        self.ops.processes = Mock(run=process)

        with self.assertRaisesRegex(AcceptanceFailure, "duplicate dispatch attempt"):
            self.ops.dispatch("duplicate", TASK, ["delegate"])

        process.assert_not_called()

    def test_static_refusal_allows_only_exact_empty_private_queue(self):
        self.static_refusal()
        self.assertTrue(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))
        self.assertTrue(self.ops.failure_queue_finished({
            "tasks": {}, "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }))

    def preadmission_cleanup(self, status, pending=(), ops=None):
        # Mock only the OS/client boundary: retain_failure_ownership,
        # safe_failure_shutdown, status decoding, and queue/state checks are real.
        if ops is None:
            ops = NativeTaskOps(SimpleNamespace(), self.base / "delegate", self.runner,
                                self.pueue, self.state_parent, self.state, self.output)
        ops.bind_supervisor(self.config, SimpleNamespace(poll=Mock(return_value=None), wait=Mock()))
        status_directory = Path(tempfile.mkdtemp(dir=self.base))
        write_json(status_directory / "stdout", status)
        process = SimpleNamespace(directory=status_directory,
                                  result={"exit_code": 0, "natural_wait": True})
        ops.client = Mock(return_value=process)
        ops.processes = SimpleNamespace(drain=Mock(return_value=list(pending)))
        return ops

    def test_preadmission_retained_cleanup_accepts_only_empty_or_implicit_default(self):
        (self.state / "root.json").unlink()
        for initialized in (False, True):
            if initialized:
                write_json(self.state / "root.json", {
                    "schema_version": 1, "root_id": ROOT, "created_at": "2026-09-15T00:00:00Z",
                })
                (self.state / ".maintenance.lock").write_bytes(b"")
            for groups in ({}, {"default": {"status": "Running", "parallel_tasks": 1}}):
                with self.subTest(initialized=initialized, groups=groups):
                    ops = self.preadmission_cleanup({"tasks": {}, "groups": groups})
                    with patch("acceptance_provider_common.time.sleep",
                               side_effect=AssertionError("empty owned queue must close")):
                        self.assertTrue(ops.retain_failure_ownership())
                    self.assertTrue(ops.closed)
                    self.assertEqual([call.args[:2] for call in ops.client.call_args_list], [
                        ("failure-queue", ["status", "--json"]),
                        ("failure-shutdown", ["shutdown"]),
                    ])
                    ops.daemon.wait.assert_called_once_with(timeout=30, expected=0)
                    for call in ops.processes.drain.call_args_list:
                        self.assertEqual(call.kwargs, {"timeout": 0, "exclude": (ops.daemon,)})

    def test_preadmission_cleanup_refuses_unknown_rows_groups_and_pending_clients(self):
        default = {"default": {"status": "Running", "parallel_tasks": 1}}
        unknown_row = {
            "id": 99, "created_at": "2026-09-15T00:00:00Z", "label": "unknown",
            "original_command": "unknown", "command": "unknown", "path": str(self.state),
            "envs": {}, "group": "default", "dependencies": [], "priority": 0,
            "status": {"Queued": {"enqueued_at": "2026-09-15T00:00:00Z"}},
        }
        cases = (
            ({"tasks": {"99": unknown_row}, "groups": default}, ()),
            ({"tasks": {}, "groups": {**default, "unknown": default["default"]}}, ()),
            ({"tasks": {}, "groups": {"default": {"status": "Paused", "parallel_tasks": 1}}}, ()),
            ({"tasks": {}, "groups": {"default": {"status": "Running", "parallel_tasks": 2}}}, ()),
            ({"tasks": {}, "groups": default}, ({"pid": 17},)),
        )
        for status, pending in cases:
            with self.subTest(status=status, pending=pending):
                provider_common._validate_pueue_status(status)
                ops = self.preadmission_cleanup(status, pending)
                self.assertFalse(ops.safe_failure_shutdown())
                self.assertFalse(ops.closed)
                ops.daemon.wait.assert_not_called()
                self.assertNotIn(["shutdown"], [call.args[1] for call in ops.client.call_args_list])

    def test_pueue_status_accepts_additive_fields(self):
        row = {
            "id": 99, "created_at": "2026-09-15T00:00:00Z", "label": "known",
            "original_command": "known", "command": "known", "path": str(self.state),
            "envs": {}, "group": "default", "dependencies": [], "priority": 0,
            "status": {"Queued": {"enqueued_at": "2026-09-15T00:00:00Z",
                                    "future_state_field": True}},
            "future_task_field": {"version": 2},
        }
        status = {
            "tasks": {"99": row},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1,
                                     "future_group_field": "ignored"}},
            "future_status_field": ["ignored"],
        }

        provider_common._validate_pueue_status(status)
        self.assertEqual(provider_common.row_state(row), "Queued")

    def test_pueue_status_rejects_unknown_lifecycle_variant(self):
        row = {
            "id": 99, "created_at": "2026-09-15T00:00:00Z", "label": "known",
            "original_command": "known", "command": "known", "path": str(self.state),
            "envs": {}, "group": "default", "dependencies": [], "priority": 0,
            "status": {
                "Done": {"enqueued_at": "2026-09-15T00:00:00Z",
                          "start": "2026-09-15T00:00:00Z",
                          "end": "2026-09-15T00:00:00Z", "result": "Success"},
                "Aborted": {"end": "2026-09-15T00:00:00Z"},
            },
        }
        status = {"tasks": {"99": row},
                  "groups": {"default": {"status": "Running", "parallel_tasks": 1}}}

        with self.assertRaises(AcceptanceFailure):
            provider_common._validate_pueue_status(status)

    def test_preadmission_cleanup_refuses_unproven_durable_or_attempted_work(self):
        status = {"tasks": {}, "groups": {"default": {"status": "Running", "parallel_tasks": 1}}}
        for name in ("tasks", "inspections", "inspection-group", "unexpected"):
            with self.subTest(durable=name):
                (self.state / name).mkdir()
                ops = self.preadmission_cleanup(status)
                self.assertFalse(ops.safe_failure_shutdown())
                ops.daemon.wait.assert_not_called()
                (self.state / name).rmdir()
        for field, value in (("dispatch_attempts", {TASK}), ("inspection_attempts", {TASK}),
                             ("tasks", {"fresh": TASK}), ("root_id", "c" * 32)):
            with self.subTest(field=field):
                ops = self.preadmission_cleanup(status)
                setattr(ops, field, value)
                self.assertFalse(ops.safe_failure_shutdown())
                ops.daemon.wait.assert_not_called()

    def test_static_authentication_refusal_uses_exact_completed_cleanup(self):
        self.static_refusal()
        response = self.ops.dispatch_observations[0]["response"]
        response["status"] = "blocked"
        response["error"] = ("provider authentication unavailable: unsupported-effective-config: "
                             "persistent ChatGPT authentication is required")
        ops = self.preadmission_cleanup({
            "tasks": {}, "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }, ops=self.ops)
        with patch("acceptance_provider_common.time.sleep",
                   side_effect=AssertionError("verified static refusal must close")):
            self.assertTrue(ops.retain_failure_ownership())
        ops.daemon.wait.assert_called_once_with(timeout=30, expected=0)
        ops.closed = False
        response["status"] = "unknown"
        self.assertFalse(ops.safe_failure_shutdown())
        response["status"] = "blocked"
        response["error"] = "unrecognized failure: credentials unavailable"
        self.assertFalse(ops.safe_failure_shutdown())

    def test_static_refusal_rejects_durable_guards_and_unknown_rows(self):
        self.static_refusal()
        (self.state / "inspections").mkdir()
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        shutil.rmtree(self.state / "inspections")
        unknown = {"id": 9, "label": "unknown", "group": "default",
                   "original_command": "unknown", "command": "unknown", "path": str(self.state),
                   "status": {"Done": {"result": "Success"}}}
        self.assertFalse(self.ops.failure_queue_finished({
            "tasks": {"9": unknown}, "groups": {},
        }))

    def test_static_refusal_requires_exact_integer_schema_version(self):
        for version in (True, 1.0, "1", None):
            with self.subTest(version=version):
                self.static_refusal()
                self.ops.dispatch_observations[0]["response"]["schema_version"] = version
                self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))
        self.static_refusal()
        self.assertTrue(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

    def test_static_refusal_rejects_uncertain_or_incomplete_attempts(self):
        self.static_refusal(exit_code=1)
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.static_refusal(response=None)
        self.ops.dispatch_observations[0]["response"] = None
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.static_refusal()
        self.ops.dispatch_attempts.add("c" * 32)
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

    def test_static_refusal_safe_shutdown_requires_no_active_clients(self):
        self.static_refusal()
        self.ops.pueue_config = self.config
        daemon = SimpleNamespace(poll=lambda: None, wait=lambda **_kwargs: None)
        self.ops.daemon = daemon
        status_directory = self.base / "status"
        status_directory.mkdir(mode=0o700)
        write_json(status_directory / "stdout", {"tasks": {}, "groups": {}})
        status_process = SimpleNamespace(directory=status_directory,
                                         result={"exit_code": 0, "natural_wait": True})
        calls = []
        self.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [])
        self.ops.client = lambda name, operation, **_kwargs: calls.append((name, operation)) or status_process

        self.assertTrue(self.ops.safe_failure_shutdown())
        self.assertTrue(self.ops.closed)
        self.assertEqual(calls, [("failure-queue", ["status", "--json"]),
                                 ("failure-shutdown", ["shutdown"])])

        self.static_refusal()
        self.ops.closed = False
        self.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [{"pid": 1}])
        self.assertFalse(self.ops.safe_failure_shutdown())

    def test_static_refusal_requires_durable_completed_response_and_exit_two(self):
        self.static_refusal()
        self.ops.dispatch_observations[0]["receipt_written"] = False
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.static_refusal()
        self.ops.dispatch_observations[0]["response"]["root_id"] = "c" * 32
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.static_refusal()
        self.ops.dispatch_observations[0]["response"]["admission"] = "not-admitted"
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.static_refusal()
        self.ops.dispatch_observations[0]["response"]["supervisor"] = {"matched": False}
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

    def test_shutdown_requires_complete_success_proof_for_inspection(self):
        self.inspection_records()
        status = self.status(self.inspection_row())
        calls = []
        self.ops.queue_status = lambda _name: status
        self.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [])
        self.ops.client = lambda name, operation, **_kwargs: calls.append((name, operation))
        self.ops.daemon = SimpleNamespace(wait=lambda **_kwargs: None)

        self.ops.shutdown()

        self.assertTrue(self.ops.closed)
        self.assertEqual(calls, [("private-shutdown", ["shutdown"])])

    def test_successful_shutdown_rejects_missing_worker_observation(self):
        directory = self.inspection_records()
        (directory / "worker-observation.json").unlink()
        with self.assertRaisesRegex(AcceptanceFailure, "proof"):
            self.ops._validate_queue_membership(self.status(self.inspection_row()), False)

    def test_inspection_expectation_requires_full_binding_before_dispatch(self):
        self.ops.dispatch_attempts.clear()

        with self.assertRaisesRegex(AcceptanceFailure, "binding is required"):
            self.ops.expect_inspection(TASK)

    def test_inspection_binding_accepts_only_the_guardian_exit_proof(self):
        directory = self.inspection_records()
        binding = read_json(directory / "request.json")["binding"]
        binding["worker_exit_proof"] = "guardian-wait-v1"

        provider_common._validate_inspection_binding(binding, "inspection binding")

        binding["worker_exit_proof"] = "untrusted-wait"
        with self.assertRaisesRegex(AcceptanceFailure, "worker exit proof is invalid"):
            provider_common._validate_inspection_binding(binding, "inspection binding")

    def test_inspection_binding_validates_paired_daemon_provenance(self):
        directory = self.inspection_records()
        binding = read_json(directory / "request.json")["binding"]
        supervisor = binding["supervisor"]
        supervisor["daemon_executable"] = "/fixture/pueued"
        supervisor["daemon_sha256"] = "a" * 64

        provider_common._validate_inspection_binding(binding, "inspection binding")

        del supervisor["daemon_sha256"]
        with self.assertRaisesRegex(AcceptanceFailure, "daemon provenance is incomplete"):
            provider_common._validate_inspection_binding(binding, "inspection binding")

    def test_managed_guardian_wait_replaces_queue_worker_observation(self):
        directory = self.inspection_records(managed_exit_proof=True)

        records = self.ops.validate_inspection_journals()

        self.assertFalse((directory / "worker-observation.json").exists())
        proof = records[TASK]["managed_lifetime"]
        self.assertEqual(proof["worker_wait"]["result"], "success")
        self.assertTrue(proof["termination"]["scope_empty"])

    def test_root_lifetime_closure_rejects_inspection_only_unfinished_state(self):
        directory = self.inspection_records(include_ordinary=False, managed_exit_proof=True)
        (directory / "lifetime-worker-wait.json").unlink()

        with self.assertRaisesRegex(AcceptanceFailure, "inspection lifetime worker Wait"):
            provider_common.validate_managed_lifetime_closure(self.state)

    def test_root_lifetime_closure_ignores_unsubmitted_managed_proof_reference(self):
        directory = self.inspection_records(include_ordinary=False, managed_exit_proof=True)
        for entry in tuple(directory.iterdir()):
            if entry.name != "request.json":
                self.assertTrue(entry.is_file())
                entry.unlink()
        write_json(directory / "proof-reference.json", {})

        closure = provider_common.validate_managed_lifetime_closure(self.state)

        self.assertEqual(closure["inspections"], {})

    def test_root_lifetime_closure_rejects_unsubmitted_unexpected_record(self):
        directory = self.inspection_records(include_ordinary=False, managed_exit_proof=True)
        for entry in tuple(directory.iterdir()):
            if entry.name != "request.json":
                entry.unlink()
        write_json(directory / "unexpected.json", {})

        with self.assertRaisesRegex(AcceptanceFailure, "unexpected records"):
            provider_common.validate_managed_lifetime_closure(self.state)

    def test_root_lifetime_closure_rejects_submitted_managed_inspection_without_lifetime(self):
        directory = self.inspection_records(include_ordinary=False, managed_exit_proof=True)
        for entry in tuple(directory.iterdir()):
            if entry.name.startswith("lifetime-"):
                entry.unlink()

        with self.assertRaisesRegex(AcceptanceFailure, "managed inspection lifetime evidence is absent"):
            provider_common.validate_managed_lifetime_closure(self.state)

    def test_provider_runtime_metadata_accepts_pending_admission_contract(self):
        meta = {
            "provider_executable": "/fixture/provider",
            "provider_version": "",
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        }

        self.assertIsNone(provider_common.validate_provider_runtime_metadata(meta))

    def test_ordinary_task_evidence_accepts_pending_runtime_metadata(self):
        directory = self.ordinary_only_admission(runtime_pending=True)

        ordinary = self.ops._durable_ordinary_admission(TASK, None)

        self.assertEqual(ordinary["task_id"], TASK)
        self.assertEqual(
            read_json(directory / "meta.json")["provider_version"], "")

    def test_ordinary_task_evidence_rejects_pending_runtime_digest(self):
        directory = self.ordinary_only_admission(runtime_pending=True)
        meta = read_json(directory / "meta.json")
        meta["runtime_preflight_sha256"] = "invalid"
        write_json(directory / "meta.json", meta, replace=True)
        self.rebind_ordinary_metadata(directory)

        with self.assertRaisesRegex(AcceptanceFailure, "preflight digest is invalid"):
            self.ops._durable_ordinary_admission(TASK, None)

    def test_ordinary_task_evidence_rejects_pending_runtime_version(self):
        directory = self.ordinary_only_admission(runtime_pending=True)
        meta = read_json(directory / "meta.json")
        meta["provider_version"] = "fixture-v2"
        write_json(directory / "meta.json", meta, replace=True)
        self.rebind_ordinary_metadata(directory)

        with self.assertRaisesRegex(AcceptanceFailure, "pending version is not empty"):
            self.ops._durable_ordinary_admission(TASK, None)

    def test_ordinary_task_evidence_rejects_pending_environment_marker(self):
        directory = self.ordinary_only_admission(runtime_pending=True)
        meta = read_json(directory / "meta.json")
        meta["environment_recorded"] = False
        write_json(directory / "meta.json", meta, replace=True)
        self.rebind_ordinary_metadata(directory)

        with self.assertRaisesRegex(AcceptanceFailure, "pending environment is not recorded"):
            self.ops._durable_ordinary_admission(TASK, None)

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

    def test_root_lifetime_closure_rejects_complete_task_with_incomplete_inspection(self):
        self.inspection_records(managed_exit_proof=True)
        ordinary = self.durable_ordinary_admission(runner_ownership="managed")
        self.managed_ordinary_lifetime(ordinary)
        (self.state / "inspections" / TASK / "lifetime-termination.json").unlink()

        with self.assertRaisesRegex(AcceptanceFailure, "inspection lifetime termination"):
            provider_common.validate_managed_lifetime_closure(self.state)

    def test_root_lifetime_closure_accepts_complete_mixed_state(self):
        self.inspection_records(managed_exit_proof=True)
        ordinary = self.durable_ordinary_admission(runner_ownership="managed")
        self.managed_ordinary_lifetime(ordinary)

        closure = provider_common.validate_managed_lifetime_closure(self.state)

        self.assertEqual(set(closure["tasks"]), {TASK})
        self.assertEqual(set(closure["inspections"]), {TASK})
        self.assertEqual(closure["tasks"][TASK]["worker_wait"]["result"], "success")
        self.assertEqual(closure["inspections"][TASK]["worker_wait"]["result"], "success")

    def test_root_lifetime_closure_ignores_managed_plan_without_supervisor_handoff(self):
        self.inspection_records()
        ordinary = self.durable_ordinary_admission(runner_ownership="managed")
        (ordinary / "supervisor.ref.json").unlink()
        shutil.rmtree(self.state / "inspections")

        closure = provider_common.validate_managed_lifetime_closure(self.state)

        self.assertEqual(closure["tasks"], {})
        self.assertEqual(closure["inspections"], {})

    def test_root_lifetime_closure_rejects_handoff_without_managed_lifetime(self):
        self.inspection_records()
        self.durable_ordinary_admission(runner_ownership="managed")
        shutil.rmtree(self.state / "inspections")

        with self.assertRaisesRegex(AcceptanceFailure, "ordinary task lifetime"):
            provider_common.validate_managed_lifetime_closure(self.state)

    def test_root_lifetime_closure_preserves_custom_and_historical_inspections(self):
        for index, options in enumerate((
                {"custom_inspection": True, "include_ordinary": False},
                {"legacy_admission": True, "include_ordinary": False})):
            with self.subTest(options=options):
                if index:
                    shutil.rmtree(self.state / "inspections")
                    self.ops.inspection_attempts.clear()
                    self.ops.inspection_bindings.clear()
                self.inspection_records(**options)
                closure = provider_common.validate_managed_lifetime_closure(self.state)
                self.assertEqual(closure["tasks"], {})
                self.assertEqual(closure["inspections"], {})

    def test_unstarted_guardian_requires_a_failed_terminal_queue_row(self):
        self.inspection_records(result=None, include_ordinary=False,
                                managed_exit_proof=True)
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}

        failed = self.status(self.inspection_row({"Failed": 1}), include_ordinary=False)
        self.assertTrue(self.ops.failure_queue_finished(failed))

        succeeded = self.status(self.inspection_row(), include_ordinary=False)
        self.assertFalse(self.ops.failure_queue_finished(succeeded))

    def test_inspection_queue_row_requires_the_control_working_directory(self):
        self.inspection_records(result=None, include_ordinary=False,
                                managed_exit_proof=True)
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}
        row = self.inspection_row({"Failed": 1})
        row["path"] = str(self.state)

        self.assertFalse(self.ops.failure_queue_finished(
            self.status(row, include_ordinary=False)))

    def test_managed_guardian_wait_rejects_changed_worker_identity(self):
        directory = self.inspection_records(managed_exit_proof=True)
        wait_path = directory / "lifetime-worker-wait.json"
        wait = read_json(wait_path)
        wait["worker_sha256"] = "0" * 64
        write_json(wait_path, wait, replace=True)

        with self.assertRaisesRegex(AcceptanceFailure, "worker Wait binding mismatch"):
            self.ops.validate_inspection_journals()

    def test_managed_guardian_proof_survives_failed_or_removed_queue_row(self):
        self.inspection_records(managed_exit_proof=True)
        self.durable_ordinary_admission()
        ordinary = self.ordinary_row()
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}

        failed_row = self.status(self.inspection_row({"Failed": 17}), ordinary=ordinary)
        self.assertTrue(self.ops.failure_queue_finished(failed_row))

        removed_row = self.status(None, ordinary=ordinary)
        self.assertTrue(self.ops.failure_queue_finished(removed_row))

    def test_partial_managed_guardian_evidence_cannot_fallback_to_queue(self):
        directory = self.inspection_records(managed_exit_proof=True)
        (directory / "lifetime-worker-wait.json").unlink()

        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

    def test_custom_inspection_uses_exact_terminal_queue_fallback(self):
        self.inspection_records(custom_inspection=True)
        self.durable_ordinary_admission()
        ordinary = self.ordinary_row()
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}

        self.assertTrue(self.ops.failure_queue_finished(
            self.status(self.inspection_row(), ordinary=ordinary)))

    def test_historical_inspection_uses_exact_terminal_queue_fallback(self):
        self.inspection_records(legacy_admission=True)
        self.durable_ordinary_admission()
        ordinary = self.ordinary_row()
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}

        self.assertTrue(self.ops.failure_queue_finished(
            self.status(self.inspection_row(), ordinary=ordinary)))

    def test_queue_fallback_rejects_wrong_supervisor_state_identity(self):
        self.inspection_records()
        status = self.status(self.inspection_row())
        status["tasks"]["42"]["path"] = str(self.base / "foreign-state")

        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_queue_fallback_rejects_duplicate_terminal_rows(self):
        self.inspection_records()
        status = self.status(self.inspection_row())
        duplicate = deepcopy(status["tasks"]["42"])
        duplicate["id"] = 43
        status["tasks"]["43"] = duplicate

        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_inspection_expectation_rejects_late_and_conflicting_registration(self):
        directory = self.inspection_records()
        binding = read_json(directory / "request.json")["binding"]

        self.ops.dispatch_attempts.discard(TASK)
        conflicting = deepcopy(binding)
        conflicting["helper_executable"] = str(self.base / "other-helper")
        with self.assertRaisesRegex(AcceptanceFailure, "different source"):
            self.ops.expect_inspection(TASK, conflicting)

        self.ops.dispatch_attempts.add(TASK)
        with self.assertRaisesRegex(AcceptanceFailure, "precede dispatch"):
            self.ops.expect_inspection(TASK, binding)

    def test_failed_journal_recomputes_embedded_task_digest_without_ordinary_task(self):
        directory = self.inspection_records(result=None, include_ordinary=False)
        request = read_json(directory / "request.json")
        request["task_sha256"] = "d" * 64
        write_json(directory / "request.json", request, replace=True)
        self.refresh_request_links(directory)
        self.ops.tasks = {}
        status = self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)
        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_model_discovery_journal_requires_complete_standalone_proof(self):
        directory = self.inspection_records(include_ordinary=False,
                                            revision="model-discovery-v1")
        records = self.ops.validate_inspection_journals()
        self.assertEqual(set(records), {TASK})
        self.assertFalse((self.state / "tasks").exists())
        (directory / "worker-observation.json").unlink()
        with self.assertRaisesRegex(AcceptanceFailure, "proof"):
            self.ops.validate_inspection_journals()

    def test_model_discovery_rejects_an_ordinary_task(self):
        self.inspection_records(revision="model-discovery-v1")
        with self.assertRaisesRegex(AcceptanceFailure, "ordinary task"):
            self.ops.validate_inspection_journals()

    def test_model_discovery_requires_its_exact_deadline(self):
        directory = self.inspection_records(include_ordinary=False,
                                            revision="model-discovery-v1")
        request = read_json(directory / "request.json")
        request["deadline"] = "2026-09-15T00:00:20Z"
        write_json(directory / "request.json", request, replace=True)
        self.refresh_request_links(directory)
        with self.assertRaisesRegex(AcceptanceFailure, "300 seconds"):
            self.ops.validate_inspection_journals()

    def test_model_discovery_accepts_legacy_one_minute_journal(self):
        self.inspection_records(include_ordinary=False, revision="model-discovery-v1",
                                legacy_model_discovery=True)
        records = self.ops.validate_inspection_journals()
        self.assertEqual(set(records), {TASK})

    def test_inspection_request_requires_exact_deadline_window(self):
        directory = self.inspection_records(result=None, include_ordinary=False)
        request = read_json(directory / "request.json")
        request["deadline"] = "2026-09-15T00:00:40Z"
        write_json(directory / "request.json", request, replace=True)
        self.refresh_request_links(directory)
        self.ops.tasks = {}
        status = self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)
        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_inspection_request_accepts_legacy_twenty_second_deadline(self):
        self.inspection_records(legacy_admission=True)
        records = self.ops.validate_inspection_journals()
        self.assertEqual(set(records), {TASK})

    def test_inspection_request_cannot_predate_root_creation(self):
        self.inspection_records()
        root = read_json(self.state / "root.json")
        root["created_at"] = "2026-09-15T00:00:01Z"
        write_json(self.state / "root.json", root, replace=True)

        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

    def test_root_creation_timestamp_remains_pinned_across_observations(self):
        self.inspection_records()
        self.durable_ordinary_admission()
        status = self.status(self.inspection_row())
        self.assertTrue(self.ops.failure_queue_finished(status))
        root = read_json(self.state / "root.json")
        original = root["created_at"]
        for changed in ("2026-09-14T23:59:59Z", "2026-09-15T00:00:01Z"):
            with self.subTest(created_at=changed):
                root["created_at"] = changed
                write_json(self.state / "root.json", root, replace=True)
                self.assertFalse(self.ops.failure_queue_finished(status))
        root["created_at"] = original
        write_json(self.state / "root.json", root, replace=True)
        self.assertTrue(self.ops.failure_queue_finished(status))

    def test_inspection_claim_timestamps_follow_request_and_each_other(self):
        directory = self.inspection_records()
        submission = read_json(directory / "submission.json")
        submission["created_at"] = "2026-09-15T00:00:03Z"
        write_json(directory / "submission.json", submission, replace=True)
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

        submission["created_at"] = "2026-09-15T00:00:01Z"
        write_json(directory / "submission.json", submission, replace=True)
        completion = read_json(directory / "completion.json")
        completion["completed_at"] = "2026-09-15T00:00:01Z"
        write_json(directory / "completion.json", completion, replace=True)
        observation = read_json(directory / "worker-observation.json")
        observation["completion_sha256"] = digest(directory / "completion.json")
        write_json(directory / "worker-observation.json", observation, replace=True)
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

    def test_inspection_cleanup_requires_submission_and_conditional_start_guards(self):
        directory = self.inspection_records()
        (directory / "submission.json").unlink()
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

        shutil.rmtree(directory)
        shutil.rmtree(self.state / "tasks" / TASK)
        directory = self.inspection_records()
        (directory / "start.json").unlink()
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

    def test_inspection_binding_must_match_expected_source(self):
        directory = self.inspection_records()
        request = read_json(directory / "request.json")
        self.ops.inspection_bindings[TASK] = deepcopy(request["binding"])
        self.ops._validate_queue_membership(self.status(self.inspection_row()), False)

        request["binding"]["helper_executable"] = str(self.base / "foreign-helper")
        write_json(directory / "request.json", request, replace=True)
        self.refresh_request_links(directory)
        with self.assertRaisesRegex(AcceptanceFailure, "binding"):
            self.ops._validate_queue_membership(self.status(self.inspection_row()), False)

    def test_native_binding_mode_reaches_failure_cleanup_and_replay(self):
        self.inspection_records()
        self.durable_ordinary_admission()
        self.ops.inspection_bindings.clear()
        status = self.status(self.inspection_row())
        self.assertFalse(self.ops.failure_queue_finished(status))

        self.ops = NativeTaskOps(
            SimpleNamespace(), self.base / "delegate", self.runner, self.pueue,
            self.state_parent, self.state, self.output,
            expected_inspection_binding_required=False)
        self.ops.bind_supervisor(self.config)
        self.ops.root_id = ROOT
        self.ops.tasks = {"fresh": TASK}
        self.ops.labels = {"fresh": f"delegate:{ROOT}:{TASK}"}
        self.ops.numbers = {"fresh": 7}
        self.ops.dispatch_attempts = {TASK}
        self.ops.inspection_attempts = {TASK}
        self.assertTrue(self.ops.failure_queue_finished(status))

        ordinary = self.state / "tasks" / TASK
        result = b"replay proof\n"
        payload = {"basename": "result.txt", "length": len(result),
                   "sha256": provider_common.sha(result)}
        outcome = {"verdict": "committed", "payload": payload,
                   "evidence_sha256": "a" * 64}
        (ordinary / "result.txt").write_bytes(result)
        write_json(ordinary / "outcome.json", outcome)
        response = {"task_id": TASK, "outcome": outcome, "payload": payload,
                    "evidence_sha256": outcome["evidence_sha256"]}
        self.ops.queue_status = lambda _name: status
        self.ops.collect = lambda _name, task: dict(response, task_id=task)

        self.ops.replay({
            "fresh": {"directory": ordinary, "outcome": outcome, "payload": payload},
        }, names=("fresh",))

    def test_fixture_binding_uses_static_definition_and_supervisor_sources(self):
        helper = self.base / "inspection-helper"
        helper_config = self.base / "helper.json"
        helper.write_bytes(b"fixture helper")
        helper.chmod(0o700)
        helper_config.write_bytes(b"{\"schema_version\":1}\n")
        helper_config.chmod(0o600)
        environment = {"FIXTURE": "yes", "LANG": "C"}
        supervisor = provider_common.supervisor_binding(
            self.pueue, self.config, self.base, digest(self.config), "pueue fixture")
        binding = inspection_acceptance.fixture_inspection_binding(
            helper, helper_config, self.base / "workspace", environment,
            self.runner, supervisor)

        self.assertEqual(binding["definition_revision"], "inspection-fixture-v1")
        self.assertEqual(binding["helper_executable"], str(helper.resolve()))
        self.assertEqual(binding["worker_executable"], str(self.runner.resolve()))
        self.assertEqual(binding["supervisor"], supervisor)
        self.assertEqual(binding["supervisor"]["endpoint"],
                         "unix:" + str((self.base / "run" / "p.sock").resolve()))
        self.assertEqual(binding["supervisor"]["config_digest"], digest(self.config))

        changed_environment = dict(environment, FIXTURE="changed")
        changed = inspection_acceptance.fixture_inspection_binding(
            helper, helper_config, self.base / "workspace", changed_environment,
            self.runner, supervisor)
        self.assertNotEqual(binding["definition_sha256"], changed["definition_sha256"])

        changed_helper_config = self.base / "other-helper.json"
        changed_helper_config.write_bytes(helper_config.read_bytes())
        changed_helper_config.chmod(0o600)
        changed_path = inspection_acceptance.fixture_inspection_binding(
            helper, changed_helper_config, self.base / "workspace", environment,
            self.runner, supervisor)
        self.assertNotEqual(binding["definition_sha256"], changed_path["definition_sha256"])

    def test_legacy_supervisor_binding_accepts_cwd_only_resolution(self):
        binding = provider_common.supervisor_binding(
            self.pueue, self.config, self.base, digest(self.config), "pueue fixture")
        binding["observed_version"] = "pueue 99.1.0"
        provider_common._validate_supervisor_binding(binding, "future supervisor")
        for key in ("resolution_os", "resolution_home", "resolution_data_local",
                    "resolution_config", "resolution_runtime", "resolution_username"):
            binding.pop(key, None)
        binding["resolution_cwd"] = str(self.base)

        provider_common._validate_supervisor_binding(binding, "legacy supervisor")

    def test_noneligible_inspection_facts_and_exit_must_be_empty_and_unavailable(self):
        directory = self.inspection_records(result="unavailable")
        result_path = directory / "result.json"
        result = read_json(result_path)
        result["facts"] = {"eligible": False}
        write_json(result_path, result, replace=True)
        completion = read_json(directory / "completion.json")
        completion["result_sha256"] = digest(result_path)
        write_json(directory / "completion.json", completion, replace=True)
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

        shutil.rmtree(directory)
        shutil.rmtree(self.state / "tasks" / TASK)
        directory = self.inspection_records(result="unavailable", include_ordinary=False)
        completion = read_json(directory / "completion.json")
        completion["native_exit"] = "successful-exit"
        write_json(directory / "completion.json", completion, replace=True)
        self.assertFalse(self.ops.failure_queue_finished(
            self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)))

    def test_inspection_result_requires_completion_record(self):
        directory = self.inspection_records(result="unavailable")
        (directory / "completion.json").unlink()
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

    def test_ordinary_attempt_without_inspection_journal_is_allowed(self):
        self.state.joinpath("inspections").mkdir(mode=0o700)
        self.ops._validate_queue_membership({
            "tasks": {"7": self.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }, False)

    def test_declared_inspection_without_journal_is_rejected(self):
        directory = self.inspection_records(result=None)
        shutil.rmtree(directory)
        self.assertFalse(self.ops.failure_queue_finished({
            "tasks": {"7": self.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }))

    def test_mixed_attempts_allow_ordinary_task_without_inspection(self):
        other = "c" * 32
        self.ops.tasks["resume"] = other
        self.ops.labels["resume"] = f"delegate:{ROOT}:{other}"
        self.ops.numbers["resume"] = 8
        self.ops.dispatch_attempts.add(other)
        self.inspection_records()
        status = self.status(self.inspection_row())
        status["tasks"]["8"] = self.ordinary_row(name="resume", task=other, number=8)

        self.ops._validate_queue_membership(status, False)

    def test_queue_rejects_unknown_inspection_row_and_wrong_argv(self):
        self.inspection_records()
        status = self.status(self.inspection_row())
        unknown = deepcopy(status)
        unknown["tasks"]["99"] = {
            "id": 99, "label": "delegation-inspection-" + ROOT + "-" + "c" * 32,
            "group": _inspection_group(ROOT), "original_command": "unknown",
            "command": "unknown", "path": str(self.state),
            "status": {"Done": {"result": "Success"}},
        }
        with self.assertRaisesRegex(AcceptanceFailure, "membership|labels"):
            self.ops._validate_queue_membership(unknown, False)

        wrong_argv = deepcopy(status)
        wrong_argv["tasks"]["42"]["command"] = shlex.join(
            [str(self.runner), "--root", str(self.state), TASK])
        with self.assertRaisesRegex(AcceptanceFailure, "argv"):
            self.ops._validate_queue_membership(wrong_argv, False)

    def test_queue_rejects_wrong_ordinary_id_group_and_argv(self):
        for mutation, message in (
                (lambda row: row.update(id=8), "numeric"),
                (lambda row: row.update(group="foreign"), "group"),
                (lambda row: row.update(command="/bin/true"), "exact")):
            with self.subTest(message=message):
                row = self.ordinary_row()
                mutation(row)
                with self.assertRaisesRegex(AcceptanceFailure, "identity|group|exact|runner"):
                    self.ops._validate_queue_membership({
                        "tasks": {"7": row},
                        "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
                    }, False)

    def test_queue_rejects_foreign_groups_even_without_inspection_rows(self):
        self.state.joinpath("inspections").mkdir(mode=0o700)
        status = {
            "tasks": {"7": self.ordinary_row()},
            "groups": {
                "default": {"status": "Running", "parallel_tasks": 1},
                "foreign": {"status": "Running", "parallel_tasks": 1},
            },
        }
        with self.assertRaisesRegex(AcceptanceFailure, "unknown supervisor group"):
            self.ops._validate_queue_membership(status, False)

    def test_ordinary_failure_cleanup_binds_exact_id_label_and_default_group(self):
        self.inspection_records()
        self.durable_ordinary_admission()
        shutil.rmtree(self.state / "inspections")
        self.ops.inspection_attempts.clear()
        self.ops.inspection_bindings.clear()
        status = {
            "tasks": {"7": self.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }
        self.assertTrue(self.ops.failure_queue_finished(status))

        wrong_id = deepcopy(status)
        wrong_id["tasks"]["7"]["id"] = 8
        self.assertFalse(self.ops.failure_queue_finished(wrong_id))

        foreign_group = deepcopy(status)
        foreign_group["tasks"]["7"]["group"] = "foreign"
        self.assertFalse(self.ops.failure_queue_finished(foreign_group))

        duplicate_label = deepcopy(status)
        duplicate_label["tasks"]["8"] = deepcopy(duplicate_label["tasks"]["7"])
        duplicate_label["tasks"]["8"]["id"] = 8
        self.assertFalse(self.ops.failure_queue_finished(duplicate_label))

    def test_managed_ordinary_done_failures_without_lifetime_closure_are_retained(self):
        self.ordinary_only_admission("managed")

        for result in ({"Failed": 17}, "Killed"):
            with self.subTest(result=result):
                self.assertFalse(
                    self.ops.failure_queue_finished(self.ordinary_only_status(result)))

    def test_managed_ordinary_missing_mismatched_or_unreadable_wait_is_retained(self):
        directory = self.ordinary_only_admission("managed")
        lifetime = self.managed_ordinary_lifetime(directory, wait_result="exit-error")
        wait_path = lifetime / "worker-wait.json"
        valid_wait = read_json(wait_path)
        status = self.ordinary_only_status({"Failed": 17})

        wait_path.unlink()
        self.assertFalse(self.ops.failure_queue_finished(status))

        write_json(wait_path, valid_wait)
        mismatched = deepcopy(valid_wait)
        mismatched["worker_sha256"] = "0" * 64
        write_json(wait_path, mismatched, replace=True)
        self.assertFalse(self.ops.failure_queue_finished(status))

        wait_path.write_text("{", encoding="utf-8")
        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_managed_ordinary_valid_closure_allows_failed_cleanup(self):
        directory = self.ordinary_only_admission("managed")
        self.managed_ordinary_lifetime(directory, wait_result="exit-error")

        self.assertTrue(self.ops.failure_queue_finished(
            self.ordinary_only_status({"Failed": 17})))

    def test_managed_ordinary_valid_closure_allows_successful_shutdown(self):
        directory = self.ordinary_only_admission("managed")
        self.managed_ordinary_lifetime(directory)
        status = self.ordinary_only_status()
        calls = []
        self.ops.queue_status = lambda _name: status
        self.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [])
        self.ops.client = lambda name, operation, **_kwargs: calls.append((name, operation))
        self.ops.daemon = SimpleNamespace(wait=lambda **_kwargs: None)

        self.ops.shutdown()

        self.assertTrue(self.ops.closed)
        self.assertEqual(calls, [("private-shutdown", ["shutdown"])])

    def test_managed_ordinary_successful_shutdown_without_closure_is_rejected(self):
        self.ordinary_only_admission("managed")
        self.ops.queue_status = lambda _name: self.ordinary_only_status()

        with self.assertRaisesRegex(AcceptanceFailure, "lifetime"):
            self.ops.shutdown()

        self.assertFalse(self.ops.closed)

    def test_managed_ordinary_not_started_proof_allows_failed_cleanup(self):
        directory = self.ordinary_only_admission("managed")
        self.managed_ordinary_lifetime(
            directory, wait_result="not-started", with_worker=False)

        self.assertTrue(self.ops.failure_queue_finished(
            self.ordinary_only_status({"FailedToSpawn": "fixture start failure"})))

    def test_managed_ordinary_exact_unqualified_failure_is_valid(self):
        directory = self.ordinary_only_admission("managed")
        self.managed_ordinary_lifetime(
            directory, wait_result="exit-error", with_worker=False,
            scope_kind="cgroup-v2")

        self.assertTrue(self.ops.failure_queue_finished(
            self.ordinary_only_status({"Failed": 17})))

    def test_managed_ordinary_previous_boot_termination_does_not_require_wait(self):
        directory = self.ordinary_only_admission("managed")
        self.managed_ordinary_lifetime(
            directory, include_wait=False, termination_cause="previous-boot")

        self.assertTrue(
            self.ops.failure_queue_finished(self.ordinary_only_status("Killed")))

    def test_managed_ordinary_lifetime_requires_domain_separated_owner_digest(self):
        directory = self.ordinary_only_admission("managed")
        spec_sha = digest(directory / "task.json")
        meta_sha = digest(directory / "meta.json")
        self.assertEqual(
            provider_common.ordinary_lifetime_owner_digest(spec_sha, meta_sha),
            provider_common.sha(
                ("delegation-layer:lifetime-owner:task:v1\n" + spec_sha +
                 "\n" + meta_sha).encode("ascii")),
        )
        lifetime = self.managed_ordinary_lifetime(directory, wait_result="exit-error")
        for name in ("intent.json", "registration.json"):
            path = lifetime / name
            record = read_json(path)
            record["owner_sha256"] = spec_sha
            write_json(path, record, replace=True)

        self.assertFalse(self.ops.failure_queue_finished(
            self.ordinary_only_status({"Failed": 17})))

    def test_managed_ordinary_lifetime_requires_exact_launch_set(self):
        directory = self.ordinary_only_admission("managed")
        lifetime = self.managed_ordinary_lifetime(directory, wait_result="exit-error")
        termination_path = lifetime / "terminated.json"
        termination = read_json(termination_path)
        termination["launch_set_sha256"] = provider_common.sha(
            provider_common.canonical_go_json([]))
        write_json(termination_path, termination, replace=True)

        self.assertFalse(self.ops.failure_queue_finished(
            self.ordinary_only_status({"Failed": 17})))

    def test_custom_ordinary_task_without_lifetime_keeps_queue_fallback(self):
        self.ordinary_only_admission("custom")

        self.assertTrue(
            self.ops.failure_queue_finished(self.ordinary_only_status("Killed")))

    def test_status_observation_requires_successful_natural_exit_and_strict_schema(self):
        directory = self.base / "status-strict"
        directory.mkdir(mode=0o700)
        write_json(directory / "stdout", {"tasks": {}, "groups": {}})
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 1, "natural_wait": True})
        with self.assertRaisesRegex(AcceptanceFailure, "did not exit successfully"):
            provider_common.status_jobs(process)

        process.result = {"exit_code": 0, "natural_wait": True}
        write_json(directory / "stdout", {"Tasks": {}, "groups": {}}, replace=True)
        with self.assertRaisesRegex(AcceptanceFailure, "missing fields"):
            provider_common.status_jobs(process)

    def test_status_observation_rejects_nonstandard_json_constants(self):
        directory = self.base / "status-constants"
        directory.mkdir(mode=0o700)
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 0, "natural_wait": True})

        for constant in ("NaN", "Infinity", "-Infinity"):
            with self.subTest(constant=constant):
                (directory / "stdout").write_text(
                    '{"tasks":' + constant + ',"groups":{}}')
                with self.assertRaisesRegex(AcceptanceFailure, "non-standard JSON constant"):
                    provider_common.status_jobs(process)

    def test_status_observation_requires_exact_integer_zero_exit(self):
        directory = self.base / "status-exit-type"
        directory.mkdir(mode=0o700)
        write_json(directory / "stdout", {"tasks": {}, "groups": {}})
        process = SimpleNamespace(directory=directory, result={"natural_wait": True})

        for exit_code in (False, 0.0, "0", None):
            with self.subTest(exit_code=exit_code):
                process.result["exit_code"] = exit_code
                with self.assertRaisesRegex(AcceptanceFailure, "did not exit successfully"):
                    provider_common.status_jobs(process)

    def test_closed_supervisor_rejects_all_new_process_launches(self):
        process = Mock()
        self.ops.processes = Mock(run=process)
        self.ops.closed = True

        with self.assertRaisesRegex(AcceptanceFailure, "supervisor is closed"):
            self.ops.direct("late-direct", ["delegate"])
        with self.assertRaisesRegex(AcceptanceFailure, "supervisor is closed"):
            self.ops.dispatch("late-dispatch", "c" * 32, ["delegate"])

        process.assert_not_called()

    def test_control_decoder_rejects_simple_fold_alias(self):
        directory = self.base / "alias"
        directory.mkdir(mode=0o700)
        (directory / "stdout").write_text('{"tasks":{},"Tasks":{}}')
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 0, "natural_wait": True})
        with self.assertRaisesRegex(AcceptanceFailure, "duplicate JSON object key"):
            provider_common.status_jobs(process)

    def test_replay_preserves_inspection_journal_and_queue(self):
        self.inspection_records()
        status = self.status(self.inspection_row())
        self.ops.queue_status = lambda _name: deepcopy(status)

        self.ops.replay({}, names=())

        self.assertEqual(self.ops.queue_before_replay, {
            "tasks": status["tasks"], "groups": status["groups"]})

    def test_failure_cleanup_accepts_only_positive_failed_inspection(self):
        self.inspection_records(result=None, include_ordinary=False)
        self.ops.tasks = {}
        failed = self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)

        self.assertTrue(self.ops.failure_queue_finished(failed))

        empty = {"tasks": {}, "groups": {}}
        self.assertFalse(self.ops.failure_queue_finished(empty))

        lost_response = deepcopy(failed)
        shutil.rmtree(self.state / "inspections" / TASK)
        self.assertFalse(self.ops.failure_queue_finished(lost_response))

    def test_failure_cleanup_recovers_durable_admission_after_response_loss(self):
        self.inspection_records()
        self.durable_ordinary_admission()
        status = self.status(self.inspection_row(), ordinary=self.ordinary_row())
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}

        self.assertTrue(self.ops.failure_queue_finished(status))

    def test_failure_cleanup_requires_exact_durable_supervisor_receipt(self):
        self.inspection_records()
        self.durable_ordinary_admission()
        status = self.status(self.inspection_row(), ordinary=self.ordinary_row())

        self.assertTrue(self.ops.failure_queue_finished(status))
        receipt = self.state / "tasks" / TASK / "supervisor.ref.json"
        receipt_record = read_json(receipt)
        receipt_record["resolution_cwd"] = str(self.base)
        write_json(receipt, receipt_record, replace=True)
        self.assertFalse(self.ops.failure_queue_finished(status))

        del receipt_record["resolution_cwd"]
        write_json(receipt, receipt_record, replace=True)
        self.assertTrue(self.ops.failure_queue_finished(status))
        receipt.unlink()
        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_failure_cleanup_rejects_task_hash_drift(self):
        self.inspection_records()
        self.durable_ordinary_admission()
        status = self.status(self.inspection_row(), ordinary=self.ordinary_row())
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}
        task_path = self.state / "tasks" / TASK / "task.json"
        task_record = read_json(task_path)
        task_record["requested_config"]["budget"] = "2m0s"
        task_path.write_bytes(provider_common._canonical_task_bytes(task_record))

        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_failure_cleanup_recovers_durable_admission_without_inspection(self):
        self.inspection_records()
        self.durable_ordinary_admission()
        shutil.rmtree(self.state / "inspections")
        self.ops.inspection_attempts.clear()
        self.ops.inspection_bindings.clear()
        status = {
            "tasks": {"7": self.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}

        self.assertTrue(self.ops.failure_queue_finished(status))

        task_path = self.state / "tasks" / TASK / "task.json"
        task_record = read_json(task_path)
        task_record["requested_config"]["budget"] = "2m0s"
        task_path.write_bytes(provider_common._canonical_task_bytes(task_record))
        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_failure_cleanup_rejects_unrecognized_inspection_result(self):
        self.inspection_records(result="unavailable")
        status = self.status(self.inspection_row("Success"),
                             ordinary=self.ordinary_row("Errored"))

        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_failed_inspection_requires_receipt_and_complete_membership(self):
        directory = self.inspection_records(result=None, include_ordinary=False)
        self.ops.tasks = {}
        status = self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)
        (directory / "receipt.json").unlink()
        self.assertFalse(self.ops.failure_queue_finished(status))

        request_sha = digest(directory / "request.json")
        write_json(directory / "receipt.json", {
            "schema_version": 1, "request_sha256": request_sha, "numeric_task_id": 42,
        })
        incomplete = {"tasks": {}, "groups": status["groups"]}
        self.assertFalse(self.ops.failure_queue_finished(incomplete))

        unknown = self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)
        unknown["tasks"]["99"] = {
            "id": 99, "label": "unknown", "group": "default",
            "original_command": "unknown", "command": "unknown", "path": str(self.state),
            "status": {"Done": {"result": "Success"}},
        }
        self.assertFalse(self.ops.failure_queue_finished(unknown))

    def test_failed_inspection_cannot_coexist_with_ordinary_task(self):
        self.inspection_records(result="unavailable")
        status = self.status(self.inspection_row({"Failed": 17}),
                             ordinary=self.ordinary_row("Errored"))
        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_retain_failure_ownership_waits_for_callers_then_observes_shutdown(self):
        pending = [{"pid": 17, "argv": ["caller"], "directory": "/owned/caller"}]
        drain_calls = []
        self.ops.processes = SimpleNamespace(
            drain=lambda **kwargs: drain_calls.append(kwargs) or
            (pending if len(drain_calls) == 1 else []))
        daemon = SimpleNamespace(poll=lambda: None)
        self.ops.daemon = daemon
        shutdown_calls = []
        self.ops.safe_failure_shutdown = lambda: shutdown_calls.append(True) or True

        with patch("acceptance_provider_common.time.sleep") as sleep:
            self.assertTrue(self.ops.retain_failure_ownership())

        self.assertEqual(len(drain_calls), 2)
        self.assertEqual(drain_calls[0]["exclude"], (daemon,))
        self.assertEqual(shutdown_calls, [True])
        sleep.assert_called_once_with(0.25)

    def test_retain_failure_ownership_stops_at_finite_observation_deadline(self):
        pending = [{"pid": 17, "argv": ["caller"], "directory": "/owned/caller"}]
        self.ops.processes = SimpleNamespace(drain=lambda **_kwargs: pending)
        self.ops.daemon = SimpleNamespace(poll=lambda: None)

        with patch.object(provider_common, "failure_cleanup_deadline", return_value=10.0), \
                patch.object(provider_common, "wait_for_failure_cleanup", return_value=False):
            self.assertFalse(self.ops.retain_failure_ownership())

        receipt = read_json(self.output / "failure-cleanup-expired.json")
        self.assertEqual(receipt["reason"],
                         "owned processes or private queue did not finish naturally")
        self.assertEqual(receipt["signals_sent"], 0)

    def test_retain_failure_ownership_does_not_pass_unknown_queue(self):
        self.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [])
        states = iter((None, 0))
        self.ops.daemon = SimpleNamespace(poll=lambda: next(states))
        self.ops.safe_failure_shutdown = lambda: False

        with patch("acceptance_provider_common.time.sleep") as sleep, \
                patch("acceptance_provider_common.time.monotonic",
                      side_effect=(10.0, 10.1, 10.2, 10.3, 10.4)):
            self.assertFalse(self.ops.retain_failure_ownership())

        self.assertEqual(sleep.call_count, 1)
        self.assertGreater(sleep.call_args.args[0], 0.0)
        self.assertAlmostEqual(sleep.call_args.args[0], 0.15, places=3)

    def test_retain_failure_ownership_retries_failed_completion_receipt(self):
        class FakePopen:
            def __init__(self, *_args, **_kwargs):
                self.pid = 702
                self.returncode = None

            def poll(self):
                return self.returncode

            def wait(self):
                return self.returncode

        original_write_json = supervisor_common.write_json
        failed = True

        def fail_first_completion(path, *args, **kwargs):
            nonlocal failed
            if Path(path).name == "completed.json" and failed:
                failed = False
                raise OSError("injected completion receipt failure")
            return original_write_json(path, *args, **kwargs)

        processes = supervisor_common.Processes(self.base / "processes", {"PATH": "/usr/bin"})
        daemon = SimpleNamespace(poll=lambda: None)
        ops = NativeTaskOps(processes, self.base / "delegate", self.runner, self.pueue,
                            self.state_parent, self.state, self.output)
        ops.bind_supervisor(self.config, daemon)
        ops.safe_failure_shutdown = lambda: True

        with patch.object(supervisor_common.subprocess, "Popen", FakePopen), \
                patch.object(supervisor_common, "write_json", side_effect=fail_first_completion):
            process = processes.start("caller", ["/bin/sh"], self.base)
            process.process.returncode = 0
            with patch("acceptance_provider_common.time.sleep") as sleep:
                self.assertTrue(ops.retain_failure_ownership())

        self.assertFalse(failed)
        self.assertTrue((process.directory / "completed.json").is_file())
        sleep.assert_called_once_with(0.25)


if __name__ == "__main__":
    unittest.main()
