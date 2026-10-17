"""Finite budget acceptance scenarios."""

from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.hermetic.contract import CASE_TIMEOUT
from scripts.delegate_acceptance.hermetic.contract import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.hermetic.contract import add_entry_label
from scripts.delegate_acceptance.hermetic.contract import label_parts
from scripts.delegate_acceptance.hermetic.contract import require_true
from scripts.delegate_acceptance.hermetic.evidence import require_bound_records
from scripts.delegate_acceptance.hermetic.evidence import require_live_raw_descriptors
from scripts.delegate_acceptance.hermetic.evidence import require_provider_evidence
from scripts.delegate_acceptance.hermetic.evidence import require_publication
from scripts.delegate_acceptance.hermetic.evidence import require_terminal_evidence
from scripts.delegate_acceptance.hermetic.files import write_bytes
from scripts.delegate_acceptance.hermetic.observations import assert_counts
from scripts.delegate_acceptance.hermetic.observations import event_names
from scripts.delegate_acceptance.hermetic.observations import wait_entry
from scripts.delegate_acceptance.hermetic.observations import wait_event
from scripts.delegate_acceptance.hermetic.observations import wait_supervisor_completion
from scripts.delegate_acceptance.hermetic.scenarios_common import complete_success_case
import time


def run_b01_invalid_budget(case):
    for invalid in ("0s", "-1s", "999999999999999999999999h"):
        _, response = case.dispatch_without_admission(budget=invalid, expected=(2,))
        require_true(response.get("error"), "invalid budget lacked diagnostic")
    for extra in (("--token-budget", "10"), ("--dollar-budget", "10")):
        _, response = case.dispatch_without_admission(extra=extra, expected=(2,))
        require_true(response.get("error"), "unsupported ceiling lacked diagnostic")
    process = case.processes.start(
        "invalid-watch",
        case.command_argv("collect", extra=("--watch", "-1ns")),
        case.base,
    )
    process.wait(CASE_TIMEOUT, expected=(2,))
    require_true((process.directory / "stdout").stat().st_size == 0 and
                 "watch" in (process.directory / "stderr").read_text().lower(),
                 "invalid watch lacked parse diagnostic")
    oversized = case.base / "oversized brief.md"
    write_bytes(oversized, b"x" * (8 * 1024 * 1024 + 1))
    _, response = case.dispatch_without_admission(brief=oversized, expected=(2,))
    require_true(response.get("error"), "oversized brief lacked diagnostic")
    _, response = case.dispatch_without_admission(
        provider="codex:exec", expected=(2,)
    )
    require_true(response.get("error"), "native profile was admitted by fixture CLI")
    assert_counts(case, A=0, S=0, E=0, K=0, M=0)


def run_b01_raw_argv(case):
    process = case.processes.start(
        "raw-argv",
        case.dispatch_argv(extra=("--", "inert-provider-argv")),
        case.base,
    )
    process.wait(CASE_TIMEOUT, expected=(2,))
    require_true((process.directory / "stdout").stat().st_size == 0,
                 "raw argv refusal unexpectedly published a response")
    require_true("raw argv" in (process.directory / "stderr").read_text().lower(),
                 "raw argv refusal lacked diagnostic")
    assert_counts(case, A=0, S=0, E=0, K=0, M=0)


def run_b01_valid(case):
    process = case.start_dispatch(omit_budget=True)
    entry = wait_entry(case, "add")
    case.root_id, _ = label_parts(add_entry_label(entry))
    case.set_status("queued")
    case.release_add()
    process.wait(CASE_TIMEOUT, expected=(0,))
    response = process.json()
    require_true(response.get("admission") == "admitted",
                 "omitted budget dispatch was not admitted")
    task = read_json(case.task_dir() / "task.json", MAX_CONTROL_BYTES)
    require_true(task.get("budget_nanos") == 30 * 60 * 1_000_000_000 and
                 task.get("requested_config", {}).get("budget") == "30m0s",
                 "omitted budget was not persisted as the 30m default")
    started = time.monotonic()
    _, pending = case.run_command("default-watch", "collect", expected=(3,))
    require_true(time.monotonic() - started < 1.0 and pending.get("pending") is not None,
                 "default watch did not remain nonblocking")
    require_bound_records(case)
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_b02(case):
    case.update_fake("add", delay=1_500_000_000)
    process = case.start_dispatch()
    entry = wait_entry(case, "add")
    case.root_id, _ = label_parts(add_entry_label(entry))
    case.set_status("queued")
    queued_at = time.monotonic()
    time.sleep(1.1)
    require_true(not (case.task_dir() / "stop" / "budget.request.json").exists(),
                 "queued time armed a budget stop")
    require_true("start-entry" not in event_names(case) and
                 not (case.provider_records / "provider.entry.json").exists(),
                 "queued time started provider execution")
    case.release_add()
    process.wait(CASE_TIMEOUT, expected=(0,))
    require_true(time.monotonic() - queued_at > 1.0,
                 "queued observation did not exceed the recorded budget")
    response = process.json()
    require_true(response.get("admission") == "admitted" and
                 response.get("publication") == "unknown",
                 "queued dispatch returned an unexpected publication")
    require_bound_records(case)
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    response = case.finish_runner(runner, expected=(0,))
    require_publication(response, "committed")
    case.set_status("ended")
    require_provider_evidence(case)
    require_terminal_evidence(case, response)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_b03(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    _, pending = case.run_command("short-watch", "collect", expected=(3,), extra=("--watch", "10ms"))
    require_true(pending.get("publication") in ("pending", "unknown"),
                 "watch expiry changed task state")
    require_live_raw_descriptors(case, pending)
    case.release_provider()
    final = case.finish_runner(runner, expected=(0,))
    require_publication(final, "committed")
    require_provider_evidence(case)
    require_terminal_evidence(case, final)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_b04(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    case.wait_path(case.provider_records / "child.provider.entry.json")
    child_complete = case.provider_records / "child.provider.complete.json"
    wait_event(case, "wait-completed")
    wait_event(case, "deadline-observed")
    names = event_names(case)
    require_true(names.index("wait-completed") < names.index("deadline-observed"),
                 "budget expiry did not observe parent Wait first")
    require_true(not child_complete.exists(),
                 "budget case lost held-child boundary before stop request")
    require_true(not (case.task_dir() / "provider.exit").exists(),
                 "budget case sealed before held child released")
    case.wait_path(case.task_dir() / "stop" / "budget.request.json", timeout=5)
    kill = wait_entry(case, "kill")
    require_true(not child_complete.exists(),
                 "budget stop was observed after child pipe had already closed")
    case.release_provider()
    for event_name in ("stdout-eof", "stderr-eof", "stdout-raw-closed", "stderr-raw-closed"):
        wait_event(case, event_name)
    names = event_names(case)
    require_true(
        names.index("deadline-observed") < names.index("stdout-eof") <
        names.index("stdout-raw-closed") and
        names.index("deadline-observed") < names.index("stderr-eof") <
        names.index("stderr-raw-closed"),
        "budget expiry did not precede both capture drains",
    )
    response = case.finish_runner(runner, expected=(0, 1))
    require_true((case.task_dir() / "stop" / "budget.request.json").exists(),
                 "budget request was not persisted before stop")
    require_true(response.get("publication") in ("committed", "rejected", "unknown"),
                 "budget path returned an invalid publication")
    require_provider_evidence(case)
    require_true(child_complete.exists(), "held child did not complete naturally")
    assert_counts(case, child_E=1)
    require_true(kill["argv"] == ["-c", str(case.production_config), "kill", str(case.numeric_id)],
                 "budget stop used an unexpected supervisor argv")
    require_terminal_evidence(case, response)
    assert_counts(case, A=1, S=1, E=1, K=1, M=0)


def run_b05(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    # start-entry precedes preflight. This case tests expiry during an
    # already-authorized native Start, not expiry before launch authorization.
    wait_event(case, "start-authorized")
    case.wait_path(case.task_dir() / "stop" / "budget.request.json", timeout=5)
    require_true(case.start_release is not None, "start delayed case lacks release")
    case.release(case.start_release)
    response = case.finish_runner(runner, expected=(0, 1))
    require_true(response.get("task_id") == case.task_id, "delayed Start lost task")
    require_provider_evidence(case)
    assert_counts(case, A=1, S=1, E=1, K=1, M=0)


def run_b06_complete_first(case):
    response = complete_success_case(case)
    require_publication(response, "committed")
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_b06_expire_first(case):
    run_b04(case)


def run_b07(case):
    case.update_fake("kill", release_path=str(case.kill_release))
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    case.wait_path(case.task_dir() / "stop" / "budget.request.json", timeout=5)
    kill = wait_entry(case, "kill")
    require_true(not (case.supervisor_records / (kill["invocation_id"] + ".completion.json")).exists(),
                 "delayed stop reply completed before publication")
    # The provider owns its natural completion.  Release its finite hold so
    # the winner can publish while the stop acknowledgement remains delayed.
    case.release_provider()
    case.wait_path(case.task_dir() / "outcome.json", timeout=5)
    require_true(not (case.supervisor_records / (kill["invocation_id"] + ".completion.json")).exists(),
                 "delayed stop reply completed before natural publication")
    case.release(case.kill_release)
    wait_supervisor_completion(case, kill, timeout=5)
    require_true(case.publication_release is not None,
                 "publication-delayed case lacks release rendezvous")
    case.release(case.publication_release)
    response = case.finish_runner(runner, expected=(0, 1))
    require_true((case.task_dir() / "outcome.json").exists(),
                 "natural provider publication was blocked by stop reply")
    require_provider_evidence(case)
    require_terminal_evidence(case, response)
    require_true(response.get("publication") in ("committed", "rejected", "unknown"),
                 "late stop returned malformed response")
    assert_counts(case, A=1, S=1, E=1, K=1, M=0)
