"""Private CLI packaged defaults workflow."""

from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_TIMEOUT_SECONDS
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_WATCH
from pathlib import Path
from scripts.delegate_acceptance.private_cli.configuration import TASK_BUDGET
from scripts.delegate_acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.delegate_acceptance.private_cli.fingerprints import file_fingerprint
from scripts.delegate_acceptance.private_cli.installation import install_adjacent_package
import json
import platform
from scripts.delegate_acceptance.private_cli.supervisor import private_shutdown_evidence
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.private_cli.queue_policy import pueue_state
from scripts.delegate_acceptance.private_cli.commands import run_json
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor
import shutil
import uuid


def packaged_default_commands(brief, workspace, task_id):
    """Construct the exact public commands used without any global path override."""
    return {
        "dispatch": [
            "dispatch",
            "--provider",
            "pi:json",
            "--brief",
            str(brief),
            "--cwd",
            str(workspace),
            "--id",
            task_id,
            "--permission",
            "read-only",
            "--budget",
            TASK_BUDGET,
            "--json",
        ],
        "status": ["status", task_id, "--json"],
        "collect": ["collect", task_id, "--watch", COLLECT_WATCH, "--json"],
    }


def isolated_default_state_path(home):
    if platform.system() == "Darwin":
        return Path(home) / "Library" / "Application Support" / "delegation-layer"
    if platform.system() == "Linux":
        return Path(home) / ".config" / "delegation-layer"
    raise AcceptanceBlocked("unsupported-native-platform")


def verify_packaged_default_path(
    owner,
    base,
    sources,
    provider_bin,
    brief,
    primary_client,
    primary_config,
    primary_env,
    response_directory,
    supervisor_cleanups,
    run_receipt=None,
    managed_state_roots=None,
    *,
    capture_root,
):
    """Prove an adjacent release bundle bootstraps through only public defaults."""
    package, workspace, default_state, config, env, cleanup = prepare_packaged_default(
        base, managed_state_roots, owner, provider_bin, run_receipt, sources, supervisor_cleanups
    )
    task_id, commands, pair, primary_metrics = _prove_packaged_bootstrap(
        brief,
        capture_root,
        cleanup,
        config,
        default_state,
        env,
        package,
        primary_client,
        primary_config,
        primary_env,
        response_directory,
        run_receipt,
        workspace,
    )
    observe_packaged_task(
        capture_root, commands, default_state, env, package, response_directory, run_receipt, sources, task_id
    )
    if run_receipt is not None:
        run_receipt.increment("collected_task_count")
        run_receipt.phase("packaged_default_shutdown")
    shutdown_private_supervisor(
        pair / "pueue", config, env, state=default_state, task_ids=(task_id,), capture_root=capture_root
    )
    supervisor_cleanups.remove(cleanup)
    pueue_snapshot(
        primary_client,
        primary_config,
        primary_env,
        primary_metrics,
        phase="packaged-coexistence-observation",
        capture_root=capture_root,
    )
    if run_receipt is not None:
        run_receipt.stage("packaged-default-path-proved")
    return _packaged_default_evidence(package)


def _prove_packaged_bootstrap(
    brief,
    capture_root,
    cleanup,
    config,
    default_state,
    env,
    package,
    primary_client,
    primary_config,
    primary_env,
    response_directory,
    run_receipt,
    workspace,
):
    primary_pid, primary_endpoint, _ = private_shutdown_evidence(primary_config)
    task_id = uuid.uuid4().hex
    cleanup["task_ids"].append(task_id)
    commands = packaged_default_commands(brief, workspace, task_id)
    if run_receipt is not None:
        run_receipt.phase("packaged_default_dispatch")
    dispatched = run_json(
        package / "delegate",
        env,
        commands["dispatch"],
        receipt=Path(response_directory) / "packaged-dispatch.json",
        phase="packaged-default-dispatch",
        capture_root=capture_root,
    )
    if (
        dispatched.get("task_id") != task_id
        or dispatched.get("task_record") != "created"
        or dispatched.get("admission") != "admitted"
    ):
        raise RuntimeError("packaged default dispatch was not durably admitted")
    if not config.is_file():
        raise RuntimeError("packaged default dispatch did not create its private config")
    pair = assert_state_supervisor_pair(default_state, package)
    packaged_pid, packaged_endpoint, _ = private_shutdown_evidence(config)
    if packaged_pid == primary_pid or packaged_endpoint == primary_endpoint:
        raise RuntimeError("packaged default root did not own a distinct private daemon")
    primary_metrics = {}
    pueue_snapshot(
        primary_client,
        primary_config,
        primary_env,
        primary_metrics,
        phase="packaged-coexistence-observation",
        capture_root=capture_root,
    )
    packaged_metrics = {}
    packaged_snapshot = pueue_snapshot(
        pair / "pueue",
        config,
        env,
        packaged_metrics,
        phase="packaged-coexistence-observation",
        capture_root=capture_root,
    )
    if run_receipt is not None:
        packaged_rows = packaged_snapshot.get("tasks", {})
        run_receipt.update_queue(
            task_count=len(packaged_rows),
            running_count=sum(1 for row in packaged_rows.values() if pueue_state(row) == "Running"),
            status_bytes=packaged_metrics.get("status_bytes"),
        )
    return task_id, commands, pair, primary_metrics


def _packaged_default_evidence(package):
    return {
        "layout": "adjacent-release-binaries",
        "binary_fingerprints": {
            name: file_fingerprint(package / name) for name in ("delegate", "delegate-run", "pueue", "pueued")
        },
        "global_path_overrides": [],
        "ambient_pueue_on_path": False,
        "fresh_bootstrap": True,
        "dispatch_status_collect": True,
        "clean_shutdown": True,
        "coexisted_with_test_owned_daemon": True,
        "proved": [
            "copied adjacent release binaries retained source identities",
            "isolated HOME resolved the default state root",
            "dispatch status and collect used no root runner or supervisor override",
            "fresh bootstrap coexisted with a distinct test-owned private daemon",
            "the packaged default daemon and endpoint stopped cleanly",
        ],
        "not_proved_here": [
            "package removal and stopped-daemon restart are proved by the current-installation scenario",
            "legacy supervisor migration is outside this default-path scenario",
        ],
    }


def prepare_packaged_default(base, managed_state_roots, owner, provider_bin, run_receipt, sources, supervisor_cleanups):
    if run_receipt is not None:
        run_receipt.phase("packaged_default_setup")
    package = install_adjacent_package(owner, sources, base / "packaged-default")
    private_home = owner.create("packaged default home", base / "h", base).directory
    workspace = owner.create("packaged default workspace", base / "packaged-workspace", base).directory
    default_state = isolated_default_state_path(private_home)
    config = default_state / ".supervisor" / "pueue.yml"
    env = {"HOME": str(private_home), "PATH": str(provider_bin), "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8"}
    if shutil.which("pueue", path=env["PATH"]) is not None:
        raise RuntimeError("packaged default PATH unexpectedly contains pueue")
    if default_state.exists() or config.exists():
        raise RuntimeError("packaged default state was not fresh")
    cleanup = {"client": package / "pueue", "config": config, "env": env, "state": default_state, "task_ids": []}
    supervisor_cleanups.append(cleanup)
    if managed_state_roots is not None:
        managed_state_roots.append((default_state, cleanup["task_ids"]))
    return package, workspace, default_state, config, env, cleanup


def observe_packaged_task(
    capture_root, commands, default_state, env, package, response_directory, run_receipt, sources, task_id
):
    if run_receipt is not None:
        run_receipt.phase("packaged_default_observation")
    observed = run_json(
        package / "delegate",
        env,
        commands["status"],
        receipt=Path(response_directory) / "packaged-status.json",
        phase="packaged-default-observation",
        capture_root=capture_root,
    )
    if observed.get("task_id") != task_id:
        raise RuntimeError("packaged default status observed a different task")
    collected = run_json(
        package / "delegate",
        env,
        commands["collect"],
        timeout=COLLECT_TIMEOUT_SECONDS,
        receipt=Path(response_directory) / "packaged-collect.json",
        phase="packaged-default-collection",
        capture_root=capture_root,
    )
    if collected.get("outcome", {}).get("verdict") != "committed":
        raise RuntimeError("packaged default collect did not publish success")
    meta = json.loads((default_state / "tasks" / task_id / "meta.json").read_text())
    runner_digest = file_fingerprint(sources["delegate-run"])["sha256"]
    expected_runner = str(default_state / ".supervisor" / ("delegate-run-" + runner_digest))
    if meta.get("runner_executable") != expected_runner:
        raise RuntimeError("packaged default dispatch did not resolve its adjacent runner")
