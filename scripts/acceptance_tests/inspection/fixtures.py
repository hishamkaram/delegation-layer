"""Hermetic inspection queue and journal checks for the native acceptance harness."""

from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace
import shlex
import shutil
import tempfile
import unittest
from unittest.mock import Mock

from scripts import acceptance_provider_common as provider_common
from scripts.acceptance_provider_common import NativeTaskOps, _inspection_group
from scripts.acceptance_supervisor_common import digest, read_json, write_json


ROOT = "a" * 32
TASK = "b" * 32


class InspectionFixture:
    def __init__(self, check: unittest.TestCase):
        self.check = check
        self.temporary = tempfile.TemporaryDirectory()
        check.addCleanup(self.close)
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

    def close(self):
        self.temporary.cleanup()

    def _write_completed_inspection(
            self, directory, request, request_sha, result, managed_exit_proof):
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

    def inspection_records(self, *, result: str | None = "eligible",
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
            self._write_completed_inspection(
                directory, request, request_sha, result, managed_exit_proof,
            )
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
            self.check.assertIn(runner_ownership, {"managed", "custom"})
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

    def unlaunched_inspection(self, operation_id: str | None = None) -> Path:
        directory = self.inspection_records(
            result=None, include_ordinary=False, managed_exit_proof=True)
        for entry in tuple(directory.iterdir()):
            if entry.name != "request.json":
                entry.unlink()
        if operation_id is not None:
            request = read_json(directory / "request.json")
            request["operation_id"] = operation_id
            request["label"] = f"{_inspection_group(ROOT)}-{operation_id}"
            (directory / "request.json").unlink()
            write_json(directory / "request.json", request)
            directory = directory.rename(directory.with_name(operation_id))
        fence = {
            "schema_version": 1, "root_id": ROOT,
            "task_id": operation_id or TASK, "owner_kind": "inspection",
            "owner_sha256": digest(directory / "request.json"),
            "deadline": "2026-09-15T00:01:00Z", "cause": "deadline-expired",
            "observed_at": "2026-09-15T00:01:00Z",
        }
        (directory / "lifetime-unlaunched.json").write_bytes(
            provider_common.canonical_go_json(fence))
        return directory
