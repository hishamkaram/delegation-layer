"""Finite admission acceptance scenarios."""

from scripts.delegate_acceptance.processes.observation import inherited_environment
from scripts.delegate_acceptance.hermetic.contract import CASE_TIMEOUT
from scripts.delegate_acceptance.hermetic.contract import add_entry_label
from scripts.delegate_acceptance.hermetic.contract import label_parts
from scripts.delegate_acceptance.hermetic.contract import private_pueue_yaml
from scripts.delegate_acceptance.hermetic.contract import require_true
from scripts.delegate_acceptance.hermetic.evidence import require_bound_records
from scripts.delegate_acceptance.hermetic.evidence import require_provider_evidence
from scripts.delegate_acceptance.hermetic.evidence import require_publication
from scripts.delegate_acceptance.hermetic.files import write_bytes
from scripts.delegate_acceptance.hermetic.observations import assert_counts
from scripts.delegate_acceptance.hermetic.observations import observe_helper_completion_if_present
from scripts.delegate_acceptance.hermetic.observations import supervisor_entries
from scripts.delegate_acceptance.hermetic.observations import wait_completion
from scripts.delegate_acceptance.hermetic.observations import wait_entry
from scripts.delegate_acceptance.hermetic.observations import wait_entry_after
from scripts.delegate_acceptance.hermetic.observations import wait_supervisor_completion
import uuid


def run_a01(case):
    process = case.start_dispatch()
    entry = wait_entry(case, "add")
    case.root_id, _ = label_parts(add_entry_label(entry))
    require_true(not (case.supervisor_records / (entry["invocation_id"] + ".completion.json")).exists(),
                 "withheld add completed before observation bound")
    process.wait(CASE_TIMEOUT, expected=(1,))
    pending = process.json()
    require_true(pending.get("admission") in ("unknown", "admitted"),
                 "ambiguous add was converted to a different admission")
    require_true(pending.get("pending") is not None or pending.get("error"),
                 "ambiguous add returned no uncertainty evidence")
    require_true((case.root / "tasks" / case.task_id / "submit.json").exists(),
                 "submit intent was not durable")
    case.set_status("stashed")
    # The add waiter belongs to the dispatcher process.  Once the bounded
    # caller observation returns, the harness cannot claim ownership of that
    # internal Wait; record whether a completion receipt is eventually visible
    # and base admission only on the durable retry observation.
    observe_helper_completion_if_present(case, entry)
    _, retry = case.retry_dispatch()
    require_publication(retry, "unknown")
    require_true(retry.get("admission") == "admitted", "retry did not attach to one add")
    case.set_status("running")
    runner = case.start_runner()
    run_response = case.finish_runner(runner, expected=(0,))
    case.set_status("ended")
    require_provider_evidence(case)
    require_true(run_response.get("publication") == "committed", "A01 runner did not commit")
    assert_counts(case, A=1, Q=1, S=1, E=1, K=0, M=0)


def run_a02_fixture(case, fixture):
    add_stdout = case.base / ("add-" + fixture + ".stdout")
    if fixture == "empty":
        write_bytes(add_stdout, b"")
    elif fixture == "duplicate":
        write_bytes(add_stdout, b"7\n7\n")
    else:
        write_bytes(add_stdout, b"not-a-task-id\n")
    case.update_fake("add", stdout_path=str(add_stdout))
    process = case.start_dispatch()
    entry = wait_entry(case, "add")
    case.root_id, _ = label_parts(add_entry_label(entry))
    case.release_add()
    process.wait(CASE_TIMEOUT, expected=(1,))
    first = process.json()
    require_true(first.get("admission") == "unknown", "ambiguous add output established admission")
    failure = first.get("failure") or {}
    require_true(failure.get("code") == "task_submission_uncertain" and
                 failure.get("stage") == "submit-task" and
                 failure.get("next_action") == "check_status",
                 "ambiguous add output returned the wrong recovery action")
    require_true((case.root / "tasks" / case.task_id / "submit.json").exists(),
                 "ambiguous add output lost submit intent")
    require_true(not (case.root / "tasks" / case.task_id / "supervisor.ref.json").exists(),
                 "ambiguous add output invented a supervisor receipt")
    case.set_status("stashed")
    _, attached = case.retry_dispatch()
    require_true(attached.get("admission") == "admitted", "retry did not recover one staged task")
    require_true((attached.get("supervisor") or {}).get("numeric_task_id") == case.numeric_id,
                 "retry did not bind the observed numeric task ID")
    require_true((case.root / "tasks" / case.task_id / "supervisor.ref.json").exists(),
                 "staged recovery did not publish a supervisor receipt")
    assert_counts(case, A=1, Q=1, S=0, E=0, K=0, M=0)


def run_a02_empty(case):
    run_a02_fixture(case, "empty")


def run_a02_duplicate(case):
    run_a02_fixture(case, "duplicate")


def run_a02_malformed(case):
    run_a02_fixture(case, "malformed")


def run_a03_config(case):
    case.dispatch_dynamic()
    original = case.production_config.read_bytes()
    write_bytes(case.production_config, original + b"# changed after binding\n", replace=True)
    _, status = case.run_command("status-changed-config", "status", expected=(1,))
    require_true(status.get("admission") != "admitted" or status.get("error"),
                 "changed config established admission")
    _, cancel = case.run_command("cancel-changed-config", "cancel", expected=(1,))
    require_true((case.task_dir() / "stop").exists(), "stop request was not durable")
    require_true(cancel.get("stop") is None or cancel.get("error"),
                 "changed config authorized stop")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_a03_label(case):
    case.dispatch_dynamic()
    case.set_status("running", label="delegate:" + case.root_id + ":" + uuid.uuid4().hex)
    _, status = case.run_command("status-wrong-label", "status", expected=(0,))
    require_true(status.get("admission") == "unknown", "wrong label was admitted")
    _, cancel = case.run_command("cancel-wrong-label", "cancel", expected=(1,))
    require_true(cancel.get("error"), "wrong label cancellation lacked uncertainty")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_a04_invalid_config(case):
    missing = case.base / "missing pueue.yml"
    _, response = case.dispatch_without_admission(
        pueue_config=missing,
        expected=(2,),
    )
    require_true(response.get("error"), "missing config did not produce diagnostic")
    assert_counts(case, A=0, S=0, E=0, K=0, M=0)


def run_a04_symlink_config(case):
    link = case.base / "config symlink.yml"
    link.symlink_to(case.production_config)
    _, response = case.dispatch_without_admission(pueue_config=link, expected=(2,))
    require_true(response.get("error"), "symlink config did not refuse")
    assert_counts(case, A=0, S=0, E=0, K=0, M=0)


def run_a04_valid(case):
    case.dispatch_dynamic()
    require_bound_records(case)
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_a04_saved_binding(case):
    """Fresh commands must ignore an unrelated ambient config path."""
    case.dispatch_dynamic()
    case.set_status("queued")
    ambient_root = case.base / "ambient"
    ambient_root.mkdir(mode=0o700)
    ambient_config = ambient_root / "unrelated pueue.yml"
    write_bytes(ambient_config, private_pueue_yaml(ambient_root))
    environment = inherited_environment()
    environment["DELEGATE_PUEUE_CONFIG"] = str(ambient_config)
    _, status = case.run_command_with_environment(
        "status-ambient-config", "status", environment, expected=(0,)
    )
    require_true(status.get("admission") == "admitted",
                 "saved binding status did not remain admitted")
    _, collected = case.run_command_with_environment(
        "collect-ambient-config", "collect", environment, expected=(3,)
    )
    require_true(collected.get("publication") in ("pending", "unknown"),
                 "saved binding collect changed pending state")
    _, cancel = case.run_command_with_environment(
        "cancel-ambient-config", "cancel", environment, expected=(0,)
    )
    require_true(cancel.get("stop", {}).get("action") == "remove",
                 "saved binding cancel did not target queued task")
    for entry in supervisor_entries(case):
        require_true(entry.get("config_path") == str(case.production_config),
                     "fresh command consulted ambient supervisor config")
    assert_counts(case, A=1, S=0, E=0, K=0, M=1)


def run_a05_concurrent(case):
    first = case.start_dispatch()
    entry = wait_entry(case, "add")
    case.root_id, _ = label_parts(add_entry_label(entry))
    case.set_status("queued")
    # Keep the first add in flight while the second caller reaches the same
    # durable intent.  This proves independent callers do not serialize on a
    # process-local lock, while the fake add still admits only one request.
    second = case.start_dispatch()
    second.wait(CASE_TIMEOUT, expected=(0, 1, 2))
    second.json()
    case.release_add()
    first.wait(CASE_TIMEOUT, expected=(0,))
    first.json()
    brief = case.base / "changed.md"
    write_bytes(brief, b"changed immutable request\n")
    _, conflict = case.retry_dispatch(brief=brief, expected=(2,))
    require_true("conflict" in conflict.get("error", "").lower(),
                 "different same-ID request was not rejected")
    require_true(first.pid != second.pid, "concurrent controls did not use distinct PIDs")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_a06(case):
    enqueue_release = case.base / "enqueue.release"
    case.update_fake("enqueue", release_path=str(enqueue_release))
    process = case.start_dispatch()
    entry = wait_entry(case, "add")
    case.root_id, _ = label_parts(add_entry_label(entry))
    case.set_status("stashed")
    case.release_add()
    enqueue = wait_entry(case, "enqueue")
    require_true(not (case.supervisor_records / (enqueue["invocation_id"] + ".completion.json")).exists(),
                 "delayed enqueue completed before its release")
    process.wait(CASE_TIMEOUT, expected=(1,))
    response = process.json()
    failure = response.get("failure") or {}
    require_true(failure.get("code") == "task_launch_uncertain" and
                 failure.get("stage") == "enqueue-task" and
                 failure.get("next_action") == "check_status",
                 "bounded enqueue observation returned the wrong recovery action")
    require_true((case.root / "tasks" / case.task_id / "supervisor.ref.json").exists(),
                 "enqueue uncertainty lost the staged supervisor receipt")
    completion = observe_helper_completion_if_present(case,
        enqueue, timeout=CASE_TIMEOUT
    )
    require_true(completion is not None,
                 "surviving control owner did not record natural enqueue completion")
    require_true(completion.get("exit_code") != 0,
                 "unreleased enqueue unexpectedly completed successfully")
    case.release(enqueue_release)
    case.set_status("stashed")
    _, retry = case.retry_dispatch(expected=(0,))
    require_true(retry.get("admission") == "admitted", "failed enqueue was not resumed from its staged receipt")
    retry_enqueue = wait_entry_after(case, "enqueue", {enqueue["invocation_id"]})
    retry_completion = wait_completion(case, retry_enqueue)
    require_true(retry_completion.get("exit_code") == 0,
                 "retry enqueue did not complete successfully")
    wait_supervisor_completion(case, retry_enqueue)
    assert_counts(case, A=1, Q=2, S=0, E=0, K=0, M=0)
