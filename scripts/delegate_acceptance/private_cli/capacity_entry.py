"""Capacity scenario entry and final receipt composition."""

from scripts.delegate_acceptance.private_cli.leases import LockedReleaseGate
from scripts.delegate_acceptance.private_cli.wrapper_policy import compact_sequence_evidence
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.private_cli.wrapper_policy import require_no_startup_control_calls
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_call_records
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_command_counts


from scripts.delegate_acceptance.private_cli.capacity import verify_current_installation_upgrade


def run_capacity_scenario(
    *, args, dispatch_workers, lease_nonce, lifetime, response_directory, run_receipt, fixture, initial
):
    lifetime.release_gate = LockedReleaseGate.acquire(lifetime.gate)
    default_group = (
        pueue_snapshot(
            lifetime.old_libexec / "pueue",
            lifetime.private_config,
            lifetime.env,
            phase="supervisor-observation",
            capture_root=lifetime.base_token,
        )
        .get("groups", {})
        .get("default", {})
    )
    if default_group.get("parallel_tasks") != 0:
        raise RuntimeError("private ordinary supervisor did not enable unlimited parallelism")
    acceptance_evidence = {}
    upgraded_tasks, status_bytes = verify_current_installation_upgrade(
        delegate=fixture.upgraded_install / "delegate",
        state=fixture.state,
        workspace=fixture.workspace,
        brief=fixture.brief,
        release_gate=lifetime.release_gate,
        waiter_directory=fixture.waiter_directory,
        env=lifetime.env,
        old_layout=lifetime.base / "old",
        old_layout_owned=lifetime.old_layout_owned,
        client=initial.initial_pair_directory / "pueue",
        config=lifetime.private_config,
        parallel_tasks=args.parallel_tasks,
        dispatch_workers=dispatch_workers,
        response_directory=response_directory,
        evidence=acceptance_evidence,
        lease_nonce=lease_nonce,
        client_call_log=lifetime.base / "old-client-starts-calls",
        attempted_task_ids=lifetime.attempted_task_ids,
        capture_root=lifetime.base_token,
        run_receipt=run_receipt,
    )
    # The upgrade probe has already removed every old-layout token;
    # never present those consumed handles to the final rollback.
    lifetime.old_layout_owned = ()
    shutdown_private_supervisor(
        initial.initial_pair_directory / "pueue",
        lifetime.private_config,
        lifetime.env,
        state=fixture.state,
        task_ids=tuple(lifetime.attempted_task_ids),
        capture_root=lifetime.base_token,
    )
    record_capacity_scenario(
        acceptance_evidence=acceptance_evidence, args=args, dispatch_workers=dispatch_workers, initial=initial,
        lifetime=lifetime, run_receipt=run_receipt, status_bytes=status_bytes, upgraded_tasks=upgraded_tasks
    )


def record_capacity_scenario(
    *, acceptance_evidence, args, dispatch_workers, initial, lifetime, run_receipt, status_bytes, upgraded_tasks
):
    wrapper_calls = (
        wrapper_call_records(lifetime.base / "old-client-starts-calls", "client", "old")
        + wrapper_call_records(lifetime.base / "old-daemon-starts-calls", "daemon", "old")
        + wrapper_call_records(lifetime.base / "upgraded-client-starts-calls", "client", "upgraded")
        + wrapper_call_records(lifetime.base / "upgraded-daemon-starts-calls", "daemon", "upgraded")
    )
    require_no_startup_control_calls(wrapper_calls)
    run_receipt.add_details(
        **{
            "task_identities": compact_sequence_evidence([initial.task_id, *upgraded_tasks]),
            "concurrent_running_tasks": args.parallel_tasks,
            "dispatch_workers": dispatch_workers,
            "concurrent_status_bytes": status_bytes,
            "capacity_proof": acceptance_evidence["capacity_proof"],
            "dispatch_batch": acceptance_evidence["dispatch_batch"],
            "status_coalescing": acceptance_evidence["status_coalescing"],
            "capacity_proof_receipt": "capacity-proof.json",
            "dispatch_batch_receipt": "dispatch-batch.json",
            "status_coalescing_receipt": "status-coalescing.json",
            "pueue_client_commands": {
                "initial": wrapper_command_counts(lifetime.base / "old-client-starts-calls"),
                "upgraded": wrapper_command_counts(lifetime.base / "upgraded-client-starts-calls"),
            },
            "pueue_wrapper_calls": compact_sequence_evidence(wrapper_calls),
            "packaged_default_path": initial.packaged_evidence,
            "checks": [
                "the CLI completed dispatch/status/collect against an isolated private supervisor",
                "the adjacent release package completed default-path dispatch status collect and shutdown",
                "private ordinary admission enabled unlimited parallelism",
                (
                    "runtime-only root required no legacy inspection group journal"
                    if args.runtime_only
                    else "private model discovery completed without changing ordinary supervisor ownership"
                ),
                "task completion survived removal of the original package layout",
                "dispatch restarted the private supervisor after package removal",
                "all requested fixture tasks retained live provider leases and lifetime-owner locks with bound native-scope registrations while Running",
                "dispatch caller concurrency and burst metrics were recorded separately from capacity proof",
                "sixteen concurrent public status calls coalesced into fewer authoritative queue snapshots",
            ],
        }
    )
    run_receipt.stage("all-current-installation-proofs-complete")
