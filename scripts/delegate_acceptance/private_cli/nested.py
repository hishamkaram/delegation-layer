"""Native nested-worker survival experiment over the real private CLI paths."""

import os
import re
import time
import uuid

from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.private_cli.capacity_ownership import prove_lifetime_owner_cohort
from scripts.delegate_acceptance.private_cli.nested_case import NestedCase
from scripts.delegate_acceptance.private_cli.nested_observation import (
    acquire_actor_lease, actor_specification, await_record, bound_private_client, lifetime_checkpoint,
    require_same_supervisor, supervisor_continuity,
)
from scripts.delegate_acceptance.private_cli.nested_policy import (
    require_challenge, require_descendant_absent, require_setup_margin, require_worker_deadline, timestamp_ns,
)
from scripts.delegate_acceptance.private_cli.queue_policy import pueue_state, task_row_by_label
from scripts.delegate_acceptance.private_cli.supervisor import pueue_command, pueue_snapshot
from scripts.delegate_acceptance.private_cli.wrapper_policy import require_no_startup_control_calls
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_call_records


def run_nested_survival(args, lifetime, fixture, receipts, receipt):
    case = NestedCase(args, lifetime, fixture, receipts, receipt)
    receipt.phase("nested_setup")
    require(not os.environ.get("DELEGATION_LAYER_PROCESS_SCOPE"), "driver carries a managed scope marker")
    driver = case.native(os.getpid())
    require(not any(re.fullmatch(r"delegate-v1-[0-9a-f]{32}", part)
                    for part in driver["cgroup"].split("/")), "driver is inside a Delegate cgroup")
    case.evidence["driver"] = driver
    bootstrap(case, case.a, "warm-a")
    if args.nested_topology == "other-warm":
        bootstrap(case, case.b, "warm-b")
    elif args.nested_topology == "other-cold":
        require(not case.b.exists() and not case.b.is_symlink(), "cold worker root already exists")
        case.evidence["cold_runtime_absent_at_unix_ns"] = time.time_ns()
    warm_before_leader = None
    if args.nested_topology != "other-cold":
        warm_before_leader = supervisor_continuity(case.b, case.configuration, lifetime.capture_token, "")
        case.evidence["warm_before_leader"] = warm_before_leader
    leader = case.dispatch(case.a, case.configuration["leader"], case.configuration["leader_brief"], "3m", "leader")
    case.responses["leader"] = leader
    leader_identity = await_record(case.controls.directory / "leader-identity.json", time.monotonic() + 30)
    leader_launch = case.match_launch(case.a, leader, leader_identity)
    leader_spec = actor_specification(case.a, leader, case.configuration, args.expected_backend)
    require(leader_spec["pid"] == leader_launch["pid"], "leader specification selected a different command")
    case.evidence["actors"]["leader"] = compact_spec(leader_spec)
    w1 = case.nested_response("w1")
    # Only now may the observer contact B. No external setup or discovery can
    # accidentally turn the cold target into the warm control.
    first_spec = actor_specification(case.b, w1, case.configuration, args.expected_backend, live=True)
    require_worker_deadline(read_lifetime(case.b, w1, "intent.json"), 0, 0)
    first_lease = case.leases.enter_context(acquire_actor_lease(first_spec, case.configuration))
    case.evidence["actors"]["w1"] = compact_spec(first_spec)
    scope = read_lifetime(case.a, leader, "intent.json")["scope_plan"]
    before = supervisor_continuity(case.b, case.configuration, lifetime.capture_token, scope)
    case.evidence["supervisor_before"] = before
    if warm_before_leader is not None:
        require_same_supervisor(warm_before_leader, before,
                                same_identity=case.tokens_match(before["process"]["process_start"],
                                                               warm_before_leader["process"]["process_start"]))
    record_actor_placement(case, leader, w1, before)
    receipt.update_counts(running_task_count=2, live_lease_count=1)
    queue_worker(case, leader_spec, first_lease)
    prove_survival(case, leader, leader_spec, first_spec, first_lease, before, scope)
    calls = wrapper_call_records(lifetime.base / "old-client-starts-calls", "client", "old")
    calls += wrapper_call_records(lifetime.base / "old-daemon-starts-calls", "daemon", "old")
    require_no_startup_control_calls(calls)
    case.evidence["wrapper_calls"] = calls
    receipt.stage("nested-survival-proven")


def compact_spec(specification):
    return {key: specification[key] for key in ("root_id", "task_id", "pid", "scope_kind", "assurance",
                                                "registration_sha256", "scope_plan_sha256", "deadline")}


def read_lifetime(state, response, name):
    return _read_control_record(state / "tasks" / response["task_id"] / "lifetime" / name, "nested lifetime record")


def bootstrap(case, state, name):
    response = case.dispatch(state, uuid.uuid4().hex, case.fixture.brief, "2m", name)
    case.collect(state, response, "collect-" + name)


def record_actor_placement(case, leader, first, supervisor):
    leader_intent = read_lifetime(case.a, leader, "intent.json")
    identity = await_record(case.controls.directory / "leader-identity.json", time.monotonic() + 5)
    leader_launch = case.match_launch(case.a, leader, identity)
    first_worker = read_lifetime(case.b, first, "worker.json")
    worker = case.native(first_worker["pid"], scope=leader_intent["scope_plan"],
                         expected_start=first_worker["process_start"])
    require(worker["present"] and worker["identity_matches"] is True, "registered worker changed")
    placement = {"guardian": case.native(worker["ppid"], scope=leader_intent["scope_plan"]),
                 "guardian_binding": "parent-of-registered-worker",
                 "worker": worker, "leader_provider": case.native(leader_launch["pid"], expected_start=leader_launch["process_start"]),
                 "leader_launch": leader_launch}
    require(placement["leader_provider"]["present"]
            and placement["leader_provider"]["identity_matches"] is True,
            "leader fixture incarnation changed before setup")
    case.evidence["placement"] = placement
    if case.args.nested_topology == "other-warm" and case.args.expected_backend == "cgroup-v2":
        parent = placement["leader_provider"]["cgroup"].rstrip("/")
        require(not supervisor["process"]["cgroup"].startswith(parent + "/")
                and supervisor["process"]["cgroup"] != parent, "warm supervisor was placed inside the leader")
    if case.args.nested_topology == "other-cold":
        intent = await_record(case.controls.directory / "w1-intent.json", time.monotonic() + 5)
        require(intent["observed_at_unix_ns"] > timestamp_ns(leader_launch["started_at"]),
                "cold nested creation did not follow leader provider launch")
    descendant = await_record(case.controls.directory / "descendant.json", time.monotonic() + 5)
    live = case.native(descendant["pid"], scope=leader_intent["scope_plan"], expected_start=descendant["process_start"])
    require(live["present"] and live["identity_matches"] is True
            and live["pid"] == live["session_id"] == live["pgid"]
            and descendant.get("orphaned_from", 0) > 1 and live["ppid"] != descendant["orphaned_from"]
            and time.time_ns() < timestamp_ns(leader_intent["deadline"]), "ordinary orphan was not proven live pre-expiry")
    case.evidence["ordinary_descendant_before"] = live
    case.evidence["ordinary_descendant_before_ns"] = time.time_ns()


def queue_worker(case, leader_spec, first_lease):
    client, environment = bound_private_client(case.b, case.lifetime.env)
    config = case.b / ".supervisor" / "pueue.yml"
    pueue_command(client, config, environment, ["pause", "--wait", "--group", "default"],
                  phase="nested-external-observer", capture_root=case.lifetime.capture_token)
    first = case.responses["w1"]
    first_label = "delegate:" + first["root_id"] + ":" + first["task_id"]
    paused = pueue_snapshot(client, config, environment, phase="nested-external-observer",
                            capture_root=case.lifetime.capture_token)
    require(pueue_state(task_row_by_label(paused, first_label) or {}) == "Running", "pause stopped W1")
    first_lease.assert_live()
    case.controls.directory.joinpath("paused").touch(mode=0o600)
    second = case.nested_response("w2")
    snapshot = pueue_snapshot(client, config, environment, phase="nested-external-observer",
                              capture_root=case.lifetime.capture_token)
    label = "delegate:" + second["root_id"] + ":" + second["task_id"]
    row = task_row_by_label(snapshot, label)
    require(row is not None and pueue_state(row) == "Queued", "W2 was not held queued")
    require(pueue_state(task_row_by_label(snapshot, first_label) or {}) == "Running", "W1 stopped before setup completed")
    first_lease.assert_live()
    directory = case.b / "tasks" / second["task_id"]
    require(all(not os.path.lexists(directory / name) for name in ("provider.start", "provider.exit")),
            "queued worker already consumed a provider launch")
    root = second["root_id"]
    known = {"delegate:" + root + ":" + task for task in case.b_ids}
    for candidate in snapshot.get("tasks", {}).values():
        label = candidate.get("label", "")
        require(label in known or (label.startswith("delegate-inspection:")
                and str(candidate.get("group", "")).startswith("delegate-inspect")), "unexpected work in owned queue")
    require_setup_margin(timestamp_ns(leader_spec["deadline"]), time.time_ns())
    case.evidence["queued"] = {"task_id": second["task_id"], "state": "Queued", "provider_launch": "absent",
                                "observed_at_unix_ns": time.time_ns()}


def prove_survival(case, leader, leader_spec, first_spec, first_lease, before, scope):
    case.receipt.phase("nested_expiry")
    deadline_ns = timestamp_ns(leader_spec["deadline"])
    watch = time.monotonic() + max(0, deadline_ns - time.time_ns()) / 1_000_000_000 + 30
    checkpoint = lifetime_checkpoint(case.a, leader, watch)
    termination = checkpoint["termination"]
    terminated_ns = timestamp_ns(termination["observed_at"])
    require(termination["cause"] == "budget" and terminated_ns >= deadline_ns,
            "leader did not expire at its actual budget deadline")
    case.evidence["milestones"]["leader_termination"] = termination
    case.receipt.update_counts(running_task_count=1)
    after = supervisor_continuity(case.b, case.configuration, case.lifetime.capture_token, scope)
    case.evidence["supervisor_after"] = after
    require_same_supervisor(before, after,
                            same_identity=case.tokens_match(after["process"]["process_start"], before["process"]["process_start"]))
    descendant_control(case, deadline_ns)
    nonce = uuid.uuid4().hex
    write_json(case.controls.directory / "challenge.json", {"nonce": nonce, "issued_at_unix_ns": time.time_ns()})
    reply = await_record(case.controls.directory / "reply.json", time.monotonic() + 5)
    launch = case.match_launch(case.b, case.responses["w1"], reply)
    require_challenge(reply, nonce, launch, terminated_ns, timestamp_ns(first_spec["deadline"]),
                      same_identity=case.tokens_match(reply["process_start"], launch["process_start"]))
    case.evidence["w1_live_owner"] = prove_lifetime_owner_cohort([first_spec], first_lease)
    case.evidence["nonce_reply"] = reply
    finish_workers(case, terminated_ns, first_lease)
    case.evidence["actors"]["leader"]["closure"] = case.close(case.a, leader, time.monotonic() + 30)


def descendant_control(case, deadline_ns):
    original = case.evidence["ordinary_descendant_before"]
    observed = case.native(original["pid"], expected_start=original["process_start"])
    require_descendant_absent(original, observed, time.time_ns(), deadline_ns,
                              (case.controls.directory / "descendant-release").exists(),
                              same_identity=observed["identity_matches"])
    case.evidence["ordinary_descendant"] = {"before": original, "after": observed,
                                            "observed_at_unix_ns": time.time_ns()}


def finish_workers(case, terminated_ns, first_lease):
    case.release("w1")
    case.evidence["actors"]["w1"]["result"] = case.collect(case.b, case.responses["w1"], "collect-w1")
    first_lease.remove_closed_fifos()
    resume_ns = time.time_ns()
    client, environment = bound_private_client(case.b, case.lifetime.env)
    pueue_command(client, case.b / ".supervisor" / "pueue.yml", environment,
                  ["start", "--group", "default"], phase="nested-external-observer",
                  capture_root=case.lifetime.capture_token)
    second = case.responses["w2"]
    spec = actor_specification(case.b, second, case.configuration, case.args.expected_backend, live=True)
    cohort = case.leases.enter_context(acquire_actor_lease(spec, case.configuration))
    require_worker_deadline(read_lifetime(case.b, second, "intent.json"), resume_ns, terminated_ns)
    case.evidence["actors"]["w2"] = compact_spec(spec)
    require(not os.path.lexists(case.b / "tasks" / second["task_id"] / "provider.exit")
            and not os.path.lexists(case.b / "tasks" / second["task_id"] / "lifetime" / "terminated.json"),
            "queued worker terminated before its gate release")
    case.release("w2")
    case.evidence["actors"]["w2"]["result"] = case.collect(case.b, second, "collect-w2")
    cohort.remove_closed_fifos()
    case.receipt.update_counts(running_task_count=0, live_lease_count=0)
