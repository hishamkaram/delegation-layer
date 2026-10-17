"""Codex sealed provider evidence validation."""

from __future__ import annotations

from copy import deepcopy
import re
from pathlib import Path

from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.tasks.publication import snapshot
from scripts.delegate_acceptance.evidence.files import digest, read_json
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.provider_drivers.codex.constants import MAX_PROVIDER_BYTES, OUTPUT_NAME
from scripts.delegate_acceptance.provider_drivers.codex.protocol import (
    parse_codex_events,
    validate_tool_free_resume,
)
from scripts.delegate_acceptance.provider_drivers.codex.identity import is_thread_id


def binding_record(provider: str, mode: str, approval: str, codex: Path,
                   codex_version: str, codex_digest: str, delegate: Path,
                   delegate_digest: str, runner: Path, runner_digest: str,
                   pueue: Path, pueue_digest: str, pueued: Path, pueued_digest: str,
                   pueue_version: str, pueued_version: str, pueue_base: Path,
                   pueue_config: Path, config_digest: str, state: Path,
                   workspace: Path, sibling: Path, probe: Path, probe_digest: str,
                   probe_bytes: int, nonce_digest: str, environment_keys: list[str],
                   acceptance_status: str, planned_native_ai_turns: int,
                   driver_digest: str) -> dict[str, object]:
    """Build the public acceptance binding from the owner's observed state."""
    return {
        "provider": provider,
        "mode": mode,
        "approval": approval,
        "codex": str(codex),
        "codex_version": codex_version,
        "codex_sha256": codex_digest,
        "delegate": str(delegate),
        "delegate_sha256": delegate_digest,
        "runner": str(runner),
        "runner_sha256": runner_digest,
        "pueue": str(pueue),
        "pueue_sha256": pueue_digest,
        "pueued": str(pueued),
        "pueued_sha256": pueued_digest,
        "pueue_version": pueue_version,
        "pueued_version": pueued_version,
        "pueue_base": str(pueue_base),
        "pueue_config": str(pueue_config),
        "config_sha256": config_digest,
        "state": str(state),
        "workspace": str(workspace),
        "sibling": str(sibling),
        "probe": str(probe),
        "probe_sha256": probe_digest,
        "probe_bytes": probe_bytes,
        "probe_mode": "0400",
        "nonce_sha256": nonce_digest,
        "environment_keys": environment_keys,
        "environment_values_in_record": False,
        "acceptance_status": acceptance_status,
        "planned_native_ai_turns": planned_native_ai_turns,
        "driver_sha256": driver_digest,
    }


def success_record(status: str, provider: str, planned_native_ai_turns: int,
                   tasks: dict[str, str], records: dict[str, dict[str, object]],
                   naturally_shutdown: bool) -> dict[str, object]:
    """Build the success receipt from validated task records."""
    return {
        "status": status,
        "provider": provider,
        "native_ai_turns": len(tasks),
        "planned_native_ai_turns": planned_native_ai_turns,
        "tasks": {
            name: {
                "task_id": tasks[name],
                "conversation_id": records[name]["reference"]["conversation_id"],
                "outcome_sha256": digest(records[name]["directory"] / "outcome.json"),
                "evidence_sha256": records[name]["outcome"]["evidence_sha256"],
            }
            for name in ("fresh", "resume")
        },
        "replay_launches": 0,
        "natural_daemon_shutdown": naturally_shutdown,
        "signals_sent": 0,
    }


def validate_manifest(directory: Path, seal: dict[str, object]) -> None:
    manifest = seal.get("raw_manifest")
    require(isinstance(manifest, list) and 2 <= len(manifest) <= 18,
            "provider.exit raw manifest is absent or has an invalid entry count")
    paths: set[str] = set()
    previous: str | None = None
    for entry in manifest:
        require(isinstance(entry, dict), "raw manifest entry is not an object")
        relative = entry.get("path")
        size = entry.get("size")
        checksum = entry.get("sha256")
        require(isinstance(relative, str) and relative.startswith("raw/") and relative not in paths,
                "raw manifest path is malformed or duplicated")
        basename = relative[4:]
        require(basename in {"stdout", "stderr"} or
                (not basename.startswith("stage.") and
                 re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,127}", basename) is not None),
                "raw manifest path is not a safe task-local basename")
        require(previous is None or previous < relative,
                "raw manifest paths are not strictly sorted")
        require(isinstance(size, int) and not isinstance(size, bool) and 0 <= size <= MAX_PROVIDER_BYTES and
                isinstance(checksum, str) and re.fullmatch(r"[0-9a-f]{64}", checksum) is not None,
                "raw manifest descriptor is malformed")
        path = directory / relative
        require(path.is_file() and not path.is_symlink(), "raw manifest file is absent")
        require(path.stat().st_size == size and digest(path) == checksum,
                "raw manifest digest mismatch")
        paths.add(relative)
        previous = relative
    require({"raw/stdout", "raw/stderr", "raw/" + OUTPUT_NAME} <= paths,
            "Codex raw manifest omitted required evidence")


def validate_record_usage(outcome: dict[str, object], parsed: dict[str, object], name: str) -> None:
    """Check cumulative event counters without deriving a task delta."""
    event_usage = parsed.get("usage")
    recorded = outcome.get("usage")
    if event_usage is None:
        require(recorded in (None, []), f"{name} invented usage without provider counters")
        return
    require(isinstance(recorded, list) and len(recorded) == 1 and isinstance(recorded[0], dict),
            f"{name} omitted cumulative provider usage")
    value = recorded[0]
    require(value.get("scope") == "conversation-cumulative" and
            value.get("source") == "provider-event" and
            value.get("reliability") == "reported",
            f"{name} usage scope or reliability is not provider-event cumulative")
    mapping = {
        "input_tokens": "input_tokens",
        "cached_input_tokens": "cache_read_tokens",
        "cache_write_input_tokens": "cache_creation_tokens",
        "output_tokens": "output_tokens",
        "reasoning_output_tokens": "thinking_tokens",
    }
    for event_name, record_name in mapping.items():
        expected = event_usage[event_name]
        if expected is None:
            require(record_name not in value or value.get(record_name) is None,
                    f"{name} synthesized missing usage counter {record_name}")
        else:
            require(value.get(record_name) == expected,
                    f"{name} usage counter {record_name} differs from turn.completed")
    require("total_tokens" not in value, f"{name} synthesized total_tokens")


def provider_record_snapshot(directory: Path) -> dict[str, dict[str, object]]:
    return snapshot(directory, ignored_prefixes=(".ack",))


def _read_task_record(directory: Path, task: str, record: str) -> dict[str, object]:
    value = read_json(directory / record)
    require(isinstance(value, dict), f"{task}/{record} must be an object")
    return value


def validate_task_request(name: str, task: str, directory: Path, root_id: str,
                          workspace: Path, predecessor: str | None,
                          provider: str, mode: str, budget: str) -> dict[str, object]:
    for record in ("brief.md", "task.json", "meta.json", "submit.json", "provider.start",
                   "provider.started.json", "provider.exit", "provider.ref.json", "outcome.json"):
        require((directory / record).is_file(), f"{name} missing evidence {record}")
    request = _read_task_record(directory, task, "task.json")
    require(request.get("root_id") == root_id and request.get("task_id") == task and
            request.get("provider") == provider and request.get("mode") == mode and
            request.get("canonical_cwd") == str(workspace),
            f"{name} task identity or workspace binding mismatch")
    requested = request.get("requested_config")
    require(isinstance(requested, dict) and requested.get("permission") == mode and
            requested.get("budget") == budget and
            request.get("budget_nanos") == 120_000_000_000,
            f"{name} request budget or permission mismatch")
    brief = (directory / "brief.md").read_bytes()
    require(request.get("brief_length") == len(brief) and request.get("brief_sha256") == sha(brief),
            f"{name} persisted brief digest mismatch")
    if predecessor is not None:
        prior = request.get("prior_session")
        require(isinstance(prior, dict) and prior.get("provider") == provider and
                prior.get("predecessor_task_id") == predecessor and
                is_thread_id(prior.get("conversation_id")),
                f"{name} lacks the exact predecessor session reference")
    else:
        require(request.get("prior_session") is None, f"{name} unexpectedly has a predecessor")
    return request


def validate_task_meta(name: str, task: str, directory: Path, root_id: str,
                       codex: Path, provider_version: str | None, workspace: Path,
                       provider_sha256: str, mode: str, approval: str,
                       revision: str) -> tuple[dict[str, object], dict[str, object], str]:
    meta = _read_task_record(directory, task, "meta.json")
    require(meta.get("root_id") == root_id and meta.get("task_id") == task and
            meta.get("provider_executable") == str(codex) and
            meta.get("provider_version") == provider_version and
            meta.get("containment") == mode and meta.get("approval") == approval,
            f"{name} meta provider or policy binding mismatch")
    effective = meta.get("effective_config")
    require(isinstance(effective, dict) and effective.get("containment") == mode and
            effective.get("approval") == approval and isinstance(effective.get("digest"), str),
            f"{name} effective read-only policy is absent")
    policy = effective.get("policy")
    require(isinstance(policy, dict) and policy.get("workspace") == str(workspace) and
            policy.get("runtime_sha256") == provider_sha256,
            f"{name} persisted Codex runtime policy is incomplete")
    profile_revision = policy.get("profile_revision")
    require(profile_revision == revision,
            f"{name} native effective policy revision is absent or stale")
    return meta, policy, revision


def validate_task_policy(name: str, meta: dict[str, object], policy: dict[str, object],
                         environment: dict[str, str], provider: str, mode: str,
                         predicate_version: str, predicate_sha256: str,
                         output_contract: str, output_name: str,
                         output_argument_index: int) -> None:
    require(not policy.get("sources"), f"{name} native policy unexpectedly inventories config sources")
    roots = policy.get("writable_roots")
    require(isinstance(roots, list) and all(isinstance(root, str) for root in roots),
            f"{name} writable runtime roots are absent")
    codex_home = environment.get("CODEX_HOME", str(Path.home() / ".codex"))
    require(str(Path(codex_home).resolve()) in {str(Path(root).resolve()) for root in roots},
            f"{name} CODEX_HOME was not preserved in runtime policy")
    predicate = meta.get("predicate")
    require(predicate == {"adapter": provider, "mode": mode, "version": predicate_version,
                         "sha256": predicate_sha256},
            f"{name} predicate binding mismatch")
    require(meta.get("output_writer_contract") == output_contract,
            f"{name} output-writer contract mismatch")
    artifacts = meta.get("output_artifacts")
    require(isinstance(artifacts, list) and len(artifacts) == 1 and
            isinstance(artifacts[0], dict) and artifacts[0].get("name") == output_name and
            artifacts[0].get("argument_index") == output_argument_index,
            f"{name} Codex output staging declaration is absent")


def validate_task_seal(name: str, task: str, directory: Path, root_id: str,
                       label: str, pueue_config: Path, pueue_version: str | None,
                       pueue: Path, meta: dict[str, object]) -> dict[str, object]:
    predicate = meta.get("predicate")
    submit = _read_task_record(directory, task, "submit.json")
    require(submit.get("root_id") == root_id and submit.get("task_id") == task and
            submit.get("label") == label and submit.get("spec_sha256") == meta.get("spec_sha256") and
            submit.get("meta_sha256") == digest(directory / "meta.json"),
            f"{name} submit binding mismatch")
    supervisor = submit.get("supervisor")
    require(isinstance(supervisor, dict) and supervisor.get("config_path") == str(pueue_config) and
            supervisor.get("observed_version") == pueue_version and
            supervisor.get("client_executable") == str(pueue),
            f"{name} submit supervisor binding mismatch")
    start = _read_task_record(directory, task, "provider.start")
    require(start.get("root_id") == root_id and start.get("task_id") == task and
            start.get("spec_sha256") == meta.get("spec_sha256") and
            start.get("meta_sha256") == digest(directory / "meta.json") and
            isinstance(start.get("budget_nanos"), int) and start["budget_nanos"] > 0,
            f"{name} provider.start binding mismatch")
    started = _read_task_record(directory, task, "provider.started.json")
    require(started.get("root_id") == root_id and started.get("task_id") == task and
            started.get("spec_sha256") == meta.get("spec_sha256") and
            started.get("meta_sha256") == digest(directory / "meta.json") and
            isinstance(started.get("diagnostic_nanos"), int) and started["diagnostic_nanos"] >= 0,
            f"{name} provider.started evidence mismatch")
    seal = _read_task_record(directory, task, "provider.exit")
    require(seal.get("root_id") == root_id and seal.get("task_id") == task and
            seal.get("spec_sha256") == meta.get("spec_sha256") and
            seal.get("meta_sha256") == digest(directory / "meta.json") and
            seal.get("invocation_state") == "started" and seal.get("exit_code") == 0 and
            seal.get("error") == "" and seal.get("predicate") == predicate,
            f"{name} provider seal mismatch")
    validate_manifest(directory, seal)
    return seal


def validate_task_result(name: str, task: str, directory: Path,
                         expected_answer: bytes, predecessor: str | None,
                         request: dict[str, object], meta: dict[str, object],
                         seal: dict[str, object], root_id: str,
                         provider: str) -> dict[str, object]:
    predicate = meta.get("predicate")
    raw_stdout = (directory / "raw" / "stdout").read_bytes()
    raw_stderr = (directory / "raw" / "stderr").read_bytes()
    require(len(raw_stdout) <= MAX_PROVIDER_BYTES and len(raw_stderr) <= MAX_PROVIDER_BYTES,
            f"{name} raw stream exceeds acceptance bound")
    parsed = parse_codex_events(raw_stdout)
    output_artifact = directory / "raw" / OUTPUT_NAME
    require(output_artifact.is_file() and not output_artifact.is_symlink(),
            f"{name} Codex last-message artifact is absent")
    output_bytes = output_artifact.read_bytes()
    require(output_bytes == parsed["final_message"],
            f"{name} output artifact disagrees with final agent message")
    require(output_bytes == expected_answer,
            f"{name} final answer bytes differ from acceptance expectation")
    resume_controls: dict[str, object] | None = None
    if predecessor is not None:
        resume_controls = validate_tool_free_resume(parsed, expected_answer)
    reference = _read_task_record(directory, task, "provider.ref.json")
    require(reference.get("root_id") == root_id and reference.get("task_id") == task and
            reference.get("spec_sha256") == meta.get("spec_sha256") and
            reference.get("meta_sha256") == digest(directory / "meta.json") and
            reference.get("provider") == provider and
            reference.get("conversation_id") == parsed["thread_id"],
            f"{name} provider thread identity mismatch")
    require(is_thread_id(reference.get("conversation_id")),
            f"{name} provider thread ID is invalid")
    outcome = _read_task_record(directory, task, "outcome.json")
    require(outcome.get("root_id") == root_id and outcome.get("task_id") == task and
            outcome.get("spec_sha256") == meta.get("spec_sha256") and
            outcome.get("meta_sha256") == digest(directory / "meta.json") and
            outcome.get("verdict") == "committed" and outcome.get("predicate") == predicate and
            outcome.get("evidence_sha256") == seal.get("manifest_sha256"),
            f"{name} outcome authority mismatch")
    validate_record_usage(outcome, parsed, name)
    payload = outcome.get("payload")
    require(isinstance(payload, dict) and payload.get("basename") == "result.txt" and
            payload.get("length") == len(expected_answer) and
            payload.get("sha256") == sha(expected_answer) and
            (directory / "result.txt").read_bytes() == expected_answer,
            f"{name} committed payload mismatch")
    return {"directory": directory, "request": request, "meta": meta, "seal": seal,
            "reference": reference, "outcome": outcome, "payload": deepcopy(payload),
            "parsed": parsed, "stderr_sha256": sha(raw_stderr),
            "resume_controls": resume_controls,
            "snapshot": provider_record_snapshot(directory)}
