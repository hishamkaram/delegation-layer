"""Private CLI capacity workflow."""

from scripts.delegate_acceptance.private_cli.configuration import COLLECT_TIMEOUT_SECONDS
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_WATCH
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort
from pathlib import Path
from scripts.delegate_acceptance.private_cli.configuration import TASK_BUDGET
from concurrent.futures import ThreadPoolExecutor
from scripts.delegate_acceptance.private_cli.capacity_records import capacity_lease_specifications
from scripts.delegate_acceptance.private_cli.fingerprints import descriptor_evidence
import hashlib
import json
from scripts.delegate_acceptance.private_cli.capacity_ownership import lease_candidate_specifications
from scripts.delegate_acceptance.private_cli.capacity_ownership import prove_lifetime_owner_cohort
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.private_cli.queue_policy import pueue_state
from scripts.delegate_acceptance.private_cli.admission import read_created_task_status
from scripts.delegate_acceptance.directories.operations import rollback_owned_directories
from scripts.delegate_acceptance.private_cli.commands import run_json
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor
from scripts.delegate_acceptance.private_cli.queue_policy import task_rows_by_label
import threading
import time
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_command_counts
from scripts.delegate_acceptance.private_cli.capacity_inputs import CapacityRequest, CapacityLeaseState
from scripts.delegate_acceptance.private_cli.capacity_dispatch import CapacityDispatcher


def verify_current_installation_upgrade(
    delegate,
    state,
    workspace,
    brief,
    release_gate,
    waiter_directory,
    env,
    old_layout,
    old_layout_owned,
    client,
    config,
    parallel_tasks,
    dispatch_workers,
    response_directory,
    evidence=None,
    *,
    lease_nonce,
    client_call_log,
    attempted_task_ids,
    capture_root,
    run_receipt=None,
):
    """Remove disposable package files while a state-owned task is running."""
    return _run_capacity_case(
        CapacityRequest(
            delegate=delegate,
            state=state,
            workspace=workspace,
            brief=brief,
            release_gate=release_gate,
            waiter_directory=waiter_directory,
            env=env,
            old_layout=old_layout,
            old_layout_owned=old_layout_owned,
            client=client,
            config=config,
            parallel_tasks=parallel_tasks,
            dispatch_workers=dispatch_workers,
            response_directory=response_directory,
            evidence=evidence,
            lease_nonce=lease_nonce,
            client_call_log=client_call_log,
            attempted_task_ids=attempted_task_ids,
            capture_root=capture_root,
            run_receipt=run_receipt,
        )
    )


def _run_capacity_case(case: CapacityRequest):
    """Remove disposable package files while a state-owned task is running."""
    waiting_brief = case.brief.with_name("waiting-brief.md")
    waiting_brief.write_text("DELEGATE_PRIVATE_UPGRADE_WAIT\n")
    receipts = Path(case.response_directory)
    receipts.mkdir(mode=448, exist_ok=True)
    evidence = case.evidence if case.evidence is not None else {}
    dispatcher = CapacityDispatcher(case, waiting_brief, receipts, evidence)
    batch = dispatcher.run()
    responses = batch.responses
    task_ids = batch.task_ids
    expected_labels = batch.expected_labels
    deadline = time.monotonic() + 120
    capacity_proof = _initial_capacity_proof(case)
    held = CapacityLeaseState()
    try:
        _observe_capacity_cohort(capacity_proof, case, deadline, expected_labels, held, responses, task_ids)
        print("Observed %d concurrent leased running tasks" % case.parallel_tasks, flush=True)
        if "status_bytes" not in capacity_proof:
            raise RuntimeError("capacity proof did not retain its Pueue status size")
        (receipts / "capacity-proof.json").write_text(json.dumps(capacity_proof, sort_keys=True) + "\n")
        if case.parallel_tasks >= 200 and capacity_proof["status_bytes"] <= 1024 * 1024:
            raise RuntimeError("large-queue acceptance did not exceed the former 1 MiB status limit")
        if case.run_receipt is not None:
            case.run_receipt.stage("live-capacity-cohort-proved")
            case.run_receipt.phase("package_removal")
        rollback_owned_directories(case.old_layout_owned)
        if case.old_layout.exists() or case.old_layout.is_symlink():
            raise RuntimeError("old package layout deletion was not verified: " + str(case.old_layout))
        _observe_upgraded_status_burst(case, evidence, receipts, responses)
        held.cohort.assert_live()
        case.release_gate.release()
        capacity_proof["gate_opened_after_proof"] = True
        (receipts / "capacity-proof.json").write_text(json.dumps(capacity_proof, sort_keys=True) + "\n")
        evidence["capacity_proof"] = capacity_proof
    finally:
        if held.cohort is not None:
            held.cohort.close()

    return _collect_and_restart_capacity(capacity_proof, case, dispatcher, receipts, task_ids)


def _initial_capacity_proof(case):
    capacity_proof = {
        "requested_task_count": case.parallel_tasks,
        "observed_running_task_count": 0,
        "peak_observed_running_task_count": 0,
        "observed_lease_candidate_count": 0,
        "peak_observed_lease_candidate_count": 0,
        "observed_live_lease_count": 0,
        "observed_lifetime_owner_lease_count": 0,
        "lease_protocol": "fifo-nonce-v1",
        "lease_nonce_sha256": hashlib.sha256(case.lease_nonce.encode()).hexdigest(),
        "queue_confirmed_while_leases_held": False,
        "proof_established_before_gate": False,
        "gate_opened_after_proof": False,
    }
    return capacity_proof


def _snapshot_capacity_candidates(capacity_proof, case, expected_labels, responses):
    snapshot_metrics = {}
    snapshot = pueue_snapshot(
        case.client,
        case.config,
        case.env,
        snapshot_metrics,
        phase="capacity-observation",
        capture_root=case.capture_root,
    )
    rows_by_label = task_rows_by_label(snapshot, expected_labels)
    observed_running = sum((1 for row in rows_by_label.values() if pueue_state(row) == "Running"))
    specifications = capacity_lease_specifications(case.state, responses, case.waiter_directory, case.lease_nonce)
    lease_candidates = len(lease_candidate_specifications(specifications))
    capacity_proof["observed_running_task_count"] = observed_running
    capacity_proof["peak_observed_running_task_count"] = max(
        capacity_proof["peak_observed_running_task_count"], observed_running
    )
    capacity_proof["observed_lease_candidate_count"] = lease_candidates
    capacity_proof["peak_observed_lease_candidate_count"] = max(
        capacity_proof["peak_observed_lease_candidate_count"], lease_candidates
    )
    if "status_bytes" in snapshot_metrics:
        capacity_proof["status_bytes"] = max(capacity_proof.get("status_bytes", 0), snapshot_metrics["status_bytes"])
    if case.run_receipt is not None:
        case.run_receipt.update_counts(running_task_count=observed_running, live_lease_count=0)
        case.run_receipt.update_queue(
            task_count=len(snapshot.get("tasks", {})),
            running_count=observed_running,
            status_bytes=snapshot_metrics.get("status_bytes"),
        )
    return observed_running, specifications, lease_candidates


def _confirm_capacity_cohort(capacity_proof, case, expected_labels, held, specifications, task_ids):
    held.cohort = LiveFIFOLeaseCohort.acquire(specifications, case.lease_nonce)
    confirmation_metrics = {}
    confirmation = pueue_snapshot(
        case.client,
        case.config,
        case.env,
        confirmation_metrics,
        phase="capacity-observation",
        capture_root=case.capture_root,
    )
    confirmed_rows = task_rows_by_label(confirmation, expected_labels)
    confirmed_running = sum((1 for row in confirmed_rows.values() if pueue_state(row) == "Running"))
    held.cohort.assert_live()
    if confirmed_running != case.parallel_tasks:
        raise RuntimeError("capacity fixture left Running while the live lease cohort formed")
    owner_cohort = prove_lifetime_owner_cohort(specifications, held.cohort)
    counted_tasks = owner_cohort.get("counted_tasks")
    if (
        not isinstance(counted_tasks, list)
        or owner_cohort.get("counted_task_count") != case.parallel_tasks
        or len(counted_tasks) != case.parallel_tasks
        or ({item.get("task_id") for item in counted_tasks if isinstance(item, dict)} != set(task_ids))
    ):
        raise RuntimeError("capacity lifetime-owner proof did not bind every admitted task")
    final_confirmation_metrics = {}
    final_confirmation = pueue_snapshot(
        case.client,
        case.config,
        case.env,
        final_confirmation_metrics,
        phase="capacity-observation",
        capture_root=case.capture_root,
    )
    final_rows = task_rows_by_label(final_confirmation, expected_labels)
    final_running = sum((1 for row in final_rows.values() if pueue_state(row) == "Running"))
    held.cohort.assert_live()
    if final_running != case.parallel_tasks:
        raise RuntimeError("capacity fixture left Running during lifetime-owner proof")
    _record_capacity_cohort(
        capacity_proof=capacity_proof,
        case=case,
        confirmation_metrics=confirmation_metrics,
        confirmed_running=confirmed_running,
        counted_tasks=counted_tasks,
        final_confirmation=final_confirmation,
        final_confirmation_metrics=final_confirmation_metrics,
        final_running=final_running,
        held=held,
        owner_cohort=owner_cohort,
    )


def _record_capacity_cohort(
    *, capacity_proof,
    case,
    confirmation_metrics,
    confirmed_running,
    counted_tasks,
    final_confirmation,
    final_confirmation_metrics,
    final_running,
    held,
    owner_cohort,
):
    capacity_proof["observed_running_task_count"] = confirmed_running
    capacity_proof["observed_live_lease_count"] = len(held.cohort.leases)
    capacity_proof["observed_lifetime_owner_lease_count"] = len(owner_cohort["counted_tasks"])
    capacity_proof["queue_confirmed_while_leases_held"] = True
    capacity_proof["proof_established_before_gate"] = True
    capacity_proof["file_descriptors_while_leases_held"] = descriptor_evidence()
    capacity_proof["lifetime_owner_overlap"] = owner_cohort
    scope_kind_counts = {}
    assurance_counts = {}
    for counted in counted_tasks:
        scope_kind = counted["scope_kind"]
        assurance = counted["assurance"]
        scope_kind_counts[scope_kind] = scope_kind_counts.get(scope_kind, 0) + 1
        assurance_counts[assurance] = assurance_counts.get(assurance, 0) + 1
    capacity_proof["scope_kind_counts"] = dict(sorted(scope_kind_counts.items()))
    capacity_proof["assurance_counts"] = dict(sorted(assurance_counts.items()))
    for metrics in (confirmation_metrics, final_confirmation_metrics):
        if "status_bytes" not in metrics:
            continue
        capacity_proof["status_bytes"] = max(capacity_proof.get("status_bytes", 0), metrics["status_bytes"])
    if case.run_receipt is not None:
        case.run_receipt.update_counts(running_task_count=final_running, live_lease_count=len(held.cohort.leases))
        case.run_receipt.update_queue(
            task_count=len(final_confirmation.get("tasks", {})),
            running_count=final_running,
            status_bytes=final_confirmation_metrics.get("status_bytes"),
        )


def _observe_capacity_cohort(capacity_proof, case, deadline, expected_labels, held, responses, task_ids):
    if case.run_receipt is not None:
        case.run_receipt.phase("capacity_observation")
    while True:
        observed_running, specifications, lease_candidates = _snapshot_capacity_candidates(
            capacity_proof, case, expected_labels, responses
        )
        if (
            observed_running == case.parallel_tasks
            and len(specifications) == case.parallel_tasks
            and (lease_candidates == case.parallel_tasks)
        ):
            _confirm_capacity_cohort(capacity_proof, case, expected_labels, held, specifications, task_ids)
            break
        if time.monotonic() >= deadline:
            raise RuntimeError("fixture tasks did not all reach concurrent leased Running state: %r" % capacity_proof)
        time.sleep(0.1)


def _observe_upgraded_status_burst(case, evidence, receipts, responses):
    status_probe_responses = responses[: min(case.parallel_tasks, 16)]
    status_probe_barrier = threading.Barrier(len(status_probe_responses))
    status_snapshots_before = wrapper_command_counts(case.client_call_log).get("status", 0)

    def observe(response):
        task_id = response["task_id"]
        status_probe_barrier.wait(timeout=30)
        observed = read_created_task_status(
            case.delegate,
            case.state,
            case.env,
            task_id,
            receipts / ("status-after-upgrade-" + task_id + ".json"),
            phase="managed-runner-observation",
            capture_root=case.capture_root,
        )
        supervisor = observed.get("supervisor", {})
        if (
            observed.get("admission") != "admitted"
            or observed.get("error")
            or observed.get("status") == "timed_out"
            or (supervisor.get("matched") is not True)
            or (supervisor.get("state") != "running")
        ):
            raise RuntimeError("upgraded installation did not observe its gated task running: %r" % observed)

    if case.run_receipt is not None:
        case.run_receipt.phase("upgraded_observation")
    with ThreadPoolExecutor(max_workers=len(status_probe_responses)) as pool:
        list(pool.map(observe, status_probe_responses))
    status_snapshot_count = wrapper_command_counts(case.client_call_log).get("status", 0) - status_snapshots_before
    status_coalescing = {
        "concurrent_status_call_count": len(status_probe_responses),
        "pueue_status_snapshot_count": status_snapshot_count,
        "shared_status_call_count": len(status_probe_responses) - status_snapshot_count,
    }
    if status_snapshot_count <= 0 or status_snapshot_count >= len(status_probe_responses):
        raise RuntimeError("concurrent public status calls did not coalesce queue snapshots: %r" % status_coalescing)
    (receipts / "status-coalescing.json").write_text(json.dumps(status_coalescing, sort_keys=True) + "\n")
    evidence["status_coalescing"] = status_coalescing


def _collect_and_restart_capacity(capacity_proof, case, dispatcher, receipts, task_ids):
    def collect(task_id):
        collected = run_json(
            case.delegate,
            case.env,
            ["--root", str(case.state), "collect", task_id, "--watch", COLLECT_WATCH, "--json"],
            timeout=COLLECT_TIMEOUT_SECONDS,
            receipt=receipts / ("collect-" + task_id + ".json"),
            phase="managed-runner-collection",
            capture_root=case.capture_root,
        )
        if collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError("removing old package files disrupted the state-owned task")
        if case.run_receipt is not None:
            case.run_receipt.increment("collected_task_count")

    if case.run_receipt is not None:
        case.run_receipt.phase("capacity_collection")
    with ThreadPoolExecutor(max_workers=min(case.parallel_tasks, 16)) as pool:
        list(pool.map(collect, task_ids))
    shutdown_private_supervisor(
        case.client, case.config, case.env, state=case.state, task_ids=tuple(task_ids), capture_root=case.capture_root
    )
    return _restart_capacity_task(capacity_proof, case, dispatcher, receipts, task_ids)


def _restart_capacity_task(capacity_proof, case, dispatcher, receipts, task_ids):
    response = run_json(
        case.delegate,
        case.env,
        [
            "--root",
            str(case.state),
            "dispatch",
            "--provider",
            "pi:json",
            "--brief",
            str(case.brief),
            "--cwd",
            str(case.workspace),
            "--permission",
            "read-only",
            "--budget",
            TASK_BUDGET,
            "--json",
        ],
        receipt=receipts / "dispatch-restarted.json",
        phase="restart-dispatch",
        capture_root=case.capture_root,
    )
    if response.get("task_record") != "created" or response.get("admission") != "admitted":
        raise RuntimeError("restart probe was not durably admitted")
    restarted_id = response["task_id"]
    dispatcher.remember_attempt(restarted_id)
    run_json(
        case.delegate,
        case.env,
        ["--root", str(case.state), "status", restarted_id, "--json"],
        receipt=receipts / "status-restarted.json",
        phase="restart-observation",
        capture_root=case.capture_root,
    )
    collected = run_json(
        case.delegate,
        case.env,
        ["--root", str(case.state), "collect", restarted_id, "--watch", COLLECT_WATCH, "--json"],
        timeout=COLLECT_TIMEOUT_SECONDS,
        receipt=receipts / "collect-restarted.json",
        phase="restart-collection",
        capture_root=case.capture_root,
    )
    if collected.get("outcome", {}).get("verdict") != "committed":
        raise RuntimeError("private supervisor restart did not complete its task")
    if case.run_receipt is not None:
        case.run_receipt.increment("collected_task_count")
        case.run_receipt.stage("current-installation-restart-proved")
    return ([*task_ids, restarted_id], capacity_proof["status_bytes"])
