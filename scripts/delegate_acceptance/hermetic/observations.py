"""Read finite process and supervisor evidence without owning new processes."""

from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.hermetic.contract import CASE_TIMEOUT
from scripts.delegate_acceptance.hermetic.contract import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.hermetic.contract import require_true
import json
import time


def assert_counts(case, **expected):
    counts = invocation_counts(case)
    for key, value in expected.items():
        require_true(counts.get(key) == value,
                     "%s count %s=%s, want %s" % (case.case_id, key, counts.get(key), value))
    return counts


def wait_entry(case, verb, timeout=CASE_TIMEOUT):
    def find():
        for path in sorted(case.supervisor_records.glob("*.entry.json")):
            try:
                receipt = read_json(path, MAX_CONTROL_BYTES)
            except (FileNotFoundError, json.JSONDecodeError):
                continue
            if receipt.get("verb") == verb:
                return receipt
        return None

    return wait_until(find, timeout=timeout, description="fake supervisor " + verb + " entry")


def supervisor_entries(case):
    entries = []
    for path in sorted(case.supervisor_records.glob("*.entry.json")):
        entries.append(read_json(path, MAX_CONTROL_BYTES))
    return entries


def wait_completion(case, entry, timeout=CASE_TIMEOUT):
    path = case.supervisor_records / (entry["invocation_id"] + ".completion.json")
    return wait_until(
        lambda: read_json(path, MAX_CONTROL_BYTES) if path.exists() else None,
        timeout=timeout,
        description="fake supervisor command completion",
    )


def wait_entry_after(case, verb, previous_invocation_ids, timeout=CASE_TIMEOUT):
    """Find the one later helper invocation by its immutable ID."""
    previous = set(previous_invocation_ids)

    def find():
        candidates = []
        for path in sorted(case.supervisor_records.glob("*.entry.json")):
            try:
                receipt = read_json(path, MAX_CONTROL_BYTES)
            except (FileNotFoundError, json.JSONDecodeError):
                continue
            if (receipt.get("verb") == verb and
                    receipt.get("invocation_id") not in previous):
                candidates.append(receipt)
        require_true(len(candidates) <= 1,
                     "multiple later fake supervisor invocations for " + verb)
        return candidates[0] if candidates else None

    return wait_until(
        find,
        timeout=timeout,
        description="later fake supervisor " + verb + " invocation",
    )


def observe_helper_completion_if_present(case, entry, timeout=1.0):
    case.external_wait_required = True
    # Register the exact helper invocation before waiting.  If the
    # dispatcher exits first, a completion may become visible later while
    # its observer Wait remains unknowable; only this invocation may be
    # exempted from the paired observer-completion check.
    case.external_wait_unknown[entry["invocation_id"]] = entry["pid"]
    path = case.supervisor_records / (entry["invocation_id"] + ".completion.json")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if path.exists():
            case.helper_completion_observed = True
            completion = read_json(path, MAX_CONTROL_BYTES)
            observer_completion = supervisor_observer_completion_for_entry(case, entry)
            owner_completion = control_owner_completion_for_entry(case, entry)
            case.external_wait_observed = (
                observer_completion is not None or owner_completion is not None
            )
            if case.external_wait_observed:
                case.external_wait_unknown.pop(entry["invocation_id"], None)
                return completion
        time.sleep(0.025)

    if path.exists():
        case.helper_completion_observed = True
        return read_json(path, MAX_CONTROL_BYTES)
    case.helper_completion_observed = False
    # The caller's process has already returned, so absence of the helper
    # receipt cannot prove a negative.  Leave external Wait unknown rather
    # than converting the observation bound into a lifecycle fact.
    case.external_wait_observed = None
    return None


def supervisor_observer_events_for_entry(case, entry):
    """Match observer events by executable, PID, and literal argv.

    The finite helper chooses its own invocation ID, while the application
    observer chooses a separate command ID.  The stable join key is the
    helper PID and the complete command argv, including the executable.
    """
    expected_argv = [entry["executable"], *entry["argv"]]
    by_command = {}
    for value in events(case):
        if value.get("kind") != "supervisor":
            continue
        command = value.get("command") or {}
        if command.get("argv") != expected_argv:
            continue
        command_id = command.get("command_id")
        if not command_id:
            continue
        by_command.setdefault(command_id, []).append(value)
    candidates = []
    for values in by_command.values():
        if any(
            (value.get("command") or {}).get("stage") == "started"
            and (value.get("command") or {}).get("pid") == entry["pid"]
            for value in values
        ):
            candidates.append(values)
    require_true(len(candidates) <= 1,
                 "supervisor recorder matched multiple command IDs for invocation " + entry["invocation_id"])
    return candidates[0] if candidates else []


def supervisor_observer_completion_for_entry(case, entry):
    return next(
        (
            value
            for value in supervisor_observer_events_for_entry(case, entry)
            if (value.get("command") or {}).get("stage") == "completed"
        ),
        None,
    )


def control_owner_completion_for_entry(case, entry):
    """Match the one-shot owner that inherited this command's Wait.

    The hidden owner argv carries the immutable command ID. Its natural
    wrapper completion therefore proves the owner remained alive through
    the exact child completion even after the invoking CLI exited.
    """
    command_ids = {
        (value.get("command") or {}).get("command_id")
        for value in supervisor_observer_events_for_entry(case, entry)
        if (value.get("command") or {}).get("command_id")
    }
    require_true(len(command_ids) <= 1,
                 "supervisor invocation mapped to multiple command IDs")
    if not command_ids:
        return None
    command_id = next(iter(command_ids))
    candidates = []
    for value in events(case):
        argv = value.get("argv") or []
        if (len(argv) == 5 and argv[1] == "--supervisor-control-owner" and
                argv[-1] == command_id and value.get("completed") and
                value.get("exit_code") == 0):
            candidates.append(value)
    require_true(len(candidates) <= 1,
                 "multiple control owners matched command " + command_id)
    return candidates[0] if candidates else None


def wait_supervisor_completion(case, entry, timeout=CASE_TIMEOUT):
    return wait_until(
        lambda: supervisor_observer_completion_for_entry(case, entry),
        timeout=timeout,
        description="supervisor observer completion for " + entry["invocation_id"],
    )


def require_supervisor_recorder(case, entries):
    """Prove each recorded helper invocation has a matching event log.

    This prevents an absent observer log from being silently interpreted as
    S=0/K=0/M=0.  An in-flight helper legitimately has entry/started only;
    a helper completion must also have a matching observer completion,
    except for the exact invocation explicitly retained as external-Wait
    unknown after its caller exited.
    """
    for entry in entries:
        events = supervisor_observer_events_for_entry(case, entry)
        stages = {
            (value.get("command") or {}).get("stage") for value in events
        }
        require_true("entry" in stages and "started" in stages,
                     "supervisor recorder missed Start for invocation " + entry["invocation_id"])
        completion_path = case.supervisor_records / (entry["invocation_id"] + ".completion.json")
        if completion_path.exists():
            helper_completion = read_json(completion_path, MAX_CONTROL_BYTES)
            require_true(
                helper_completion.get("invocation_id") == entry["invocation_id"] and
                helper_completion.get("pid") == entry["pid"] and
                helper_completion.get("executable") == entry["executable"] and
                helper_completion.get("config_path") == entry["config_path"] and
                helper_completion.get("argv") == entry["argv"],
                "supervisor helper completion identity changed for invocation " + entry["invocation_id"],
            )
            completion = supervisor_observer_completion_for_entry(case, entry)
            if completion is None:
                require_true(
                    case.external_wait_unknown.get(entry["invocation_id"]) == entry["pid"] or
                    control_owner_completion_for_entry(case, entry) is not None,
                    "supervisor recorder missed completion for invocation " + entry["invocation_id"],
                )
            elif case.external_wait_unknown.get(entry["invocation_id"]) == entry["pid"]:
                case.external_wait_observed = True
                case.external_wait_unknown.pop(entry["invocation_id"], None)
    return entries


def observed_supervisor_completions(case):
    """Return command IDs whose wrapper observer wrote a completed event."""
    result = []
    for value in events(case):
        if value.get("kind") != "supervisor":
            continue
        command = value.get("command") or {}
        if command.get("stage") == "completed" and command.get("command_id"):
            result.append(command["command_id"])
    return sorted(set(result))


def provider_entry(case):
    path = case.provider_records / "provider.entry.json"
    return read_json(path, MAX_CONTROL_BYTES)


def provider_completion(case):
    path = case.provider_records / "provider.complete.json"
    return read_json(path, MAX_CONTROL_BYTES)


def events(case):
    found = []
    for path in sorted(events_dir_files(case)):
        try:
            value = read_json(path, MAX_CONTROL_BYTES)
        except (FileNotFoundError, json.JSONDecodeError):
            continue
        if isinstance(value, dict):
            found.append(value)
    return found


def events_dir_files(case):
    return case.events_dir.rglob("*.json")


def wait_event(case, name, timeout=CASE_TIMEOUT):
    def find():
        return name in event_names(case)

    return wait_until(find, timeout=timeout, description="execution event " + name)


def event_names(case):
    return [
        value.get("name", value.get("event"))
        for value in events(case)
        if value.get("name", value.get("event")) is not None
    ]


def execution_sequences(case):
    grouped = {}
    for value in events(case):
        if value.get("kind") != "execution":
            continue
        grouped.setdefault(value.get("invocation_id"), []).append(value)
    return [
        sorted(values, key=lambda value: value.get("sequence", 0))
        for values in grouped.values()
    ]


def runner_execution_sequence(case):
    candidates = [
        values for values in execution_sequences(case)
        if any(value.get("name") == "start-entry" for value in values)
    ]
    require_true(len(candidates) == 1,
                 "expected exactly one runner start-entry, found %d" % len(candidates))
    return candidates[0]


def invocation_counts(case):
    entries = supervisor_entries(case)
    require_supervisor_recorder(case, entries)
    names = event_names(case)
    # The root convenience receipts are first-writer summaries. Count
    # immutable per-invocation receipts instead, so a duplicate provider
    # launch cannot hide behind the same root filename.
    provider_entries = []
    child_entries = []
    allowed_task_ids = set(case.chain_task_ids)
    for path in sorted((case.provider_records / "invocations").glob("*/provider.entry.json")):
        value = read_json(path, MAX_CONTROL_BYTES)
        require_true(value.get("invocation_id") == path.parent.name,
                     "provider invocation receipt ID changed")
        require_true(value.get("task_id") in allowed_task_ids and value.get("pid", 0) > 0,
                     "provider invocation receipt identity changed")
        if value.get("scenario") == "child" or "--child" in value.get("process_argv", []):
            child_entries.append(path)
        else:
            provider_entries.append(path)
    return {
        "A": sum(1 for item in entries if item.get("verb") == "add"),
        "Q": sum(1 for item in entries if item.get("verb") == "enqueue"),
        "K": sum(1 for item in entries if item.get("verb") == "kill"),
        "M": sum(1 for item in entries if item.get("verb") == "remove"),
        "S": names.count("start-entry"),
        "E": len(provider_entries),
        "child_E": len(child_entries),
        "supervisor_entries": len(entries),
        "event_names": names,
    }
