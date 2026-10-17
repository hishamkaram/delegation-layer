"""Exercise finite continuation turns and validate their sealed evidence."""

from pathlib import Path
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.hermetic.contract import CASE_TIMEOUT
from scripts.delegate_acceptance.hermetic.contract import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.hermetic.contract import MODE
from scripts.delegate_acceptance.hermetic.contract import PREDICATE
from scripts.delegate_acceptance.hermetic.contract import PREDICATE_SHA256
from scripts.delegate_acceptance.hermetic.contract import add_entry_label
from scripts.delegate_acceptance.hermetic.contract import require_true
from scripts.delegate_acceptance.hermetic.evidence import require_publication
from scripts.delegate_acceptance.hermetic.files import task_id
from scripts.delegate_acceptance.hermetic.files import write_bytes
from scripts.delegate_acceptance.hermetic.observations import assert_counts
from scripts.delegate_acceptance.hermetic.observations import execution_sequences
from scripts.delegate_acceptance.hermetic.observations import supervisor_entries
import copy
import json


def require_chain_session_release(case, record):
    """Prove a terminal continuation task released its predecessor claim."""
    matches = []
    for path in case.root.rglob("*.release.json"):
        try:
            value = read_json(path, MAX_CONTROL_BYTES)
        except (FileNotFoundError, json.JSONDecodeError):
            continue
        if value.get("task_id") == record["task_id"]:
            matches.append((path, value))
    require_true(len(matches) == 1,
                 "continuation release receipt count changed for " + record["label"])
    path, release = matches[0]
    require_true(
        release.get("schema_version") == 1 and
        release.get("root_id") == case.root_id and
        release.get("task_id") == record["task_id"] and
        release.get("provider") == PREDICATE and
        release.get("conversation_id") == record["session_id"] and
        isinstance(release.get("predecessor_evidence_sha256"), str) and
        len(release["predecessor_evidence_sha256"]) == 64 and
        release.get("predecessor_evidence_sha256") == record.get("outcome", {}).get("evidence_sha256") and
        release.get("released_at"),
        "continuation release receipt identity changed for " + record["label"],
    )
    record["session_release"] = {"path": str(path), **release}
    return release


def require_chain_task_links(case, records):
    require_true(len(records) == 3, "continuation chain did not create A/B/C records")
    expected_ids = [record["task_id"] for record in records]
    require_true(len(set(expected_ids)) == 3, "continuation chain reused a task ID")
    for index, record in enumerate(records):
        task_record = read_json(
            case.root / "tasks" / record["task_id"] / "task.json",
            MAX_CONTROL_BYTES,
        )
        require_true(
            task_record.get("root_id") == case.root_id and
            task_record.get("task_id") == record["task_id"] and
            task_record.get("provider") == PREDICATE and
            task_record.get("mode") == MODE,
            "continuation task request identity changed for " + record["label"],
        )
        prior = task_record.get("prior_session")
        if index == 0:
            require_true(prior is None, "initial chain task unexpectedly has a predecessor")
            continue
        previous = records[index - 1]
        require_true(
            prior == {
                "provider": PREDICATE,
                "conversation_id": record["session_id"],
                "predecessor_task_id": previous["task_id"],
            },
            "continuation predecessor link changed for " + record["label"],
        )


def require_chain_provider_receipts(case, records):
    invocations = case.provider_records / "invocations"
    paths = sorted(invocations.glob("*/provider.entry.json"))
    require_true(len(paths) == len(records),
                 "immutable provider invocation count changed for continuation chain")
    by_task = {}
    for path in paths:
        entry = read_json(path, MAX_CONTROL_BYTES)
        completion = read_json(path.parent / "provider.complete.json", MAX_CONTROL_BYTES)
        task_value = entry.get("task_id")
        require_true(task_value not in by_task,
                     "continuation chain launched a duplicate provider task")
        by_task[task_value] = (entry, completion)
        require_true(
            entry.get("invocation_id") == path.parent.name and
            entry.get("pid", 0) > 0 and
            completion.get("invocation_id") == entry.get("invocation_id") and
            completion.get("pid") == entry.get("pid") and
            completion.get("natural") is True and
            completion.get("exit_code") == 0,
            "provider natural completion receipt changed",
        )
    require_true(set(by_task) == {record["task_id"] for record in records},
                 "provider receipts do not cover exactly A/B/C")
    pids = {entry["pid"] for entry, _ in by_task.values()}
    require_true(len(pids) == len(records), "continuation chain reused a provider PID")
    for record in records:
        entry, completion = by_task[record["task_id"]]
        require_true(
            entry.get("session_id") == record["session_id"] and
            entry.get("cwd") == str(case.work) and
            completion.get("natural") is True,
            "provider session or natural completion identity changed for " + record["label"],
        )
        record["provider_entry"] = entry
        record["provider_completion"] = completion
    return by_task


def run_session_chain(case, expect_session_block=False):
    """Exercise public A -> B -> C terminal continuation ownership."""
    conversation = "supervisor-chain-" + case.task_id
    records = []
    record_a = _run_initial_chain_turn(case, conversation, records)
    record_b = _run_held_chain_turn(case, conversation, records, record_a, expect_session_block)
    record_c = _prepare_final_chain_turn(case, conversation, records)
    if expect_session_block:
        _require_baseline_session_block(case, conversation, records, record_b, record_c)
        return
    _run_final_chain_turn(case, record_b, record_c)
    _require_repeated_chain_collection(case, records)
    assert_counts(case, A=3, S=3, E=3, child_E=0, K=0, M=0)
    verbs = [entry.get("verb") for entry in supervisor_entries(case)]
    require_true(
        set(verbs).issubset({"add", "add-help", "enqueue", "enqueue-help", "parallel", "status", "--version"}) and
        verbs.count("add") == 3,
        "continuation chain made an unexpected supervisor call",
    )
    case.chain_summary = {
        "conversation_id": conversation,
        "task_ids": [record["task_id"] for record in records],
        "labels": [record["label"] for record in records],
        "natural_runner_waits": [record["runner_receipt"]["natural_wait"] for record in records],
        "natural_provider_completions": [record["provider_completion"]["natural"] for record in records],
    }


def _run_initial_chain_turn(case, conversation, records):
    record_a = configure_chain_task(case,
        "A",
        "chain A answer: α\nliteral \\n Ω\n",
        conversation,
        scenario="success",
        lifetime_ms=250,
        task_value=case.task_id,
        brief=case.brief,
    )
    records.append(record_a)
    case.dispatch_dynamic(status="queued")
    case.set_status("running", label="delegate:" + case.root_id + ":" + record_a["task_id"])
    runner_a = start_runner_for(case, record_a)
    response_a = finish_runner_for(case, record_a, runner_a)
    require_publication(response_a, "committed")
    case.set_status("ended", label="delegate:" + case.root_id + ":" + record_a["task_id"])
    wait_provider_entry_for_task(case, record_a)
    snapshot_chain_provider(case, record_a)
    require_chain_terminal(case, record_a, response_a)
    return record_a


def _run_held_chain_turn(case, conversation, records, record_a, expect_session_block):
    record_b = configure_chain_task(case,
        "B",
        "chain B answer: β\nheld until release\n",
        conversation,
        scenario="hold",
        lifetime_ms=5000,
        provider_hold=True,
        task_value=task_id(),
    )
    records.append(record_b)
    dispatch_chain_task(case, record_b, resume_task=record_a["task_id"])
    case.set_status("running", label="delegate:" + case.root_id + ":" + record_b["task_id"])
    runner_b = start_runner_for(case, record_b)
    entry_b = wait_provider_entry_for_task(case, record_b)
    wait_chain_provider_hold(case, record_b, entry_b)
    require_true(
        not (case.root / "tasks" / record_b["task_id"] / "provider.exit").exists() and
        not (case.root / "tasks" / record_b["task_id"] / "outcome.json").exists(),
        "held continuation published before its provider natural exit",
    )
    case.release_provider()
    response_b = finish_runner_for(case, record_b, runner_b)
    require_publication(response_b, "committed")
    case.set_status("ended", label="delegate:" + case.root_id + ":" + record_b["task_id"])
    snapshot_chain_provider(case, record_b)
    require_chain_terminal(case, record_b, response_b)
    if expect_session_block:
        release_paths = [
            path for path in case.root.rglob("*.release.json")
            if read_json(path, MAX_CONTROL_BYTES).get("task_id") == record_b["task_id"]
        ]
        require_true(not release_paths,
                     "baseline unexpectedly released the B continuation claim")
    else:
        require_chain_session_release(case, record_b)
    return record_b


def _prepare_final_chain_turn(case, conversation, records):
    record_c = configure_chain_task(case,
        "C",
        "chain C answer: γ\nthird terminal successor\n",
        conversation,
        scenario="success",
        lifetime_ms=250,
        provider_hold=False,
        task_value=task_id(),
    )
    records.append(record_c)
    return record_c


def _require_baseline_session_block(case, conversation, records, record_b, record_c):
    if case.add_release.exists():
        case.add_release.unlink()
    blocked = case.processes.start(
        "dispatch-chain-C-session-busy",
        case.dispatch_argv(
            brief=record_c["brief"],
            task=record_c["task_id"],
            extra=("--resume-task", record_b["task_id"]),
        ),
        case.base,
    )
    blocked.wait(CASE_TIMEOUT, expected=(1,))
    blocked_response = blocked.json()
    require_true(
        "session" in blocked_response.get("error", "").lower() and
        "busy" in blocked_response.get("error", "").lower(),
        "baseline continuation refusal did not identify a busy session",
    )
    require_true(
        not any(
            entry.get("verb") == "add" and
            add_entry_label(entry).endswith(":" + record_c["task_id"])
            for entry in supervisor_entries(case)
        ),
        "baseline session refusal reached supervisor admission",
    )
    require_chain_task_links(case, records)
    require_chain_provider_receipts(case, records[:2])
    assert_counts(case, A=2, S=2, E=2, child_E=0, K=0, M=0)
    verbs = [entry.get("verb") for entry in supervisor_entries(case)]
    require_true(
        set(verbs).issubset({"add", "add-help", "enqueue", "enqueue-help", "parallel", "status", "--version"}) and
        verbs.count("add") == 2,
        "baseline session refusal made an unexpected supervisor call",
    )
    case.chain_summary = {
        "conversation_id": conversation,
        "task_ids": [record["task_id"] for record in records],
        "blocked_successor": record_c["task_id"],
        "blocked_error": blocked_response["error"],
        "natural_runner_waits": [record["runner_receipt"]["natural_wait"] for record in records[:2]],
        "natural_provider_completions": [record["provider_completion"]["natural"] for record in records[:2]],
    }
    return


def _run_final_chain_turn(case, record_b, record_c):
    dispatch_chain_task(case, record_c, resume_task=record_b["task_id"])
    case.set_status("running", label="delegate:" + case.root_id + ":" + record_c["task_id"])
    runner_c = start_runner_for(case, record_c)
    response_c = finish_runner_for(case, record_c, runner_c)
    require_publication(response_c, "committed")
    case.set_status("ended", label="delegate:" + case.root_id + ":" + record_c["task_id"])
    wait_provider_entry_for_task(case, record_c)
    snapshot_chain_provider(case, record_c)
    require_chain_terminal(case, record_c, response_c)
    require_chain_session_release(case, record_c)


def _require_repeated_chain_collection(case, records):
    require_chain_task_links(case, records)
    require_chain_provider_receipts(case, records)
    expected_outcomes = {
        record["task_id"]: record["outcome"] for record in records
    }
    for record in records:
        provider_ref_before = (case.root / "tasks" / record["task_id"] / "provider.ref.json").read_bytes()
        _, collected = run_command_for(case,
            record,
            "collect-chain-" + record["label"],
            "collect",
            expected=(0,),
        )
        _, collected_again = run_command_for(case,
            record,
            "collect-chain-repeat-" + record["label"],
            "collect",
            expected=(0,),
        )
        for value in (collected, collected_again):
            require_publication(value, "committed")
            require_true(
                value.get("task_id") == record["task_id"] and
                value.get("outcome") == expected_outcomes[record["task_id"]] and
                value.get("payload") == record["outcome"]["payload"],
                "terminal collection changed chain identity for " + record["label"],
            )
        provider_ref_after = (case.root / "tasks" / record["task_id"] / "provider.ref.json").read_bytes()
        require_true(provider_ref_after == provider_ref_before,
                     "terminal collection changed provider session evidence")



def run_session_chain_baseline(case):
    """Record the pre-fix negative control against an old helper binary set."""
    run_session_chain(case, expect_session_block=True)


def configure_chain_task(case, label, answer, session_id, scenario="success",
                         lifetime_ms=250, provider_hold=False,
                         task_value=None, brief=None):
    """Retarget the acceptance-only provider config for one chain task.

    The profile/config path stays fixed, while the task ID and session
    identity are changed before that task is dispatched.  Provider
    invocation receipts remain in the shared immutable invocation tree;
    this is why the chain can prove three distinct natural launches while
    the public CLI continues to use one root and one supervisor binding.
    """
    if task_value is None:
        task_value = task_id()
    if brief is None:
        brief = case.base / ("chain brief " + label + " ' ; literal.md")
        write_bytes(
            brief,
            (
                "Continuation chain brief " + label + "\n"
                "literal slash-n token: \\n\n"
                "$(touch DO_NOT_CREATE) ; <&> | literal\n"
            ).encode(),
        )
    settings = dict(case.provider_settings)
    settings.update(
        {
            "scenario": scenario,
            "task_id": task_value,
            "session_id": session_id,
            "answer": answer,
            "lifetime_ms": lifetime_ms,
            "rendezvous_path": str(case.provider_release) if provider_hold else "",
            "child_lifetime_ms": 0,
        }
    )
    write_json(case.provider_config, settings, replace=True)
    profile = read_json(case.profile_config)
    profile["provider_config_sha256"] = digest(case.provider_config)
    write_json(case.profile_config, profile, replace=True)
    case.provider_settings = settings
    if task_value not in case.chain_task_ids:
        case.chain_task_ids.append(task_value)
    record = {
        "label": label,
        "task_id": task_value,
        "brief": Path(brief),
        "answer": answer,
        "session_id": session_id,
        "scenario": scenario,
        "provider_settings": copy.deepcopy(settings),
    }
    case.chain_records.append(record)
    return record


def wait_entry_for_task(case, task_value, timeout=CASE_TIMEOUT):
    """Return the one add receipt whose label names task_value."""
    require_true(case.root_id is not None, "chain root identity is not known")
    label = "delegate:" + case.root_id + ":" + task_value

    def find():
        for path in sorted(case.supervisor_records.glob("*.entry.json")):
            try:
                entry = read_json(path, MAX_CONTROL_BYTES)
            except (FileNotFoundError, json.JSONDecodeError):
                continue
            if entry.get("verb") == "add" and add_entry_label(entry) == label:
                return entry
        return None

    return wait_until(
        find,
        timeout=timeout,
        description="fake supervisor add entry for " + task_value,
    )


def dispatch_chain_task(case, record, resume_task=None, expected=(0,)):
    """Submit one chain task through the public dispatch executable."""
    # Each fake add has an explicit release rendezvous.  Remove only this
    # harness-owned marker so the next add has the same controlled
    # entry/status/release observation as the first one.
    if case.add_release.exists():
        case.add_release.unlink()
    extra = () if resume_task is None else ("--resume-task", resume_task)
    process = case.processes.start(
        "dispatch-chain-" + record["label"],
        case.dispatch_argv(
            brief=record["brief"],
            task=record["task_id"],
            extra=extra,
        ),
        case.base,
    )
    entry = wait_entry_for_task(case, record["task_id"])
    case.set_status(
        "queued",
        label="delegate:" + case.root_id + ":" + record["task_id"],
    )
    case.release_add()
    process.wait(CASE_TIMEOUT, expected=expected)
    response = process.json()
    require_true(response.get("task_id") == record["task_id"],
                 "chain dispatch response task mismatch")
    require_true(response.get("admission") == "admitted",
                 "chain dispatch was not admitted")
    record["dispatch_entry"] = entry
    record["dispatch_response"] = response
    require_chain_bound_records(case, record)
    return process, response


def require_chain_bound_records(case, record):
    task_dir = case.root / "tasks" / record["task_id"]
    task_record = read_json(task_dir / "task.json", MAX_CONTROL_BYTES)
    submit = read_json(task_dir / "submit.json", MAX_CONTROL_BYTES)
    meta = read_json(task_dir / "meta.json", MAX_CONTROL_BYTES)
    require_true(task_record["root_id"] == case.root_id and
                 task_record["task_id"] == record["task_id"] and
                 task_record["provider"] == PREDICATE,
                 "chain task request identity changed")
    require_true(submit["label"] == "delegate:" + case.root_id + ":" + record["task_id"] and
                 submit["supervisor"]["config_path"] == str(case.production_config) and
                 submit["supervisor"]["client_executable"] == str(case.pueue),
                 "chain supervisor binding changed")
    require_true(meta["provider_executable"] == str(case.provider) and
                 meta["provider_version"] == "fixture-v2",
                 "chain provider binding changed")
    return task_dir


def start_runner_for(case, record):
    return case.processes.start(
        "runner-chain-" + record["label"],
        [str(case.runner), "--root", str(case.root), record["task_id"], "--json"],
        case.base,
    )


def finish_runner_for(case, record, process, expected=(0,)):
    process.wait(CASE_TIMEOUT, expected=expected)
    allowed = {expected} if isinstance(expected, int) else set(expected)
    require_true(process.result["natural_wait"] is True and
                 process.result["exit_code"] in allowed,
                 "chain runner did not complete with a natural wait")
    response = process.json()
    record["runner_receipt"] = copy.deepcopy(process.result)
    record["runner_response"] = response
    return response


def run_command_for(case, record, name, command, expected=(0,), extra=()):
    """Run a public read/control command against one chained task ID."""
    argv = [
        str(case.delegate),
        "--root",
        str(case.root),
        command,
        record["task_id"],
        "--json",
        *extra,
    ]
    process = case.processes.run(
        name,
        argv,
        case.base,
        expected=expected,
        timeout=CASE_TIMEOUT,
    )
    response = process.json()
    record.setdefault("control_receipts", []).append({
        "name": name,
        "command": command,
        "response": response,
        "process": copy.deepcopy(process.result),
    })
    return process, response


def wait_provider_entry_for_task(case, record, timeout=CASE_TIMEOUT):
    invocations = case.provider_records / "invocations"

    def find():
        if not invocations.is_dir():
            return None
        for directory in sorted(invocations.iterdir()):
            path = directory / "provider.entry.json"
            if not path.exists():
                continue
            try:
                entry = read_json(path, MAX_CONTROL_BYTES)
            except (FileNotFoundError, json.JSONDecodeError):
                continue
            if entry.get("task_id") == record["task_id"]:
                return entry
        return None

    entry = wait_until(
        find,
        timeout=timeout,
        description="provider invocation entry for " + record["task_id"],
    )
    record["provider_entry"] = entry
    return entry


def wait_chain_provider_hold(case, record, entry, timeout=CASE_TIMEOUT):
    waiting = Path(str(case.provider_release) + ".waiting")
    case.wait_path(waiting, timeout=timeout)
    expected = (str(entry["pid"]) + ":waiting\n").encode()
    require_true(waiting.read_bytes() == expected,
                 "chain provider rendezvous PID changed")
    return entry["pid"]


def snapshot_chain_provider(case, record, expected_exit=0):
    """Freeze independent provider artifacts before the next task rewrites them."""
    entry = record["provider_entry"]
    invocation_dir = Path(entry["invocation_dir"])
    completion_path = invocation_dir / "provider.complete.json"
    completion = wait_until(
        lambda: read_json(completion_path, MAX_CONTROL_BYTES)
        if completion_path.exists() else None,
        timeout=CASE_TIMEOUT,
        description="provider completion for " + record["task_id"],
    )
    settings = record["provider_settings"]
    expected_argv = [str(case.provider), str(case.provider_config), *settings["argv"]]
    brief_bytes = (case.provider_records / "brief.input").read_bytes()
    require_true(brief_bytes == record["brief"].read_bytes(),
                 "chain provider stdin bytes changed for " + record["label"])
    require_true(entry["task_id"] == record["task_id"] and
                 entry["session_id"] == record["session_id"] and
                 entry["process_argv"] == expected_argv and
                 entry["pid"] > 0 and completion["pid"] == entry["pid"] and
                 completion["natural"] is True and
                 completion["exit_code"] == expected_exit,
                 "chain provider completion identity changed")
    for name in ("stdout", "stderr"):
        source = case.provider_records / ("expected." + name)
        target = case.base / ("chain-" + record["label"] + ".expected." + name)
        write_bytes(target, source.read_bytes())
        record["expected_" + name] = target
    brief_target = case.base / ("chain-" + record["label"] + ".brief.input")
    write_bytes(brief_target, brief_bytes)
    record["expected_brief"] = brief_target
    record["provider_completion"] = completion
    return entry, completion


def chain_execution_sequence(case, record):
    candidates = [
        values for values in execution_sequences(case)
        if any(record["task_id"] in value.get("argv", []) for value in values)
    ]
    require_true(len(candidates) == 1,
                 "expected one runner event sequence for " + record["task_id"])
    return candidates[0]


def require_chain_terminal(case, record, response):
    task_dir = case.root / "tasks" / record["task_id"]
    seal = read_json(task_dir / "provider.exit", MAX_CONTROL_BYTES)
    outcome = read_json(task_dir / "outcome.json", MAX_CONTROL_BYTES)
    require_true(response.get("publication") == "committed" and
                 outcome["verdict"] == "committed" and
                 response.get("outcome") == outcome,
                 "chain task did not publish one committed winner")
    for value in (seal, outcome):
        require_true(value["schema_version"] == 1 and
                     value["root_id"] == case.root_id and
                     value["task_id"] == record["task_id"],
                     "chain terminal identity changed")
        require_true(value["predicate"] == {
            "adapter": "fixture:test",
            "mode": "read-only",
            "version": "2",
            "sha256": PREDICATE_SHA256,
        }, "chain predicate identity changed")
    require_true(seal["invocation_state"] == "started" and
                 seal["exit_code"] == 0 and seal["error"] == "",
                 "chain provider seal was not a clean natural exit")
    require_true(outcome["spec_sha256"] == seal["spec_sha256"] and
                 outcome["meta_sha256"] == seal["meta_sha256"] and
                 outcome["evidence_sha256"] == seal["manifest_sha256"] and
                 outcome["predicate"] == seal["predicate"],
                 "chain outcome changed sealed identity")
    for name in ("stderr", "stdout"):
        expected = record["expected_" + name].read_bytes()
        actual = (task_dir / "raw" / name).read_bytes()
        require_true(actual == expected,
                     "chain raw " + name + " bytes changed")
    payload_descriptor = outcome["payload"]
    require_true(payload_descriptor["basename"] == "result.txt" and
                 (task_dir / "result.txt").read_bytes() == record["answer"].encode() and
                 payload_descriptor["length"] == len(record["answer"].encode()) and
                 payload_descriptor["sha256"] == sha(record["answer"].encode()),
                 "chain payload bytes or descriptor changed")
    require_true(response.get("payload") == payload_descriptor and
                 response.get("evidence_sha256") == seal["manifest_sha256"],
                 "chain response lost terminal identity")
    provider_ref = read_json(task_dir / "provider.ref.json", MAX_CONTROL_BYTES)
    require_true(provider_ref["provider"] == PREDICATE and
                 provider_ref["root_id"] == case.root_id and
                 provider_ref["task_id"] == record["task_id"] and
                 provider_ref["conversation_id"] == record["session_id"] and
                 provider_ref["meta_sha256"] == sha((task_dir / "meta.json").read_bytes()) and
                 provider_ref["spec_sha256"] == sha((task_dir / "task.json").read_bytes()),
                 "chain provider session identity was not durably recorded")
    record["seal"] = seal
    record["outcome"] = outcome
    record["provider_ref"] = provider_ref
    _require_chain_execution_events(case, record)


def _require_chain_execution_events(case, record):
    values = chain_execution_sequence(case, record)
    names = [value.get("name") for value in values]
    wait_events = [value for value in values if value.get("name") == "wait-completed"]
    require_true(len(wait_events) == 1,
                 "chain runner did not record exactly one Wait event")
    runner_receipt = record.get("runner_receipt") or {}
    require_true(
        wait_events[0].get("pid") == runner_receipt.get("pid") and
        wait_events[0].get("argv") == values[0].get("argv") and
        runner_receipt.get("natural_wait") is True and
        runner_receipt.get("exit_code") == 0,
        "chain Wait event is not bound to its owned runner process",
    )
    provider_entry = record.get("provider_entry")
    provider_completion = record.get("provider_completion")
    require_true(
        isinstance(provider_entry, dict) and isinstance(provider_completion, dict) and
        provider_entry.get("task_id") == record["task_id"] and
        provider_entry.get("session_id") == record["session_id"] and
        provider_entry.get("pid", 0) > 0 and
        provider_completion.get("pid") == provider_entry.get("pid") and
        provider_completion.get("natural") is True and
        provider_completion.get("exit_code") == 0,
        "chain Wait lacks a unique natural provider receipt binding",
    )
    for name in (
        "start-entry", "started", "parent-fds-closed", "started-receipt",
        "wait-completed", "stdout-eof", "stderr-eof", "stdout-raw-closed",
        "stderr-raw-closed", "completion-observed", "timer-disarmed",
        "sealed", "published",
    ):
        require_true(name in names,
                     "chain runner event missing: " + record["task_id"] + ":" + name)
    position = {name: names.index(name) for name in names}
    require_true(position["start-entry"] < position["started"] and
                 position["wait-completed"] < position["completion-observed"] <
                 position["timer-disarmed"] < position["sealed"] <
                 position["published"],
                 "chain runner Wait/seal events are out of order")
    require_true(position["stdout-eof"] < position["stdout-raw-closed"] <
                 position["completion-observed"] and
                 position["stderr-eof"] < position["stderr-raw-closed"] <
                 position["completion-observed"],
                 "chain capture close events are out of order")

