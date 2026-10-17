"""Finite control acceptance scenarios."""

from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.hermetic.contract import CASE_TIMEOUT
from scripts.delegate_acceptance.hermetic.contract import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.hermetic.contract import require_true
from scripts.delegate_acceptance.hermetic.evidence import require_bound_records
from scripts.delegate_acceptance.hermetic.evidence import require_provider_evidence
from scripts.delegate_acceptance.hermetic.evidence import require_publication
from scripts.delegate_acceptance.hermetic.evidence import require_supervisor_argv
from scripts.delegate_acceptance.hermetic.evidence import require_terminal_evidence
from scripts.delegate_acceptance.hermetic.files import task_id
from scripts.delegate_acceptance.hermetic.observations import assert_counts
from scripts.delegate_acceptance.hermetic.observations import event_names
from scripts.delegate_acceptance.hermetic.observations import wait_entry
import time


def run_c01_mismatch(case):
    case.dispatch_dynamic()
    case.set_empty_status()
    _, response = case.run_command("cancel-unmatched", "cancel", expected=(1,))
    require_true((case.task_dir() / "stop").exists(), "unmatched stop request disappeared")
    require_true(response.get("error"), "unmatched stop lacked unknown result")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_c02_valid(case):
    case.dispatch_dynamic()
    case.set_status("running")
    _, response = case.run_command("cancel-running", "cancel", expected=(0,))
    stop = response.get("stop")
    require_true(isinstance(stop, dict), "valid running cancel lacked stop response")
    require_true(
        stop.get("requested") is True and stop.get("matched") is True and
        stop.get("attempted") is True and stop.get("action") == "kill" and
        stop.get("numeric_task_id") == case.numeric_id and
        stop.get("acknowledged") is True and stop.get("terminated") is False,
        "valid running stop facts were not distinct",
    )
    require_supervisor_argv(
        case, "kill",
        ["-c", str(case.production_config), "kill", str(case.numeric_id)],
    )
    assert_counts(case, A=1, S=0, E=0, K=1, M=0)


def run_c02_mismatch(case):
    case.dispatch_dynamic()
    case.set_status("running", numeric_id=case.numeric_id + 1)
    _, response = case.run_command("cancel-wrong-id", "cancel", expected=(1,))
    require_true(response.get("error"), "mismatched numeric stop did not remain unknown")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_c03_queued(case):
    case.dispatch_dynamic()
    case.set_status("queued")
    _, response = case.run_command("cancel-queued", "cancel", expected=(0,))
    stop = response.get("stop")
    require_true(isinstance(stop, dict), "queued cancellation lacked stop response")
    require_true(
        stop.get("requested") is True and stop.get("matched") is True and
        stop.get("attempted") is True and stop.get("action") == "remove" and
        stop.get("numeric_task_id") == case.numeric_id and
        stop.get("acknowledged") is True and stop.get("terminated") is False,
        "queued removal facts were not distinct",
    )
    require_supervisor_argv(
        case, "remove",
        ["-c", str(case.production_config), "remove", str(case.numeric_id)],
    )
    assert_counts(case, A=1, S=0, E=0, K=0, M=1)


def run_c03_transition(case):
    case.dispatch_dynamic()
    case.set_status("queued")
    case.update_fake("remove", release_path=str(case.remove_release), exit_code=7)
    process = case.processes.start(
        "cancel-transition",
        case.command_argv("cancel"),
        case.base,
    )
    remove = wait_entry(case, "remove")
    case.set_status("running")
    case.release(case.remove_release)
    process.wait(CASE_TIMEOUT, expected=(1,))
    response = process.json()
    require_true(response.get("error"), "refused queue removal was reported as success")
    stop = response.get("stop")
    require_true(
        isinstance(stop, dict) and stop.get("action") == "remove" and
        stop.get("acknowledged") is False and stop.get("terminated") is False,
        "refused queue removal changed stop facts",
    )
    require_true(remove["argv"] == [
        "-c", str(case.production_config), "remove", str(case.numeric_id)
    ], "queue transition used an unexpected remove argv")
    assert_counts(case, A=1, S=0, E=0, K=0, M=1)


def run_c04(case):
    case.dispatch_dynamic()
    case.set_status("running")
    _, cancel = case.run_command("cancel-ack", "cancel", expected=(0,))
    require_true((case.task_dir() / "stop").exists(), "acknowledged stop request absent")
    stop = cancel.get("stop")
    require_true(
        isinstance(stop, dict) and stop.get("requested") is True and
        stop.get("matched") is True and stop.get("acknowledged") is True and
        stop.get("terminated") is False and stop.get("observed_state") == "running" and
        stop.get("numeric_task_id") == case.numeric_id and stop.get("action") == "kill",
        "acknowledgment was conflated with termination",
    )
    request_id = stop["request_id"]
    request = case.stop_record(request_id)
    reply = read_json(case.task_dir() / "stop" / (request_id + ".reply.json"), MAX_CONTROL_BYTES)
    require_true(request["request_id"] == request_id and request["cause"] == "user" and
                 request["numeric_task_id"] == case.numeric_id,
                 "stop request identity was not durable")
    require_true(reply["request_id"] == request_id and reply["acknowledged"] is True,
                 "stop acknowledgment reply was not durable")
    require_true(not (case.task_dir() / "stop" / (request_id + ".observed.json")).exists(),
                 "termination observation was fabricated at acknowledgment")
    case.set_status("ended")
    _, status = case.run_command("status-ended-stop", "status", expected=(0,))
    status_stops = status.get("stops") or []
    observed = next((item for item in status_stops if item.get("request_id") == request_id), None)
    require_true(
        observed is not None and observed.get("acknowledged") is True and
        observed.get("terminated") is True and observed.get("observed_state") == "ended",
        "positive ended observation did not update the same stop request",
    )
    observed_record = read_json(
        case.task_dir() / "stop" / (request_id + ".observed.json"), MAX_CONTROL_BYTES
    )
    require_true(
        observed_record["request_id"] == request_id and
        observed_record["task_id"] == case.task_id and
        observed_record["state"] == "ended" and
        observed_record["terminated"] is True,
        "ended observation record changed scope",
    )
    _, collected = case.run_command("collect-without-seal", "collect", expected=(3,))
    require_true(collected.get("outcome") is None, "stop evidence fabricated outcome")
    assert_counts(case, A=1, S=0, E=0, K=1, M=0)


def run_c05(case):
    case.update_provider(
        scenario="hold", lifetime_ms=CASE_TIMEOUT * 1000,
        rendezvous_path=str(case.provider_release),
    )
    case.update_fake("kill", exit_code=7)
    case.dispatch_dynamic()
    case.set_status("running")
    hold_started = time.monotonic()
    runner = case.start_runner(expected=(0,))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    _, cancel = case.run_command("cancel-refused", "cancel", expected=(1,))
    require_true(cancel.get("error"), "failed stop lacked operational error")
    stop = cancel.get("stop") or {}
    require_true(stop.get("requested") is True and stop.get("acknowledged") is False and
                 stop.get("terminated") is False,
                 "failed cancellation fabricated acknowledgement or termination")
    request = case.stop_record(stop["request_id"])
    require_true(request.get("cause") == "user", "failed cancellation lost durable intent")
    require_true(not (case.task_dir() / "outcome.json").exists(),
                 "held provider published before explicit release")
    case.release_provider()
    require_true(time.monotonic() - hold_started < CASE_TIMEOUT,
                 "provider hold could have expired before explicit release")
    response = case.finish_runner(runner, expected=(0,))
    require_publication(response, "committed")
    require_terminal_evidence(case, response)
    task_dir = case.task_dir()
    outcome_before = (task_dir / "outcome.json").read_bytes()
    payload_descriptor = read_json(task_dir / "outcome.json", MAX_CONTROL_BYTES)["payload"]
    payload_path = task_dir / payload_descriptor["basename"]
    payload_before = payload_path.read_bytes()
    case.set_status("ended")
    _, terminal = case.run_command("cancel-terminal", "cancel", expected=(0,))
    require_publication(terminal, "committed")
    require_true((task_dir / "outcome.json").read_bytes() == outcome_before,
                 "terminal cancellation changed the sealed outcome")
    require_true(payload_path.read_bytes() == payload_before,
                 "terminal cancellation changed the sealed payload")
    require_terminal_evidence(case, terminal)
    require_provider_evidence(case)
    assert_counts(case, A=1, S=1, E=1, K=1, M=0)


def run_c05_before_start(case):
    case.update_fake("kill", exit_code=7)
    case.dispatch_dynamic()
    case.set_status("running")
    _, cancel = case.run_command("cancel-before-start", "cancel", expected=(1,))
    stop = cancel.get("stop") or {}
    require_true(cancel.get("error") and stop.get("requested") is True and
                 stop.get("acknowledged") is False and stop.get("terminated") is False,
                 "failed stop changed its durable cancellation authority")
    require_true(case.stop_record(stop["request_id"]).get("cause") == "user",
                 "failed cancellation lost durable intent")
    runner = case.start_runner(expected=(1,))
    response = case.finish_runner(runner, expected=(1,))
    require_true(response.get("error"), "cancelled task launch was not refused")
    require_true(not (case.provider_records / "provider.entry.json").exists() and
                 all(not (case.task_dir() / name).exists()
                     for name in ("provider.start", "provider.exit", "outcome.json")),
                 "cancelled task launched a provider or fabricated terminal evidence")
    require_bound_records(case)
    assert_counts(case, A=1, S=0, E=0, K=1, M=0)


def run_c06(case):
    case.dispatch_dynamic()
    case.set_empty_status()
    _, first = case.run_command("cancel-unknown", "cancel", expected=(1,))
    require_true(first.get("error"), "first unresolved cancel lacked uncertainty")
    case.run_command("status-poll-one", "status", expected=(0,))
    case.run_command("status-poll-two", "status", expected=(0,))
    case.set_status("running")
    _, second = case.run_command("cancel-explicit-later", "cancel", expected=(0, 1))
    require_true(second.get("stop") is not None or second.get("stops"),
                 "later explicit cancel lacked a new response")
    requests = list((case.task_dir() / "stop").glob("*.request.json"))
    require_true(len(requests) == 2, "explicit later cancel did not receive new request ID")
    assert_counts(case, A=1, S=0, E=0, K=1, M=0)


def run_c07_locked_unknown(case):
    """A matching pueue Locked row stays unknown before a fresh provider Start."""
    case.dispatch_dynamic(status="queued")

    untargeted_label = "delegate:" + case.root_id + ":" + task_id()
    case.set_status("locked", label=untargeted_label)
    untargeted = case.start_runner(name="locked-untargeted")
    untargeted_response = case.finish_runner(untargeted, expected=(1,))
    untargeted_supervisor = untargeted_response.get("supervisor") or {}
    require_true(
        untargeted_supervisor.get("matched") is False and
        untargeted_supervisor.get("state") == "unknown" and
        untargeted_response.get("error"),
        "untargeted Locked row was not rejected as unknown",
    )

    case.set_status("locked")
    runner = case.start_runner(name="locked-matching")
    response = case.finish_runner(runner, expected=(1,))
    supervisor = response.get("supervisor") or {}
    require_true(
        supervisor.get("matched") is True and
        supervisor.get("state") == "unknown" and
        supervisor.get("numeric_task_id") == case.numeric_id and
        response.get("liveness") == "undetermined" and
        response.get("error") and "unknown" in response["error"].lower(),
        "matching Locked row was not refused before provider Start",
    )
    require_true(
        not (case.task_dir() / "provider.start").exists() and
        not (case.provider_records / "provider.entry.json").exists() and
        "start-entry" not in event_names(case),
        "unknown supervisor state reached provider Start",
    )
    require_bound_records(case)
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_c08_ended_unsealed_cancel(case):
    """An ended supervised row can be cancelled without kill/remove mutation."""
    case.dispatch_dynamic(status="queued")
    case.set_status("ended")
    _, response = case.run_command("cancel-ended-unsealed", "cancel", expected=(0,))
    stop = response.get("stop")
    require_true(
        isinstance(stop, dict) and
        stop.get("requested") is True and
        stop.get("matched") is True and
        stop.get("attempted") is False and
        stop.get("numeric_task_id") == case.numeric_id and
        stop.get("acknowledged") is None and
        stop.get("observed_state") == "ended" and
        stop.get("terminated") is True,
        "immediate ended cancellation did not report proven termination",
    )
    request_id = stop["request_id"]
    request = case.stop_record(request_id)
    observation = read_json(
        case.task_dir() / "stop" / (request_id + ".observed.json"), MAX_CONTROL_BYTES
    )
    require_true(
        request["request_id"] == request_id and
        request["task_id"] == case.task_id and
        request["numeric_task_id"] == case.numeric_id,
        "ended cancellation request was not durably bound",
    )
    require_true(
        observation["request_id"] == request_id and
        observation["task_id"] == case.task_id and
        observation["numeric_task_id"] == case.numeric_id and
        observation["state"] == "ended" and
        observation["terminated"] is True,
        "ended cancellation response disagreed with persisted observation",
    )
    _, reloaded = case.run_command("status-ended-after-cancel", "status", expected=(0,))
    stop_rows = reloaded.get("stops") or []
    persisted = next(
        (row for row in stop_rows if row.get("request_id") == request_id), None
    )
    require_true(
        isinstance(persisted, dict) and
        persisted.get("observed_state") == "ended" and
        persisted.get("terminated") is True,
        "fresh status did not preserve persisted ended observation",
    )
    require_true(
        not (case.task_dir() / "provider.start").exists() and
        not (case.task_dir() / "provider.exit").exists() and
        not (case.task_dir() / "outcome.json").exists(),
        "ended unsealed cancellation fabricated provider terminal evidence",
    )
    require_bound_records(case)
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)
