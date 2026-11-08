"""Read-only native and durable observations; never bootstrap a supervisor."""

import hashlib
import json
import os
from pathlib import Path
import stat
import time
import uuid

from scripts.delegate_acceptance.evidence.task_entry import observe_task_entry
from scripts.delegate_acceptance.evidence.files import digest, path_present
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require, _hex_digest
from scripts.delegate_acceptance.evidence.json_decoder import unique_object
from scripts.delegate_acceptance.lifetime.evidence import _validate_managed_ordinary_lifetime
from scripts.delegate_acceptance.private_cli.capacity_records import capacity_lease_specifications
from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.lifetime.evidence import _read_lifetime_launches
from scripts.delegate_acceptance.private_cli.capacity_ownership import lease_candidate_specifications
from scripts.delegate_acceptance.private_cli.fixture_lifetime import ClosedRoot
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort, ordinary_lifetime_owner_live
from scripts.delegate_acceptance.private_cli.nested_fixture import native_observation
from scripts.delegate_acceptance.private_cli.nested_fixture import fixture_environment
from scripts.delegate_acceptance.private_cli.capture import run_captured_text_command
from scripts.delegate_acceptance.private_cli.nested_policy import require_backend
from scripts.delegate_acceptance.private_cli.supervisor import private_shutdown_evidence
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.private_cli.queue_policy import task_row_by_label
from scripts.delegate_acceptance.supervisor.binding import _validate_supervisor_binding
from scripts.delegate_acceptance.tasks.admission import _read_ordinary_supervisor_receipt


def await_record(path, deadline):
    while time.monotonic() < deadline:
        if path_present(path):
            return _read_control_record(path, "nested fixture record")
        time.sleep(0.02)
    raise TimeoutError("nested fixture record did not arrive: " + str(path))


def observe_native(configuration, capture, pid, *, scope="", expected_start="", deadline=None):
    output = Path(configuration["controls"]) / ("native-" + uuid.uuid4().hex + ".json")
    native_observation(configuration, capture, pid, output, DELEGATE_NESTED_FIXTURE_SCOPE=scope,
                       DELEGATE_NESTED_FIXTURE_EXPECTED_TOKEN=expected_start, deadline=deadline)
    observation = _read_control_record(output, "native process observation")
    require(observation.get("pid") == pid and type(observation.get("present")) is bool
            and type(observation.get("identity_matches")) is bool,
            "native process observation changed identity")
    return observation


def compare_native_tokens(configuration, capture, observed, persisted, *, deadline=None):
    """Only the Go identity owner interprets platform token equivalence."""
    output = Path(configuration["controls"]) / ("identity-" + uuid.uuid4().hex + ".json")
    native_observation(configuration, capture, 0, output, mode="compare-identity", deadline=deadline,
                       DELEGATE_NESTED_FIXTURE_OBSERVED_TOKEN=observed,
                       DELEGATE_NESTED_FIXTURE_EXPECTED_TOKEN=persisted)
    result = _read_control_record(output, "native token comparison")
    require(type(result.get("identity_matches")) is bool, "native identity comparison omitted its verdict")
    return result["identity_matches"]


def prove_task_absence(configuration, capture, state, root, task_id, *, deadline=None, closed: ClosedRoot | None = None):
    """Invoke the maintained task-store authority; never reconstruct its locks."""
    expected_refusal = None
    if closed is not None:
        held = closed["disposal"]
        require(held.state == state, "closed-root disposal lease names another state root")
        held.require_current()
        closure = closed["closure"]
        tasks = closure.get("tasks")
        previous = tasks.get(task_id) if isinstance(tasks, dict) else None
        if not (closure.get("root_id") == root and isinstance(previous, dict)
                and previous.get("disposition") == "never-launched" and _hex_digest(previous.get("refusal_sha256"))):
            raise AcceptanceFailure("closed-root shutdown did not authorize this never-launched task")
        expected_refusal = previous["refusal_sha256"]
    directory = state / "tasks" / task_id
    receipt_path = directory / "supervisor.ref.json"
    if path_present(receipt_path):
        receipt = _read_control_record(receipt_path, "never-launched admission")
        metadata = _read_control_record(directory / "meta.json", "never-launched metadata")
        supervisor_config = metadata["supervisor_config"]
        if not isinstance(supervisor_config, dict):
            raise AcceptanceFailure(f"ordinary supervisor receipt binding changed for {task_id}")
        label, _numeric, _meta_digest = _read_ordinary_supervisor_receipt(
            supervisor_config, directory, directory / "meta.json", root, digest(directory / "task.json"), task_id)
        client, selected = bound_private_client(state, configuration["observer_environment"])
        published = _read_control_record(state / ".supervisor" / "supervisor.identity.json", "private binding")["binding"]
        if not isinstance(published, dict):
            raise AcceptanceFailure("never-launched admission names another supervisor generation")
        require(all(receipt.get(key) == published.get(key) for key in ("config_path", "config_digest", "endpoint")),
                "never-launched admission names another supervisor generation")
        if closed is None:
            queue = pueue_snapshot(client, state / ".supervisor" / "pueue.yml", selected,
                                   phase="nested-external-observer", capture_root=capture, deadline=deadline)
            require(task_row_by_label(queue, label) is None,
                    "never-launched proof requires its exact queue row removed first")
    else:
        require(not path_present(directory / "submit.json"),
                "retained: submitted task has no settled immutable queue receipt")
    proof = observe_task_store(configuration, capture, state, root, task_id, "task-absence", deadline=deadline)
    require(proof.get("never_started") is True and _hex_digest(proof.get("refusal_sha256")),
            "canonical task owner did not prove never-launched disposition")
    if closed is not None:
        closed["disposal"].require_current()
        require(proof["refusal_sha256"] == expected_refusal,
                "never-launched refusal changed after authorized shutdown")
    return proof


def observe_task_store(configuration, capture, state, root, task_id, mode, *, deadline=None):
    environment = fixture_environment(configuration, DELEGATE_NESTED_FIXTURE_MODE=mode,
                                      DELEGATE_NESTED_FIXTURE_ROOT=str(state), DELEGATE_NESTED_FIXTURE_TASK=task_id)
    bound = min(deadline, time.monotonic() + 10) if deadline is not None else time.monotonic() + 10
    result = run_captured_text_command([configuration["native_fixture"], "-test.run=^TestNestedTaskAbsenceFixture$"],
                                      environment, None, deadline=bound, capture_root=capture)
    if result.returncode != 0:
        raise RuntimeError("canonical never-launched observer failed: " + result.stderr[-1000:] + result.stdout[-1000:])
    prefix = "NESTED_TASK_OBSERVATION\t"
    require(len(result.stdout.encode()) <= 1024 * 1024, "never-launched observer exceeded its output bound")
    lines = [line[len(prefix):] for line in result.stdout.splitlines() if line.startswith(prefix)]
    require(len(lines) == 1, "never-launched observer omitted its exact response")
    proof = json.loads(lines[0], object_pairs_hook=unique_object)
    require(isinstance(proof, dict) and set(proof) == {"root_id", "task_id", "never_started", "refusal_sha256",
                                                     "prepared", "spec_sha256", "meta_sha256"}
            and proof.get("root_id") == root and proof.get("task_id") == task_id,
            "canonical task observer changed identity or response shape")
    return proof


def observe_bootstrap_task_entry(configuration, capture, state, root, task_id, token, *, deadline):
    """Adapt the nested fixture's capture owner to shared task-store observation."""
    require(deadline is not None, "bootstrap task observation deadline expired")
    def run(arguments, environment, bound):
        result = run_captured_text_command(arguments, environment, None, deadline=bound,
                                          capture_root=capture)
        require(result.returncode == 0, "canonical bootstrap task entry observation failed")
        return result.stdout

    return observe_task_entry(
        state, root, task_id, token, observer=Path(configuration["native_fixture"]),
        environment=fixture_environment(configuration), deadline=deadline, run=run)


def reconcile_prepared_actor(case, state, task_id):
    root = _read_control_record(state / "root.json", "owned state root")["root_id"]
    proof = observe_task_store(case.configuration, case.lifetime.capture_token, state, root, task_id,
                               "task-identity", deadline=case.cleanup_deadline)
    directory = state / "tasks" / task_id
    require(proof.get("prepared") is True and proof.get("spec_sha256") == digest(directory / "task.json")
            and proof.get("meta_sha256") == digest(directory / "meta.json"), "prepared actor identity was not proven")
    # An observed actor reference is not a replacement dispatch response.
    return {"root_id": root, "task_id": task_id}


def fixture_launch(state, response, identity, *, tokens_match):
    """The Go journal owns launches; fixture self-report selects an exact incarnation.

    Preflight and provider commands share this journal. Sequence and inventory
    order cannot identify which command runs the fixture.
    """
    directory = state / "tasks" / response["task_id"] / "lifetime"
    registration_path = directory / "registration.json"
    registration = _read_control_record(registration_path, "fixture registration")
    launches = _read_lifetime_launches(
        directory=directory, evidence="nested fixture", owner_kind="task", recovered=False,
        registration_created=_canonical_timestamp(registration.get("created_at"), "fixture registration time"),
        registration_digest=digest(registration_path), root=response["root_id"], task=response["task_id"],
        window_deadline=None,
    )
    require(type(identity.get("pid")) is int and identity["pid"] > 1
            and isinstance(identity.get("process_start"), str) and identity["process_start"],
            "fixture self-report has no native incarnation")
    matched = [launch for launch in launches if launch["pid"] == identity["pid"]
               and tokens_match(identity["process_start"], launch["process_start"]) is True]
    require(len(matched) == 1, "fixture incarnation does not match one registered launch")
    return matched[0]


def actor_specification(state, response, configuration, expected, *, live=False):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        specs = capacity_lease_specifications(state, [response], Path(configuration["waiters"]), configuration["nonce"])
        if live:
            specs = lease_candidate_specifications(specs)
        if specs:
            require_backend(specs[0], expected)
            return specs[0]
        time.sleep(0.02)
    raise TimeoutError("nested actor did not reach registered native launch: " + response["task_id"])


def acquire_actor_lease(specification, configuration):
    cohort = LiveFIFOLeaseCohort.acquire([specification], configuration["nonce"])
    cohort.assert_live()
    return cohort


def lifetime_checkpoint(state, response, deadline):
    """Validate Wait/termination through canonical policy without requiring disposal."""
    directory = state / "tasks" / response["task_id"]
    last_error = None
    while time.monotonic() < deadline:
        try:
            return _validate_managed_ordinary_lifetime(directory, response["root_id"], response["task_id"],
                                                       digest(directory / "task.json"), digest(directory / "meta.json"))
        except (FileNotFoundError, AcceptanceFailure) as error:
            last_error = error
            time.sleep(0.05)
    raise RuntimeError("leader termination checkpoint remained unproven: " + str(last_error)) from last_error


def supervisor_continuity(state, configuration, capture, scope):
    config = state / ".supervisor" / "pueue.yml"
    publication = _read_control_record(state / ".supervisor" / "supervisor.identity.json", "private binding")
    binding = publication.get("binding")
    if not (publication.get("schema_version") == 1 and isinstance(binding, dict)
            and binding.get("config_path") == str(config)):
        raise AcceptanceFailure("private binding does not name the owned root")
    _validate_supervisor_binding(binding, "nested private supervisor")
    provenance = _read_control_record(state / ".supervisor" / "daemon.identity.json", "daemon launch provenance")
    require(provenance.get("executable") == binding.get("daemon_executable")
            and provenance.get("sha256") == binding.get("daemon_sha256"), "daemon provenance changed published pair")
    pid, endpoint, socket_identity = private_shutdown_evidence(config)
    require(binding.get("endpoint") == "unix:" + str(endpoint), "private binding changed its endpoint")
    process = observe_native(configuration, capture, pid, scope=scope)
    require(process["present"] is True, "survival_lost: private daemon is absent before recovery")
    return {"binding_sha256": digest(state / ".supervisor" / "supervisor.identity.json"),
            "config_sha256": hashlib.sha256(config.read_bytes()).hexdigest(),
            "socket_identity": list(socket_identity), "process": process}


def bound_private_client(state, environment):
    publication = _read_control_record(state / ".supervisor" / "supervisor.identity.json", "private binding")
    binding = publication.get("binding")
    if not isinstance(binding, dict):
        raise AcceptanceFailure("nested private supervisor is not an object")
    _validate_supervisor_binding(binding, "nested private supervisor")
    require(binding["config_path"] == str(state / ".supervisor" / "pueue.yml"), "observer binding escaped owned root")
    selected = dict(environment)
    for source, target in (("resolution_home", "HOME"), ("resolution_config", "XDG_CONFIG_HOME"),
                           ("resolution_data_local", "XDG_DATA_HOME"), ("resolution_runtime", "XDG_RUNTIME_DIR")):
        selected.pop(target, None)
        if source in binding:
            selected[target] = binding[source]
    client = Path(binding["client_executable"])
    require(client.is_absolute() and str(client) == binding["client_executable"],
            "published private client path is not canonical")
    require(not _historical_private_client_copy(state, binding, client),
            "published private client is a historical state copy; installed selection is required")
    try:
        metadata = client.lstat()
        canonical = client.resolve(strict=True)
        executable = os.access(client, os.X_OK)
    except (OSError, ValueError, RuntimeError) as error:
        raise AcceptanceFailure("published private client is unavailable") from error
    require(stat.S_ISREG(metadata.st_mode) and canonical == client and executable,
            "published private client is not a canonical installed executable")
    return client, selected


def _historical_private_client_copy(state, binding, client):
    """Match Go control/legacy_executable.go's validated client-layout classifier."""
    directory = client.parent
    return (binding.get("executable_identity") is None and client.name == "pueue"
            and directory.parent == state / ".supervisor" and directory.name.startswith("pueue-pair-")
            and _hex_digest(directory.name.removeprefix("pueue-pair-")))


def require_same_supervisor(before, after, *, same_identity):
    require(all(before[key] == after[key] for key in ("binding_sha256", "config_sha256", "socket_identity"))
            and before["process"]["pid"] == after["process"]["pid"]
            and same_identity is True,
            "survival_lost: supervisor was restarted or rebound")


def await_actor_closure(case, state, response, deadline):
    """A task checkpoint plus lease/native disposal; root-wide closure comes last."""
    directory = state / "tasks" / response["task_id"]
    if not path_present(directory / "lifetime" / "intent.json"):
        proof = case.prove_unlaunched(state, response["root_id"], response["task_id"])
        require(not ordinary_lifetime_owner_live(directory / "lifetime" / "owner.lock"),
                "never-launched task still has a lifetime owner")
        return {"disposition": "never-launched", "owner_released": True, "refusal_sha256": proof["refusal_sha256"]}
    checkpoint = lifetime_checkpoint(state, response, deadline)
    while time.monotonic() < deadline:
        if ordinary_lifetime_owner_live(directory / "lifetime" / "owner.lock"):
            time.sleep(0.02)
            continue
        references = [checkpoint["worker"], *checkpoint["launches"]]
        if any((current := case.native(ref["pid"], expected_start=ref["process_start"], deadline=deadline))["present"]
               and current["identity_matches"] is True for ref in references):
            time.sleep(0.02)
            continue
        intent, registration = checkpoint["intent"], checkpoint["registration"]
        output = case.controls.directory / ("scope-" + uuid.uuid4().hex + ".json")
        native_observation(case.configuration, case.lifetime.capture_token, 0, output, mode="scope",
                           deadline=deadline,
                           DELEGATE_NESTED_FIXTURE_KIND=intent["scope_kind"],
                           DELEGATE_NESTED_FIXTURE_ASSURANCE=intent["assurance"],
                           DELEGATE_NESTED_FIXTURE_PLAN=intent["scope_plan"],
                           DELEGATE_NESTED_FIXTURE_SCOPE_ID=registration["scope_id"])
        scope = _read_control_record(output, "native scope disposal")
        if scope.get("persistent_scope_present") is False:
            return {"termination": checkpoint["termination"], "owner_released": True,
                    "worker_wait": checkpoint["worker_wait"], "scope_disposed": True}
        time.sleep(0.02)
    raise TimeoutError("native task disposal or lifetime owner release remained unproven")
