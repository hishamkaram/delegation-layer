"""Pure managed-lifetime and no-launch fence validation over supplied facts."""

from __future__ import annotations

from datetime import datetime
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.timestamps import (
    _canonical_timestamp,
    _timestamp_at_or_after,
    _timestamp_before,
)
from scripts.delegate_acceptance.evidence.validation import _exact_keys, _hex_digest, require
from scripts.delegate_acceptance.lifetime.identity import _lifetime_token


def _validate_lifetime_intent(
        *, created_floor: tuple[datetime, int] | None,
        evidence: str,
        expected_deadline: str | None,
        intent: dict[str, object],
        owner_digest: str,
        owner_kind: str,
        recovered: bool,
        root: str,
        task: str,
        window_deadline: tuple[datetime, int] | None,
) -> tuple[str, str, tuple[datetime, int]]:
    _exact_keys(intent, {
        "schema_version", "root_id", "task_id", "owner_kind", "owner_sha256",
        "scope_kind", "assurance", "scope_plan", "boot_id", "deadline", "created_at",
    }, label=f"{evidence} lifetime intent for {task}")
    require(type(intent.get("schema_version")) is int and intent["schema_version"] == 1 and
            intent.get("root_id") == root and intent.get("task_id") == task and
            intent.get("owner_kind") == owner_kind and
            intent.get("owner_sha256") == owner_digest,
            f"{evidence} lifetime intent binding mismatch for {task}")
    scope_kind = intent.get("scope_kind")
    assurance = intent.get("assurance")
    require((scope_kind == "cgroup-v2" and assurance == "exact") or
            (scope_kind == "process-group" and assurance == "cooperative"),
            f"{evidence} lifetime intent scope is invalid for {task}")
    intent_deadline = _canonical_timestamp(
        intent.get("deadline"), f"{evidence} lifetime intent deadline for {task}")
    require(_lifetime_token(intent.get("scope_plan")) and
            _lifetime_token(intent.get("boot_id")) and
            (expected_deadline is None or intent.get("deadline") == expected_deadline) and
            (window_deadline is None or intent_deadline == window_deadline),
            f"{evidence} lifetime intent facts are invalid for {task}")
    intent_created = _canonical_timestamp(
        intent.get("created_at"), f"{evidence} lifetime intent created_at for {task}")
    require(recovered or ((created_floor is None or _timestamp_at_or_after(intent_created, created_floor)) and
            (window_deadline is None or _timestamp_before(intent_created, window_deadline))),
            f"{evidence} lifetime intent time is invalid for {task}")
    return scope_kind, assurance, intent_created


def _validate_lifetime_registration(
        *, assurance: str,
        evidence: str,
        intent: dict[str, object],
        intent_created: tuple[datetime, int],
        owner_digest: str,
        owner_kind: str,
        recovered: bool,
        registration: dict[str, object],
        root: str,
        scope_kind: str,
        task: str,
        window_deadline: tuple[datetime, int] | None,
) -> tuple[datetime, int]:
    _exact_keys(registration, {
        "schema_version", "root_id", "task_id", "owner_kind", "owner_sha256",
        "scope_kind", "assurance", "scope_id", "boot_id", "deadline", "created_at",
    }, label=f"{evidence} lifetime registration for {task}")
    require(type(registration.get("schema_version")) is int and
            registration["schema_version"] == 1 and
            registration.get("root_id") == root and registration.get("task_id") == task and
            registration.get("owner_kind") == owner_kind and
            registration.get("owner_sha256") == owner_digest and
            registration.get("scope_kind") == scope_kind and
            registration.get("assurance") == assurance and
            registration.get("boot_id") == intent.get("boot_id") and
            registration.get("deadline") == intent.get("deadline") and
            _lifetime_token(registration.get("scope_id")),
            f"{evidence} lifetime registration binding mismatch for {task}")
    registration_created = _canonical_timestamp(
        registration.get("created_at"), f"{evidence} lifetime registration created_at for {task}")
    require(recovered or (_timestamp_at_or_after(registration_created, intent_created) and
            (window_deadline is None or _timestamp_before(registration_created, window_deadline))),
            f"{evidence} lifetime registration time is invalid for {task}")
    return registration_created


def _validate_lifetime_wait(
        *, assurance: str,
        evidence: str,
        recovered: bool,
        registration_digest: str,
        root: str,
        task: str,
        wait: dict[str, object] | None,
        worker: dict[str, object] | None,
        worker_digest: str,
        worker_started: tuple[datetime, int],
) -> None:
    if wait is not None and not recovered:
        _exact_keys(wait, {
            "schema_version", "root_id", "task_id", "registration_sha256",
            "child_pid", "result", "observed_at",
        }, {"worker_sha256", "exit_code"}, label=f"{evidence} lifetime worker Wait for {task}")
        wait_result = wait.get("result")
        exit_code = wait.get("exit_code")
        worker_sha = wait.get("worker_sha256", "")
        child_pid = wait.get("child_pid")
        started_wait = (
            (wait_result == "success" and type(exit_code) is int and exit_code == 0) or
            (wait_result == "exit-error" and type(exit_code) is int and exit_code != 0) or
            (wait_result == "wait-error" and (exit_code is None or type(exit_code) is int)))
        not_started = (wait_result == "not-started" and worker_sha == "" and
                       child_pid == 0 and exit_code is None)
        require(type(wait.get("schema_version")) is int and wait["schema_version"] == 1 and
                wait.get("root_id") == root and wait.get("task_id") == task and
                wait.get("registration_sha256") == registration_digest and
                (worker_sha == "" or _hex_digest(worker_sha)) and
                type(child_pid) is int and (started_wait or not_started),
                f"{evidence} lifetime worker Wait binding mismatch for {task}")
        if worker is None:
            require(not_started or
                    (assurance == "exact" and worker_sha == "" and child_pid > 0),
                    f"{evidence} lifetime worker Wait binding mismatch for {task}")
        else:
            require(worker_sha == worker_digest and child_pid == worker["pid"] and started_wait,
                    f"{evidence} lifetime worker Wait binding mismatch for {task}")
        wait_observed = _canonical_timestamp(
            wait.get("observed_at"), f"{evidence} lifetime worker Wait observed_at for {task}")
        require(_timestamp_at_or_after(wait_observed, worker_started),
                f"{evidence} lifetime worker Wait time is invalid for {task}")


def _validate_lifetime_termination(
        *, evidence: str,
        launches: list[dict[str, object]],
        recovered: bool,
        registration_created: tuple[datetime, int],
        registration_digest: str,
        root: str,
        task: str,
        termination: dict[str, object],
        wait: dict[str, object] | None,
) -> None:
    _exact_keys(termination, {
        "schema_version", "root_id", "task_id", "registration_sha256", "launch_count",
        "launch_set_sha256", "cause", "scope_empty", "observed_at",
    }, label=f"{evidence} lifetime termination for {task}")
    canonical_launches = []
    for launch in launches:
        canonical_launch = {
            "schema_version": launch["schema_version"],
            "root_id": launch["root_id"],
            "task_id": launch["task_id"],
            "registration_sha256": launch["registration_sha256"],
            "sequence": launch["sequence"],
            "pid": launch["pid"],
            "process_start": launch["process_start"],
        }
        if launch.get("pgid", 0) != 0:
            canonical_launch["pgid"] = launch["pgid"]
        canonical_launch["started_at"] = launch["started_at"]
        canonical_launches.append(canonical_launch)
    launch_set_digest = sha(canonical_go_json(canonical_launches))
    require(type(termination.get("schema_version")) is int and
            termination["schema_version"] == 1 and
            termination.get("root_id") == root and termination.get("task_id") == task and
            termination.get("registration_sha256") == registration_digest and
            type(termination.get("launch_count")) is int and
            termination["launch_count"] == len(launches) and
            termination.get("launch_set_sha256") == launch_set_digest and
            termination.get("cause") in {
                "completed", "budget", "requested", "recovery", "previous-boot"} and
            termination.get("scope_empty") is True,
            f"{evidence} lifetime termination binding mismatch for {task}")
    terminated_at = _canonical_timestamp(
        termination.get("observed_at"), f"{evidence} lifetime termination observed_at for {task}")
    require(recovered or _timestamp_at_or_after(terminated_at, registration_created),
            f"{evidence} lifetime termination time is invalid for {task}")
    require(wait is not None or termination.get("cause") == "previous-boot",
            f"{evidence} lifetime worker Wait is missing for {task}")


def _canonical_unlaunched_bytes(
        value: object, root: str, operation: str, owner_digest: str, deadline: str,
        label: str = "workspace unlaunched fence", *, owner_kind: str = "workspace") -> bytes:
    """Validate the typed no-native-start proof and marshal its Go bytes."""
    require(isinstance(value, dict), f"{label} is not an object")
    required = {
        "schema_version", "root_id", "task_id", "owner_kind", "owner_sha256",
        "deadline", "cause", "observed_at",
    }
    _exact_keys(value, required, label=label)
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("root_id") == root and value.get("task_id") == operation and
            owner_kind in {"workspace", "inspection"} and
            value.get("owner_kind") == owner_kind and
            value.get("owner_sha256") == owner_digest and
            value.get("deadline") == deadline and
            value.get("cause") == "deadline-expired",
            f"{label} binding is invalid")
    deadline_value = _canonical_timestamp(value.get("deadline"), label + " deadline")
    observed = _canonical_timestamp(value.get("observed_at"), label + " observed_at")
    require(_timestamp_at_or_after(observed, deadline_value),
            f"{label} observed_at precedes deadline")
    canonical = {
        "schema_version": value["schema_version"],
        "root_id": value["root_id"],
        "task_id": value["task_id"],
        "owner_kind": value["owner_kind"],
        "owner_sha256": value["owner_sha256"],
        "deadline": value["deadline"],
        "cause": value["cause"],
        "observed_at": value["observed_at"],
    }
    return canonical_go_json(canonical)
