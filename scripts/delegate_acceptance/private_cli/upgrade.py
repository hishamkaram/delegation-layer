"""Private CLI upgrade workflow."""

from scripts.delegate_acceptance.private_cli.configuration import COLLECT_TIMEOUT_SECONDS
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_WATCH
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from contextlib import ExitStack
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort
from scripts.delegate_acceptance.private_cli.leases import LockedReleaseGate
from scripts.delegate_acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.delegate_acceptance.private_cli.supervisor import await_managed_lifetime_closure
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.private_cli.capacity_records import capacity_lease_specifications
from contextlib import contextmanager
import fcntl
import hashlib
from scripts.delegate_acceptance.private_cli.installation import install_fixture
from scripts.delegate_acceptance.private_cli.wrappers import install_wrapper
import json
import os
from scripts.delegate_acceptance.private_cli.supervisor import private_daemon_process_gone
from scripts.delegate_acceptance.private_cli.supervisor import private_shutdown_endpoint_closed
from scripts.delegate_acceptance.private_cli.supervisor import private_shutdown_evidence
from scripts.delegate_acceptance.private_cli.capacity_ownership import prove_lifetime_owner_cohort
from scripts.delegate_acceptance.private_cli.supervisor import pueue_command
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.private_cli.queue_policy import pueue_state
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.private_cli.commands import run_json
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor
from scripts.delegate_acceptance.tasks.publication import snapshot
import time
import uuid
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.evidence.files import write_json


@contextmanager
def private_fixture_transition(state):
    """Serialize historical fixture seeds with the production transition owner."""
    supervisor = state / ".supervisor"
    with ExitStack() as owners:
        for name in ("transition.lock", "bootstrap.lock"):
            descriptor = os.open(supervisor / name, os.O_RDWR | os.O_NOFOLLOW)
            owners.callback(os.close, descriptor)

            def acquire():
                try:
                    fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    return True
                except BlockingIOError:
                    return False

            wait_until(acquire, timeout=30, description="historical seed lifecycle lock")
        yield supervisor


def seed_historical_private_paths(state, libexec, *, split):
    """Seed only historical mutable provenance under production lock ordering."""
    with private_fixture_transition(state) as supervisor:
        for name in ("migration.json", "bootstrap.pending", "identity.pending.json"):
            require(not (supervisor / name).exists(), "historical seed found pending " + name)
        record = read_json(supervisor / "supervisor.identity.json")
        binding = record["binding"]
        identity = read_json(supervisor / "daemon.identity.json")
        for member, field in (("pueue", "client"), ("pueued", "daemon")):
            require(
                hashlib.sha256((libexec / member).read_bytes()).hexdigest() == binding[field + "_sha256"],
                "historical seed changed executable bytes",
            )
            binding[field + "_executable"] = str(libexec / member)
        write_json(supervisor / "supervisor.identity.json", record, replace=True)
        if not split:
            identity["executable"] = binding["daemon_executable"]
            write_json(supervisor / "daemon.identity.json", identity, replace=True)
        return binding


def run_pending_package_upgrade(
    delegate,
    client,
    libexec,
    real_client,
    real_daemon,
    state,
    config,
    env,
    receipts,
    supervisor_cleanups,
    attempted_task_ids,
    *,
    capture_root,
):
    """Interrupt a real migration, remove its seeded legacy package, and resume."""
    base = capture_root.directory
    package_owner = DirectoryOwner()
    try:
        (
            pending_libexec,
            active,
            legacy_config,
            original_config,
            legacy_pid,
            legacy_endpoint,
            legacy_socket_identity,
            legacy_cleanup,
        ) = stage_pending_package(
            attempted_task_ids=attempted_task_ids, base=base, config=config, env=env, package_owner=package_owner,
            real_client=real_client, real_daemon=real_daemon, state=state, supervisor_cleanups=supervisor_cleanups
        )
        new_socket = interrupt_pending_migration(
            active=active,
            capture_root=capture_root,
            client=client,
            config=config,
            delegate=delegate,
            env=env,
            legacy_config=legacy_config,
            original_config=original_config,
            package_owner=package_owner,
            pending_libexec=pending_libexec,
            receipts=receipts,
            state=state,
        )
        install_wrapper(libexec / "pueue", real_client, base / "retarget-client", "retarget-install", "client")
        install_wrapper(libexec / "pueued", real_daemon, base / "retarget-daemon", "retarget-install", "daemon")
        migrated = run_json(
            delegate,
            env,
            ["--root", str(state), "supervisor", "migrate", "--json"],
            receipt=receipts / "upgrade-retarget.json",
            capture_root=capture_root,
            capture_response=True,
        )
        require(
            migrated.get("supervisor_control", {}).get("state") == "migrated",
            "native migration did not retarget its removed pre-shutdown successor",
        )
        await_pending_predecessor(
            capture_root, legacy_cleanup, legacy_endpoint, legacy_pid, legacy_socket_identity, supervisor_cleanups
        )
        pair = assert_state_supervisor_pair(state, libexec)
        published = read_json(state / ".supervisor" / "supervisor.identity.json")["binding"]
        require(
            published["daemon_executable"] == str(pair / "pueued") and published["endpoint"] == "unix:" + new_socket,
            "native retarget did not publish the actual ready generation",
        )
        return {
            "historical_state": "seeded-legacy-target-before-shutdown",
            "active_work_refused": True,
            "removed_successor_package": True,
            "committed_retarget": True,
            "predecessor_shutdown": "verified",
            "active_binding": published,
        }
    finally:
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
    real_daemon,
    supervisor_cleanups,
    *,
    capture_root,
):
    """Prove seeded historical path splits and removed-package recovery with real Pueue."""
    evidence = {"historical_state": "seeded-provenance", "tasks": []}
    run_receipt.add_details(supervisor_upgrade_recovery=evidence)

    dispatch, collect = _upgrade_task_operations(
        attempted_task_ids=attempted_task_ids, capture_root=capture_root, delegate=delegate, env=env,
        evidence=evidence, receipts=receipts, run_receipt=run_receipt, state=state, workspace=workspace
    )
    pair, legacy_binding, historical, historical_directory, history = prepare_upgrade_history(
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
        gate=gate,
        historical=historical,
        historical_directory=historical_directory,
        history=history,
        lease_nonce=lease_nonce,
        legacy_binding=legacy_binding,
        pair=pair,
        real_client=real_client,
        real_daemon=real_daemon,
        receipts=receipts,
        run_receipt=run_receipt,
        state=state,
        supervisor_cleanups=supervisor_cleanups,
        upgraded_libexec=upgraded_libexec,
        waiter_directory=waiter_directory,
    )


def _retarget_pending_successor(pending_libexec, state, supervisor):
    migration = read_json(supervisor / "migration.json")
    require(
        not any(
            migration.get(name, False)
            for name in ("legacy_shutdown_requested", "legacy_shutdown_dispatched", "legacy_stopped")
        ),
        "native migration crossed shutdown before its active-work refusal",
    )
    group_path = state / "inspection-group" / "migration.json"
    group = read_json(group_path)
    require(
        group["successor_sha256"] == hashlib.sha256(canonical_go_json(group["successor"])).hexdigest(),
        "native migration fixture did not preserve Go's exact proposal identity",
    )
    target = group["successor"]["supervisor"]
    for member, field in (("pueue", "client"), ("pueued", "daemon")):
        require(
            hashlib.sha256((pending_libexec / member).read_bytes()).hexdigest() == target[field + "_sha256"],
            "legacy pending target seed changed executable bytes",
        )
        target[field + "_executable"] = str(pending_libexec / member)
    migration["successor"]["executable"] = target["daemon_executable"]
    group["successor_sha256"] = hashlib.sha256(canonical_go_json(group["successor"])).hexdigest()
    write_json(supervisor / "migration.json", migration, replace=True)
    write_json(group_path, group, replace=True)


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
    *, attempted_task_ids, base, config, env, package_owner, real_client, real_daemon, state, supervisor_cleanups
):
    package = package_owner.create("pending successor package", base / "pending-package", base).directory
    pending_libexec = package_owner.create("pending successor libexec", package / "libexec", package).directory
    install_fixture(real_client, pending_libexec / "pueue")
    install_fixture(real_daemon, pending_libexec / "pueued")
    active = read_json(state / ".supervisor" / "supervisor.identity.json")["binding"]
    legacy_config = base / "upgrade-legacy-control.yml"
    original_config = config.read_text()
    legacy_config.write_text(original_config)
    legacy_config.chmod(0o600)
    legacy_pid, legacy_endpoint, legacy_socket_identity = private_shutdown_evidence(config)
    legacy_cleanup = {
        "client": real_client,
        "config": legacy_config,
        "env": env,
        "state": state,
        "task_ids": attempted_task_ids,
    }
    supervisor_cleanups.append(legacy_cleanup)
    return (
        pending_libexec,
        active,
        legacy_config,
        original_config,
        legacy_pid,
        legacy_endpoint,
        legacy_socket_identity,
        legacy_cleanup,
    )


def prove_split_provenance(
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
    seed_historical_private_paths(state, old_libexec, split=True)
    split = dispatch(delegate, "upgrade-path-split", brief)
    collect(split, "upgrade-path-split")
    require(identity_path.read_bytes() == identity_before, "path-only repair rewrote the existing daemon identity")
    require(
        read_json(state / ".supervisor" / "supervisor.identity.json")["binding"]["daemon_executable"]
        == str(pair / "pueued"),
        "path-only repair did not select retained launch provenance",
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
        _retarget_pending_successor(pending_libexec, state, supervisor)
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
    *, attempted_task_ids,
    brief,
    capture_root,
    client,
    collect,
    config,
    dispatch,
    env,
    old_delegate,
    old_libexec,
    receipts,
    remove_old_package,
    state,
    workspace,
):
    legacy_binding = seed_historical_private_paths(state, old_libexec, split=False)
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
    shutdown_private_supervisor(
        client, config, env, state=state, task_ids=tuple(attempted_task_ids), capture_root=capture_root
    )
    remove_old_package()
    require(not old_libexec.exists(), "old package was not removed")
    return legacy_binding, historical, historical_directory, history


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
    gate,
    historical,
    historical_directory,
    history,
    lease_nonce,
    legacy_binding,
    pair,
    real_client,
    real_daemon,
    receipts,
    run_receipt,
    state,
    supervisor_cleanups,
    upgraded_libexec,
    waiter_directory,
):
    _prove_recovered_capacity(
        brief=brief, collect=collect, delegate=delegate, dispatch=dispatch, evidence=evidence, gate=gate,
        lease_nonce=lease_nonce, run_receipt=run_receipt, state=state, waiter_directory=waiter_directory
    )
    collect(historical, "upgrade-historical-after-removal")
    require(snapshot(historical_directory) == history, "upgrade recovery mutated historical task evidence")
    active = read_json(state / ".supervisor" / "supervisor.identity.json")["binding"]
    require(
        active["client_executable"] == str(pair / "pueue") and active["daemon_executable"] == str(pair / "pueued"),
        "removed-package recovery did not publish actual retained launch paths",
    )
    require(
        active["client_sha256"] == legacy_binding["client_sha256"]
        and active["daemon_sha256"] == legacy_binding["daemon_sha256"],
        "removed-package recovery changed binary generation",
    )
    for name in ("migration.json", "bootstrap.pending", "identity.pending.json"):
        require(not (state / ".supervisor" / name).exists(), "recovery stranded " + name)
    assert_state_supervisor_pair(state, upgraded_libexec)
    evidence["pending_package_upgrade"] = run_pending_package_upgrade(
        delegate=delegate,
        client=client,
        libexec=upgraded_libexec,
        real_client=real_client,
        real_daemon=real_daemon,
        state=state,
        config=config,
        env=env,
        receipts=receipts,
        supervisor_cleanups=supervisor_cleanups,
        attempted_task_ids=attempted_task_ids,
        capture_root=capture_root,
    )
    collect(historical, "upgrade-history-after-retarget")
    require(snapshot(historical_directory) == history, "explicit retarget mutated historical task evidence")
    shutdown_private_supervisor(
        client, config, env, state=state, task_ids=tuple(attempted_task_ids), capture_root=capture_root
    )
    evidence.update(
        relocated_binding=active,
        active_binding=read_json(state / ".supervisor" / "supervisor.identity.json")["binding"],
        preserved_task_snapshot=history,
        recovered_running_tasks=2,
        supervisor_shutdown="verified",
    )


def await_pending_predecessor(
    capture_root, legacy_cleanup, legacy_endpoint, legacy_pid, legacy_socket_identity, supervisor_cleanups
):
    stop_deadline = time.monotonic() + 30

    def legacy_stopped():
        return private_daemon_process_gone(
            legacy_pid, stop_deadline, capture_root=capture_root
        ) and private_shutdown_endpoint_closed(legacy_endpoint, legacy_socket_identity, stop_deadline)

    wait_until(legacy_stopped, timeout=30, description="retargeted predecessor process and endpoint closure")
    supervisor_cleanups.remove(legacy_cleanup)


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
):
    pair = prove_split_provenance(
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
        attempted_task_ids=attempted_task_ids,
        brief=brief,
        capture_root=capture_root,
        client=client,
        collect=collect,
        config=config,
        dispatch=dispatch,
        env=env,
        old_delegate=old_delegate,
        old_libexec=old_libexec,
        receipts=receipts,
        remove_old_package=remove_old_package,
        state=state,
        workspace=workspace,
    )
    return pair, legacy_binding, historical, historical_directory, history
