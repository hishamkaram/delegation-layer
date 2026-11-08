"""Read and validate the records sealed by one Antigravity task."""

from __future__ import annotations

import shlex
from pathlib import Path
from typing import TypedDict

from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import _optional_hex_digest
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.evidence.digests import sha
from scripts.acceptance.tasks.runtime_metadata import validate_provider_runtime_metadata
from scripts.acceptance.evidence.files import read_json
from scripts.acceptance.provider_drivers.agy.constants import CANONICAL_TASK_BUDGET
from scripts.acceptance.provider_drivers.agy.constants import L3_NATIVE_TIMEOUT
from scripts.acceptance.provider_drivers.agy.constants import MODE
from scripts.acceptance.provider_drivers.agy.constants import PREDICATE_SHA256
from scripts.acceptance.provider_drivers.agy.constants import PREDICATE_VERSION
from scripts.acceptance.provider_drivers.agy.constants import PROVIDER
from scripts.acceptance.provider_drivers.agy.queue_observation import row_state
from scripts.acceptance.provider_drivers.agy.runtime import provider_runtime_roots


class TaskEvidence(TypedDict):
    meta: dict[str, object]
    provider_exit: dict[str, object]
    outcome: dict[str, object]
    payload: bytes
    events: dict[str, int]


def directory_snapshot(
        directory: Path, ignored_prefixes: tuple[str, ...] = ()) -> dict[str, dict[str, object]]:
    require(directory.is_dir() and not directory.is_symlink(), f"evidence directory is absent: {directory}")
    snapshot: dict[str, dict[str, object]] = {}
    for path in sorted(directory.rglob("*")):
        if path.is_symlink():
            raise AcceptanceFailure(f"evidence contains symlink: {path}")
        if not path.is_file() or path.name.endswith(".staging") or any(
                path.name.startswith(prefix) for prefix in ignored_prefixes):
            continue
        data = path.read_bytes()
        relative = str(path.relative_to(directory))
        snapshot[relative] = {"bytes": len(data), "sha256": sha(data)}
    return snapshot


def immutable_task_snapshot(state: Path, task: str) -> dict[str, dict[str, object]]:
    directory = state / "tasks" / task
    require(directory.is_dir() and not directory.is_symlink(), f"task directory is absent: {task}")
    return directory_snapshot(directory, ignored_prefixes=(".ack",))


def read_task_record(state: Path, task: str, name: str) -> dict[str, object]:
    value = read_json(state / "tasks" / task / name)
    require(isinstance(value, dict), f"{task}/{name} must be an object")
    return value


def validate_task_request(name: str, task: str, request: dict[str, object], root_id: str) -> None:
    require(request.get("root_id") == root_id and request.get("task_id") == task and
            request.get("provider") == PROVIDER and request.get("mode") == MODE,
            f"{name} task identity or provider binding mismatch")
    requested = request.get("requested_config")
    if (not isinstance(requested, dict) or requested.get("budget") != CANONICAL_TASK_BUDGET or
            request.get("budget_nanos") != 120_000_000_000):
        raise AcceptanceFailure(f"{name} outer task budget is not 120s")
    expected_timeout = L3_NATIVE_TIMEOUT if name == "L3" else None
    if expected_timeout is None:
        require(requested.get("native_timeout") in (None, ""),
                f"{name} unexpectedly requested native timeout")
    else:
        require(requested.get("native_timeout") == expected_timeout,
                f"{name} native timeout is not 3s")


def validate_submit_record(
        *, name: str, task: str, submit: dict[str, object], root_id: str, label: str,
        pueue_config: Path, pueue_version: str, pueue: Path) -> dict[str, object]:
    require(submit.get("root_id") == root_id and submit.get("task_id") == task and
            submit.get("label") == label, f"{name} submit evidence identity mismatch")
    supervisor = submit.get("supervisor")
    if (not isinstance(supervisor, dict) or supervisor.get("config_path") != str(pueue_config) or
            supervisor.get("observed_version") != pueue_version or
            supervisor.get("client_executable") != str(pueue)):
        raise AcceptanceFailure(f"{name} submit supervisor binding mismatch")
    return supervisor


def validate_start_records(
        name: str, task: str, start: dict[str, object], started: dict[str, object], root_id: str) -> None:
    budget = start.get("budget_nanos")
    diagnostic = started.get("diagnostic_nanos")
    require(start.get("root_id") == root_id and start.get("task_id") == task and
            isinstance(budget, int) and budget > 0,
            f"{name} provider.start evidence identity mismatch")
    require(started.get("root_id") == root_id and started.get("task_id") == task and
            isinstance(diagnostic, int) and diagnostic >= 0,
            f"{name} provider.started evidence identity mismatch")


def validate_meta_supervisor(
        name: str, meta: dict[str, object], supervisor: dict[str, object]) -> dict[str, object]:
    require(meta.get("supervisor_config") == supervisor,
            f"{name} meta supervisor binding differs from submit evidence")
    predicate = meta.get("predicate")
    if (not isinstance(predicate, dict) or predicate.get("adapter") != PROVIDER or
            predicate.get("mode") != MODE or predicate.get("version") != PREDICATE_VERSION or
            predicate.get("sha256") != PREDICATE_SHA256):
        raise AcceptanceFailure(f"{name} predicate contract mismatch")
    return predicate


def validate_effective_policy(
        name: str, meta: dict[str, object], workspace: Path) -> tuple[dict[str, object], str]:
    validate_provider_runtime_metadata(meta, label=f"{name} provider runtime metadata")
    effective = meta.get("effective_config")
    if (not isinstance(effective, dict) or effective.get("containment") != MODE or
            not isinstance(effective.get("digest"), str) or not effective.get("digest")):
        raise AcceptanceFailure(f"{name} effective policy is absent")
    policy = effective.get("policy")
    if (not isinstance(policy, dict) or policy.get("workspace") != str(workspace) or
            not _optional_hex_digest(policy.get("runtime_sha256"))):
        raise AcceptanceFailure(f"{name} effective policy binding mismatch")
    profile_revision = policy.get("profile_revision")
    if not isinstance(profile_revision, str) or not profile_revision:
        raise AcceptanceFailure(f"{name} effective policy revision is absent")
    return policy, profile_revision


def validate_policy_sources(
        name: str, policy: dict[str, object], environment: dict[str, str], profile_revision: str) -> None:
    writable_roots = policy.get("writable_roots")
    expected_roots = [str(root) for root in provider_runtime_roots(environment)]
    require(writable_roots == expected_roots,
            f"{name} persisted writable runtime roots differ from the inherited environment")
    require(profile_revision == "native-permissions-v1" and not policy.get("sources"),
            f"{name} unexpectedly inventoried provider configuration")


def validate_provider_exit(
        *, name: str, task: str, meta: dict[str, object], started: dict[str, object],
        provider_exit: dict[str, object],
        root_id: str, predicate: dict[str, object], expected_verdict: str) -> None:
    launch_identity = validate_provider_runtime_metadata(
        meta, launch_evidence=(started, provider_exit),
        label=f"{name} provider runtime metadata")
    require(isinstance(launch_identity, dict),
            f"{name} provider launch identity is unavailable")
    require(provider_exit.get("root_id") == root_id and provider_exit.get("task_id") == task and
            provider_exit.get("invocation_state") == "started" and
            provider_exit.get("predicate") == predicate,
            f"{name} provider.exit identity mismatch")
    if expected_verdict == "committed":
        require(provider_exit.get("exit_code") == 0 and provider_exit.get("error") == "",
                f"{name} committed despite a provider exit fault")


def validate_outcome(
        name: str, task: str, expected_verdict: str, directory: Path,
        outcome: dict[str, object], provider_exit: dict[str, object]) -> bytes:
    require(outcome.get("task_id") == task and outcome.get("verdict") == expected_verdict,
            f"{name} outcome verdict mismatch")
    payload = outcome.get("payload")
    if not isinstance(payload, dict) or not isinstance(payload.get("basename"), str):
        raise AcceptanceFailure(f"{name} payload descriptor missing")
    payload_path = directory / payload["basename"]
    require(payload_path.is_file() and not payload_path.is_symlink(), f"{name} payload is absent")
    payload_bytes = payload_path.read_bytes()
    require(len(payload_bytes) == payload.get("length") and sha(payload_bytes) == payload.get("sha256"),
            f"{name} payload digest mismatch")
    require((directory / "raw" / "stdout").is_file() and
            (directory / "raw" / "stderr").is_file(), f"{name} raw streams are absent")
    raw_manifest = provider_exit.get("raw_manifest")
    require(isinstance(raw_manifest, list) and
            {entry.get("path") for entry in raw_manifest if isinstance(entry, dict)} ==
            {"raw/stdout", "raw/stderr"}, f"{name} raw manifest is incomplete")
    return payload_bytes


def read_inspection_records(
        state: Path, expected: set[str]) -> dict[str, dict[str, object]]:
    """Load the immutable request/receipt pair for each runtime probe."""
    parent = state / "inspections"
    if not parent.is_dir():
        return {}
    records: dict[str, dict[str, object]] = {}
    for directory in sorted(parent.iterdir()):
        if not directory.is_dir() or directory.is_symlink():
            continue
        task = directory.name
        request = read_json(directory / "request.json")
        receipt = read_json(directory / "receipt.json")
        require(isinstance(request, dict) and isinstance(receipt, dict),
                f"inspection record {task} is malformed")
        records[task] = {"request": request, "receipt": receipt}
    require(not expected or set(records) == expected,
            "runtime inspection records do not match admitted attempts")
    return records


def validate_inspection_queue_row(
        row: dict[str, object], record: dict[str, object], runner: Path, state: Path) -> None:
    request = record["request"]
    receipt = record["receipt"]
    if not isinstance(request, dict) or not isinstance(receipt, dict):
        raise AcceptanceFailure("inspection record is malformed")
    task = request.get("task_id")
    label = request.get("label")
    group = request.get("group")
    number = receipt.get("numeric_task_id")
    require(isinstance(task, str) and isinstance(label, str) and isinstance(group, str) and
            type(number) is int and number >= 0, "inspection identity is malformed")
    require(row.get("id") == number and row.get("label") == label and row.get("group") == group,
            f"inspection queue identity mismatch for {task}")
    require(row_state(row) == "Done", f"inspection worker is not Done for {task}")
    status = row.get("status")
    require(isinstance(status, dict) and isinstance(status.get("Done"), dict) and
            status["Done"].get("result") == "Success",
            f"inspection worker did not succeed for {task}")
    expected = [str(runner), "--inspection", "--guardian", "--supervised",
                "--root", str(state), task]
    for key in ("original_command", "command"):
        command = row.get(key)
        if not isinstance(command, str):
            raise AcceptanceFailure(f"inspection queue row missing {key} for {task}")
        try:
            parsed = shlex.split(command)
        except ValueError as error:
            raise AcceptanceFailure(f"inspection queue argv is malformed for {task}") from error
        require(parsed == expected, f"inspection queue argv mismatch for {task}")
