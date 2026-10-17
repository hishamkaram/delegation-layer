"""Validate captured task publication against fixture expectations."""

from pathlib import Path
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.hermetic.contract import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.hermetic.contract import PREDICATE_SHA256
from scripts.delegate_acceptance.hermetic.contract import canonical_json_bytes
from scripts.delegate_acceptance.hermetic.contract import require_true
from scripts.delegate_acceptance.hermetic.observations import provider_completion
from scripts.delegate_acceptance.hermetic.observations import provider_entry
from scripts.delegate_acceptance.hermetic.observations import runner_execution_sequence
from scripts.delegate_acceptance.hermetic.observations import supervisor_entries
import json


def require_supervisor_argv(case, verb, expected):
    matches = [entry for entry in supervisor_entries(case) if entry.get("verb") == verb]
    require_true(len(matches) == 1,
                 "expected one fake supervisor %s entry, found %d" % (verb, len(matches)))
    require_true(matches[0].get("argv") == expected,
                 "fake supervisor %s argv changed: %s" % (verb, matches[0].get("argv")))
    return matches[0]


def require_publication(response, publication, code=None):
    require_true(response.get("publication") == publication,
                 "unexpected publication: " + json.dumps(response, sort_keys=True))
    if code is not None:
        require_true(response.get("schema_version") == 1, "unversioned response")


def require_live_raw_descriptors(case, response):
    """Check F11's bounded, observation-only live stream descriptors."""
    raw = response.get("raw")
    require_true(isinstance(raw, list) and len(raw) == 2,
                 "pending response omitted live raw descriptors")
    for descriptor, name in zip(raw, ("stderr", "stdout")):
        require_true(isinstance(descriptor, dict), "raw descriptor is not an object")
        expected_path = str(case.task_dir() / "raw" / name)
        require_true(descriptor.get("path") == expected_path,
                     "raw descriptor path escaped task root")
        require_true(Path(descriptor["path"]).is_absolute(),
                     "raw descriptor path is not absolute")
        require_true(descriptor.get("available") is True,
                     "existing live raw stream was marked unavailable")
        require_true(descriptor.get("sealed") is False,
                     "pending response invented a sealed stream")
        require_true("size" not in descriptor and "sha256" not in descriptor,
                     "pending response invented final raw identity")


def require_bound_records(case):
    task_path = case.root / "tasks" / case.task_id
    submit = read_json(task_path / "submit.json")
    meta = read_json(task_path / "meta.json")
    require_true(submit["label"] == "delegate:" + case.root_id + ":" + case.task_id,
                 "saved submit label changed")
    require_true(submit["supervisor"]["config_path"] == str(case.production_config),
                 "saved supervisor config path changed")
    require_true(submit["supervisor"]["client_executable"] == str(case.pueue),
                 "saved supervisor executable changed")
    require_true(meta["provider_executable"] == str(case.provider),
                 "saved provider executable changed")
    require_true(meta["provider_version"] == "fixture-v2",
                 "saved provider version changed")
    return task_path


def require_provider_evidence(case, expect_exit=0):
    entry = provider_entry(case)
    completion = provider_completion(case)
    expected_argv = [str(case.provider), str(case.provider_config), *case.provider_settings["argv"]]
    require_true(entry["pid"] == completion["pid"] and entry["pid"] > 0,
                 "provider entry/completion PID mismatch")
    require_true(completion["natural"] is True and completion["exit_code"] == expect_exit,
                 "provider did not complete with expected natural exit")
    require_true(entry["task_id"] == case.task_id and entry["cwd"] == str(case.work),
                 "provider identity/cwd changed")
    require_true(entry["process_argv"] == expected_argv,
                 "provider literal argv changed")
    require_true(
        (case.provider_records / "brief.input").read_bytes() == case.brief.read_bytes(),
        "provider stdin bytes changed",
    )
    return entry, completion


def expected_raw_files(case):
    prefix = "child." if case.provider_settings["scenario"] == "parent_exit" else ""
    return {
        name: case.provider_records / (prefix + "expected." + name)
        for name in ("stderr", "stdout")
    }


def expected_payload_bytes(case):
    scenario = case.provider_settings["scenario"]
    if scenario == "large":
        count = case.provider_settings["stdout_bytes"] or 2 * 1024 * 1024
        return b"S" * count
    if scenario == "parent_exit":
        return b"child fixture answer"
    if scenario == "empty":
        return b"empty_answer"
    if scenario == "diagnostic":
        return b"stderr_error"
    if scenario in ("malformed", "truncated"):
        return b"malformed_output"
    if scenario == "nonzero":
        return b"provider_exit: 7"
    answer = case.provider_settings["answer"]
    return answer.encode()


def require_terminal_evidence(case, response, expect_exit=0):
    """Validate independent raw, seal, payload, and event identities."""
    task_dir = case.task_dir()
    seal = read_json(task_dir / "provider.exit", MAX_CONTROL_BYTES)
    outcome = read_json(task_dir / "outcome.json", MAX_CONTROL_BYTES)
    require_true(seal["schema_version"] == 1 and outcome["schema_version"] == 1,
                 "terminal records are unversioned")
    for record in (seal, outcome):
        require_true(record["root_id"] == case.root_id and record["task_id"] == case.task_id,
                     "terminal record identity changed")
        require_true(record["predicate"] == {
            "adapter": "fixture:test",
            "mode": "read-only",
            "version": "2",
            "sha256": PREDICATE_SHA256,
        }, "terminal predicate identity changed")
    require_true(seal["invocation_state"] == "started" and
                 seal["exit_code"] == expect_exit and seal["error"] == "",
                 "provider seal has unexpected invocation result")
    require_true(
        outcome["spec_sha256"] == seal["spec_sha256"] and
        outcome["meta_sha256"] == seal["meta_sha256"] and
        outcome["evidence_sha256"] == seal["manifest_sha256"] and
        outcome["predicate"] == seal["predicate"],
        "outcome does not preserve seal identity",
    )
    expected_manifest = []
    for name in ("stderr", "stdout"):
        expected_path = expected_raw_files(case)[name]
        expected = expected_path.read_bytes()
        actual_path = task_dir / "raw" / name
        actual = actual_path.read_bytes()
        require_true(actual == expected, "raw " + name + " bytes changed")
        expected_manifest.append({
            "path": "raw/" + name,
            "size": len(expected),
            "sha256": sha(expected),
        })
    require_true(seal["raw_manifest"] == expected_manifest,
                 "raw manifest does not match independent fixture bytes")
    require_true(
        seal["manifest_sha256"] == sha(canonical_json_bytes(expected_manifest)),
        "raw manifest digest changed",
    )
    payload_descriptor = outcome["payload"]
    payload_name = payload_descriptor["basename"]
    require_true(payload_name in ("result.txt", "publish.reject"),
                 "unexpected terminal payload name")
    payload = (task_dir / payload_name).read_bytes()
    expected_payload = expected_payload_bytes(case)
    require_true(payload == expected_payload, "terminal payload bytes changed")
    require_true(
        payload_descriptor == {
            "basename": payload_name,
            "length": len(expected_payload),
            "sha256": sha(expected_payload),
        },
        "terminal payload descriptor changed",
    )
    require_true(response.get("outcome") == outcome and
                 response.get("payload") == payload_descriptor and
                 response.get("evidence_sha256") == seal["manifest_sha256"],
                 "runner response changed terminal identity")
    expected_verdict = (
        "committed"
        if case.provider_settings["scenario"]
        in ("success", "large", "hold", "parent_exit", "late_session")
        else "rejected"
    )
    require_true(outcome["verdict"] == expected_verdict,
                 "terminal verdict changed")
    require_terminal_event_sequence(case)


def require_terminal_event_sequence(case):
    values = runner_execution_sequence(case)
    names = [value.get("name") for value in values]
    required = (
        "start-entry", "started", "parent-fds-closed", "started-receipt",
        "wait-completed", "stdout-eof", "stderr-eof",
        "stdout-raw-closed", "stderr-raw-closed", "completion-observed",
        "timer-disarmed", "sealed", "published",
    )
    for name in required:
        require_true(name in names, "runner event missing: " + name)
    position = {name: names.index(name) for name in names}
    require_true(position["start-entry"] < position["started"] <
                 position["parent-fds-closed"] < position["started-receipt"],
                 "runner Start events are out of order")
    require_true(position["stdout-eof"] < position["stdout-raw-closed"] <
                 position["completion-observed"],
                 "stdout close events are out of order")
    require_true(position["stderr-eof"] < position["stderr-raw-closed"] <
                 position["completion-observed"],
                 "stderr close events are out of order")
    require_true(position["wait-completed"] < position["completion-observed"] <
                 position["timer-disarmed"] < position["sealed"] <
                 position["published"],
                 "runner Wait/seal events are out of order")
