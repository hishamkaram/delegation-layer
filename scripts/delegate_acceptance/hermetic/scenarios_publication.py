"""Finite publication acceptance scenarios."""

from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.hermetic.contract import CASE_TIMEOUT
from scripts.delegate_acceptance.hermetic.contract import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.hermetic.contract import PREDICATE
from scripts.delegate_acceptance.hermetic.contract import canonical_json_bytes
from scripts.delegate_acceptance.hermetic.contract import require_true
from scripts.delegate_acceptance.hermetic.evidence import expected_raw_files
from scripts.delegate_acceptance.hermetic.evidence import require_live_raw_descriptors
from scripts.delegate_acceptance.hermetic.evidence import require_provider_evidence
from scripts.delegate_acceptance.hermetic.evidence import require_publication
from scripts.delegate_acceptance.hermetic.evidence import require_terminal_evidence
from scripts.delegate_acceptance.hermetic.observations import assert_counts
from scripts.delegate_acceptance.hermetic.observations import invocation_counts
from scripts.delegate_acceptance.hermetic.observations import runner_execution_sequence
from scripts.delegate_acceptance.hermetic.observations import wait_event
from scripts.delegate_acceptance.hermetic.scenarios_common import complete_success_case


def run_p01(case):
    response = complete_success_case(case)
    require_publication(response, "committed")
    require_provider_evidence(case)
    task_dir = case.task_dir()
    require_true((task_dir / "raw" / "stdout").read_bytes() ==
                 (case.provider_records / "expected.stdout").read_bytes(),
                 "stdout bytes changed in transport")
    require_true((task_dir / "raw" / "stderr").read_bytes() ==
                 (case.provider_records / "expected.stderr").read_bytes(),
                 "stderr bytes changed in transport")
    require_true(not (case.work / "DO_NOT_CREATE").exists(),
                 "shell-looking literal was executed")
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p02(case):
    case.update_provider(stdout_bytes=2 * 1024 * 1024 + 17,
                         stderr_bytes=2 * 1024 * 1024 + 29)
    response = complete_success_case(case)
    require_publication(response, "committed")
    raw_stdout = case.task_dir() / "raw" / "stdout"
    raw_stderr = case.task_dir() / "raw" / "stderr"
    require_true(raw_stdout.stat().st_size > 2 * 1024 * 1024, "stdout did not exercise backpressure")
    require_true(raw_stderr.stat().st_size > 2 * 1024 * 1024, "stderr did not exercise backpressure")
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p03(case):
    scenario = case.provider_settings["scenario"]
    expected_provider_exit = 7 if scenario == "nonzero" else 0
    response = complete_success_case(case, provider_expected=expected_provider_exit)
    if scenario == "success":
        require_publication(response, "committed")
        expected_code = 0
    else:
        require_publication(response, "rejected")
        expected_code = 4
        _, collected = case.run_command("collect-" + scenario, "collect", expected=(4,))
        require_true(collected.get("publication") == "rejected",
                     "semantic refusal was not preserved by collect")
        outcome = collected.get("outcome") or {}
        require_true(isinstance(outcome, dict) and outcome.get("verdict") == "rejected",
                     "semantic collection omitted its rejected outcome")
        prefixes = {
            "empty": "empty_answer",
            "diagnostic": "stderr_error",
            "malformed": "malformed_output",
            "truncated": "malformed_output",
            "nonzero": "provider_exit",
        }
        payload = response.get("payload") or {}
        basename = payload.get("basename")
        require_true(isinstance(basename, str) and basename,
                     "semantic refusal omitted payload descriptor")
        refusal_text = (case.task_dir() / basename).read_text()
        require_true(refusal_text.startswith(prefixes[scenario]),
                     "wrong refusal reason: " + refusal_text)
        require_provider_evidence(case, expected_provider_exit)
    require_true(expected_code in (0, 4), "invalid semantic code mapping")
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p04(case):
    case.dispatch_dynamic(status="queued")
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    wait_event(case, "wait-completed")
    require_true(not (case.task_dir() / "provider.exit").exists(),
                 "parent Wait fabricated a seal while child held raw pipe")
    require_true(not (case.task_dir() / "outcome.json").exists(),
                 "parent Wait fabricated an outcome while child held raw pipe")
    child_entry = case.provider_records / "child.provider.entry.json"
    child_complete = case.provider_records / "child.provider.complete.json"
    require_true(child_entry.exists() and not child_complete.exists(),
                 "held-pipe observation missed the child completion boundary")
    case.release_provider()
    response = case.finish_runner(runner, expected=(0,))
    require_publication(response, "committed")
    require_provider_evidence(case)
    child_entry = case.provider_records / "child.provider.entry.json"
    require_true(child_entry.exists() and child_complete.exists(),
                 "finite pipe-owning child receipt is missing")
    require_true(invocation_counts(case)["child_E"] == 1, "unexpected child provider count")
    require_terminal_evidence(case, response)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p05_start(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0,))
    response = case.finish_runner(runner, expected=(0,))
    require_publication(response, "rejected")
    task_dir = case.task_dir()
    seal = read_json(task_dir / "provider.exit", MAX_CONTROL_BYTES)
    outcome = read_json(task_dir / "outcome.json", MAX_CONTROL_BYTES)
    require_true(seal["invocation_state"] == "start_failed" and seal["exit_code"] == 1,
                 "definite Start failure did not produce a start_failed seal")
    require_true(not (task_dir / "raw" / "stdout").read_bytes() and
                 (task_dir / "raw" / "stderr").read_bytes(),
                 "Start failure diagnostics were not durably captured")
    manifest = []
    for name in ("stderr", "stdout"):
        data = (task_dir / "raw" / name).read_bytes()
        manifest.append({"path": "raw/" + name, "size": len(data), "sha256": sha(data)})
    require_true(seal["raw_manifest"] == manifest and
                 seal["manifest_sha256"] == sha(canonical_json_bytes(manifest)),
                 "Start failure raw seal changed")
    payload_name = outcome["payload"]["basename"]
    payload = (task_dir / payload_name).read_text()
    require_true(payload.startswith("start_failed: "),
                 "Start failure predicate refusal reason changed")
    require_true(response.get("outcome") == outcome and
                 response.get("payload") == outcome["payload"],
                 "Start failure response lost terminal identity")
    require_true(not (case.provider_records / "provider.entry.json").exists(),
                 "provider entered after definite Start failure")
    retry_runner = case.start_runner(expected=(0, 1), name="retry-start-failed")
    retry = case.finish_runner(retry_runner, expected=(0, 1))
    require_true(retry.get("task_id") == case.task_id, "start failure task lost")
    values = runner_execution_sequence(case)
    names = [value.get("name") for value in values]
    require_true("start-entry" in names and "start-failed" in names and
                 "sealed" in names and "published" in names,
                 "Start failure event sequence was incomplete")
    require_true(names.index("start-entry") < names.index("start-failed") <
                 names.index("sealed") < names.index("published"),
                 "Start failure events are out of order")
    assert_counts(case, A=1, S=1, E=0, K=0, M=0)


def run_p05_fault(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(1,))
    response = case.finish_runner(runner, expected=(1,))
    require_true(response.get("error"), "infrastructure failure was swallowed")
    require_true(not (case.task_dir() / "provider.exit").exists(),
                 "infrastructure failure fabricated a seal")
    require_provider_evidence(case)
    for name in ("stderr", "stdout"):
        require_true(
            (case.task_dir() / "raw" / name).read_bytes()
            == expected_raw_files(case)[name].read_bytes(),
            "infrastructure fault changed raw " + name,
        )
    event_names = [
        value.get("name") for value in runner_execution_sequence(case)
    ]
    for name in ("start-entry", "started", "parent-fds-closed",
                 "started-receipt", "wait-completed",
                 "stdout-raw-closed", "stderr-raw-closed",
                 "completion-observed", "timer-disarmed"):
        require_true(name in event_names, "fault event missing: " + name)
    require_true(
        ("stdout-eof" in event_names or "stdout-drained-after-error" in event_names) and
        ("stderr-eof" in event_names or "stderr-drained-after-error" in event_names),
        "fault path did not observe both capture drains",
    )
    require_true("sealed" not in event_names and "published" not in event_names,
                 "fault path fabricated terminal events")
    retry = case.start_runner(expected=(1,), name="retry-runner")
    retry_response = case.finish_runner(retry, expected=(1,))
    require_true(retry_response.get("error"), "retry bypassed existing start guard")
    counts = invocation_counts(case)
    require_true(counts["S"] == 1 and counts["E"] == 1, "fault retry launched provider twice")
    require_true(counts["A"] == 1 and counts["K"] == 0 and counts["M"] == 0,
                 "fault path mutated supervisor")


def run_p06_overlap(case):
    case.dispatch_dynamic()
    case.set_status("running")
    first = case.start_runner(expected=(0, 1), name="runner-a")
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    second = case.start_runner(expected=(1,), name="runner-b")
    second_response = case.finish_runner(second, expected=(1,))
    require_true(second_response.get("error"), "overlapping runner acquired start")
    require_true(first.pid != second.pid and
                 not (case.task_dir() / "provider.exit").exists() and
                 not (case.task_dir() / "outcome.json").exists(),
                 "overlapping runner changed terminal authority")
    case.release_provider()
    first_response = case.finish_runner(first, expected=(0,))
    require_publication(first_response, "committed")
    case.set_status("ended")
    require_provider_evidence(case)
    require_terminal_evidence(case, first_response)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p06_abandon(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(73,))
    case.wait_path(case.provider_records / "provider.entry.json")
    # The abandon hook exits immediately after observing completion, before
    # the runner can publish a JSON response.  Observe its natural exit
    # directly; an empty stdout stream is part of this fault's evidence.
    runner.wait(CASE_TIMEOUT, expected=(73,))
    require_true(not (case.task_dir() / "provider.exit").exists(),
                 "abandon-before-seal fabricated a seal")
    retry = case.start_runner(expected=(1,), name="retry-after-abandon")
    retry_response = case.finish_runner(retry, expected=(1,))
    require_true(retry_response.get("error"), "abandoned task relaunched")
    require_provider_evidence(case)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p07(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    metadata_path = case.task_dir() / "meta.json"
    metadata_before = metadata_path.read_bytes()
    provider_ref_path = case.task_dir() / "provider.ref.json"
    require_true(not provider_ref_path.exists(),
                 "late session identity appeared before provider evidence")
    _, pending = case.run_command("collect-late-pending", "collect", expected=(3,))
    require_true(pending.get("publication") in ("pending", "unknown"),
                 "early collect fabricated publication")
    require_live_raw_descriptors(case, pending)
    _, status = case.run_command("status-late", "status", expected=(0,))
    require_true(not (case.task_dir() / "provider.ref.json").exists(),
                 "late session identity was recorded before provider session evidence")
    require_true(status.get("publication") in ("unknown", "pending"),
                 "late session status fabricated publication")
    _, logs = case.run_command("logs-late", "logs", expected=(0,))
    require_live_raw_descriptors(case, logs)
    case.release_provider()
    final = case.finish_runner(runner, expected=(0,))
    require_publication(final, "committed")
    case.set_status("ended")
    require_true(provider_ref_path.exists(),
                 "late session identity was not recorded")
    metadata_after = metadata_path.read_bytes()
    require_true(metadata_after == metadata_before,
                 "late session identity overwrote immutable metadata")
    provider_ref = read_json(provider_ref_path, MAX_CONTROL_BYTES)
    provider_config = read_json(case.provider_config, MAX_CONTROL_BYTES)
    require_true(
        provider_ref == {
            "meta_sha256": sha(metadata_before),
            "provider": PREDICATE,
            "observed_at": provider_ref["observed_at"],
            "schema_version": 1,
            "root_id": case.root_id,
            "task_id": case.task_id,
            # task.json is the immutable request; TaskRecord deliberately has
            # no self-referential spec_sha256 field.  The production binding
            # uses the digest of its exact canonical bytes.
            "spec_sha256": sha((case.task_dir() / "task.json").read_bytes()),
            "conversation_id": provider_config["session_id"],
        },
        "late provider identity changed session or immutable binding",
    )
    _, collected = case.run_command("collect-late-final", "collect", expected=(0,))
    _, collected_again = case.run_command("collect-late-repeat", "collect", expected=(0,))
    require_publication(collected, "committed")
    require_publication(collected_again, "committed")
    for value in (collected, collected_again):
        require_true(
            value.get("outcome") == final.get("outcome") and
            value.get("payload") == final.get("payload") and
            value.get("evidence_sha256") == final.get("evidence_sha256"),
            "repeated collection changed the sealed winner",
        )
    require_provider_evidence(case)
    require_terminal_evidence(case, final)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)
