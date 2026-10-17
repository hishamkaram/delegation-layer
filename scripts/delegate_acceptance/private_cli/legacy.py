"""Historical install-bound task and inspection upgrade workflow."""

from scripts.delegate_acceptance.private_cli.capture import start_legacy_inspection


import hashlib
import json
import os
import shlex
import uuid

from scripts.delegate_acceptance.directories.operations import rollback_owned_directories, remove_owned_directory


from scripts.delegate_acceptance.private_cli.configuration import COLLECT_TIMEOUT_SECONDS
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_WATCH
from scripts.delegate_acceptance.private_cli.configuration import TASK_BUDGET
from scripts.delegate_acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.delegate_acceptance.private_cli.wrapper_policy import compact_sequence_evidence
from scripts.delegate_acceptance.private_cli.capture import output_tail
from scripts.delegate_acceptance.private_cli.supervisor import pueue_command
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.private_cli.queue_policy import pueue_state
from scripts.delegate_acceptance.private_cli.capture import reap_captured_process
from scripts.delegate_acceptance.private_cli.wrapper_policy import require_no_startup_control_calls
from scripts.delegate_acceptance.private_cli.commands import run_json
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor
from scripts.delegate_acceptance.private_cli.queue_policy import task_row_by_label
from scripts.delegate_acceptance.private_cli.supervisor import wait_for_pueue_state
from scripts.delegate_acceptance.private_cli.supervisor import wait_for_queued_label_prefix
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_call_records


def run_legacy_scenario(
    inspection_runner_check,
    legacy_mode,
    lifetime,
    queued_runner_check,
    response_directory,
    run_receipt,
    fixture,
    initial,
):
    old_runner_path, previous_inspection_labels, queued_label, queued_task_id = queue_legacy_task(
        fixture, initial, legacy_mode, lifetime, response_directory
    )
    inspection_task_id, inspection_label = queue_legacy_inspection(initial, lifetime, previous_inspection_labels)
    state_runner_path = repair_legacy_queued_task(
        fixture, lifetime, old_runner_path, queued_label, queued_task_id, response_directory
    )
    validate_legacy_inspection(
        fixture, inspection_label, inspection_task_id, legacy_mode, lifetime, old_runner_path, state_runner_path
    )
    collect_legacy_workers(
        fixture,
        initial,
        legacy_mode,
        lifetime,
        old_runner_path,
        queued_label,
        queued_task_id,
        response_directory,
        state_runner_path,
    )
    complete_legacy_package_removal(fixture, initial, lifetime, response_directory)
    upgraded_task_id = dispatch_upgraded_task(fixture, initial, lifetime, response_directory, state_runner_path)
    restarted_task_id = restart_upgraded_supervisor(fixture, initial, lifetime, response_directory)
    shutdown_private_supervisor(
        lifetime.upgraded_libexec / "pueue",
        lifetime.private_config,
        lifetime.env,
        state=fixture.state,
        task_ids=tuple(lifetime.attempted_task_ids),
        capture_root=lifetime.base_token,
    )
    wrapper_calls = (
        wrapper_call_records(lifetime.base / "old-client-starts-calls", "client", "old")
        + wrapper_call_records(lifetime.base / "old-daemon-starts-calls", "daemon", "old")
        + wrapper_call_records(lifetime.base / "upgraded-client-starts-calls", "client", "upgraded")
        + wrapper_call_records(lifetime.base / "upgraded-daemon-starts-calls", "daemon", "upgraded")
    )
    require_no_startup_control_calls(wrapper_calls)
    run_receipt.add_details(
        **{
            "task_identities": compact_sequence_evidence(
                [initial.task_id, queued_task_id, upgraded_task_id, restarted_task_id]
            ),
            "inspection_identity": compact_sequence_evidence([inspection_task_id]),
            "pueue_wrapper_calls": compact_sequence_evidence(wrapper_calls),
            "packaged_default_path": initial.packaged_evidence,
            "checks": [
                "two installed CLI layouts resolved separate private Pueue pairs",
                "the initial CLI completed dispatch/status/collect against an isolated private supervisor",
                "the adjacent release package completed default-path dispatch status collect and shutdown",
                queued_runner_check,
                "the upgraded status repaired the queued runner before old package files were removed",
                "the upgraded CLI completed the migrated queued task after old package paths were removed",
                inspection_runner_check,
                "the upgraded CLI completed another bounded dispatch and collected its result",
                "adoption preserved old daemon launch provenance and did not start a second daemon",
                "a stopped endpoint restarted only the current packaged daemon and republished its identity",
            ],
        }
    )
    run_receipt.stage("all-migration-proofs-complete")


def queue_legacy_task(fixture, initial, legacy_mode, lifetime, response_directory):
    old_pueue = pause_legacy_queue(lifetime)
    queued_task_id = uuid.uuid4().hex
    lifetime.attempted_task_ids.append(queued_task_id)
    queued_dispatch = run_json(
        lifetime.old_install / "delegate",
        lifetime.env,
        [
            "--root",
            str(fixture.state),
            "dispatch",
            "--provider",
            "pi:json",
            "--brief",
            str(fixture.brief),
            "--cwd",
            str(fixture.workspace),
            "--id",
            queued_task_id,
            "--permission",
            "read-only",
            "--budget",
            TASK_BUDGET,
            "--json",
        ],
        receipt=response_directory / "dispatch-queued.json",
        phase="ordinary-dispatch",
        capture_root=lifetime.base_token,
    )
    if queued_dispatch.get("task_id") != queued_task_id or queued_dispatch.get("admission") != "admitted":
        raise RuntimeError("legacy CLI did not admit the task that should remain queued")
    queued_label = "delegate:" + initial.dispatch["root_id"] + ":" + queued_task_id
    queued_row = task_row_by_label(
        pueue_snapshot(
            old_pueue,
            lifetime.private_config,
            lifetime.env,
            phase="supervisor-observation",
            capture_root=lifetime.base_token,
        ),
        queued_label,
    )
    if queued_row is None or pueue_state(queued_row) != "Queued":
        raise RuntimeError("legacy task did not remain queued in the paused default group")
    old_queued_command = queued_row.get("original_command", "")
    old_runner_path = str(lifetime.old_install / "delegate-run")
    initial_runner_digest = hashlib.sha256(fixture.runner_binary.read_bytes()).hexdigest()
    stable_runner_path = str(fixture.state / ".supervisor" / ("delegate-run-" + initial_runner_digest))
    if legacy_mode and old_runner_path not in old_queued_command:
        raise RuntimeError("legacy queued command does not reference the removable install path")
    if not legacy_mode and stable_runner_path not in old_queued_command:
        raise RuntimeError("current CLI did not submit the durable state-root runner path")
    previous_inspection_labels = {
        row.get("label")
        for row in pueue_snapshot(
            lifetime.old_libexec / "pueue",
            lifetime.private_config,
            lifetime.env,
            phase="inspection-observation",
            capture_root=lifetime.base_token,
        )
        .get("tasks", {})
        .values()
        if row.get("group") == initial.inspection_group
        and (row.get("label") or "").startswith(initial.inspection_prefix)
    }
    return old_runner_path, previous_inspection_labels, queued_label, queued_task_id


def queue_legacy_inspection(initial, lifetime, previous_inspection_labels):
    # Queue an old-release inspection worker behind its private group
    # blocker. Start this after the ordinary dispatch has been admitted so
    # the two foreground CLI operations never race over supervisor calls.
    inspection_blocker = lifetime.base / "inspection-queue-blocker"
    inspection_blocker.write_text(
        "#!/bin/sh\nset -eu\nwhile [ ! -e " + shlex.quote(str(lifetime.gate)) + " ]; do sleep 0.05; done\n"
    )
    inspection_blocker.chmod(0o700)
    pueue_command(
        lifetime.old_libexec / "pueue",
        lifetime.private_config,
        lifetime.env,
        [
            "add",
            "--escape",
            "--group",
            initial.inspection_group,
            "--label",
            "inspection-upgrade-blocker",
            "--print-task-id",
            "--",
            str(inspection_blocker),
        ],
        phase="inspection-worker",
        capture_root=lifetime.base_token,
    )
    wait_for_pueue_state(
        lifetime.old_libexec / "pueue",
        lifetime.private_config,
        lifetime.env,
        "inspection-upgrade-blocker",
        "Running",
        phase="inspection-observation",
        capture_root=lifetime.base_token,
    )
    lifetime.inspection_process = start_legacy_inspection(
        lifetime.old_install / "delegate", initial.models_args, lifetime.env
    )
    inspection_row = wait_for_queued_label_prefix(
        lifetime.old_libexec / "pueue",
        lifetime.private_config,
        lifetime.env,
        initial.inspection_group,
        initial.inspection_prefix,
        previous_inspection_labels,
        lifetime.inspection_process,
        phase="inspection-observation",
        capture_root=lifetime.base_token,
    )
    inspection_label = inspection_row["label"]
    inspection_task_id = inspection_label[len(initial.inspection_prefix) :]
    if len(inspection_task_id) != 32 or any(char not in "0123456789abcdef" for char in inspection_task_id):
        raise RuntimeError("queued inspection label does not contain a valid task identity")
    return inspection_task_id, inspection_label


def repair_legacy_queued_task(fixture, lifetime, old_runner_path, queued_label, queued_task_id, response_directory):
    # Let the upgraded public CLI recover the queued task while the legacy
    # install is still present, so it can prove the saved runner is managed
    # and move the command to the new immutable state-root runner.
    lifetime.env["PATH"] = (
        str(fixture.provider_bin)
        + os.pathsep
        + str(fixture.upgraded_install)
        + os.pathsep
        + str(lifetime.upgraded_libexec)
        + os.pathsep
        + str(lifetime.old_install)
        + os.pathsep
        + str(lifetime.old_libexec)
        + os.pathsep
        + "/usr/bin:/bin"
    )
    repaired_status = run_json(
        fixture.upgraded_install / "delegate",
        lifetime.env,
        ["--root", str(fixture.state), "status", queued_task_id, "--json"],
        receipt=response_directory / "status-queued-after-upgrade.json",
        phase="managed-runner-observation",
        capture_root=lifetime.base_token,
    )
    if repaired_status.get("task_id") != queued_task_id:
        raise RuntimeError("upgraded status did not observe the queued legacy task")
    migrated_row = task_row_by_label(
        pueue_snapshot(
            lifetime.upgraded_libexec / "pueue",
            lifetime.private_config,
            lifetime.env,
            phase="supervisor-observation",
            capture_root=lifetime.base_token,
        ),
        queued_label,
    )
    if migrated_row is None or pueue_state(migrated_row) != "Queued":
        raise RuntimeError("upgraded CLI lost the legacy queued task during status recovery")
    migrated_command = migrated_row.get("original_command", "")
    upgraded_digest = hashlib.sha256(fixture.upgraded_runner_binary.read_bytes()).hexdigest()
    state_runner_path = str(fixture.state / ".supervisor" / ("delegate-run-" + upgraded_digest))
    if state_runner_path not in migrated_command:
        queued_meta = json.loads((fixture.state / "tasks" / queued_task_id / "meta.json").read_text())
        raise RuntimeError(
            "upgraded status did not migrate the queue to its immutable state-root runner: "
            "status=%r runner=%r ownership=%r expected=%r command=%r"
            % (
                repaired_status,
                queued_meta.get("runner_executable"),
                queued_meta.get("runner_ownership"),
                state_runner_path,
                migrated_command,
            )
        )
    if old_runner_path in migrated_command:
        raise RuntimeError("migrated queue command still references the old installation")
    return state_runner_path


def validate_legacy_inspection(
    fixture, inspection_label, inspection_task_id, legacy_mode, lifetime, old_runner_path, state_runner_path
):
    migrated_inspection = task_row_by_label(
        pueue_snapshot(
            lifetime.upgraded_libexec / "pueue",
            lifetime.private_config,
            lifetime.env,
            phase="inspection-observation",
            capture_root=lifetime.base_token,
        ),
        inspection_label,
    )
    if migrated_inspection is None or pueue_state(migrated_inspection) != "Queued":
        raise RuntimeError("upgraded status lost the queued inspection worker")
    migrated_inspection_command = migrated_inspection.get("original_command", "")
    inspection_request_path = fixture.state / "inspections" / inspection_task_id / "request.json"
    inspection_request = json.loads(inspection_request_path.read_text())
    upgrade_record_path = fixture.state / "inspections" / inspection_task_id / "worker-upgrade.json"
    if legacy_mode:
        if old_runner_path not in migrated_inspection_command or state_runner_path in migrated_inspection_command:
            raise RuntimeError(
                "upgraded status changed the runner for a legacy inspection with unknown ownership: "
                + migrated_inspection_command
            )
        if inspection_request.get("binding", {}).get("runner_ownership"):
            raise RuntimeError("legacy inspection unexpectedly recorded runner ownership")
        if upgrade_record_path.exists():
            raise RuntimeError("ambiguous legacy inspection was incorrectly authorized for worker migration")
    else:
        if (
            state_runner_path not in migrated_inspection_command
            or inspection_request.get("binding", {}).get("runner_ownership") != "managed"
        ):
            raise RuntimeError("managed inspection did not retain its recorded state-root runner")
    assert_state_supervisor_pair(fixture.state, lifetime.upgraded_libexec)


def collect_legacy_workers(
    fixture,
    initial,
    legacy_mode,
    lifetime,
    old_runner_path,
    queued_label,
    queued_task_id,
    response_directory,
    state_runner_path,
):
    # Keep the old Pueue client layout until the running models command
    # finishes polling. For legacy records, retain only its pinned worker;
    # for new records, the state-root copy is sufficient.
    if legacy_mode:
        old_install_token = next(
            entry for entry in lifetime.old_layout_owned if entry.directory == lifetime.old_install
        )
        remove_owned_directory(old_install_token)
        lifetime.old_layout_owned = tuple(
            entry for entry in lifetime.old_layout_owned if entry is not old_install_token
        )
        if (lifetime.old_install / "delegate").exists():
            raise RuntimeError("old CLI remained after simulated package cleanup")

    upgraded_pueue = lifetime.upgraded_libexec / "pueue"
    migrated_row = task_row_by_label(
        pueue_snapshot(
            upgraded_pueue,
            lifetime.private_config,
            lifetime.env,
            phase="supervisor-observation",
            capture_root=lifetime.base_token,
        ),
        queued_label,
    )
    if migrated_row is None or pueue_state(migrated_row) != "Queued":
        raise RuntimeError("upgraded CLI lost the legacy queued task")
    migrated_command = migrated_row.get("original_command", "")
    if state_runner_path not in migrated_command:
        raise RuntimeError("upgraded CLI did not retain the migrated content-addressed runner")
    if old_runner_path in migrated_command:
        raise RuntimeError("migrated queue command still references the removed install")

    pueue_command(
        upgraded_pueue,
        lifetime.private_config,
        lifetime.env,
        ["start", "--group", "default"],
        phase="supervisor-control",
        capture_root=lifetime.base_token,
    )
    lifetime.gate.touch(mode=0o600)
    queued_collected = run_json(
        fixture.upgraded_install / "delegate",
        lifetime.env,
        ["--root", str(fixture.state), "collect", queued_task_id, "--watch", COLLECT_WATCH, "--json"],
        timeout=COLLECT_TIMEOUT_SECONDS,
        receipt=response_directory / "collect-queued.json",
        phase="managed-runner-collection",
        capture_root=lifetime.base_token,
    )
    if queued_collected.get("outcome", {}).get("verdict") != "committed":
        raise RuntimeError("upgraded CLI did not run and collect the migrated legacy queued task")
    upgraded_status = run_json(
        fixture.upgraded_install / "delegate",
        lifetime.env,
        ["--root", str(fixture.state), "status", initial.task_id, "--json"],
        receipt=response_directory / "status-upgraded.json",
        phase="ordinary-observation",
        capture_root=lifetime.base_token,
    )
    if upgraded_status.get("task_id") != initial.task_id:
        raise RuntimeError("upgraded CLI could not recover the earlier task supervisor binding")
    reap_legacy_inspection(lifetime)


def complete_legacy_package_removal(fixture, initial, lifetime, response_directory):
    # Once the worker has finished, remove the remaining old package and
    # prove the upgraded CLI uses only its state-root supervisor pair.
    if (lifetime.base / "old").exists() or (lifetime.base / "old").is_symlink():
        rollback_owned_directories(lifetime.old_layout_owned)
        lifetime.old_layout_owned = ()
    if lifetime.old_install.exists() or lifetime.old_libexec.exists():
        raise RuntimeError("old package paths remained after simulated package cleanup")
    state_root_models = run_json(
        fixture.upgraded_install / "delegate",
        lifetime.env,
        initial.models_args,
        receipt=response_directory / "models-upgraded.json",
        phase="inspection-worker",
        capture_root=lifetime.base_token,
    )
    if state_root_models.get("status") != "available" or [
        model.get("id") for model in state_root_models.get("models", [])
    ] != ["openai/gpt-4.1"]:
        raise RuntimeError("upgraded CLI could not inspect models after old package removal")


def dispatch_upgraded_task(fixture, initial, lifetime, response_directory, state_runner_path):
    upgraded_task_id = uuid.uuid4().hex
    lifetime.attempted_task_ids.append(upgraded_task_id)
    lifetime.env["PATH"] = (
        str(fixture.provider_bin) + os.pathsep + str(fixture.upgraded_install) + os.pathsep + "/usr/bin:/bin"
    )
    upgraded_dispatch = run_json(
        fixture.upgraded_install / "delegate",
        lifetime.env,
        [
            "--root",
            str(fixture.state),
            "dispatch",
            "--provider",
            "pi:json",
            "--brief",
            str(fixture.brief),
            "--cwd",
            str(fixture.workspace),
            "--id",
            upgraded_task_id,
            "--permission",
            "read-only",
            "--budget",
            TASK_BUDGET,
            "--json",
        ],
        receipt=response_directory / "dispatch-upgraded.json",
        phase="ordinary-dispatch",
        capture_root=lifetime.base_token,
    )
    if upgraded_dispatch.get("task_id") != upgraded_task_id or upgraded_dispatch.get("admission") != "admitted":
        raise RuntimeError("upgraded CLI did not dispatch through the existing private supervisor")
    upgraded_snapshot = pueue_snapshot(
        lifetime.upgraded_libexec / "pueue",
        lifetime.private_config,
        lifetime.env,
        phase="supervisor-observation",
        capture_root=lifetime.base_token,
    )
    default_group = upgraded_snapshot.get("groups", {}).get("default", {})
    if default_group.get("parallel_tasks") != 0:
        raise RuntimeError("private ordinary supervisor did not enable unlimited parallelism")
    upgraded_meta = json.loads((fixture.state / "tasks" / upgraded_task_id / "meta.json").read_text())
    if upgraded_meta.get("runner_executable") != state_runner_path:
        raise RuntimeError("upgraded task metadata did not retain the state-root runner")
    upgraded_collected = run_json(
        fixture.upgraded_install / "delegate",
        lifetime.env,
        ["--root", str(fixture.state), "collect", upgraded_task_id, "--watch", COLLECT_WATCH, "--json"],
        timeout=COLLECT_TIMEOUT_SECONDS,
        receipt=response_directory / "collect-upgraded.json",
        phase="ordinary-collection",
        capture_root=lifetime.base_token,
    )
    if upgraded_collected.get("outcome", {}).get("verdict") != "committed":
        raise RuntimeError("upgraded CLI did not collect the successful second task")
    if fixture.upgraded_daemon_marker.exists():
        raise RuntimeError("compatible live supervisor was restarted during ordinary upgraded dispatch")
    if fixture.old_daemon_marker.read_text().splitlines() != ["started"]:
        raise RuntimeError("ordinary upgraded dispatch restarted the original private daemon")
    adopted_identity = json.loads(initial.identity_path.read_text())
    if (
        adopted_identity.get("executable") != str(initial.initial_daemon)
        or adopted_identity.get("sha256") != initial.old_digest
    ):
        raise RuntimeError("ordinary upgrade adoption rewrote the live daemon identity")
    return upgraded_task_id


def restart_upgraded_supervisor(fixture, initial, lifetime, response_directory):
    # A stopped endpoint is the only ordinary path that starts the current
    # bundled pair. This proves the successor is launched only after the
    # old daemon is actually unavailable.
    shutdown_private_supervisor(
        lifetime.upgraded_libexec / "pueue",
        lifetime.private_config,
        lifetime.env,
        state=fixture.state,
        task_ids=tuple(lifetime.attempted_task_ids),
        capture_root=lifetime.base_token,
    )
    restarted_task_id = uuid.uuid4().hex
    lifetime.attempted_task_ids.append(restarted_task_id)
    restarted_dispatch = run_json(
        fixture.upgraded_install / "delegate",
        lifetime.env,
        [
            "--root",
            str(fixture.state),
            "dispatch",
            "--provider",
            "pi:json",
            "--brief",
            str(fixture.brief),
            "--cwd",
            str(fixture.workspace),
            "--id",
            restarted_task_id,
            "--permission",
            "read-only",
            "--budget",
            TASK_BUDGET,
            "--json",
        ],
        receipt=response_directory / "dispatch-restarted.json",
        phase="restart-dispatch",
        capture_root=lifetime.base_token,
    )
    if restarted_dispatch.get("task_id") != restarted_task_id or restarted_dispatch.get("admission") != "admitted":
        raise RuntimeError("upgraded CLI did not restart the private supervisor after it stopped")
    restarted_collected = run_json(
        fixture.upgraded_install / "delegate",
        lifetime.env,
        ["--root", str(fixture.state), "collect", restarted_task_id, "--watch", COLLECT_WATCH, "--json"],
        timeout=COLLECT_TIMEOUT_SECONDS,
        receipt=response_directory / "collect-restarted.json",
        phase="restart-collection",
        capture_root=lifetime.base_token,
    )
    if restarted_collected.get("outcome", {}).get("verdict") != "committed":
        raise RuntimeError("upgraded CLI did not collect the task after restarting its supervisor")
    if not fixture.upgraded_daemon_marker.is_file() or fixture.upgraded_daemon_marker.read_text().splitlines() != [
        "started"
    ]:
        raise RuntimeError("stopped supervisor did not start exactly one upgraded daemon")
    upgraded_identity = json.loads(initial.identity_path.read_text())
    if (
        upgraded_identity.get("executable") != str(initial.upgraded_daemon)
        or upgraded_identity.get("sha256") != initial.upgraded_daemon_digest
    ):
        raise RuntimeError("restarted supervisor did not publish the upgraded daemon identity")
    return restarted_task_id


def pause_legacy_queue(lifetime):
    old_pueue = lifetime.old_libexec / "pueue"
    pueue_command(
        old_pueue,
        lifetime.private_config,
        lifetime.env,
        ["pause", "--group", "default"],
        phase="supervisor-control",
        capture_root=lifetime.base_token,
    )
    paused_snapshot = pueue_snapshot(
        old_pueue,
        lifetime.private_config,
        lifetime.env,
        phase="supervisor-observation",
        capture_root=lifetime.base_token,
    )
    if paused_snapshot.get("groups", {}).get("default", {}).get("status") != "Paused":
        raise RuntimeError("private default group did not pause for the migration fixture")
    return old_pueue


def reap_legacy_inspection(lifetime):
    if lifetime.inspection_process is None:
        raise RuntimeError("queued inspection process was not started")
    process = lifetime.inspection_process
    inspection_stdout, inspection_stderr, reaped, reap_error = reap_captured_process(process, 45)
    if reaped:
        lifetime.inspection_process = None
    if reap_error is not None:
        raise reap_error
    if process.returncode != 0:
        raise RuntimeError(
            "legacy CLI did not finish the queued inspection using its preserved pinned worker: "
            "exit=%d stdout=%r stderr=%r"
            % (process.returncode, output_tail(inspection_stdout), output_tail(inspection_stderr))
        )
    try:
        migrated_models = json.loads(inspection_stdout)
    except json.JSONDecodeError as error:
        raise RuntimeError("migrated models command returned invalid JSON: " + str(error)) from error
    if migrated_models.get("status") != "available" or [
        model.get("id") for model in migrated_models.get("models", [])
    ] != ["openai/gpt-4.1"]:
        raise RuntimeError("legacy pinned worker did not complete the queued inspection")
