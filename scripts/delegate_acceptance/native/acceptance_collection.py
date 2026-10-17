"""Composed native acceptance behavior: collection."""

from __future__ import annotations

from collections.abc import Mapping

from copy import deepcopy
from pathlib import Path
from scripts.delegate_acceptance.evidence.files import digest, read_json
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, _hex_digest, _task_identity, require
from scripts.delegate_acceptance.inspections.binding import INSPECTION_GROUP_PREFIX
from scripts.delegate_acceptance.native.binding_policy import _validate_bound_provider_record
from scripts.delegate_acceptance.native.diagnostics import _rejection_diagnostic_summary
from scripts.delegate_acceptance.native.errors import BlockedFailure
from scripts.delegate_acceptance.native.fixtures import marker_answer, verify_interactive_answer, verify_read_only_answer, verify_review_answer, verify_write_fixture
from scripts.delegate_acceptance.native.acceptance_diagnostics import authentication_unavailable
from scripts.delegate_acceptance.native.workspace import task_digest_snapshot, workspace_snapshot
from scripts.delegate_acceptance.supervisor.queue_policy import done_result
from scripts.delegate_acceptance.tasks.publication import verify_collected_outcome
from scripts.delegate_acceptance.native.constants import CANONICAL_TASK_BUDGET, CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET, TASK_BUDGET_NANOS, TIMEOUT_RESUME_SOURCE_BUDGET_NANOS


def terminal_private_queue(owner, status: dict[str, object]) -> bool:
    acceptance = owner
    try:
        status = acceptance._without_model_discovery_queue(status)
        if acceptance.root_id is None:
            return acceptance._empty_private_queue(status)
        return acceptance._terminal_private_queue(status)
    except (AcceptanceFailure, KeyError, OSError, RuntimeError, TypeError,
            UnicodeError, ValueError, RecursionError):
        return False

def _without_model_discovery_queue(owner, status: dict[str, object]) -> dict[str, object]:
    """Exclude only the distinct model-inspection group from task shutdown."""
    acceptance = owner
    root_id = getattr(acceptance, "models_root_id", None)
    if not isinstance(root_id, str):
        return status
    rows = status.get("tasks")
    groups = status.get("groups")
    require(isinstance(rows, dict) and isinstance(groups, dict),
            "private queue status is malformed")
    model_group = INSPECTION_GROUP_PREFIX + root_id
    if model_group not in groups:
        return status
    retained_rows: dict[object, object] = {}
    model_rows: dict[object, object] = {}
    for numeric_id, row in rows.items():
        require(isinstance(row, dict), "private queue contains a malformed row")
        if row.get("group") == model_group:
            label = row.get("label")
            require(isinstance(label, str) and label.startswith(model_group + "-"),
                    "model discovery queue label is invalid")
            require(done_result(row) is not None,
                    "model discovery queue row is not terminal")
            model_rows[numeric_id] = row
            continue
        retained_rows[numeric_id] = row
    model_groups = {name: group for name, group in groups.items()
                    if name in {"default", model_group}}
    model_status = {"tasks": model_rows, "groups": model_groups}
    acceptance._validate_model_discovery_evidence(
        status if getattr(acceptance, "discover_models_only", False) else model_status,
        model_group)
    retained_groups = {name: group for name, group in groups.items()
                       if name != model_group}
    return {"tasks": retained_rows, "groups": retained_groups}

def _expected_inspection_attempts(owner) -> set[str]:
    """Project durable inspection expectations into the canonical owner."""
    acceptance = owner
    require(acceptance.root_id is not None, "root identity is unavailable")
    expected: set[str] = set()
    runtime_only: set[str] = set()
    for task_id in acceptance.tasks.values():
        require(_task_identity(task_id),
                f"ordinary task has an invalid inspection identity: {task_id}")
        metadata = read_json(acceptance.state / "tasks" / task_id / "meta.json")
        require(metadata.get("root_id") == acceptance.root_id and
                metadata.get("task_id") == task_id,
                f"task {task_id} metadata identity changed")
        if "runtime_preflight_sha256" in metadata:
            runtime_only.add(task_id)
        else:
            expected.add(task_id)

    acceptance.ops.inspection_attempts = expected
    parent = acceptance.state / "inspections"
    try:
        parent.lstat()
    except FileNotFoundError:
        return expected
    require(parent.is_dir() and not parent.is_symlink(),
            "inspection journal parent is not a regular directory")
    for path in parent.iterdir():
        require(_task_identity(path.name), "inspection journal has an invalid task identity")
        require(path.is_dir() and not path.is_symlink(),
                "inspection journal task entry is not a regular directory")
        require(path.name not in runtime_only,
                f"runtime-only task has an unexpected inspection journal: {path.name}")
    return expected

def _validate_inspection_runtime_bindings(
        owner, journals: dict[str, dict[str, object]]) -> None:
    """Correlate validated inspection bindings with admitted runtimes."""
    acceptance = owner
    for task_id, journal in journals.items():
        identity = acceptance._provider_bindings.identity_for(task_id)
        require(isinstance(identity, Mapping),
                f"inspection task has no admitted provider runtime identity: {task_id}")
        request = journal.get("request")
        binding = request.get("binding") if isinstance(request, dict) else None
        require(isinstance(binding, dict) and
                binding.get("helper_executable") == identity.get("executable") and
                binding.get("helper_sha256") == identity.get("sha256"),
                f"inspection helper binding does not match admitted runtime for {task_id}")

def _terminal_private_queue(owner, status: dict[str, object]) -> bool:
    acceptance = owner
    require(acceptance.root_id is not None, "root identity is unavailable")
    acceptance.ops.root_id = acceptance.root_id
    acceptance._expected_inspection_attempts()
    acceptance._validate_terminal_default_group(status)
    journals = acceptance.ops._validate_queue_membership(status, allow_failure=False)
    acceptance._validate_inspection_runtime_bindings(journals)
    return True

def collect(owner, name: str, task_id: str) -> dict[str, object]:
    acceptance = owner
    acceptance.ops.root_id = acceptance.root_id
    directory = acceptance.state / "tasks" / task_id
    try:
        response = acceptance.ops.collect(name, task_id, expected={0, 4})
    except (AcceptanceFailure, RuntimeError) as error:
        raise AcceptanceFailure(f"{name} collection failed: {error}") from error
    rejected = response.get("publication") == "rejected"
    try:
        outcome, published_payload = verify_collected_outcome(
            response, directory, "rejected" if rejected else "committed")
    except AcceptanceFailure:
        raise
    except (OSError, ValueError, TypeError, RecursionError, RuntimeError) as error:
        raise AcceptanceFailure(f"{name} collection evidence is malformed") from error

    try:
        provider_exit = read_json(directory / "provider.exit")
    except (OSError, ValueError, TypeError, RecursionError, RuntimeError) as error:
        raise AcceptanceFailure(f"{name} provider evidence is malformed") from error
    require(isinstance(provider_exit, dict), f"{name} provider evidence is malformed")
    require(provider_exit.get("root_id") == acceptance.root_id and
            provider_exit.get("task_id") == task_id and
            provider_exit.get("invocation_state") == "started" and
            isinstance(provider_exit.get("error"), str) and
            type(provider_exit.get("exit_code")) is int and
            isinstance(provider_exit.get("raw_manifest"), list) and
            isinstance(provider_exit.get("manifest_sha256"), str) and
            len(provider_exit["manifest_sha256"]) == 64 and
            all(character in "0123456789abcdef"
                for character in provider_exit["manifest_sha256"]) and
            outcome.get("evidence_sha256") == provider_exit.get("manifest_sha256"),
            f"{name} lacks sealed provider evidence")
    if not rejected:
        require(provider_exit.get("exit_code") == 0 and provider_exit.get("error") == "",
                f"{name} lacks sealed provider evidence")
    collection_payload = response.get("payload")
    require(isinstance(collection_payload, dict) and
            collection_payload == outcome.get("payload") and
            response.get("evidence_sha256") == outcome.get("evidence_sha256"),
            f"{name} collection receipt descriptors are inconsistent")
    if authentication_unavailable(directory):
        raise BlockedFailure(f"{acceptance.provider} authentication is unavailable")
    if rejected:
        raise AcceptanceFailure(
            f"{name} provider result rejected: {_rejection_diagnostic_summary(outcome)}")
    require(published_payload, f"{name} committed an empty response")
    if acceptance.scenario == "interactive":
        verify_interactive_answer(published_payload, name)
    elif acceptance.mode == "read-only":
        if acceptance.provider == "codex:review":
            verify_review_answer(published_payload, name)
        else:
            verify_read_only_answer(
                published_payload, marker_answer(acceptance.marker_text, continuation="resume" in name), name)
    acceptance.records[name] = {"task_id": task_id, "directory": directory,
                          "outcome": deepcopy(outcome),
                          "payload": deepcopy(collection_payload),
                          "snapshot": task_digest_snapshot(directory)}
    return response

def collect_timed_out_source(owner, task_id: str) -> dict[str, object]:
    acceptance = owner
    acceptance.ops.root_id = acceptance.root_id
    directory = acceptance.state / "tasks" / task_id
    response = acceptance.ops.collect("timeout-source-collect", task_id, expected={3, 4})
    if response.get("publication") == "rejected":
        verify_collected_outcome(response, directory, "rejected")
    else:
        require(response.get("publication") != "committed" and
                response.get("outcome") is None and
                not (directory / "outcome.json").exists(),
                "timed-out source fabricated a terminal outcome")
    require(not (directory / "result.txt").exists(),
            "timed-out source published answer evidence")
    return response

def _read_bound_provider_evidence(
        owner, task_id: str, directory: Path, name: str,
        meta_digest: str, spec_digest: object) -> dict[str, object]:
    acceptance = owner
    evidence = read_json(directory / name)
    return _validate_bound_provider_record(
        evidence, name, acceptance.root_id, task_id, meta_digest, spec_digest)

def _read_bound_provider_started_evidence(
        owner, task_id: str, directory: Path,
        meta_digest: str, spec_digest: object) -> dict[str, object]:
    acceptance = owner
    acceptance._read_bound_provider_evidence(
        task_id, directory, "provider.start", meta_digest, spec_digest)
    return acceptance._read_bound_provider_evidence(
        task_id, directory, "provider.started.json", meta_digest, spec_digest)

def _validate_provider_metadata_binding(
        owner, task_id: str, directory: Path,
        meta: dict[str, object], spec_digest: object) -> None:
    acceptance = owner
    require(meta.get("root_id") == acceptance.root_id and meta.get("task_id") == task_id and
            isinstance(spec_digest, str) and _hex_digest(spec_digest) and
            spec_digest == digest(directory / "task.json"),
            f"task {task_id} metadata binding is invalid")

def _request_binding(owner, task_id: str, require_launch_evidence: bool = False,
                     require_started_evidence: bool = False,
                     budget: str = CANONICAL_TASK_BUDGET,
                     budget_nanos: int = TASK_BUDGET_NANOS) -> tuple[
        Path, dict[str, object], dict[str, object], str, str, Path]:
    acceptance = owner
    require(not (require_launch_evidence and require_started_evidence),
            "provider launch evidence modes are mutually exclusive")
    directory = acceptance.state / "tasks" / task_id
    require(directory.is_dir() and not directory.is_symlink(),
            f"task {task_id} directory is absent")
    record = read_json(directory / "task.json")
    require(record.get("root_id") == acceptance.root_id and record.get("task_id") == task_id and
            record.get("provider") == acceptance.provider and record.get("mode") == acceptance.mode and
            record.get("canonical_cwd") == str(acceptance.workspace),
            f"task {task_id} changed its immutable binding")
    requested = record.get("requested_config")
    brief = directory / "brief.md"
    require(isinstance(requested, dict) and requested.get("permission") == acceptance.mode and
            requested.get("budget") == budget and
            record.get("budget_nanos") == budget_nanos and
            brief.is_file() and not brief.is_symlink() and
            record.get("brief_length") == brief.stat().st_size and
            record.get("brief_sha256") == digest(brief),
            f"task {task_id} request binding is invalid")
    if acceptance.scenario == "interactive":
        require(requested.get("interactive") is True,
                f"task {task_id} did not persist interactive mode")
    meta = read_json(directory / "meta.json")
    meta_digest = digest(directory / "meta.json")
    spec_digest = meta.get("spec_sha256")
    acceptance._validate_provider_metadata_binding(task_id, directory, meta, spec_digest)
    launch_evidence = None
    started_evidence = None
    if require_launch_evidence:
        launch_evidence = (
            acceptance._read_bound_provider_evidence(
                task_id, directory, "provider.started.json", meta_digest, spec_digest),
            read_json(directory / "provider.exit"),
        )
    elif require_started_evidence:
        started_evidence = acceptance._read_bound_provider_started_evidence(
            task_id, directory, meta_digest, spec_digest)
    identity = acceptance._record_provider_runtime_identity(
        task_id, meta, launch_evidence, started_evidence)
    if acceptance.provider_version is None:
        acceptance.provider_version = identity["version"]
        acceptance.write_binding(task_id, identity)
    return directory, record, meta, meta_digest, spec_digest, brief

def timeout_source_binding(owner, task_id: str) -> Path:
    acceptance = owner
    directory, record, _meta, _meta_digest, _spec_digest, _brief = acceptance._request_binding(
        task_id, require_started_evidence=True,
        budget=CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET,
        budget_nanos=TIMEOUT_RESUME_SOURCE_BUDGET_NANOS)
    requested = record.get("requested_config")
    args = getattr(acceptance, "args", None)
    requested_model = getattr(args, "model", None)
    requested_effort = getattr(args, "effort", None)
    require(isinstance(requested, dict) and requested.get("permission") == acceptance.mode and
            ((requested_model and requested.get("model") == requested_model) or
             (not requested_model and requested.get("model") in (None, ""))) and
            ((requested_effort and requested.get("effort") == requested_effort) or
             (not requested_effort and requested.get("effort") in (None, ""))) and
            record.get("prior_session") is None,
            "timed-out source request binding changed")
    reference = read_json(directory / "provider.ref.json")
    require(reference.get("root_id") == acceptance.root_id and reference.get("task_id") == task_id and
            reference.get("provider") == acceptance.provider and
            isinstance(reference.get("conversation_id"), str) and bool(reference["conversation_id"]),
            "timed-out source has no resumable provider session")
    require(not (directory / "outcome.json").exists() and not (directory / "result.txt").exists(),
            "timed-out source published terminal artifacts")
    return directory

def _timeout_workspace_snapshot(owner, resumed: bool) -> dict[str, dict[str, object]]:
    acceptance = owner
    require(acceptance.workspace_baseline is not None,
            "timeout-recovery workspace baseline is unavailable")
    expected = set(acceptance.workspace_baseline)
    expected.add(acceptance.timeout_state.marker_file.name)
    if resumed:
        expected.add(acceptance.timeout_state.resume_file.name)
    actual = workspace_snapshot(acceptance.workspace)
    require(set(actual) == expected,
            "timeout-recovery workspace contains an unexpected file")
    for name, snapshot_record in acceptance.workspace_baseline.items():
        require(actual.get(name) == snapshot_record,
                "timeout-recovery changed a harness-controlled workspace file")
    require(acceptance.timeout_state.marker_file.read_bytes() ==
            (acceptance.timeout_state.marker + "\n").encode(),
            "timeout source did not create its exact marker")
    if resumed:
        require(acceptance.timeout_state.resume_file.read_bytes() ==
                (acceptance.timeout_state.resume_marker + "\n").encode(),
                "resumed timeout task did not create its exact marker")
    return actual

def _workspace_write_snapshot(owner, continuation: bool) -> dict[str, dict[str, object]]:
    acceptance = owner
    require(acceptance.nonce is not None, "write acceptance nonce is unavailable")
    require(acceptance.workspace_baseline is not None,
            "write acceptance workspace baseline is unavailable")
    acceptance._validate_pi_runtime_roots()
    generated = {
        "acceptance-write-target.txt", "acceptance-write-check.txt",
        "acceptance-write-continue-check.txt",
    }
    require(generated.isdisjoint(acceptance.workspace_baseline),
            "write acceptance generated file was present in the baseline")
    expected = set(acceptance.workspace_baseline)
    expected.update({"acceptance-write-target.txt", "acceptance-write-check.txt"})
    if continuation:
        expected.add("acceptance-write-continue-check.txt")
    actual = workspace_snapshot(acceptance.workspace)
    expected.update(name for name in actual if acceptance._is_pi_runtime_entry(name))
    git_directory = acceptance.workspace / ".git"
    for path in acceptance.workspace.rglob("*"):
        require(not path.is_symlink(), "write acceptance workspace contains a symlink")
        if path.is_dir():
            relative = str(path.relative_to(acceptance.workspace))
            require(acceptance.provider == "codex:exec" and
                    (path == git_directory or git_directory in path.parents) or
                    acceptance._is_pi_runtime_entry(relative),
                    "write acceptance workspace contains an unexpected directory")
    require(set(actual) == expected,
            "write acceptance workspace contains unexpected files")
    fixture_name = str(acceptance.fixture_file.relative_to(acceptance.workspace))
    for name, record in acceptance.workspace_baseline.items():
        if name == fixture_name:
            continue
        require(actual.get(name) == record,
                "write acceptance changed a harness-controlled workspace record")
    require(acceptance.workspace_marker.read_bytes() == acceptance.marker_bytes and
            acceptance.nonce_file.read_bytes() == (acceptance.nonce + "\n").encode(),
            "write acceptance changed harness-controlled files")
    verify_write_fixture(acceptance.nonce, acceptance.fixture_file)
    return actual

def _is_pi_runtime_entry(owner, relative: str) -> bool:
    acceptance = owner
    return (acceptance.provider == "pi:json" and acceptance.mode == "workspace-write" and
            (relative == ".pi" or relative == ".pi/tasks" or relative.startswith(".pi/tasks/")))

def _validate_pi_runtime_roots(owner) -> None:
    acceptance = owner
    if acceptance.provider != "pi:json" or acceptance.mode != "workspace-write":
        return
    for relative in (".pi", ".pi/tasks"):
        path = acceptance.workspace / relative
        if path.is_symlink() or path.exists():
            require(path.is_dir() and not path.is_symlink(),
                    f"Pi runtime root is not a directory: {relative}")

def task_binding(owner, task_id: str, predecessor: str | None = None,
                 predecessor_session: str | None = None) -> None:
    acceptance = owner
    directory, record, meta, meta_digest, spec_digest, _ = acceptance._request_binding(
        task_id, require_launch_evidence=True)
    acceptance._validate_task_selection(task_id, record)
    predicate = acceptance._validate_task_policy(task_id, meta)
    manifest_digest = acceptance._validate_task_provider_receipts(
        task_id, directory, meta_digest, spec_digest, predicate)
    acceptance._validate_task_outcome(
        task_id, directory, meta_digest, spec_digest, predicate,
        record, predecessor, predecessor_session, manifest_digest)

def _validate_task_selection(owner, task_id: str, record: dict[str, object]) -> None:
    acceptance = owner
    requested = record.get("requested_config")
    args = getattr(acceptance, "args", None)
    requested_model = getattr(args, "model", None)
    requested_effort = getattr(args, "effort", None)
    require(isinstance(requested, dict) and
            ((requested_model and requested.get("model") == requested_model) or
             (not requested_model and requested.get("model") in (None, ""))) and
            ((requested_effort and requested.get("effort") == requested_effort) or
             (not requested_effort and requested.get("effort") in (None, ""))),
            f"task {task_id} model or effort selection changed")
    if acceptance.provider == "codex:review":
        require(requested.get("review_target") == {"kind": "uncommitted"},
                f"task {task_id} Codex review target is not persisted")

def _validate_task_policy(owner, task_id: str, meta: dict[str, object]) -> dict[str, object]:
    acceptance = owner
    identity = acceptance._provider_bindings.identity_for(task_id)
    require(isinstance(identity, Mapping),
            f"task {task_id} admitted provider runtime identity is unavailable")
    pending_runtime = "runtime_preflight_sha256" in meta
    require(meta.get("containment") == acceptance.mode,
            f"task {task_id} metadata containment changed")
    effective = meta.get("effective_config")
    policy = effective.get("policy") if isinstance(effective, dict) else None
    require(isinstance(effective, dict) and effective.get("containment") == acceptance.mode and
            isinstance(effective.get("digest"), str) and
            isinstance(policy, dict) and policy.get("workspace") == str(acceptance.workspace) and
            (_hex_digest(policy.get("runtime_sha256")) if pending_runtime
             else policy.get("runtime_sha256") == identity["sha256"]),
            f"task {task_id} effective policy binding is invalid")
    if acceptance.provider == "antigravity:print":
        expected_approval = "plan" if acceptance.mode == "read-only" else "always-proceed"
        require(effective.get("approval") == expected_approval,
                f"task {task_id} AGY native approval mode is invalid")
    predicate = meta.get("predicate")
    require(isinstance(predicate, dict) and predicate.get("adapter") == acceptance.provider and
            predicate.get("mode") == acceptance.mode and isinstance(predicate.get("version"), str) and
            bool(predicate["version"]) and isinstance(predicate.get("sha256"), str) and
            len(predicate["sha256"]) == 64 and
            all(character in "0123456789abcdef" for character in predicate["sha256"]),
            f"task {task_id} predicate binding is invalid")
    return predicate

def _validate_task_provider_receipts(
        owner, task_id: str, directory: Path, meta_digest: str,
        spec_digest: str, predicate: dict[str, object]) -> str:
    acceptance = owner
    submit = read_json(directory / "submit.json")
    label = f"delegate:{acceptance.root_id}:{task_id}"
    supervisor = submit.get("supervisor")
    require(submit.get("root_id") == acceptance.root_id and submit.get("task_id") == task_id and
            submit.get("spec_sha256") == spec_digest and submit.get("meta_sha256") == meta_digest and
            submit.get("label") == label and isinstance(supervisor, dict) and
            supervisor.get("client_executable") == str(acceptance.pueue) and
            supervisor.get("config_path") == str(acceptance.config) and
            supervisor.get("observed_version") == acceptance.pueue_version,
            f"task {task_id} supervisor submission binding is invalid")
    for name in ("provider.start", "provider.started.json"):
        acceptance._read_bound_provider_evidence(
            task_id, directory, name, meta_digest, spec_digest)
    provider_exit = read_json(directory / "provider.exit")
    manifest_digest = provider_exit.get("manifest_sha256")
    if not isinstance(manifest_digest, str):
        raise AcceptanceFailure(f"task {task_id} provider seal binding is invalid")
    require(provider_exit.get("root_id") == acceptance.root_id and
            provider_exit.get("task_id") == task_id and
            provider_exit.get("spec_sha256") == spec_digest and
            provider_exit.get("meta_sha256") == meta_digest and
            provider_exit.get("invocation_state") == "started" and
            provider_exit.get("exit_code") == 0 and provider_exit.get("error") == "" and
            provider_exit.get("predicate") == predicate and
            isinstance(provider_exit.get("raw_manifest"), list) and
            len(manifest_digest) == 64 and
            all(character in "0123456789abcdef" for character in manifest_digest),
            f"task {task_id} provider seal binding is invalid")
    return manifest_digest

def _validate_task_outcome(
        owner, task_id: str, directory: Path, meta_digest: str, spec_digest: str,
        predicate: dict[str, object], record: dict[str, object],
        predecessor: str | None, predecessor_session: str | None, manifest_digest: str) -> None:
    acceptance = owner
    reference = read_json(directory / "provider.ref.json")
    session = reference.get("conversation_id")
    require(reference.get("root_id") == acceptance.root_id and reference.get("task_id") == task_id and
            reference.get("spec_sha256") == spec_digest and
            reference.get("meta_sha256") == meta_digest and reference.get("provider") == acceptance.provider and
            isinstance(session, str) and bool(session),
            f"task {task_id} provider session binding is invalid")
    outcome = read_json(directory / "outcome.json")
    require(outcome.get("root_id") == acceptance.root_id and outcome.get("task_id") == task_id and
            outcome.get("spec_sha256") == spec_digest and outcome.get("meta_sha256") == meta_digest and
            outcome.get("verdict") == "committed" and outcome.get("predicate") == predicate and
            outcome.get("evidence_sha256") == manifest_digest,
            f"task {task_id} outcome authority binding is invalid")
    prior = record.get("prior_session")
    if predecessor is None:
        require(prior is None, f"task {task_id} unexpectedly has a predecessor")
        return
    require(isinstance(prior, dict) and prior.get("provider") == acceptance.provider and
            prior.get("predecessor_task_id") == predecessor and
            prior.get("conversation_id") == predecessor_session,
            f"task {task_id} lacks the exact predecessor session binding")

def session_ref(owner, task_id: str) -> str:
    acceptance = owner
    reference = read_json(acceptance.state / "tasks" / task_id / "provider.ref.json")
    require(reference.get("root_id") == acceptance.root_id and reference.get("task_id") == task_id and
            reference.get("provider") == acceptance.provider,
            f"task {task_id} provider reference identity changed")
    session = reference.get("conversation_id")
    require(isinstance(session, str) and bool(session),
            f"task {task_id} has no provider session identity")
    return session

def replay(owner, records: dict[str, dict[str, object]] | None = None,
           names: tuple[str, ...] = ("fresh", "resume")) -> None:
    acceptance = owner
    if records is None:
        records = {}
        for name, record_name in (("fresh", "fresh-collect"), ("resume", "resume-collect")):
            record = acceptance.records[record_name]
            records[name] = {
                "directory": record["directory"],
                "outcome": record["outcome"],
                "payload": record["payload"],
            }
    acceptance.ops.root_id = acceptance.root_id
    if names == ("fresh", "resume"):
        acceptance.ops.replay(records)
    else:
        acceptance.ops.replay(records, names=names)
