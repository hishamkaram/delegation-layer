"""Private CLI packaged defaults workflow.

The installed-default core runs one finite fixture task through an installed
delegate reached only by public defaults: dispatch, status, collect, the task's
own managed lifetime closure and a verified daemon and endpoint shutdown. The
adjacent-package scenario and the candidate install bootstrap probe both use it;
their callers own every directory, the supervisor ledger and the capture root.
"""

from dataclasses import dataclass
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_TIMEOUT_SECONDS
from scripts.delegate_acceptance.private_cli.configuration import COLLECT_WATCH
from pathlib import Path
from scripts.delegate_acceptance.private_cli.configuration import TASK_BUDGET
from scripts.delegate_acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.delegate_acceptance.private_cli.installation import install_adjacent_package
import json
import platform
from scripts.delegate_acceptance.private_cli.supervisor import await_managed_lifetime_closure
from scripts.delegate_acceptance.private_cli.supervisor import private_shutdown_evidence
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.private_cli.queue_policy import pueue_state
from scripts.delegate_acceptance.private_cli.commands import run_json
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_pending_private_supervisor
import shutil
import uuid


INSTALLED_BINARIES = ("delegate", "delegate-run", "pueue", "pueued")


@dataclass(frozen=True)
class InstalledDefaults:
    """One installation driven only through public defaults.

    ``delegate`` is the installed entry the caller resolved and executes;
    ``supervisor_directory`` holds the supervisor pair the installation ships;
    ``executables`` are the selected (name, path) of the four installed binaries.
    The isolated ``env`` HOME selects ``state``; ``workspace`` is the task cwd.
    """

    delegate: Path
    supervisor_directory: Path
    executables: tuple[tuple[str, Path], ...]
    env: dict[str, str]
    state: Path
    workspace: Path
    brief: Path
    response_directory: Path

    @property
    def config(self):
        return self.state / ".supervisor" / "pueue.yml"

    def executable(self, name):
        return dict(self.executables)[name]


@dataclass(frozen=True)
class AdmittedTask:
    """The one fixture task an installed default dispatch durably admitted."""

    root_id: str
    task_id: str
    commands: dict[str, list[str]]
    pair: Path


@dataclass(frozen=True)
class InstalledBootstrap:
    """The committed and closed fixture turn of one installed default, by observed identity.

    The paths are those of the executed delegate, the runner the task recorded and
    the supervisor pair its state root selected; ``worker_result``,
    ``termination_cause`` and ``scope_empty`` come from the task's own lifetime closure.
    The core returns this only after the daemon exited and its endpoint closed.
    """

    root_id: str
    task_id: str
    delegate: Path
    runner: Path
    client: Path
    daemon: Path
    outcome: str
    worker_result: str
    termination_cause: str
    scope_empty: bool


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


def register_installed_default(installed, supervisor_cleanups, managed_state_roots=None):
    """Refuse a non-fresh state root, then add its supervisor ledger entry before any dispatch."""
    if installed.state.exists() or installed.config.exists():
        raise RuntimeError("packaged default state was not fresh")
    cleanup = {
        "delegate": installed.delegate,
        "client": installed.supervisor_directory / "pueue",
        "config": installed.config,
        "env": installed.env,
        "state": installed.state,
        "task_ids": [],
    }
    supervisor_cleanups.append(cleanup)
    if managed_state_roots is not None:
        managed_state_roots.append((installed.state, cleanup["task_ids"]))
    return cleanup


def dispatch_installed_default(installed, cleanup, *, capture_root, run_receipt=None):
    """Dispatch one task; require durable admission and the expected state-root supervisor pair."""
    task_id = uuid.uuid4().hex
    cleanup["task_ids"].append(task_id)
    commands = packaged_default_commands(installed.brief, installed.workspace, task_id)
    if run_receipt is not None:
        run_receipt.phase("packaged_default_dispatch")
    dispatched = run_json(
        installed.delegate,
        installed.env,
        commands["dispatch"],
        receipt=Path(installed.response_directory) / "packaged-dispatch.json",
        phase="packaged-default-dispatch",
        capture_root=capture_root,
    )
    if (
        dispatched.get("task_id") != task_id
        or dispatched.get("task_record") != "created"
        or dispatched.get("admission") != "admitted"
    ):
        raise RuntimeError("packaged default dispatch was not durably admitted")
    root_id = dispatched.get("root_id")
    if not isinstance(root_id, str) or not root_id:
        raise RuntimeError("packaged default dispatch did not report its root identity")
    if not installed.config.is_file():
        raise RuntimeError("packaged default dispatch did not create its private config")
    pair = assert_state_supervisor_pair(installed.state, installed.supervisor_directory)
    for name in ("pueue", "pueued"):
        if (pair / name).resolve() != installed.executable(name).resolve():
            raise RuntimeError("packaged default state root selected an unexpected supervisor member " + name)
    return AdmittedTask(root_id=root_id, task_id=task_id, commands=commands, pair=pair)


def _require_task_identity(response, admitted, message):
    if response.get("task_id") != admitted.task_id or response.get("root_id") != admitted.root_id:
        raise RuntimeError(message)


def require_task_closure(closure, admitted):
    """Return the dispatched task's own closure; an empty or other-root closure proves nothing about it."""
    tasks = closure.get("tasks") if closure.get("root_id") == admitted.root_id else None
    task = tasks.get(admitted.task_id) if isinstance(tasks, dict) else None
    if not isinstance(task, dict):
        raise RuntimeError("managed lifetime closure does not include the dispatched task")
    return task


def collect_installed_default(installed, admitted, *, capture_root, run_receipt=None):
    """Observe and collect the admitted task, check its runner, then await its own closure.

    Returns the observed installed runner path after the collected verdict was committed.
    """
    if run_receipt is not None:
        run_receipt.phase("packaged_default_observation")
    observed = run_json(
        installed.delegate,
        installed.env,
        admitted.commands["status"],
        receipt=Path(installed.response_directory) / "packaged-status.json",
        phase="packaged-default-observation",
        capture_root=capture_root,
    )
    _require_task_identity(observed, admitted, "packaged default status observed a different task")
    collected = run_json(
        installed.delegate,
        installed.env,
        admitted.commands["collect"],
        timeout=COLLECT_TIMEOUT_SECONDS,
        receipt=Path(installed.response_directory) / "packaged-collect.json",
        phase="packaged-default-collection",
        capture_root=capture_root,
    )
    outcome = collected.get("outcome", {})
    if outcome.get("verdict") != "committed":
        raise RuntimeError("packaged default collect did not publish success")
    _require_task_identity(outcome, admitted, "packaged default collect published a different task")
    meta = json.loads((installed.state / "tasks" / admitted.task_id / "meta.json").read_text())
    expected_runner = installed.executable("delegate-run").resolve(strict=True)
    if meta.get("runner_executable") != str(expected_runner):
        raise RuntimeError("packaged default dispatch did not resolve its adjacent runner")
    require_task_closure(await_managed_lifetime_closure(installed.state, (admitted.task_id,)), admitted)
    return expected_runner


def shutdown_installed_default(installed, admitted, cleanup, supervisor_cleanups, *, capture_root, disposal=None):
    """Shut down through the selected pair, drop the ledger entry, and return the task's revalidated closure.

    A final-disposal caller passes its ``disposal`` owner, which then keeps the state root's lease held
    after the shutdown; without one the shutdown holds the transition lease only while it runs.
    """
    if "owner" not in cleanup:
        cleanup["client"] = admitted.pair / "pueue"
    closure = shutdown_pending_private_supervisor(
        cleanup, supervisor_cleanups, capture_root=capture_root, disposal=disposal)
    return require_task_closure(closure, admitted)


def prove_installed_default_bootstrap(installed, *, supervisor_cleanups, capture_root, disposal=None):
    """Run one finite fixture turn through ``installed`` and return its closed bootstrap facts.

    The ledger entry is registered before dispatch and stays in ``supervisor_cleanups``
    on any failure for the caller's cleanup; success removes it after verified shutdown.
    A caller that deletes the state root passes its ``disposal`` owner, which holds the
    root's lease from the verified shutdown on.
    """
    cleanup = register_installed_default(installed, supervisor_cleanups)
    admitted = dispatch_installed_default(installed, cleanup, capture_root=capture_root)
    runner = collect_installed_default(installed, admitted, capture_root=capture_root)
    task = shutdown_installed_default(
        installed, admitted, cleanup, supervisor_cleanups, capture_root=capture_root, disposal=disposal)
    wait = task.get("worker_wait") or {}
    termination = task.get("termination") or {}
    return InstalledBootstrap(
        root_id=admitted.root_id,
        task_id=admitted.task_id,
        delegate=installed.delegate.resolve(),
        runner=runner,
        client=(admitted.pair / "pueue").resolve(),
        daemon=(admitted.pair / "pueued").resolve(),
        outcome="committed",
        worker_result=wait.get("result", ""),
        termination_cause=termination.get("cause", ""),
        scope_empty=termination.get("scope_empty") is True,
    )


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
    disposal,
):
    """Prove an adjacent release bundle bootstraps through only public defaults."""
    if run_receipt is not None:
        run_receipt.phase("packaged_default_setup")
    package, installed = prepare_packaged_default(
        base=base, brief=brief, owner=owner, provider_bin=provider_bin,
        response_directory=response_directory, sources=sources,
    )
    cleanup = register_installed_default(installed, supervisor_cleanups, managed_state_roots)
    primary_pid, primary_endpoint, _ = private_shutdown_evidence(primary_config)
    admitted = dispatch_installed_default(installed, cleanup, capture_root=capture_root, run_receipt=run_receipt)
    packaged_pid, packaged_endpoint, _ = private_shutdown_evidence(installed.config)
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
    _observe_packaged_queue(installed, admitted, capture_root=capture_root, run_receipt=run_receipt)
    collect_installed_default(installed, admitted, capture_root=capture_root, run_receipt=run_receipt)
    if run_receipt is not None:
        run_receipt.increment("collected_task_count")
        run_receipt.phase("packaged_default_shutdown")
    shutdown_installed_default(installed, admitted, cleanup, supervisor_cleanups, capture_root=capture_root,
                               disposal=disposal)
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


def _observe_packaged_queue(installed, admitted, *, capture_root, run_receipt):
    packaged_metrics = {}
    packaged_snapshot = pueue_snapshot(
        admitted.pair / "pueue",
        installed.config,
        installed.env,
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


def _packaged_default_evidence(package):
    return {
        "layout": "adjacent-release-binaries",
        "installed_executables": {
            "schema_version": 1, "representation": "installed-paths",
            "paths": {name: str((package / name).resolve()) for name in INSTALLED_BINARIES},
        },
        "global_path_overrides": [],
        "ambient_pueue_on_path": False,
        "fresh_bootstrap": True,
        "dispatch_status_collect": True,
        "clean_shutdown": True,
        "coexisted_with_test_owned_daemon": True,
        "proved": [
            "selected adjacent installed executables in place",
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


def prepare_packaged_default(*, base, brief, owner, provider_bin, response_directory, sources):
    """Select the adjacent installation and create its isolated HOME and workspace; PATH holds no pueue."""
    package = install_adjacent_package(owner, sources, base / "packaged-default")
    private_home = owner.create("packaged default home", base / "h", base).directory
    workspace = owner.create("packaged default workspace", base / "packaged-workspace", base).directory
    env = {"HOME": str(private_home), "PATH": str(provider_bin), "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8"}
    if shutil.which("pueue", path=env["PATH"]) is not None:
        raise RuntimeError("packaged default PATH unexpectedly contains pueue")
    installed = InstalledDefaults(
        delegate=package / "delegate",
        supervisor_directory=package,
        executables=tuple((name, (package / name).resolve()) for name in INSTALLED_BINARIES),
        env=env,
        state=isolated_default_state_path(private_home),
        workspace=workspace,
        brief=brief,
        response_directory=Path(response_directory),
    )
    return package, installed
