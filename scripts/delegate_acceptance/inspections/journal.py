"""Read and bind inspection scheduling, result, and closure evidence."""

from __future__ import annotations

from datetime import datetime, timedelta
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.evidence.records import (
    _read_control_record,
    _read_optional_control_record,
)
from scripts.delegate_acceptance.evidence.timestamps import (
    _canonical_timestamp,
    _timestamp_at_or_after,
    _timestamp_before,
)
from scripts.delegate_acceptance.evidence.validation import _exact_keys, require
from scripts.delegate_acceptance.inspections.authority import _inspection_exit_authority
from scripts.delegate_acceptance.inspections.binding import (
    ADMISSION_INSPECTION_TIMEOUT_SECONDS,
    MODEL_DISCOVERY_TIMEOUT_SECONDS,
    _inspection_group_parallelism,
    _inspection_label,
    _validate_claim_record,
    _validate_inspection_binding,
)
from scripts.delegate_acceptance.inspections.data import InspectionCompletion, InspectionRequest
from scripts.delegate_acceptance.inspections.result_policy import (
    _validate_inspection_result,
    _validate_inspection_verdict,
)
from scripts.delegate_acceptance.supervisor.binding import observed_pueue_version
from scripts.delegate_acceptance.supervisor.config_policy import resolved_supervisor_config_digest
from scripts.delegate_acceptance.tasks.specification import _validate_task_record
from scripts.delegate_acceptance.workspaces.admission_evidence import (
    _inspection_request_identity,
    _validate_historical_workspace_attempt,
)
from pathlib import Path


def validate_inspection_evidence(
        *, state: Path, root: str, root_created_at: tuple[datetime, int] | None,
        runner: Path, pueue: Path, pueue_config: Path | None,
        inspection_bindings: dict[str, dict[str, object]], task: str,
        directory: Path, allow_failure: bool,
        expected_binding_required: bool = True) -> dict[str, object]:
    request = _read_inspection_request(
        directory=directory, expected_binding_required=expected_binding_required, inspection_bindings=inspection_bindings,
        pueue=pueue,
        pueue_config=pueue_config, root=root, root_created_at=root_created_at, runner=runner, state=state, task=task)
    request_digest = digest(request.request_path)
    authority = _inspection_exit_authority(
        directory=directory, root=root, task=request.operation_id, binding=request.binding, request_digest=request_digest,
        request_created=request.created, request_deadline=request.request["deadline"], deadline=request.deadline)
    managed_lifetime = authority["managed_lifetime"]
    require(isinstance(managed_lifetime, dict) or managed_lifetime is None,
            f"inspection exit authority is invalid for {task}")
    historical_unlaunched = (
        request.historical_aborted and authority["mode"] == "guardian-unlaunched")
    completion = _read_inspection_completion(
        authority=authority, created=request.created, deadline=request.deadline, directory=directory,
        historical_unlaunched=historical_unlaunched, managed_lifetime=managed_lifetime, ordinary_matches=request.ordinary_matches,
        request_digest=request_digest, task=task)
    _validate_inspection_completion(
        completion=completion.completion, completion_path=directory / "completion.json", created=request.created,
        deadline=request.deadline, observation=completion.observation, receipt_id=completion.receipt_id,
        request_digest=request_digest, result=completion.result, result_path=directory / "result.json",
        start_created=completion.start_created, task=task)
    _validate_inspection_verdict(
        allow_failure=allow_failure, authority=authority, completion=completion.completion, deadline=request.deadline,
        historical_aborted=request.historical_aborted, managed_lifetime=managed_lifetime,
        managed_worker_succeeded=completion.managed_worker_succeeded, observation=completion.observation,
        ordinary_matches=request.ordinary_matches, result=completion.result, standalone=request.standalone, task=task)
    return {
        "task_id": task, "operation_id": request.operation_id,
        "identity_id": request.operation_id, "root_id": root,
        "group": request.group, "group_parallelism": request.group_parallelism,
        "label": request.label, "numeric_task_id": completion.receipt_id,
        "request": request.request, "request_digest": request_digest,
        "receipt": completion.receipt, "submission": completion.submission,
        "start": completion.start, "result": completion.result,
        "completion": completion.completion,
        "worker_observation": completion.observation,
        "managed_lifetime": managed_lifetime,
        "inspection_exit_authority": authority,
        "ordinary_task_present": request.ordinary_present,
        "ordinary_task_matches": request.ordinary_matches,
        "historical_aborted": request.historical_aborted,
        "attempt": request.attempt,
    }


def _ordinary_inspection_binding(
        embedded_task: dict[str, object],
        identity: dict[str, object],
        root: str,
        standalone: bool,
        state: Path,
        task: str,
        task_digest: str,
) -> tuple[bool, bool, dict[str, object] | None, bool]:
    ordinary_directory = state / "tasks" / task
    ordinary_present = ordinary_directory.exists() or ordinary_directory.is_symlink()
    ordinary_matches = False
    attempt = identity["attempt"]
    closure = attempt.get("closure") if isinstance(attempt, dict) else None
    historical_aborted = (isinstance(closure, dict) and
                          closure.get("disposition") == "aborted-not-launched")
    if ordinary_present:
        require(ordinary_directory.is_dir() and not ordinary_directory.is_symlink(),
                f"ordinary task directory is not a regular directory for {task}")
        ordinary_task_path = ordinary_directory / "task.json"
        ordinary_task = _read_control_record(ordinary_task_path,
                                             f"ordinary task record for {task}")
        ordinary_digest = _validate_task_record(ordinary_task, root, task,
                                                f"ordinary task record for {task}")
        require(ordinary_digest == digest(ordinary_task_path),
                f"ordinary task record is not canonical for {task}")
        if embedded_task == ordinary_task and task_digest == ordinary_digest:
            require(not historical_aborted,
                    f"ordinary task exists for an aborted admission for {task}")
            ordinary_matches = True
        else:
            require(not standalone,
                    f"standalone discovery admitted a changed ordinary task for {task}")
            _validate_historical_workspace_attempt(
                state, attempt, ordinary_task, root,
                f"inspection task history for {task}")
            historical_aborted = True
    require(not ordinary_present or not standalone,
            f"standalone discovery admitted an ordinary task for {task}")
    return ordinary_present, ordinary_matches, attempt, historical_aborted


def _validate_inspection_sources(
        binding: dict[str, object],
        expected_binding_required: bool,
        inspection_bindings: dict[str, dict[str, object]],
        pueue: Path,
        pueue_config: Path | None,
        runner: Path,
        task: str,
) -> None:
    expected_binding = inspection_bindings.get(task)
    if expected_binding_required:
        require(expected_binding is not None,
                f"inspection request has no expected source binding for {task}")
        require(binding == expected_binding,
                f"inspection request binding changed from expected source for {task}")
    require(binding.get("worker_executable") == str(runner),
            f"inspection request worker mismatch for {task}")
    require(binding.get("worker_sha256") == digest(runner),
            f"inspection request worker digest mismatch for {task}")
    supervisor = binding.get("supervisor")
    require(pueue_config is not None, "inspection supervisor config is unavailable")
    require(supervisor.get("client_executable") == str(pueue) and
            supervisor.get("client_sha256") == digest(pueue) and
            supervisor.get("config_path") == str(pueue_config) and
            supervisor.get("config_digest") == digest(pueue_config),
            f"inspection request supervisor binding mismatch for {task}")
    supervisor_base = pueue_config.parent
    require(supervisor.get("resolved_config_sha256") ==
            resolved_supervisor_config_digest(supervisor_base) and
            supervisor.get("endpoint") ==
            "unix:" + str(supervisor_base / "run" / "p.sock"),
            f"inspection request supervisor generation mismatch for {task}")
    observed_pueue_version(supervisor.get("observed_version"), "pueue")


def _validate_inspection_completion(
        *, completion: dict[str, object] | None,
        completion_path: Path,
        created: tuple[datetime, int],
        deadline: tuple[datetime, int],
        observation: dict[str, object] | None,
        receipt_id: int | None,
        request_digest: str,
        result: dict[str, object] | None,
        result_path: Path,
        start_created: tuple[datetime, int] | None,
        task: str,
) -> None:
    if completion is not None:
        _exact_keys(completion, {
            "schema_version", "request_sha256", "result_sha256", "completed_at", "native_exit",
        }, label=f"inspection completion for {task}")
        require(result is not None and type(completion.get("schema_version")) is int and
                completion["schema_version"] == 1 and
                completion.get("request_sha256") == request_digest and
                completion.get("result_sha256") == digest(result_path),
                f"inspection completion binding mismatch for {task}")
        require((result.get("reason") == "eligible" and
                 completion.get("native_exit") == "successful-exit") or
                (result.get("reason") != "eligible" and
                 completion.get("native_exit") == "unavailable"),
                f"inspection completion exit disagrees with result for {task}")
        require(completion.get("native_exit") in {"successful-exit", "unavailable"},
                f"inspection completion exit classification is invalid for {task}")
        completed = _canonical_timestamp(completion.get("completed_at"),
                                         f"inspection completion for {task}")
        require(_timestamp_at_or_after(completed, created),
                f"inspection completion precedes request for {task}")
        require(start_created is not None and _timestamp_at_or_after(completed, start_created),
                f"inspection completion precedes start for {task}")
        if result.get("reason") == "eligible":
            require(_timestamp_before(completed, deadline),
                    f"eligible inspection completed after deadline for {task}")
    if observation is not None:
        _exact_keys(observation, {
            "schema_version", "request_sha256", "completion_sha256", "numeric_task_id", "state",
        }, label=f"inspection worker observation for {task}")
        require(type(observation.get(
            "schema_version",
        )) is int and observation["schema_version"] == 1 and
                observation.get("request_sha256") == request_digest and
                isinstance(completion, dict) and
                observation.get("completion_sha256") == digest(completion_path) and
                observation.get("numeric_task_id") == receipt_id and
                type(observation.get("numeric_task_id")) is int and
                observation.get("state") == "succeeded",
                f"inspection worker observation binding mismatch for {task}")




def _read_inspection_completion(
        *, authority: dict[str, object],
        created: tuple[datetime, int],
        deadline: tuple[datetime, int],
        directory: Path,
        historical_unlaunched: bool,
        managed_lifetime: dict[str, object] | None,
        ordinary_matches: bool,
        request_digest: str,
        task: str,
) -> InspectionCompletion:
    receipt, receipt_id, submission = _read_inspection_scheduling(directory, historical_unlaunched, request_digest, task)
    submission_created = created if submission is None else _validate_claim_record(
        submission, request_digest, f"inspection submission for {task}", created, deadline)

    result_path = directory / "result.json"
    completion_path = directory / "completion.json"
    observation_path = directory / "worker-observation.json"
    result = _read_optional_control_record(result_path, f"inspection result for {task}")
    completion = _read_optional_control_record(completion_path, f"inspection completion for {task}")
    observation = _read_optional_control_record(
        observation_path, f"inspection worker observation for {task}",
    )
    start = _read_optional_control_record(directory / "start.json",
                                          f"inspection start for {task}")
    if authority["mode"] == "guardian-unlaunched":
        require(start is None and result is None and completion is None and observation is None,
                f"unlaunched inspection has scheduling records for {task}")
    managed_worker_succeeded = False
    if authority["mode"] in {"guardian", "guardian-unstarted"}:
        require(observation is None,
                f"managed inspection has conflicting queue observation for {task}")
    if authority["mode"] == "guardian":
        managed_wait = managed_lifetime["worker_wait"]
        # The shared lifetime validator owns valid missing-Wait recovery
        # variants. A validated absence is never projected as success.
        managed_worker_succeeded = (isinstance(managed_wait, dict) and
                                    managed_wait.get("result") == "success")
    if result is not None or completion is not None or observation is not None or ordinary_matches:
        require(start is not None, f"inspection start guard is missing for {task}")
    start_created: tuple[datetime, int] | None = None
    if start is not None:
        start_created = _validate_claim_record(
            start, request_digest, f"inspection start for {task}", created, deadline)
        require(_timestamp_at_or_after(start_created, submission_created),
                f"inspection start precedes submission for {task}")
    _validate_inspection_result(completion, observation, request_digest, result, task)
    return InspectionCompletion(
        receipt=receipt,
        receipt_id=receipt_id,
        submission=submission,
        start=start,
        result=result,
        completion=completion,
        observation=observation,
        managed_worker_succeeded=managed_worker_succeeded,
        start_created=start_created,
    )


def _read_inspection_request(
        *, directory: Path,
        expected_binding_required: bool,
        inspection_bindings: dict[str, dict[str, object]],
        pueue: Path,
        pueue_config: Path | None,
        root: str,
        root_created_at: tuple[datetime, int] | None,
        runner: Path,
        state: Path,
        task: str,
) -> InspectionRequest:
    request_path = directory / "request.json"
    request = _read_control_record(request_path, f"inspection request for {task}")
    identity = _inspection_request_identity(
        state, root, request, directory.name,
        f"inspection request for {task}")
    require(identity["task_id"] == task,
            f"inspection request logical task mismatch for {task}")
    operation_id = identity["operation_id"]
    label = _inspection_label(root, operation_id)
    embedded_task = identity["task"]
    embedded_digest = identity["task_sha256"]
    task_digest = request.get("task_sha256")
    require(task_digest == embedded_digest,
            f"inspection request task digest changed for {task}")
    created = _canonical_timestamp(request.get("created_at"),
                                   f"inspection request created_at for {task}")
    deadline = _canonical_timestamp(request.get("deadline"),
                                    f"inspection request deadline for {task}")
    require(root_created_at is not None and
            _timestamp_at_or_after(created, root_created_at),
            f"inspection request precedes root creation for {task}")
    binding = request.get("binding")
    _validate_inspection_binding(binding, f"inspection request binding for {task}")
    group = request.get("group")
    group_parallelism = _inspection_group_parallelism(
        state, root, binding, group, f"inspection request for {task}")
    require(request.get("label") == label,
            f"inspection request label mismatch for {task}")
    standalone = binding.get("definition_revision") == "model-discovery-v1"
    timeout_seconds = (MODEL_DISCOVERY_TIMEOUT_SECONDS if standalone
                       else ADMISSION_INSPECTION_TIMEOUT_SECONDS)
    require(deadline[0] - created[0] in {timedelta(seconds=value) for value in timeout_seconds} and
            deadline[1] == created[1],
            f"inspection request deadline is not exactly {timeout_seconds[0]} seconds for {task}")

    ordinary_present, ordinary_matches, attempt, historical_aborted = _ordinary_inspection_binding(
        embedded_task, identity, root, standalone, state, task, task_digest,
    )
    _validate_inspection_sources(
        binding, expected_binding_required, inspection_bindings, pueue, pueue_config, runner, task,
    )
    return InspectionRequest(
        request_path=request_path,
        request=request,
        operation_id=operation_id,
        label=label,
        created=created,
        deadline=deadline,
        binding=binding,
        group=group,
        group_parallelism=group_parallelism,
        standalone=standalone,
        ordinary_present=ordinary_present,
        ordinary_matches=ordinary_matches,
        attempt=attempt,
        historical_aborted=historical_aborted,
    )


def _read_inspection_scheduling(
        directory: Path,
        historical_unlaunched: bool,
        request_digest: str,
        task: str,
) -> tuple[dict[str, object] | None, int | None, dict[str, object] | None]:
    if historical_unlaunched:
        receipt = _read_optional_control_record(
            directory / "receipt.json", f"inspection receipt for {task}")
        submission = _read_optional_control_record(
            directory / "submission.json", f"inspection submission for {task}")
    else:
        receipt = _read_control_record(
            directory / "receipt.json", f"inspection receipt for {task}")
        submission = _read_control_record(
            directory / "submission.json", f"inspection submission for {task}")
    require(receipt is None or submission is not None,
            f"inspection scheduling records are incomplete for {task}")
    receipt_id = None
    if receipt is not None:
        _exact_keys(receipt, {"schema_version", "request_sha256", "numeric_task_id"},
                    label=f"inspection receipt for {task}")
        receipt_id = receipt.get("numeric_task_id")
        require(type(receipt.get("schema_version")) is int and receipt["schema_version"] == 1 and
                receipt.get("request_sha256") == request_digest and
                isinstance(receipt_id, int) and not isinstance(
                    receipt_id, bool,
                ) and receipt_id >= 0,
                f"inspection receipt binding mismatch for {task}")
    return receipt, receipt_id, submission
