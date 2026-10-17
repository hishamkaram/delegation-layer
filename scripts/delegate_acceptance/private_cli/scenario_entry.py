"""Dedicated stop, external handoff, and historical recovery scenario entry."""

import os
import uuid

from scripts.delegate_acceptance.directories.operations import rollback_owned_directories


from scripts.delegate_acceptance.private_cli.wrapper_policy import compact_sequence_evidence
from scripts.delegate_acceptance.private_cli.fixtures import install_pi_fixture
from scripts.delegate_acceptance.private_cli.wrapper_policy import require_no_startup_control_calls
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_call_records


from scripts.delegate_acceptance.private_cli.stop import run_default_stop_acceptance
from scripts.delegate_acceptance.private_cli.handoff_cleanup import prepare_external_supervisor_directories
from scripts.delegate_acceptance.private_cli.handoff import run_external_inspection_handoff_acceptance
from scripts.delegate_acceptance.private_cli.upgrade import run_supervisor_upgrade_recovery


def run_recovery_scenario(lease_nonce, lifetime, response_directory, run_receipt, fixture):
    lifetime.env["PATH"] = str(fixture.provider_bin) + os.pathsep + "/usr/bin:/bin"

    def remove_old_package():
        rollback_owned_directories(lifetime.old_layout_owned)
        lifetime.old_layout_owned = ()

    run_receipt.phase("supervisor_upgrade_recovery")
    run_supervisor_upgrade_recovery(
        lifetime.old_install / "delegate",
        fixture.upgraded_install / "delegate",
        lifetime.upgraded_libexec / "pueue",
        lifetime.old_libexec,
        lifetime.upgraded_libexec,
        fixture.state,
        fixture.workspace,
        fixture.brief,
        lifetime.gate,
        fixture.waiter_directory,
        lifetime.private_config,
        lifetime.env,
        response_directory,
        lease_nonce,
        run_receipt,
        lifetime.attempted_task_ids,
        remove_old_package,
        fixture.pueue_binary,
        fixture.pueued_binary,
        lifetime.supervisor_cleanups,
        capture_root=lifetime.base_token,
    )
    run_receipt.stage("supervisor-upgrade-recovery-proved")


def run_stop_scenario(lease_nonce, lifetime, response_directory, run_receipt, fixture):
    run_receipt.phase("default_stop")
    default_stop = run_default_stop_acceptance(
        lifetime.old_install / "delegate",
        lifetime.old_libexec / "pueue",
        fixture.state,
        fixture.workspace,
        fixture.brief,
        lifetime.gate,
        fixture.waiter_directory,
        lifetime.private_config,
        lifetime.env,
        response_directory,
        fixture.provider_bin / "pi",
        lease_nonce,
        run_receipt,
        lifetime.attempted_task_ids,
        capture_root=lifetime.base_token,
    )
    wrapper_calls = wrapper_call_records(
        lifetime.base / "old-client-starts-calls", "client", "old"
    ) + wrapper_call_records(lifetime.base / "old-daemon-starts-calls", "daemon", "old")
    require_no_startup_control_calls(wrapper_calls)
    if not fixture.old_daemon_marker.is_file() or fixture.old_daemon_marker.read_text().splitlines() != ["started"]:
        raise RuntimeError("default-stop scenario did not start exactly one private daemon")
    if fixture.upgraded_daemon_marker.exists():
        raise RuntimeError("default-stop scenario started an unexpected upgraded daemon")
    run_receipt.add_details(
        default_stop=default_stop,
        pueue_wrapper_calls=compact_sequence_evidence(wrapper_calls),
        checks=[
            "a user cancellation was issued only after the exact fake provider lease was live",
            "a sub-second budget termination was issued only after the exact fake provider lease was live",
            "both tasks reached canonical worker Wait and empty native-scope closure",
            "the private supervisor shutdown was verified after both closures",
        ],
    )
    run_receipt.stage("default-stop-proved")


def run_handoff_scenarios(lease_nonce, lifetime, response_directory, run_receipt, fixture):
    handoff_cases = {}
    case_specs = (
        ("explicit-migrate", "explicit-migrate"),
        ("automatic-models", "automatic-models"),
        ("automatic-dispatch", "automatic-dispatch"),
    )
    for case_index, (case_label, handoff_action) in enumerate(case_specs):
        case_root, case_state, case_gate, case_waiters, case_private_config, case_nonce = prepare_handoff_case_paths(
            case_index, case_label, fixture, lease_nonce, lifetime
        )
        external_root_token = lifetime.ownership.create(
            "external inspection supervisor " + case_label, case_root / "external-supervisor", case_root
        )
        prepare_external_supervisor_directories(lifetime.ownership, external_root_token, case_label)
        external_process_root_token = lifetime.ownership.create(
            "external inspection supervisor process capture " + case_label,
            case_root / "external-supervisor-processes",
            case_root,
        )
        case_task_ids = []
        lifetime.managed_state_roots.append((case_state, case_task_ids))
        lifetime.handoff_cleanup_bindings.append(
            {
                "client": lifetime.old_libexec / "pueue",
                "config": case_private_config,
                "env": lifetime.env,
                "state": case_state,
                "task_ids": case_task_ids,
            }
        )
        case_responses = response_directory / ("external-handoff-" + case_label)
        case_responses.mkdir(mode=0o700)
        install_pi_fixture(fixture.provider_bin, case_gate, case_waiters, case_nonce)
        lifetime.private_config = case_private_config
        run_receipt.phase("external_inspection_" + case_label.replace("-", "_"))
        handoff = run_external_inspection_handoff_acceptance(
            lifetime.old_install / "delegate",
            lifetime.old_libexec / "pueue",
            lifetime.old_libexec / "pueued",
            case_state,
            fixture.workspace,
            case_gate,
            case_waiters,
            case_private_config,
            lifetime.env,
            case_responses,
            fixture.provider_bin / "pi",
            case_nonce,
            run_receipt,
            case_task_ids,
            external_root_token,
            external_process_root_token,
            capture_root=lifetime.base_token,
            handoff_action=handoff_action,
            case_label=case_label,
            handoff_cases=handoff_cases,
        )
        handoff_cases[case_label] = handoff
        lifetime.attempted_task_ids.extend(case_task_ids)
    run_receipt.add_details(
        external_inspection_handoff=handoff_cases,
        external_inspection_handoff_case_count=len(handoff_cases),
        external_inspection_handoff_task_count=len(lifetime.attempted_task_ids),
        checks=[
            "the empty Delegate state root created its inspection journal on the external supervisor",
            "an active external task remained bound and running during explicit private handoff",
            "private migration published the exact inspection journal successor without external control",
            "automatic model discovery handed off an external-only root without a private pre-bootstrap",
            "automatic dispatch handed off a separate external-only root without a private pre-bootstrap",
            "fresh tasks used the private supervisor after each handoff",
            "all immutable task bindings survived collection and canonical lifetime closure",
        ],
    )
    run_receipt.stage("external-inspection-handoff-all-cases-proved")


def prepare_handoff_case_paths(case_index, case_label, fixture, lease_nonce, lifetime):
    if case_index == 0:
        case_root = lifetime.base
        case_state = fixture.state
        case_gate = lifetime.gate
        case_waiters = fixture.waiter_directory
        case_private_config = lifetime.private_config
        case_nonce = lease_nonce
    else:
        case_root = lifetime.ownership.create(
            "external inspection case " + case_label, lifetime.base / ("external-handoff-" + case_label), lifetime.base
        ).directory
        case_state = lifetime.ownership.create(
            "external inspection state " + case_label, case_root / "state", case_root
        ).directory
        case_gate = case_root / "blocker-gate"
        case_waiters = lifetime.ownership.create(
            "external inspection waiters " + case_label, case_root / "provider-waiters", case_root
        ).directory
        case_private_config = case_state / ".supervisor" / "pueue.yml"
        case_nonce = uuid.uuid4().hex
    return case_root, case_state, case_gate, case_waiters, case_private_config, case_nonce
