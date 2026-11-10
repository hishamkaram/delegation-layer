"""Private CLI upgrade workflow."""

from contextlib import ExitStack
from scripts.acceptance.private_cli.configuration import COLLECT_TIMEOUT_SECONDS
from scripts.acceptance.private_cli.configuration import COLLECT_WATCH
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.private_cli.leases import LiveFIFOLeaseCohort
from scripts.acceptance.private_cli.leases import LockedReleaseGate
from scripts.acceptance.private_cli.leases import private_fixture_transition
from scripts.acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.acceptance.private_cli.supervisor import await_managed_lifetime_closure
from scripts.acceptance.evidence.encoding import canonical_go_json
from scripts.acceptance.private_cli.capacity.records import capacity_lease_specifications
from scripts.acceptance.processes.contracts import command_closure_unproven
import hashlib
from scripts.acceptance.private_cli.installation import install_package_layout
from scripts.acceptance.private_cli.installation import install_supervisor_pair
import json
import os
import stat
from scripts.acceptance.private_cli.supervisor import predecessor_closed
from scripts.acceptance.private_cli.supervisor import capture_predecessor_shutdown_target
from scripts.acceptance.private_cli.capacity.ownership import prove_lifetime_owner_cohort
from scripts.acceptance.private_cli.supervisor import pueue_command
from scripts.acceptance.private_cli.supervisor import pueue_snapshot
from scripts.acceptance.private_cli.queue_policy import pueue_state
from scripts.acceptance.evidence.files import read_json
from scripts.acceptance.directories.policy import require
from scripts.acceptance.private_cli.commands import run_json
from scripts.acceptance.private_cli.finalization import release_pending_predecessor
from scripts.acceptance.private_cli.supervisor import shutdown_private_supervisor
from scripts.acceptance.tasks.publication import snapshot
import time
import uuid
from scripts.acceptance.processes.observation import wait_until


def observe_installed_package(state, libexec):
    """Read the actual current generation without fabricating historical provenance."""
    with private_fixture_transition(state) as supervisor:
        for name in ("migration.json", "bootstrap.pending", "identity.pending.json"):
            require(not (supervisor / name).exists(), "package observation found pending " + name)
        binding = read_json(supervisor / "supervisor.identity.json")["binding"]
        identity = read_json(supervisor / "daemon.identity.json")
        require(binding.get("executable_identity") == "installed-path-v1"
                and identity.get("executable_identity") == "installed-path-v1",
                "package observation requires installed-path identities")
        require(not binding.get("client_sha256") and not binding.get("daemon_sha256")
                and not identity.get("sha256"), "current installation recorded an executable digest")
        assert_state_supervisor_pair(state, libexec)
        return binding


def _inspection_source_identity(binding: object) -> dict[str, str]:
    if not isinstance(binding, dict):
        raise RuntimeError("inspection migration source binding is missing")
    identity: dict[str, str] = {}
    # Match inspection.GroupGeneration's Go struct order, including its newline.
    for field in ("config_path", "config_digest", "resolved_config_sha256", "endpoint"):
        value = binding.get(field)
        if not isinstance(value, str) or not value:
            raise RuntimeError("inspection migration source has invalid " + field)
        identity[field] = value
    return identity


def _inspection_migration_directory(state, source):
    identity = _inspection_source_identity(source)
    directory = state / "inspection-group"
    request = read_json(directory / "request.json")
    require(isinstance(request, dict), "inspection root request is invalid")
    if _inspection_source_identity(request.get("supervisor")) == identity:
        return directory
    generation = hashlib.sha256(canonical_go_json(identity)).hexdigest()[:16]
    directory = state / ("inspection-group-" + generation)
    request = read_json(directory / "request.json")
    require(isinstance(request, dict) and _inspection_source_identity(request.get("supervisor")) == identity,
            "inspection generation request disagrees with migration source")
    return directory


def run_pending_package_upgrade(
    delegate, client, real_client, state, config, env, receipts,
    supervisor_cleanups, attempted_task_ids, *, capture_root, fixture_base, source_root,
):
    """Retarget a reversible removed proposal, retaining immutable group history."""
    package_owner = DirectoryOwner()
    failure = None
    try:
        pending_libexec, active, legacy_config, original_config, legacy_cleanup = stage_pending_package(
            attempted_task_ids=attempted_task_ids, base=fixture_base, config=config, env=env,
            package_owner=package_owner, real_client=real_client,
            state=state, supervisor_cleanups=supervisor_cleanups, source_root=source_root,
        )
        interrupt_pending_migration(
            active=active, capture_root=capture_root, client=client, config=config,
            delegate=pending_libexec.parent / "install" / "delegate", env=env,
            legacy_config=legacy_config, original_config=original_config, package_owner=package_owner,
            pending_libexec=pending_libexec, receipts=receipts, state=state,
        )
        private_journal = state / ".supervisor" / "migration.json"
        private_migration = read_json(private_journal)
        require(isinstance(private_migration, dict)
                and _inspection_source_identity(private_migration.get("legacy")) == _inspection_source_identity(active),
                "accepted migration source disagrees with the active generation")
        group_directory = _inspection_migration_directory(state, active)
        root_group = state / "inspection-group"
        accepted_paths = (group_directory / "request.json", group_directory / "migration.json",
                          root_group / "request.json", root_group / "migration.json")
        accepted = {path: path.read_bytes() for path in accepted_paths}
        completed = root_group / "migration-complete.json"
        completed_history = completed.read_bytes() if completed.exists() else None
        proposal = read_json(group_directory / "migration.json")
        rows_before = pueue_snapshot(client, legacy_config, env, capture_root=capture_root).get("tasks")
        require(isinstance(rows_before, dict), "retarget queue rows are unavailable")
        retargeted = run_json(
            delegate, env, ["--root", str(state), "supervisor", "migrate", "--json"], expected=(0,),
            receipt=receipts / "upgrade-reversible-retarget.json", capture_root=capture_root, capture_response=True,
        )
        control = retargeted.get("supervisor_control")
        require(isinstance(control, dict) and control.get("state") == "migrated"
                and control.get("next_action") == "retry_dispatch",
                "migration did not complete reversible retarget")
        require(all(path.read_bytes() == content for path, content in accepted.items())
                and (completed_history is None or completed.read_bytes() == completed_history),
                "retarget rewrote immutable inspection proposal or root history")
        current = observe_installed_package(state, client.parent)
        require(current["client_executable"] == active["client_executable"]
                and current["daemon_executable"] == active["daemon_executable"],
                "retarget did not select the active installed pair")
        require(current["config_path"] == str(config)
                and current["endpoint"] == "unix:" + str(config.parent / "run" / "upgrade.sock"),
                "retarget did not publish the upgrade endpoint")
        completion = read_json(group_directory / "migration-complete.json")
        require(isinstance(completion, dict) and isinstance(completion.get("successor"), dict),
                "retarget completion is invalid")
        successor_sha256 = hashlib.sha256(canonical_go_json(completion["successor"])).hexdigest()
        require(completion.get("schema_version") == proposal["schema_version"]
                and completion.get("root_id") == proposal["root_id"]
                and completion.get("legacy_sha256") == proposal["legacy_sha256"]
                and completion["successor"].get("supervisor") == current
                and completion.get("successor_sha256") == successor_sha256
                and successor_sha256 != proposal["successor_sha256"],
                "retarget completion does not match the current generation")
        require(read_json(group_directory / ("migration-" + successor_sha256[:32] + ".json")) == completion,
                "retarget completion lacks its exact selected proposal")
        require(pueue_snapshot(client, config, env, capture_root=capture_root).get("tasks") == rows_before,
                "retarget changed or replayed queue rows")
        await_pending_predecessor(capture_root, legacy_cleanup["predecessor"])
        release_pending_predecessor(legacy_cleanup, supervisor_cleanups)
        return {
            "successor": "available-current-installation", "active_work_refused": True,
            "prepared_successor_removed": True, "reversible_retarget_completed": True,
            "inspection_proposals_preserved": True, "queue_rows_preserved": True,
            "predecessor_shutdown": "verified", "active_binding": current,
        }
    except BaseException as error:
        failure = error
        raise
    finally:
        if failure is not None and command_closure_unproven(failure):
            failure.add_note("installation child closure is unproven; retained " + str(fixture_base / "pending-package"))
            package_owner.release()
        else:
            package_owner.rollback()


def run_supervisor_upgrade_recovery(
    old_delegate,
    delegate,
    client,
    old_libexec,
    upgraded_libexec,
    state,
    workspace,
    brief,
    gate,
    waiter_directory,
    config,
    env,
    receipts,
    lease_nonce,
    run_receipt,
    attempted_task_ids,
    remove_old_package,
    real_client,
    supervisor_cleanups,
    *,
    capture_root,
    fixture_base,
    source_root,
):
    """Prove warm installed control, package removal, and reversible successor retargeting."""
    evidence = {"historical_state": "original-installed-records", "tasks": []}
    run_receipt.add_details(supervisor_upgrade_recovery=evidence)

    dispatch, collect = _upgrade_task_operations(
        attempted_task_ids=attempted_task_ids, capture_root=capture_root, delegate=delegate, env=env,
        evidence=evidence, receipts=receipts, run_receipt=run_receipt, state=state, workspace=workspace
    )
    legacy_binding, historical, historical_directory, history = prepare_upgrade_history(
        attempted_task_ids=attempted_task_ids,
        brief=brief,
        capture_root=capture_root,
        client=client,
        collect=collect,
        config=config,
        delegate=delegate,
        dispatch=dispatch,
        env=env,
        old_delegate=old_delegate,
        old_libexec=old_libexec,
        receipts=receipts,
        remove_old_package=remove_old_package,
        state=state,
        workspace=workspace,
        gate=gate,
        waiter_directory=waiter_directory,
        lease_nonce=lease_nonce,
        evidence=evidence,
    )
    finish_upgrade_recovery(
        attempted_task_ids=attempted_task_ids,
        brief=brief,
        capture_root=capture_root,
        client=client,
        collect=collect,
        config=config,
        delegate=delegate,
        dispatch=dispatch,
        env=env,
        evidence=evidence,
        fixture_base=fixture_base,
        source_root=source_root,
        gate=gate,
        historical=historical,
        historical_directory=historical_directory,
        history=history,
        lease_nonce=lease_nonce,
        legacy_binding=legacy_binding,
        real_client=real_client,
        receipts=receipts,
        run_receipt=run_receipt,
        state=state,
        supervisor_cleanups=supervisor_cleanups,
        upgraded_libexec=upgraded_libexec,
        waiter_directory=waiter_directory,
    )


def require_pending_successor(pending_libexec, state, supervisor):
    """Validate the reversible installed proposal without editing its executable paths."""
    migration = read_json(supervisor / "migration.json")
    require(not any(migration.get(name, False) for name in
                    ("legacy_shutdown_requested", "legacy_shutdown_dispatched", "legacy_stopped")),
            "native migration crossed shutdown before its active-work refusal")
    require(migration.get("legacy_pid", 0) == 0 and migration.get("legacy_process_start", "") == "",
            "native migration captured shutdown provenance before its active-work refusal")
    group_directory = _inspection_migration_directory(state, migration.get("legacy"))
    group = read_json(group_directory / "migration.json")
    require(isinstance(group, dict), "accepted group proposal is invalid")
    require(group["successor_sha256"] == hashlib.sha256(canonical_go_json(group["successor"])).hexdigest(),
            "native migration fixture did not preserve Go's exact proposal identity")
    target = group["successor"]["supervisor"]
    require(target.get("executable_identity") == "installed-path-v1"
            and not target.get("client_sha256") and not target.get("daemon_sha256"),
            "accepted successor did not use installed-path identities")
    for member, field in (("pueue", "client"), ("pueued", "daemon")):
        selected = pending_libexec / member
        require(selected.is_file() and os.access(selected, os.X_OK)
                and target[field + "_executable"] == str(selected),
                "accepted successor did not select its actual installed " + member)
    require(migration["successor"]["executable"] == target["daemon_executable"],
            "accepted daemon identity disagrees with the installation proposal")


def _upgrade_task_operations(
    *, attempted_task_ids, capture_root, delegate, env, evidence, receipts, run_receipt, state, workspace
):
    def dispatch(binary, name, task_brief):
        task_id = uuid.uuid4().hex
        attempted_task_ids.append(task_id)
        response = run_json(
            binary,
            env,
            [
                "--root",
                str(state),
                "dispatch",
                "--provider",
                "pi:json",
                "--id",
                task_id,
                "--brief",
                str(task_brief),
                "--cwd",
                str(workspace),
                "--permission",
                "read-only",
                "--budget",
                "5m",
                "--json",
            ],
            receipt=receipts / (name + ".json"),
            capture_root=capture_root,
            capture_response=True,
        )
        require(
            response.get("task_record") == "created"
            and response.get("task_id") == task_id
            and response.get("admission") == "admitted"
            and not response.get("failure"),
            "upgrade scenario dispatch was not admitted: " + name,
        )
        run_receipt.increment("dispatch_completed_count")
        run_receipt.increment("admitted_task_count")
        evidence["tasks"].append(task_id)
        return response

    def collect(response, name):
        task_id = response["task_id"]
        run_json(
            delegate,
            env,
            ["--root", str(state), "status", task_id, "--json"],
            receipt=receipts / (name + "-status.json"),
            capture_root=capture_root,
        )
        result = run_json(
            delegate,
            env,
            ["--root", str(state), "collect", task_id, "--watch", COLLECT_WATCH, "--json"],
            timeout=COLLECT_TIMEOUT_SECONDS,
            receipt=receipts / (name + "-collect.json"),
            capture_root=capture_root,
        )
        require(
            result.get("outcome", {}).get("verdict") == "committed", "upgrade scenario task did not commit: " + name
        )
        await_managed_lifetime_closure(state, (task_id,), timeout=30)
        run_receipt.increment("collected_task_count")

    return dispatch, collect


def _prove_recovered_capacity(
    *, brief, collect, delegate, dispatch, evidence, gate, lease_nonce, run_receipt, state, waiter_directory
):
    waiting_brief = brief.with_name("upgrade-waiting.md")
    waiting_brief.write_text("DELEGATE_PRIVATE_UPGRADE_WAIT\n")
    release = LockedReleaseGate.acquire(gate)
    cohort = None
    try:
        responses = [dispatch(delegate, "upgrade-recovered-%d" % i, waiting_brief) for i in range(2)]

        def live_specifications():
            specifications = capacity_lease_specifications(state, responses, waiter_directory, lease_nonce)
            if len(specifications) == 2 and all(item["path"].exists() for item in specifications):
                return specifications
            return False

        specifications = wait_until(live_specifications, timeout=45, description="recovered live provider leases")
        cohort = LiveFIFOLeaseCohort.acquire(specifications, lease_nonce)
        cohort.assert_live()
        evidence["live_owner_proof"] = prove_lifetime_owner_cohort(specifications, cohort)
        run_receipt.update_counts(running_task_count=2, live_lease_count=2)
        release.release()
        for index, response in enumerate(responses):
            collect(response, "upgrade-recovered-%d" % index)
    finally:
        release.release()
        if cohort is not None:
            cohort.close()


def stage_pending_package(
    *, attempted_task_ids, base, config, env, package_owner, real_client, state, supervisor_cleanups,
    source_root,
):
    package = package_owner.create("pending successor package", base / "pending-package", base).directory
    _install, _canonical, pending_libexec = install_package_layout(package_owner, source_root, package, False)
    install_supervisor_pair(pending_libexec)
    active = read_json(state / ".supervisor" / "supervisor.identity.json")["binding"]
    legacy_config = base / "upgrade-legacy-control.yml"
    with private_fixture_transition(state):
        predecessor = capture_predecessor_shutdown_target(config, state, legacy_config)
        # Register before any fallible step, including lease exit, so the finalizer owns the handle.
        legacy_cleanup = {
            "client": real_client,
            "env": env,
            "task_ids": attempted_task_ids,
            "predecessor": predecessor,
        }
        supervisor_cleanups.append(legacy_cleanup)
    # The capture proved these exact bytes are the canonical UTF-8 config.
    original_config = predecessor.backup_content.decode("utf-8")
    return pending_libexec, active, legacy_config, original_config, legacy_cleanup


def prove_warm_installation(
    *, attempted_task_ids,
    brief,
    capture_root,
    client,
    collect,
    config,
    delegate,
    dispatch,
    env,
    old_delegate,
    old_libexec,
    state,
):
    first = dispatch(old_delegate, "upgrade-bootstrap", brief)
    collect(first, "upgrade-bootstrap")
    pair = assert_state_supervisor_pair(state, old_libexec)
    identity_path = state / ".supervisor" / "daemon.identity.json"
    identity_before = identity_path.read_bytes()
    binding_path = state / ".supervisor" / "supervisor.identity.json"
    binding_before = binding_path.read_bytes()
    warm = dispatch(delegate, "upgrade-warm-installation", brief)
    collect(warm, "upgrade-warm-installation")
    require(identity_path.read_bytes() == identity_before, "warm installed control rewrote the daemon identity")
    require(binding_path.read_bytes() == binding_before, "warm installed control rewrote the published binding")
    require(
        read_json(state / ".supervisor" / "supervisor.identity.json")["binding"]["daemon_executable"]
        == str(pair / "pueued"),
        "warm installed control changed the daemon installation",
    )
    shutdown_private_supervisor(
        client, config, env, state=state, task_ids=tuple(attempted_task_ids), capture_root=capture_root
    )
    return pair


def interrupt_pending_migration(
    *, active,
    capture_root,
    client,
    config,
    delegate,
    env,
    legacy_config,
    original_config,
    package_owner,
    pending_libexec,
    receipts,
    state,
):
    label = "native-upgrade-blocker-" + uuid.uuid4().hex
    numeric = pueue_command(
        client,
        config,
        env,
        ["add", "--stashed", "--print-task-id", "-l", label, "--", "/usr/bin/true"],
        capture_root=capture_root,
    ).strip()
    require(numeric.isdecimal(), "native upgrade blocker omitted its numeric ID")
    old_socket = active["endpoint"].removeprefix("unix:")
    new_socket = str(config.parent / "run" / "upgrade.sock")
    old_setting = "unix_socket_path: " + json.dumps(old_socket)
    require(original_config.count(old_setting) == 1, "native migration fixture could not locate its old socket")
    with private_fixture_transition(state):
        config.write_text(original_config.replace(old_setting, "unix_socket_path: " + json.dumps(new_socket)))
    blocked = run_json(
        delegate,
        env,
        ["--root", str(state), "supervisor", "migrate", "--json"],
        expected=(1,),
        receipt=receipts / "upgrade-active-work.json",
        capture_root=capture_root,
        capture_response=True,
    )
    require(
        blocked.get("supervisor_control", {}).get("state") == "blocked",
        "native migration did not preserve the stashed blocker",
    )
    with private_fixture_transition(state) as supervisor:
        require_pending_successor(pending_libexec, state, supervisor)
    row = pueue_snapshot(client, legacy_config, env, capture_root=capture_root)["tasks"].get(numeric)
    require(
        isinstance(row, dict) and row.get("label") == label and pueue_state(row) == "Stashed",
        "native upgrade blocker identity changed before removal",
    )
    pueue_command(client, legacy_config, env, ["remove", numeric], capture_root=capture_root)
    package_owner.rollback()
    require(not pending_libexec.exists(), "pending successor package was not removed")
    return new_socket


def record_historical_package(
    *, brief,
    capture_root,
    collect,
    dispatch,
    env,
    old_delegate,
    old_libexec,
    receipts,
    state,
    workspace,
):
    legacy_binding = observe_installed_package(state, old_libexec)
    run_json(
        old_delegate,
        env,
        ["--root", str(state), "models", "--provider", "pi:json", "--cwd", str(workspace), "--json"],
        receipt=receipts / "upgrade-historical-models.json",
        capture_root=capture_root,
    )
    historical = dispatch(old_delegate, "upgrade-historical-task", brief)
    collect(historical, "upgrade-historical-task")
    historical_directory = state / "tasks" / historical["task_id"]
    require(
        read_json(historical_directory / "meta.json")["supervisor_config"] == legacy_binding,
        "historical task did not retain installation-bound metadata",
    )
    history = snapshot(historical_directory)
    return legacy_binding, historical, historical_directory, history


def _warm_authority_records(state):
    """Read provenance, including the absence of optional completed history."""
    with private_fixture_transition(state) as supervisor:
        records = {}
        for name in ("supervisor.identity.json", "daemon.identity.json", "migration-history.json"):
            path = supervisor / name
            require(not path.is_symlink() and (not path.exists() or path.is_file()),
                    "warm authority record is not a regular file: " + name)
            records[name] = path.read_bytes() if path.exists() else None
        return records


def _upgrade_live_leases(state, responses, waiter_directory, lease_nonce):
    def ready():
        specifications = capacity_lease_specifications(state, responses, waiter_directory, lease_nonce)
        if len(specifications) == len(responses) and all(item["path"].exists() for item in specifications):
            return specifications
        return False

    return wait_until(ready, timeout=45, description="live package-upgrade provider leases")


def _require_one_warm_queue_identity(state, response, client, config, env, capture_root):
    task_id = response["task_id"]
    receipt = read_json(state / "tasks" / task_id / "supervisor.ref.json")
    require(isinstance(receipt, dict) and receipt.get("task_id") == task_id
            and isinstance(receipt.get("label"), str) and bool(receipt["label"])
            and type(receipt.get("numeric_task_id")) is int and receipt["numeric_task_id"] >= 0,
            "warm dispatch has no exact durable supervisor receipt")
    rows = pueue_snapshot(client, config, env, capture_root=capture_root).get("tasks")
    require(isinstance(rows, dict), "warm dispatch queue snapshot is unavailable")
    matches = [row for row in rows.values() if isinstance(row, dict) and row.get("label") == receipt.get("label")]
    require(len(matches) == 1 and type(matches[0].get("id")) is int
            and matches[0]["id"] == receipt["numeric_task_id"]
            and pueue_state(matches[0]) == "Running",
            "warm dispatch did not retain exactly one matching running queue identity")


def prove_live_package_removal(
    *, brief, capture_root, client, collect, config, delegate, dispatch, env, gate,
    historical_directory, history, lease_nonce, old_delegate, old_libexec, receipts,
    remove_old_package, state, waiter_directory,
):
    """Remove the old installation while its daemon and an admitted task stay live."""
    waiting_brief = brief.with_name("upgrade-live-waiting.md")
    waiting_brief.write_text("DELEGATE_PRIVATE_UPGRADE_WAIT\n")
    with ExitStack() as cleanup:
        release = LockedReleaseGate.acquire(gate)
        cleanup.callback(release.release)
        gate_identity = os.stat(gate, follow_symlinks=False)
        original = dispatch(old_delegate, "upgrade-live-original", waiting_brief)
        specifications = _upgrade_live_leases(state, [original], waiter_directory, lease_nonce)
        original_cohort = LiveFIFOLeaseCohort.acquire(specifications, lease_nonce)
        cleanup.callback(original_cohort.close)
        original_directory = state / "tasks" / original["task_id"]
        inputs = {name: (original_directory / name).read_bytes()
                  for name in ("task.json", "meta.json", "submit.json", "supervisor.ref.json")}
        authority = _warm_authority_records(state)
        remove_old_package()
        require(not old_libexec.exists() and not old_delegate.exists(), "live old package was not removed")
        original_cohort.assert_live()
        # Ordinary dispatch must perform the first recovery, without a prior
        # ensure-owner preparing the missing helper or runtime receipt for it.
        fresh = dispatch(delegate, "upgrade-live-fresh", waiting_brief)
        fresh_specifications = _upgrade_live_leases(state, [fresh], waiter_directory, lease_nonce)
        fresh_cohort = LiveFIFOLeaseCohort.acquire(fresh_specifications, lease_nonce)
        cleanup.callback(fresh_cohort.close)
        original_cohort.assert_live()
        fresh_cohort.assert_live()
        scheduling_path = state / ".supervisor" / "ordinary-scheduling.json"
        scheduling_before = scheduling_path.read_bytes()
        scheduling = read_json(scheduling_path)
        runtime = scheduling.get("supervisor") if isinstance(scheduling, dict) else None
        require(isinstance(runtime, dict) and runtime.get("client_executable") == str(client)
                and runtime.get("daemon_executable") == str(client.parent / "pueued"),
                "live dispatch did not select the current installed pair")
        prepared = run_json(
            delegate, env, ["--root", str(state), "supervisor", "ensure-owner", "--json"],
            receipt=receipts / "upgrade-live-ensure-owner.json", capture_root=capture_root, capture_response=True,
        )
        require(prepared.get("status") == "owner_ready", "removed-package owner did not become ready")
        require(scheduling_path.read_bytes() == scheduling_before,
                "already-ready ensure-owner changed the runtime scheduling receipt")
        original_cohort.assert_live()
        fresh_cohort.assert_live()
        _require_one_warm_queue_identity(state, fresh, client, config, env, capture_root)
        require(_warm_authority_records(state) == authority, "warm repair changed daemon provenance or history")
        require(snapshot(historical_directory) == history, "warm repair changed committed task history")
        release.release()
        collect(fresh, "upgrade-live-fresh")
        collect(original, "upgrade-live-original")
        require(all((original_directory / name).read_bytes() == data for name, data in inputs.items()),
                "warm repair changed the original active task's immutable inputs")
        require(_warm_authority_records(state) == authority, "warm task completion changed daemon provenance or history")
        require(snapshot(historical_directory) == history, "warm task completion changed committed task history")
    visible = os.stat(gate, follow_symlinks=False)
    require(stat.S_ISREG(visible.st_mode) and visible.st_uid == os.getuid()
            and (visible.st_dev, visible.st_ino) == (gate_identity.st_dev, gate_identity.st_ino),
            "warm release gate was replaced before owned cleanup")
    gate.unlink()
    return {"original_task": original["task_id"], "fresh_task": fresh["task_id"],
            "old_install_removed_while_active": True, "active_task_survived": True,
            "runtime_receipt_refreshed": True, "first_recovery_by_dispatch": True,
            "one_fresh_queue_identity": True,
            "published_provenance_and_history_preserved": True}


def finish_upgrade_recovery(
    *, attempted_task_ids,
    brief,
    capture_root,
    client,
    collect,
    config,
    delegate,
    dispatch,
    env,
    evidence,
    fixture_base,
    source_root,
    gate,
    historical,
    historical_directory,
    history,
    lease_nonce,
    legacy_binding,
    real_client,
    receipts,
    run_receipt,
    state,
    supervisor_cleanups,
    upgraded_libexec,
    waiter_directory,
):
    migrated = run_json(
        delegate, env, ["--root", str(state), "supervisor", "migrate", "--json"],
        timeout=120,
        receipt=receipts / "upgrade-supervisor-migrate.json",
        phase="supervisor-migration",
        capture_root=capture_root,
        capture_response=True,
    )
    control = migrated.get("supervisor_control")
    require(isinstance(control, dict) and control.get("state") == "migrated"
            and control.get("next_action") == "retry_dispatch",
            "upgrade migration did not authorize recovered capacity")
    _prove_recovered_capacity(
        brief=brief, collect=collect, delegate=delegate, dispatch=dispatch, evidence=evidence, gate=gate,
        lease_nonce=lease_nonce, run_receipt=run_receipt, state=state, waiter_directory=waiter_directory
    )
    collect(historical, "upgrade-historical-after-removal")
    require(snapshot(historical_directory) == history, "upgrade recovery mutated historical task evidence")
    active = read_json(state / ".supervisor" / "supervisor.identity.json")["binding"]
    require(
        active["client_executable"] == str(upgraded_libexec / "pueue")
        and active["daemon_executable"] == str(upgraded_libexec / "pueued"),
        "removed-package recovery did not select the current installation",
    )
    require(
        active.get("executable_identity") == "installed-path-v1"
        and not active.get("client_sha256") and not active.get("daemon_sha256"),
        "removed-package recovery did not publish installed-path identity",
    )
    for name in ("migration.json", "bootstrap.pending", "identity.pending.json"):
        require(not (state / ".supervisor" / name).exists(), "recovery stranded " + name)
    assert_state_supervisor_pair(state, upgraded_libexec)
    evidence["pending_package_upgrade"] = run_pending_package_upgrade(
        delegate=delegate,
        client=client,
        real_client=real_client,
        state=state,
        config=config,
        env=env,
        receipts=receipts,
        supervisor_cleanups=supervisor_cleanups,
        attempted_task_ids=attempted_task_ids,
        capture_root=capture_root,
        fixture_base=fixture_base,
        source_root=source_root,
    )
    require(snapshot(historical_directory) == history, "reversible retarget mutated historical task evidence")
    evidence.update(
        retired_binding=legacy_binding,
        active_binding=read_json(state / ".supervisor" / "supervisor.identity.json")["binding"],
        preserved_task_snapshot=history,
        recovered_running_tasks=2,
        predecessor_shutdown="verified",
    )


def await_pending_predecessor(capture_root, predecessor):
    stop_deadline = time.monotonic() + 30
    wait_until(
        lambda: predecessor_closed(predecessor, stop_deadline, capture_root=capture_root),
        timeout=30,
        description="captured predecessor process and endpoint closure",
    )


def prepare_upgrade_history(
    *, attempted_task_ids,
    brief,
    capture_root,
    client,
    collect,
    config,
    delegate,
    dispatch,
    env,
    old_delegate,
    old_libexec,
    receipts,
    remove_old_package,
    state,
    workspace,
    gate,
    waiter_directory,
    lease_nonce,
    evidence,
):
    prove_warm_installation(
        attempted_task_ids=attempted_task_ids,
        brief=brief,
        capture_root=capture_root,
        client=client,
        collect=collect,
        config=config,
        delegate=delegate,
        dispatch=dispatch,
        env=env,
        old_delegate=old_delegate,
        old_libexec=old_libexec,
        state=state,
    )
    legacy_binding, historical, historical_directory, history = record_historical_package(
        brief=brief,
        capture_root=capture_root,
        collect=collect,
        dispatch=dispatch,
        env=env,
        old_delegate=old_delegate,
        old_libexec=old_libexec,
        receipts=receipts,
        state=state,
        workspace=workspace,
    )
    evidence["live_package_removal"] = prove_live_package_removal(
        brief=brief, capture_root=capture_root, client=client, collect=collect, config=config,
        delegate=delegate, dispatch=dispatch, env=env, gate=gate, historical_directory=historical_directory,
        history=history, lease_nonce=lease_nonce, old_delegate=old_delegate, old_libexec=old_libexec,
        receipts=receipts, remove_old_package=remove_old_package, state=state, waiter_directory=waiter_directory,
    )
    # Retain the independent stopped-daemon migration case after the live proof.
    shutdown_private_supervisor(
        client, config, env, state=state, task_ids=tuple(attempted_task_ids), capture_root=capture_root
    )
    return legacy_binding, historical, historical_directory, history
