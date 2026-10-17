"""Private external-handoff workflow with one finalization ledger."""

from scripts.delegate_acceptance.private_cli.handoff_observation import ObservedHandoffTask
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_TIMEOUT_SECONDS
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_WATCH
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_TASK_BUDGET
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_TASK_COUNT
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort
from scripts.delegate_acceptance.private_cli.leases import LockedReleaseGate
from pathlib import Path
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.private_cli.fingerprints import REPOSITORY
from scripts.delegate_acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.delegate_acceptance.private_cli.supervisor import await_managed_lifetime_closure
from scripts.delegate_acceptance.private_cli.handoff_cleanup import cleanup_external_handoff_cohorts
from scripts.delegate_acceptance.supervisor.config_policy import config_for
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_dispatch_evidence
from scripts.delegate_acceptance.private_cli.handoff_policy import external_handoff_closure_evidence
from scripts.delegate_acceptance.private_cli.fingerprints import file_fingerprint
import hashlib
import json
from scripts.delegate_acceptance.private_cli.capacity_ownership import prove_lifetime_owner_cohort
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.private_cli.queue_policy import pueue_state
from scripts.delegate_acceptance.private_cli.handoff_journal import read_external_inspection_handoff_journal
from scripts.delegate_acceptance.private_cli.handoff_supervisor import registered_process_handle
from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.private_cli.leases import require_empty_fixture_waiters
from scripts.delegate_acceptance.private_cli.handoff_policy import require_handoff_task_binding
from scripts.delegate_acceptance.private_cli.commands import run_json
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor
from scripts.delegate_acceptance.private_cli.queue_policy import task_row_by_label
import uuid
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.private_cli.handoff_tasks import HandoffTaskCommands
from scripts.delegate_acceptance.private_cli.handoff_supervisor import ExternalSupervisorControl
from dataclasses import dataclass, field
from scripts.delegate_acceptance.directories.operations import OwnedDirectory
from scripts.delegate_acceptance.processes.operations import Process
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt


@dataclass(frozen=True)
class HandoffCase:
    """One case's input handles; mutable ledgers are references to their existing owner."""

    delegate: Path
    client: Path
    pueued: Path
    state: Path
    workspace: Path
    gate: Path
    waiter_directory: Path
    private_config: Path
    env: dict[str, str]
    response_directory: Path
    provider_fixture: Path
    lease_nonce: str
    run_receipt: AcceptanceReceipt
    attempted_task_ids: list[str]
    external_root_token: OwnedDirectory
    external_process_root_token: OwnedDirectory
    capture_root: OwnedDirectory
    handoff_action: str
    case_label: str
    handoff_cases: dict[str, dict[str, object]]


@dataclass
class HandoffCleanup:
    """One case's finalizer references; Processes remains the sole process registry."""

    external_processes: Processes | None = None
    external_process: Process | None = None
    release_gate: LockedReleaseGate | None = None
    old_cohort: LiveFIFOLeaseCohort | None = None
    new_cohort: LiveFIFOLeaseCohort | None = None
    primary_error: BaseException | None = None
    cleanup_errors: list[str] = field(default_factory=list)
    delegate_control_started: bool = False


def run_external_inspection_handoff_acceptance(
    delegate,
    client,
    pueued,
    state,
    workspace,
    gate,
    waiter_directory,
    private_config,
    env,
    response_directory,
    provider_fixture,
    lease_nonce,
    run_receipt,
    attempted_task_ids,
    external_root_token,
    external_process_root_token,
    *,
    capture_root,
    handoff_action,
    case_label,
    handoff_cases,
):
    """Prove one external inspection handoff entrypoint and its private successor."""
    return _run_handoff_case(
        HandoffCase(
            delegate=delegate,
            client=client,
            pueued=pueued,
            state=state,
            workspace=workspace,
            gate=gate,
            waiter_directory=waiter_directory,
            private_config=private_config,
            env=env,
            response_directory=response_directory,
            provider_fixture=provider_fixture,
            lease_nonce=lease_nonce,
            run_receipt=run_receipt,
            attempted_task_ids=attempted_task_ids,
            external_root_token=external_root_token,
            external_process_root_token=external_process_root_token,
            capture_root=capture_root,
            handoff_action=handoff_action,
            case_label=case_label,
            handoff_cases=handoff_cases,
        )
    )


def _run_handoff_case(case: HandoffCase):
    resources = HandoffCleanup()
    "Prove one external inspection handoff entrypoint and its private successor."
    task_commands, supervisor_control, evidence, record_evidence = _initialize_handoff_case(case, resources)
    try:
        old_task, external_pid = _start_external_task(case, evidence, resources, supervisor_control, task_commands)
        automatic_response = _trigger_private_handoff(case, evidence, task_commands)
        journal = _observe_handoff_transition(
            case=case,
            evidence=evidence,
            external_before_name=old_task.snapshot_name,
            external_calls_before=old_task.wrapper_calls,
            external_pid=external_pid,
            old_label=old_task.label,
            old_row_before=old_task.row,
            resources=resources,
            supervisor_control=supervisor_control,
            task_commands=task_commands,
        )
        private_pair = assert_state_supervisor_pair(case.state, case.client.parent)
        if automatic_response is None:
            new_task_id = uuid.uuid4().hex
            case.attempted_task_ids.append(new_task_id)
            new_response = task_commands.dispatch_fixture(
                new_task_id, task_commands.private_prefix, "dispatch-private.json"
            )
        else:
            new_task_id = automatic_response["task_id"]
            new_response = automatic_response
        new_specs, resources.new_cohort = task_commands.live_cohort(
            new_response, case.client, case.private_config, "private-supervisor-observation"
        )
        case.run_receipt.update_counts(running_task_count=2, live_lease_count=2)
        new_meta_path, new_meta_sha256 = _prove_handoff_owners(
            case=case,
            evidence=evidence,
            external_pid=external_pid,
            journal=journal,
            new_response=new_response,
            new_specs=new_specs,
            new_task_id=new_task_id,
            old_meta_sha256=old_task.metadata_sha256,
            old_response=old_task.response,
            old_specs=old_task.specifications,
            private_pair=private_pair,
            resources=resources,
            task_commands=task_commands,
        )
        _close_handoff_scopes(case, evidence, new_task_id, old_task.task_id, resources)
        _collect_handoff_tasks(
            case=case,
            new_meta_path=new_meta_path,
            new_meta_sha256=new_meta_sha256,
            new_task_id=new_task_id,
            old_meta_bytes=old_task.metadata,
            old_meta_path=old_task.metadata_path,
            old_task_id=old_task.task_id,
            task_commands=task_commands,
        )
        return _complete_handoff_success(case, evidence, old_task, record_evidence, supervisor_control)
    except BaseException as error:
        resources.primary_error = error
        record_evidence()
        raise
    finally:
        _finalize_handoff_case(case, evidence, record_evidence, resources, supervisor_control)


def _initialize_handoff_case(case, resources):
    require(
        case.handoff_action in ("explicit-migrate", "automatic-models", "automatic-dispatch"),
        "external inspection handoff action is unsupported: " + str(case.handoff_action),
    )
    require(
        case.case_label not in case.handoff_cases, "external inspection handoff case is duplicated: " + case.case_label
    )
    receipts = Path(case.response_directory)
    case.external_root_token.assert_live()
    case.external_process_root_token.assert_live()
    external_root = case.external_root_token.directory
    external_config = external_root / "pueue.yml"
    external_config.write_text(json.dumps(config_for(external_root), indent=2) + "\n")
    external_config.chmod(0o600)
    external_endpoint = external_root / "run" / "p.sock"
    external_pid_path = external_root / "run" / "p.pid"
    resources.external_processes = Processes(
        case.external_process_root_token.directory,
        environment=case.env,
        capture_root=case.external_process_root_token,
        return_result=True,
    )
    evidence = _initial_handoff_evidence(case, external_config, external_endpoint, external_pid_path)

    def record_evidence():
        case.handoff_cases[case.case_label] = evidence
        case.run_receipt.add_details(external_inspection_handoff=case.handoff_cases)

    record_evidence()
    waiting_brief = Path(case.state).parent / "external-handoff-waiting-brief.md"
    task_commands = HandoffTaskCommands(
        delegate=case.delegate,
        env=case.env,
        state=case.state,
        workspace=case.workspace,
        waiting_brief=waiting_brief,
        receipts=receipts,
        capture_root=case.capture_root,
        run_receipt=case.run_receipt,
        waiter_directory=case.waiter_directory,
        lease_nonce=case.lease_nonce,
        external_config=external_config,
    )
    supervisor_control = ExternalSupervisorControl(
        external_processes=resources.external_processes,
        client=case.client,
        env=case.env,
        external_config=external_config,
        external_endpoint=external_endpoint,
        external_pid_path=external_pid_path,
        capture_root=case.capture_root,
        evidence=evidence,
    )
    waiting_brief.write_text("DELEGATE_PRIVATE_UPGRADE_WAIT\n")
    waiting_brief.chmod(0o600)
    case.run_receipt.phase("external_inspection_setup")
    return (task_commands, supervisor_control, evidence, record_evidence)


def _initial_handoff_evidence(case, external_config, external_endpoint, external_pid_path):
    evidence = {
        "scenario": EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
        "case": case.case_label,
        "handoff_action": case.handoff_action,
        "requested_task_count": EXTERNAL_INSPECTION_HANDOFF_TASK_COUNT,
        "task_budget": EXTERNAL_INSPECTION_HANDOFF_TASK_BUDGET,
        "provider": "pi:json",
        "fixture": "fake-pi-live-fifo",
        "provider_fixture": file_fingerprint(case.provider_fixture),
        "source_fingerprints": {
            "acceptance_private_cli.py": file_fingerprint(REPOSITORY / "scripts" / "acceptance_private_cli.py"),
            "acceptance_supervisor_common.py": file_fingerprint(
                REPOSITORY / "scripts" / "acceptance_supervisor_common.py"
            ),
        },
        "supervisors": {
            "external": {
                "config": str(external_config),
                "config_fingerprint": file_fingerprint(external_config),
                "endpoint": str(external_endpoint),
                "pid_path": str(external_pid_path),
                "daemon_shutdown": "pending",
            },
            "private": {"config": str(case.private_config), "daemon_shutdown": "pending"},
        },
        "tasks": {},
        "journal": {},
        "cleanup": {
            "fixture_waiter_entries": "pending",
            "lifetime_closure": "canonical-managed-validator",
            "external_daemon": "pending",
            "private_daemon": "pending",
        },
    }
    return evidence


def _prove_external_daemon_ready(evidence, resources, supervisor_control):
    wait_until(supervisor_control.wait_external_ready, timeout=30, description="fixture external supervisor readiness")
    require(supervisor_control.external_pid_path.is_file(), "fixture external supervisor PID record is missing")
    external_pid = int(supervisor_control.external_pid_path.read_text().strip())
    require(external_pid == resources.external_process.pid, "fixture external PID identity changed at startup")
    evidence["supervisors"]["external"].update({"daemon_pid": external_pid, "pid_identity": {"pid": external_pid}})
    return external_pid


def _inspect_external_head(case, evidence, supervisor_control, task_commands):
    models = run_json(
        case.delegate,
        case.env,
        [*task_commands.external_prefix, "models", "--provider", "pi:json", "--cwd", str(case.workspace), "--json"],
        timeout=120,
        receipt=task_commands.receipts / "models-external.json",
        phase="external-inspection-worker",
        capture_root=case.capture_root,
    )
    if models.get("status") != "available" or [model.get("id") for model in models.get("models", [])] != [
        "openai/gpt-4.1"
    ]:
        raise RuntimeError("external fixture model inspection did not complete")
    require(
        not case.private_config.parent.exists(),
        "external-only inspection unexpectedly created private supervisor state",
    )
    journal_request = case.state / "inspection-group" / "request.json"
    require(journal_request.is_file(), "external inspection journal was not created")
    legacy_request = json.loads(journal_request.read_text())
    require(
        legacy_request.get("supervisor", {}).get("config_path") == str(supervisor_control.external_config),
        "inspection journal was not bound to the external supervisor",
    )
    evidence["journal"]["external_request"] = legacy_request
    evidence["journal"]["external_request_sha256"] = hashlib.sha256(journal_request.read_bytes()).hexdigest()
    evidence["journal"]["creation_command"] = "models"
    return legacy_request


def _observe_external_task(case, legacy_request, old_response, old_task_id, supervisor_control, task_commands):
    old_meta_path, old_meta_bytes, old_meta_sha256 = task_commands.task_meta(old_task_id)
    require_handoff_task_binding(old_meta_bytes, legacy_request["supervisor"])
    external_before = pueue_snapshot(
        case.client,
        supervisor_control.external_config,
        case.env,
        phase="external-supervisor-observation",
        capture_root=case.capture_root,
    )
    external_before_name = task_commands.write_snapshot("pueue-external-before-migrate.json", external_before)
    old_label = "delegate:" + old_response["root_id"] + ":" + old_task_id
    old_row_before = task_row_by_label(external_before, old_label)
    require(
        old_row_before is not None and pueue_state(old_row_before) == "Running",
        "external task was not running before migration",
    )
    external_calls_before = task_commands.external_client_calls()
    return (
        old_meta_path,
        old_meta_bytes,
        old_meta_sha256,
        old_label,
        old_row_before,
        external_before_name,
        external_calls_before,
    )


def _trigger_private_handoff(case, evidence, task_commands):
    case.run_receipt.phase("external_inspection_handoff")
    automatic_response = None
    if case.handoff_action == "explicit-migrate":
        migration = run_json(
            case.delegate,
            case.env,
            [*task_commands.private_prefix, "supervisor", "migrate", "--json"],
            timeout=120,
            receipt=task_commands.receipts / "supervisor-migrate.json",
            phase="supervisor-migration",
            capture_root=case.capture_root,
        )
        control = migration.get("supervisor_control") or {}
        if control.get("state") != "migrated":
            raise RuntimeError("supervisor migrate did not publish a migrated state")
        evidence["handoff_trigger"] = {
            "kind": "explicit-command",
            "command": "supervisor migrate",
            "state": control.get("state"),
        }
    elif case.handoff_action == "automatic-models":
        automatic_models = run_json(
            case.delegate,
            case.env,
            [*task_commands.private_prefix, "models", "--provider", "pi:json", "--cwd", str(case.workspace), "--json"],
            timeout=120,
            receipt=task_commands.receipts / "models-automatic.json",
            phase="automatic-inspection-worker",
            capture_root=case.capture_root,
        )
        if automatic_models.get("status") != "available" or [
            model.get("id") for model in automatic_models.get("models", [])
        ] != ["openai/gpt-4.1"]:
            raise RuntimeError("automatic model discovery did not complete")
        evidence["handoff_trigger"] = {
            "kind": "automatic-default-path",
            "command": "models",
            "status": automatic_models.get("status"),
            "model_ids": [model.get("id") for model in automatic_models.get("models", [])],
        }
    else:
        automatic_task_id = uuid.uuid4().hex
        case.attempted_task_ids.append(automatic_task_id)
        automatic_response = task_commands.dispatch_fixture(
            automatic_task_id, task_commands.private_prefix, "dispatch-automatic.json"
        )
        evidence["handoff_trigger"] = {
            "kind": "automatic-default-path",
            "command": "dispatch",
            "task_id": automatic_task_id,
            "task_record": automatic_response.get("task_record"),
            "admission": automatic_response.get("admission"),
        }
    return automatic_response


def _observe_handoff_transition(
    *, case,
    evidence,
    external_before_name,
    external_calls_before,
    external_pid,
    old_label,
    old_row_before,
    resources,
    supervisor_control,
    task_commands,
):
    external_calls_after = task_commands.external_client_calls()
    if external_calls_after != external_calls_before:
        raise RuntimeError("private handoff issued a control call to the external supervisor")
    external_after = pueue_snapshot(
        case.client,
        supervisor_control.external_config,
        case.env,
        phase="external-supervisor-observation",
        capture_root=case.capture_root,
    )
    external_after_name = task_commands.write_snapshot("pueue-external-after-migrate.json", external_after)
    old_row_after = task_row_by_label(external_after, old_label)
    require(
        old_row_after == old_row_before and pueue_state(old_row_after) == "Running",
        "external task or queue row changed during private handoff",
    )
    require(resources.external_process.poll() is None, "external daemon stopped during private handoff")
    require(
        supervisor_control.external_pid_path.read_text().strip() == str(external_pid),
        "external daemon PID changed during private handoff",
    )
    require(
        file_fingerprint(supervisor_control.external_config)
        == evidence["supervisors"]["external"]["config_fingerprint"],
        "external supervisor config changed during private handoff",
    )
    supervisor_status = run_json(
        case.delegate,
        case.env,
        [*task_commands.private_prefix, "supervisor", "status", "--json"],
        timeout=120,
        receipt=task_commands.receipts / "supervisor-status-after-handoff.json",
        phase="private-supervisor-observation",
        capture_root=case.capture_root,
    )
    journal = read_external_inspection_handoff_journal(
        case.state, case.private_config, supervisor_status=supervisor_status
    )
    evidence["journal"].update(
        {
            "handoff": journal["migration"],
            "private_successor": journal["successor"],
            "canonical_status": journal["canonical_status"],
        }
    )
    evidence["external_immutability"] = {
        "task_row_unchanged": True,
        "daemon_pid_unchanged": True,
        "config_unchanged": True,
        "external_control_calls_during_migration": 0,
        "snapshots": [external_before_name, external_after_name],
    }
    return journal


def _prove_handoff_owners(
    *, case,
    evidence,
    external_pid,
    journal,
    new_response,
    new_specs,
    new_task_id,
    old_meta_sha256,
    old_response,
    old_specs,
    private_pair,
    resources,
    task_commands,
):
    new_meta_path, new_meta_bytes, new_meta_sha256 = task_commands.task_meta(new_task_id)
    require_handoff_task_binding(new_meta_bytes, journal["successor"]["supervisor"])
    private_pid_path = case.private_config.parent / "run" / "pueue.pid"
    require(private_pid_path.is_file(), "private supervisor PID record is missing")
    private_pid = int(private_pid_path.read_text().strip())
    require(private_pid != external_pid, "private handoff reused the external daemon PID")
    evidence["supervisors"]["private"].update({"daemon_pid": private_pid, "pair": str(private_pair)})
    old_owner_proof = prove_lifetime_owner_cohort(old_specs, resources.old_cohort)
    new_owner_proof = prove_lifetime_owner_cohort(new_specs, resources.new_cohort)
    evidence["tasks"]["external"] = {
        "dispatch": default_stop_dispatch_evidence(old_response),
        "meta_sha256_before_migrate": old_meta_sha256,
        "meta_binding": "external",
        "owner_proof": old_owner_proof,
    }
    evidence["tasks"]["private"] = {
        "dispatch": default_stop_dispatch_evidence(new_response),
        "meta_sha256_before_collect": new_meta_sha256,
        "meta_binding": "private",
        "owner_proof": new_owner_proof,
    }
    return (new_meta_path, new_meta_sha256)


def _close_handoff_scopes(case, evidence, new_task_id, old_task_id, resources):
    resources.release_gate.release()
    resources.release_gate = None
    closures = await_managed_lifetime_closure(case.state, tuple(case.attempted_task_ids), timeout=45)
    for role, task_id in (("external", old_task_id), ("private", new_task_id)):
        closure = closures.get("tasks", {}).get(task_id)
        evidence["tasks"][role]["closure"] = external_handoff_closure_evidence(closure, task_id)
    old_cleanup = resources.old_cohort.remove_closed_fifos()
    new_cleanup = resources.new_cohort.remove_closed_fifos()
    evidence["tasks"]["external"]["fixture_lease_cleanup"] = old_cleanup
    evidence["tasks"]["private"]["fixture_lease_cleanup"] = new_cleanup
    resources.old_cohort.close()
    resources.old_cohort = None
    resources.new_cohort.close()
    resources.new_cohort = None
    require_empty_fixture_waiters(case.waiter_directory)
    evidence["cleanup"]["fixture_waiter_entries"] = 0
    case.run_receipt.update_counts(running_task_count=0, live_lease_count=0)


def _collect_handoff_tasks(
    *, case, new_meta_path, new_meta_sha256, new_task_id, old_meta_bytes, old_meta_path, old_task_id, task_commands
):
    old_collected = run_json(
        case.delegate,
        case.env,
        [*task_commands.external_prefix, "collect", old_task_id, "--watch", COLLECT_WATCH, "--json"],
        timeout=COLLECT_TIMEOUT_SECONDS,
        receipt=task_commands.receipts / "collect-external.json",
        phase="external-task-collection",
        capture_root=case.capture_root,
    )
    new_collected = run_json(
        case.delegate,
        case.env,
        [*task_commands.private_prefix, "collect", new_task_id, "--watch", COLLECT_WATCH, "--json"],
        timeout=COLLECT_TIMEOUT_SECONDS,
        receipt=task_commands.receipts / "collect-private.json",
        phase="private-task-collection",
        capture_root=case.capture_root,
    )
    for role, collected in (("external", old_collected), ("private", new_collected)):
        if collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError(role + " handoff task did not publish a committed outcome")
    require(old_meta_path.read_bytes() == old_meta_bytes, "external task metadata changed after handoff and collection")
    require(
        hashlib.sha256(new_meta_path.read_bytes()).hexdigest() == new_meta_sha256,
        "private task metadata changed after collection",
    )
    case.run_receipt.increment("collected_task_count", 2)


def _finalize_handoff_case(case, evidence, record_evidence, resources, supervisor_control):
    if resources.release_gate is not None:
        try:
            resources.release_gate.release()
        except BaseException as error:
            resources.cleanup_errors.append("fixture gate release failed: " + str(error))
    closure_proven = cleanup_external_handoff_cohorts(
        case.state,
        tuple(case.attempted_task_ids),
        case.waiter_directory,
        (resources.old_cohort, resources.new_cohort),
        evidence,
        resources.cleanup_errors,
        delegate_control_started=resources.delegate_control_started,
    )
    if closure_proven:
        try:
            if registered_process_handle(resources.external_processes, resources.external_process) is not None:
                supervisor_control.shutdown_external()
        except BaseException as error:
            resources.cleanup_errors.append("external supervisor cleanup failed: " + str(error))
    else:
        evidence["supervisors"]["external"]["daemon_shutdown"] = "retained-unproven-lifetime-closure"
        evidence["cleanup"]["external_daemon"] = "retained-unproven-lifetime-closure"
        resources.cleanup_errors.append("external supervisor retained because managed lifetime closure is unproven")
    try:
        pending = supervisor_control.record_external_process_registry()
        if pending:
            resources.cleanup_errors.append(
                "external supervisor process registry still has pending entries: " + repr(pending)
            )
    except BaseException as error:
        resources.cleanup_errors.append("external supervisor registry observation failed: " + str(error))
    if resources.cleanup_errors:
        evidence["cleanup"]["errors"] = resources.cleanup_errors
        record_evidence()
        if resources.primary_error is None:
            raise RuntimeError("external inspection handoff cleanup failed: " + "; ".join(resources.cleanup_errors))


def _start_external_task(case, evidence, resources, supervisor_control, task_commands):
    resources.external_process = resources.external_processes.start(
        "external-daemon",
        [str(case.pueued), "-c", str(supervisor_control.external_config)],
        case.external_root_token.directory,
        on_spawned=supervisor_control.remember_external_process,
    )
    external_pid = _prove_external_daemon_ready(evidence, resources, supervisor_control)
    resources.delegate_control_started = True
    legacy_request = _inspect_external_head(case, evidence, supervisor_control, task_commands)
    resources.release_gate = LockedReleaseGate.acquire(case.gate)
    old_task_id = uuid.uuid4().hex
    case.attempted_task_ids.append(old_task_id)
    old_response = task_commands.dispatch_fixture(old_task_id, task_commands.external_prefix, "dispatch-external.json")
    old_specs, resources.old_cohort = task_commands.live_cohort(
        old_response, case.client, supervisor_control.external_config, "external-supervisor-observation"
    )
    case.run_receipt.update_counts(running_task_count=1, live_lease_count=1)
    (
        old_meta_path,
        old_meta_bytes,
        old_meta_sha256,
        old_label,
        old_row_before,
        external_before_name,
        external_calls_before,
    ) = _observe_external_task(case, legacy_request, old_response, old_task_id, supervisor_control, task_commands)
    return (
        ObservedHandoffTask(
            task_id=old_task_id,
            response=old_response,
            specifications=old_specs,
            metadata_path=old_meta_path,
            metadata=old_meta_bytes,
            metadata_sha256=old_meta_sha256,
            label=old_label,
            row=old_row_before,
            snapshot_name=external_before_name,
            wrapper_calls=external_calls_before,
        ),
        external_pid,
    )


def _complete_handoff_success(case, evidence, old_task, record_evidence, supervisor_control):
    supervisor_control.shutdown_external()
    shutdown_private_supervisor(
        case.client,
        case.private_config,
        case.env,
        state=case.state,
        task_ids=tuple(case.attempted_task_ids),
        capture_root=case.capture_root,
    )
    evidence["supervisors"]["private"]["daemon_shutdown"] = "verified"
    evidence["cleanup"]["private_daemon"] = "verified"
    evidence["tasks"]["external"].update(
        {
            "meta_sha256_after_collect": hashlib.sha256(old_task.metadata_path.read_bytes()).hexdigest(),
            "collection": "external-binding",
        }
    )
    evidence["tasks"]["private"]["collection"] = "private-binding"
    record_evidence()
    case.run_receipt.stage("external-inspection-handoff-proved")
    return evidence
