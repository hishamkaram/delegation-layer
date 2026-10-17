"""Claude provider sealed evidence validation."""

from __future__ import annotations

from decimal import Decimal, InvalidOperation
from copy import deepcopy
import json
from pathlib import Path
import re
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.tasks.publication import read_outcome_payload
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.tasks.publication import snapshot
from scripts.delegate_acceptance.evidence.files import digest, read_json
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.provider_drivers.claude.constants import (
    MAX_PROVIDER_BYTES,
)
from scripts.delegate_acceptance.provider_drivers.claude.protocol import (
    parse_claude_events,
    validate_claude_result,
    validate_init_profile,
    validate_tool_free_resume,
)
from scripts.delegate_acceptance.provider_drivers.claude.identity import is_session_id


def provider_record_snapshot(directory: Path) -> dict[str, dict[str, object]]:
    return snapshot(directory, ignored_prefixes=(".ack",))
def validate_manifest(directory: Path, seal: dict[str, object]) -> None:
    manifest = seal.get("raw_manifest")
    require(isinstance(manifest, list), "Claude raw manifest is absent")
    paths: set[str] = set()
    previous: str | None = None
    for entry in manifest:
        require(isinstance(entry, dict), "Claude raw manifest entry is not an object")
        relative = entry.get("path")
        size = entry.get("size")
        checksum = entry.get("sha256")
        require(isinstance(relative, str) and relative.startswith("raw/") and relative not in paths,
                "Claude raw manifest path is malformed or duplicated")
        basename = relative[4:]
        require(basename in {"stdout", "stderr"} or
                re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,127}", basename) is not None,
                "Claude raw manifest path is not a safe task-local basename")
        require(previous is None or previous < relative, "Claude raw manifest is not strictly sorted")
        require(isinstance(size, int) and not isinstance(size, bool) and
                0 <= size <= MAX_PROVIDER_BYTES and isinstance(checksum, str) and
                re.fullmatch(r"[0-9a-f]{64}", checksum) is not None,
                "Claude raw manifest descriptor is malformed")
        path = directory / relative
        require(path.is_file() and not path.is_symlink(), "Claude raw manifest file is absent")
        require(path.stat().st_size == size and digest(path) == checksum,
                "Claude raw manifest digest mismatch")
        paths.add(relative)
        previous = relative
    require({"raw/stdout", "raw/stderr"} <= paths, "Claude raw manifest omitted required evidence")
    canonical = json.dumps(
        [{"path": entry["path"], "size": entry["size"], "sha256": entry["sha256"]}
         for entry in manifest], ensure_ascii=True, separators=(",", ":")) + "\n"
    require(seal.get("manifest_sha256") == sha(canonical.encode("utf-8")),
            "Claude provider.exit manifest_sha256 does not match canonical raw manifest")
_USAGE_FIELDS = (
    ("input_tokens", "input_tokens"),
    ("output_tokens", "output_tokens"),
    ("thinking_tokens", "thinking_tokens"),
    ("total_tokens", "total_tokens"),
    ("cache_read_input_tokens", "cache_read_tokens"),
    ("cache_creation_input_tokens", "cache_creation_tokens"),
)
_MODEL_USAGE_FIELDS = (
    ("inputTokens", "input_tokens"),
    ("outputTokens", "output_tokens"),
    ("thinkingTokens", "thinking_tokens"),
    ("totalTokens", "total_tokens"),
    ("cacheReadInputTokens", "cache_read_tokens"),
    ("cacheCreationInputTokens", "cache_creation_tokens"),
    ("costUSD", "estimated_cost_usd"),
)
_INTEGER_USAGE_FIELDS = {
    "input_tokens", "output_tokens", "thinking_tokens", "total_tokens",
    "cache_read_tokens", "cache_creation_tokens", "num_turns",
}


def _usage_record(scope: str) -> dict[str, object]:
    return {
        "scope": scope,
        "source": "provider-envelope",
        "reliability": "reported",
        "input_tokens": None,
        "output_tokens": None,
        "thinking_tokens": None,
        "total_tokens": None,
        "cache_read_tokens": None,
        "cache_creation_tokens": None,
        "num_turns": None,
        "duration_seconds": None,
        "model": None,
        "estimated_cost_usd": None,
    }


def _set_if_present(target: dict[str, object], source: dict[str, object],
                    source_name: str, target_name: str) -> None:
    if source_name in source and source[source_name] is not None:
        target[target_name] = source[source_name]


def _main_agent_usage(result: dict[str, object], parsed: dict[str, object],
                      name: str) -> dict[str, object]:
    usage = result.get("usage")
    main = _usage_record("main-agent")
    if isinstance(usage, dict):
        for source_name, target_name in _USAGE_FIELDS:
            _set_if_present(main, usage, source_name, target_name)
    _set_if_present(main, result, "num_turns", "num_turns")
    if result.get("duration_ms") is not None:
        milliseconds = result["duration_ms"]
        require(isinstance(milliseconds, int) and not isinstance(milliseconds, bool),
                f"{name} duration_ms is not an integer")
        seconds = str(milliseconds // 1000)
        remainder = milliseconds % 1000
        if remainder:
            seconds = (seconds + ".%03d" % remainder).rstrip("0")
        main["duration_seconds"] = seconds
    init = parsed.get("init")
    if isinstance(init, dict) and init.get("model") is not None:
        main["model"] = init.get("model")
    return main


def _whole_tree_usage(result: dict[str, object], name: str) -> list[dict[str, object]]:
    records: list[dict[str, object]] = []
    if result.get("total_cost_usd") is not None:
        total = _usage_record("whole-tree")
        total["estimated_cost_usd"] = result["total_cost_usd"]
        records.append(total)
    model_usage = result.get("modelUsage")
    if isinstance(model_usage, dict):
        for model in sorted(model_usage):
            fields = model_usage[model]
            require(isinstance(fields, dict), f"{name} modelUsage entry is not an object")
            record = _usage_record("whole-tree")
            record["model"] = model
            for source_name, target_name in _MODEL_USAGE_FIELDS:
                _set_if_present(record, fields, source_name, target_name)
            records.append(record)
    return records


def _validate_usage_field(actual: dict[str, object], want: dict[str, object],
                          field_name: str, name: str) -> None:
    value = want[field_name]
    if value is None:
        require(field_name not in actual or actual[field_name] is None,
                f"{name} synthesized missing usage field {field_name}")
    elif field_name == "estimated_cost_usd":
        _validate_cost_field(actual, value, field_name, name)
    elif field_name in _INTEGER_USAGE_FIELDS:
        require(isinstance(actual.get(field_name), int) and
                not isinstance(actual.get(field_name), bool) and actual.get(field_name) == value,
                f"{name} usage field {field_name} differs from native value")
    else:
        require(actual.get(field_name) == value,
                f"{name} usage field {field_name} differs from native value")


def _validate_cost_field(actual: dict[str, object], value: object,
                         field_name: str, name: str) -> None:
    require(isinstance(actual.get(field_name), str),
            f"{name} usage cost {field_name} is not a string")
    try:
        actual_number = Decimal(str(actual.get(field_name)))
        expected_number = Decimal(str(value))
    except (InvalidOperation, TypeError, ValueError) as error:
        raise AcceptanceFailure(f"{name} usage cost {field_name} is malformed") from error
    require(actual_number == expected_number,
            f"{name} usage field {field_name} differs from native value")


def _validate_usage_records(recorded: list[object], expected: list[dict[str, object]],
                            name: str) -> None:
    require(len(recorded) == len(expected),
            f"{name} usage records were omitted or synthesized")
    for index, (actual, want) in enumerate(zip(recorded, expected)):
        require(isinstance(actual, dict), f"{name} usage entry {index} is not an object")
        require("task_delta" not in actual, f"{name} synthesized a task usage delta")
        require(set(actual) <= set(want), f"{name} usage entry {index} has unknown fields")
        for field_name in want:
            _validate_usage_field(actual, want, field_name, name)


def validate_record_usage(outcome: dict[str, object], parsed: dict[str, object], name: str) -> None:
    """Check every native usage view, preserving null counters and scopes."""
    result = parsed.get("result_event")
    require(isinstance(result, dict), f"{name} result usage source is absent")
    usage = result.get("usage")
    model_usage = result.get("modelUsage")
    native_counter = any(result.get(field) is not None for field in ("duration_ms", "num_turns"))
    has_native_usage = (usage is not None or bool(model_usage) or
                        result.get("total_cost_usd") is not None or native_counter)
    recorded = outcome.get("usage")
    if not has_native_usage:
        require(recorded in (None, []), f"{name} invented usage without provider counters")
        return
    require(isinstance(recorded, list), f"{name} usage is not a list")
    expected: list[dict[str, object]] = []
    if isinstance(usage, dict) or native_counter:
        expected.append(_main_agent_usage(result, parsed, name))
    expected.extend(_whole_tree_usage(result, name))
    if not expected:
        require(recorded in (None, []), f"{name} invented usage without provider counters")
        return
    _validate_usage_records(recorded, expected, name)


def _read_task_record(directory: Path, task: str, record: str) -> dict[str, object]:
    value = read_json(directory / record)
    require(isinstance(value, dict), f"{task}/{record} must be an object")
    return value


def validate_task_request(*, name: str, task: str, directory: Path, root_id: str,
                          workspace: Path, predecessor: str | None,
                          provider: str, mode: str, budget: str) -> dict[str, object]:
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
    if predecessor is None:
        require(request.get("prior_session") is None, f"{name} unexpectedly has a predecessor")
    else:
        prior = request.get("prior_session")
        require(isinstance(prior, dict) and prior.get("provider") == provider and
                prior.get("predecessor_task_id") == predecessor and
                is_session_id(prior.get("conversation_id")),
                f"{name} lacks the exact predecessor session reference")
    return request


def validate_task_meta(*, name: str, task: str, directory: Path, root_id: str,
                       claude: Path, provider_version: str | None, workspace: Path,
                       provider_sha256: str,
                       provider: str, mode: str, approval: str,
                       revision: str) -> tuple[dict[str, object], str]:
    meta = _read_task_record(directory, task, "meta.json")
    require(meta.get("root_id") == root_id and meta.get("task_id") == task and
            meta.get("provider_executable") == str(claude) and
            meta.get("provider_version") == provider_version and
            meta.get("containment") == mode and meta.get("approval") == approval,
            f"{name} meta provider or policy binding mismatch")
    effective = meta.get("effective_config")
    require(isinstance(effective, dict) and effective.get("containment") == mode and
            effective.get("approval") == approval and isinstance(effective.get("digest"), str),
            f"{name} effective Claude policy is absent")
    policy = effective.get("policy")
    require(isinstance(policy, dict) and policy.get("workspace") == str(workspace) and
            policy.get("runtime_sha256") == provider_sha256,
            f"{name} persisted Claude profile policy is incomplete")
    profile_revision = policy.get("profile_revision")
    require(profile_revision == revision,
            f"{name} effective policy is not the native Claude profile")
    require(policy.get("sources") in (None, []),
            f"{name} native policy unexpectedly contains source inventory")
    return meta, revision


def validate_task_contract(name: str, meta: dict[str, object], provider: str,
                           mode: str, predicate_version: str,
                           predicate_sha256: str) -> None:
    predicate = meta.get("predicate")
    require(predicate == {"adapter": provider, "mode": mode,
                         "version": predicate_version, "sha256": predicate_sha256},
            f"{name} predicate binding is absent")
    inputs = meta.get("input_files")
    require(inputs in (None, []),
            f"{name} native Claude task unexpectedly declares configuration inputs")
    require(meta.get("output_artifacts") in (None, []) and
            meta.get("output_writer_contract") in (None, ""),
            f"{name} unexpectedly declared a Claude output artifact")


def validate_task_publication(*, name: str, task: str, directory: Path,
                              root_id: str, label: str, pueue_config: Path,
                              pueue_version: str | None, pueue: Path,
                              meta: dict[str, object]) -> dict[str, object]:
    predicate = meta.get("predicate")
    submit = _read_task_record(directory, task, "submit.json")
    require(submit.get("root_id") == root_id and submit.get("task_id") == task and
            submit.get("label") == label and
            submit.get("spec_sha256") == meta.get("spec_sha256") and
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
    return seal


def validate_task_outcome(*, name: str, task: str, directory: Path,
                          expected_answer: bytes, predecessor: str | None,
                          expected_session: str | None, request: dict[str, object],
                          meta: dict[str, object], seal: dict[str, object],
                          root_id: str, workspace: Path,
                          provider: str) -> dict[str, object]:
    predicate = meta.get("predicate")
    validate_manifest(directory, seal)
    raw = (directory / "raw/stdout").read_bytes()
    require(len(raw) <= MAX_PROVIDER_BYTES and
            (directory / "raw/stderr").stat().st_size <= MAX_PROVIDER_BYTES,
            f"{name} raw stream exceeds acceptance bound")
    parsed = parse_claude_events(raw)
    profile = validate_init_profile(parsed, expected_session=expected_session,
                                    expected_cwd=str(workspace))
    answer = validate_claude_result(parsed, expected_session=expected_session,
                                    expected_answer=expected_answer)
    require(answer == expected_answer, f"{name} final result differs from committed answer")
    reference = _read_task_record(directory, task, "provider.ref.json")
    require(reference.get("root_id") == root_id and reference.get("task_id") == task and
            reference.get("provider") == provider and
            reference.get("conversation_id") == parsed["session_id"] and
            is_session_id(reference.get("conversation_id")),
            f"{name} provider session identity mismatch")
    outcome = _read_task_record(directory, task, "outcome.json")
    require(outcome.get("root_id") == root_id and outcome.get("task_id") == task and
            outcome.get("spec_sha256") == meta.get("spec_sha256") and
            outcome.get("meta_sha256") == digest(directory / "meta.json") and
            outcome.get("verdict") == "committed" and outcome.get("predicate") == predicate and
            outcome.get("evidence_sha256") == seal.get("manifest_sha256"),
            f"{name} outcome authority mismatch")
    validate_record_usage(outcome, parsed, name)
    payload = outcome.get("payload")
    require(read_outcome_payload(directory, outcome) == expected_answer,
            f"{name} committed payload mismatch")
    resume_controls = None
    if predecessor is not None:
        resume_controls = validate_tool_free_resume(
            parsed, expected_answer, str(reference["conversation_id"]))
    return {"directory": directory, "request": request, "meta": meta, "seal": seal,
            "reference": reference, "outcome": outcome, "payload": deepcopy(payload),
            "parsed": parsed, "init": profile, "resume_controls": resume_controls,
            "snapshot": provider_record_snapshot(directory)}
