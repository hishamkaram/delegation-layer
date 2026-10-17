"""CLI composition for the managed-workspace acceptance scenario."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import json
import os
from pathlib import Path
import sys
import tempfile
import uuid

import scripts.delegate_acceptance.private_cli.installation as private_cli_installation
from scripts.delegate_acceptance.private_cli.fingerprints import REPOSITORY
from scripts.delegate_acceptance.directories.operations import DirectoryOwner, OwnedDirectory
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.processes.observation import inherited_environment
from .context import (
    AcceptanceSetupFailure, WorkspaceContext,
)
from .evidence import ReceiptFinalizer
from .model import (
    AcceptanceBlocked, DEFAULT_CAPACITY_TASKS, DEFAULT_DISPATCH_WORKERS,
    DEFAULT_WORKSPACE_BUDGET, DRIVER_DESCRIPTION,
)
from .source import (
    ENTRYPOINT_PATH, _assert_source_untouched, _git_executable, _install_workspace_fixture,
    _require_executable,
    _prepare_source, _write_briefs, build_source_manifest,
)
from .lifecycle import _run_success
from .timeout import _run_timeout
from .capacity import _run_capacity


@dataclass(frozen=True)
class WorkspaceLayout:
    base: Path
    state: Path
    source: Path
    provider_bin: Path
    waiter_directory: Path
    response_directory: Path
    home_dir: Path
    install: Path
    libexec: Path
    gate_path: Path
    fixture_session_marker: Path
    lease_nonce: str


def _create_layout(
    owner: DirectoryOwner, root_token: OwnedDirectory, binaries: dict[str, Path],
) -> WorkspaceLayout:
    base = root_token.directory
    state = owner.create("managed workspace state", base / "state", base).directory
    source = owner.create("managed workspace source", base / "source", base).directory
    provider_bin = owner.create("provider fixture bin", base / "provider-bin", base).directory
    waiter_directory = owner.create("provider fixture waiters", base / "provider-waiters", base).directory
    response_directory = owner.create("task command responses", base / "responses", base).directory
    home_dir = owner.create("private CLI home", base / "home", base).directory
    package_root = owner.create("private package root", base / "package", base).directory
    install, _canonical_bin, libexec = private_cli_installation.install_package_layout(
        owner, binaries["delegate"], binaries["runner"], package_root, False,
    )
    private_cli_installation.install_fixture(binaries["pueue"], libexec / "pueue")
    private_cli_installation.install_fixture(binaries["pueued"], libexec / "pueued")
    gate_path = base / "fixture-gate"
    fixture_session_marker = base / "native-fixture-session.json"
    lease_nonce = uuid.uuid4().hex
    _install_workspace_fixture(
        provider_bin, gate_path, waiter_directory, lease_nonce, fixture_session_marker,
    )
    return WorkspaceLayout(
        base, state, source, provider_bin, waiter_directory, response_directory,
        home_dir, install, libexec, gate_path, fixture_session_marker, lease_nonce,
    )


def _workspace_environment(layout: WorkspaceLayout) -> dict[str, str]:
    environment = inherited_environment()
    for key in tuple(environment):
        if key.startswith("GIT_"):
            environment.pop(key, None)
    environment.update({
        "HOME": str(layout.home_dir),
        "PATH": os.pathsep.join((
            str(layout.provider_bin), str(layout.install), str(layout.libexec), "/usr/bin", "/bin",
        )),
        "XDG_CONFIG_HOME": str(layout.home_dir / ".config"),
        "XDG_DATA_HOME": str(layout.home_dir / ".local" / "share"),
        "LANG": "C.UTF-8",
        "LC_ALL": "C.UTF-8",
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_TERMINAL_PROMPT": "0",
    })
    return environment


def _make_context(
    args: argparse.Namespace,
    binaries: dict[str, Path],
) -> tuple[WorkspaceContext, dict[str, Path]]:
    owner = DirectoryOwner()
    home = Path.home().resolve(strict=True)
    try:
        base_path = Path(tempfile.mkdtemp(prefix=".dl-workspaces-", dir=str(home))).resolve(strict=True)
    except OSError as error:
        raise AcceptanceBlocked("private-acceptance-root-unavailable") from error
    root_token = owner.adopt_created("managed workspace acceptance root", base_path, home)
    try:
        return _make_context_unchecked(args, binaries, owner, root_token)
    except BaseException as error:
        raise AcceptanceSetupFailure(error, owner, root_token) from error


def _make_context_unchecked(
    args: argparse.Namespace,
    binaries: dict[str, Path],
    owner: DirectoryOwner,
    root_token: OwnedDirectory,
) -> tuple[WorkspaceContext, dict[str, Path]]:
    layout = _create_layout(owner, root_token, binaries)
    environment = _workspace_environment(layout)
    git_path = _git_executable()
    binaries_for_receipt = {
        "delegate": binaries["delegate"],
        "runner": binaries["runner"],
        "pueue": binaries["pueue"],
        "pueued": binaries["pueued"],
        "git": git_path,
        "fixture": layout.provider_bin / "pi",
    }
    processes = Processes(
        layout.base,
        environment=environment,
        capture_root=root_token,
        environment_keys_only=True,
    )
    context = WorkspaceContext(
        owner=owner,
        root_token=root_token,
        base=layout.base,
        state=layout.state,
        source=layout.source,
        brief_success=layout.base / "brief-success.md",
        brief_success_reuse=layout.base / "brief-success-reuse.md",
        brief_timeout=layout.base / "brief-timeout.md",
        brief_timeout_resume=layout.base / "brief-timeout-resume.md",
        brief_capacity=layout.base / "brief-capacity.md",
        delegate=layout.install / "delegate",
        pueue=layout.libexec / "pueue",
        pueued=layout.libexec / "pueued",
        private_config=layout.state / ".supervisor" / "pueue.yml",
        gate_path=layout.gate_path,
        waiter_directory=layout.waiter_directory,
        fixture_session_marker=layout.fixture_session_marker,
        response_directory=layout.response_directory,
        capture_root=root_token,
        processes=processes,
        environment=environment,
        source_baseline={},
        lease_nonce=layout.lease_nonce,
        workspace_budget=args.workspace_budget,
    )
    _prepare_source(context)
    _write_briefs(context)
    return context, binaries_for_receipt


def _run(args: argparse.Namespace, finalizer: ReceiptFinalizer, context: WorkspaceContext) -> tuple[str, str]:
    manifest = build_source_manifest(context)
    finalizer.set_source_manifest(manifest)
    if context.source_baseline:
        finalizer.set_details(source_baseline=context.source_baseline)
    # The context owns this append-only proof list. Keep its receipt view even
    # when a later scenario phase fails; the final verdict still stays failed.
    finalizer.set_details(native_proofs=context.native_proofs)
    context.container_parent_token = None
    container_parent = context.state.parent / ("." + context.state.name + ".workspaces")
    if container_parent.exists() or container_parent.is_symlink():
        raise AcceptanceBlocked("managed-workspace-container-parent-already-exists")
    if args.scenario == "lifecycle":
        finalizer.set_details(scenario="lifecycle")
        lifecycle = _run_success(context)
        finalizer.set_details(lifecycle=lifecycle)
        timeout = _run_timeout(context)
        source_final = _assert_source_untouched(context)
        finalizer.set_details(
            scenario="lifecycle",
            lifecycle=lifecycle,
            timeout=timeout,
            native_proofs=context.native_proofs,
            source_final=source_final,
            task_count=len(context.task_ids),
            workspace_count=len(context.workspace_ids),
            command_response_count=context.command_count,
        )
    else:
        capacity = _run_capacity(context, args.parallel_tasks, args.dispatch_workers)
        source_final = _assert_source_untouched(context)
        finalizer.set_details(
            scenario="capacity",
            capacity=capacity,
            native_proofs=context.native_proofs,
            source_final=source_final,
            task_count=len(context.task_ids),
            workspace_count=len(context.workspace_ids),
            command_response_count=context.command_count,
        )
    return "passed", "managed workspace fixture acceptance passed"


def _parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=DRIVER_DESCRIPTION)
    parser.add_argument("--delegate", type=Path, required=True)
    parser.add_argument("--runner", type=Path, required=True)
    parser.add_argument("--pueue", type=Path, required=True)
    parser.add_argument("--pueued", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--scenario", choices=("lifecycle", "capacity"), default="lifecycle")
    parser.add_argument("--parallel-tasks", type=int, default=DEFAULT_CAPACITY_TASKS)
    parser.add_argument("--dispatch-workers", type=int, default=DEFAULT_DISPATCH_WORKERS)
    parser.add_argument("--workspace-budget", default=DEFAULT_WORKSPACE_BUDGET)
    args = parser.parse_args(argv)
    if args.parallel_tasks <= 0:
        parser.error("--parallel-tasks must be positive")
    if args.dispatch_workers <= 0:
        parser.error("--dispatch-workers must be positive")
    if args.scenario == "lifecycle" and args.parallel_tasks != DEFAULT_CAPACITY_TASKS:
        parser.error("--parallel-tasks is only configurable for --scenario capacity")
    if args.scenario == "capacity":
        args.dispatch_workers = min(args.parallel_tasks, args.dispatch_workers)
    output = args.output.resolve()
    if output.exists() or output.is_symlink():
        parser.error("--output already exists: " + str(output))
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    args.output = output
    return args


def _new_finalizer(args: argparse.Namespace, invocation: list[str]) -> ReceiptFinalizer:
    return ReceiptFinalizer(
        args.output,
        [sys.argv[0], *invocation],
        {
            "acceptance-driver": ENTRYPOINT_PATH,
            "private-fixture": Path(REPOSITORY / "scripts" / "acceptance_private_cli.py"),
            "delegate": args.delegate,
            "runner": args.runner,
            "pueue": args.pueue,
            "pueued": args.pueued,
        },
    )


def _execute_scenario(
    args: argparse.Namespace, finalizer: ReceiptFinalizer,
) -> tuple[WorkspaceContext | None, AcceptanceSetupFailure | None, str, str]:
    context: WorkspaceContext | None = None
    setup_failure: AcceptanceSetupFailure | None = None
    verdict = "failed"
    reason = "acceptance did not reach a verdict"
    try:
        binaries = {
            "delegate": _require_executable(args.delegate, "delegate"),
            "runner": _require_executable(args.runner, "runner"),
            "pueue": _require_executable(args.pueue, "pueue"),
            "pueued": _require_executable(args.pueued, "pueued"),
        }
        context, receipt_binaries = _make_context(args, binaries)
        finalizer.binaries.update(receipt_binaries)
        finalizer.refresh_tool_fingerprints()
        verdict, reason = _run(args, finalizer, context)
    except AcceptanceSetupFailure as error:
        setup_failure = error
        if isinstance(error.cause, AcceptanceBlocked):
            verdict = "BLOCKED"
        else:
            verdict = "failed"
        reason = type(error.cause).__name__ + ": " + str(error.cause)
        finalizer.set_details(failure={"type": type(error.cause).__name__,
                                       "message": str(error.cause)})
    except AcceptanceBlocked as error:
        verdict = "BLOCKED"
        reason = str(error)
    except BaseException as error:
        verdict = "failed"
        reason = type(error).__name__ + ": " + str(error)
        finalizer.set_details(failure={"type": type(error).__name__, "message": str(error)})
    return context, setup_failure, verdict, reason


def _finalize_run(
    finalizer: ReceiptFinalizer, context: WorkspaceContext | None,
    setup_failure: AcceptanceSetupFailure | None, verdict: str, reason: str,
) -> tuple[str, str]:
    cleanup: dict[str, object] = {"status": "not-started"}
    if context is None:
        finalizer.finalize(verdict, reason, cleanup)
        if setup_failure is not None:
            try:
                cleanup = setup_failure.cleanup()
            except BaseException as error:
                verdict = "failed"
                reason = "setup-cleanup-error: " + type(error).__name__ + ": " + str(error)
                cleanup = {
                    "authorized": False,
                    "decision": "setup-cleanup-failed-evidence-retained",
                    "error": str(error),
                }
            finalizer.finalize(verdict, reason, cleanup)
        return verdict, reason
    finalizer.finalize(
        verdict,
        reason,
        {"status": "committed-before-cleanup", "retained_until": "compact-receipt"},
    )
    try:
        cleanup = context.cleanup(verdict)
    except BaseException as error:
        verdict = "failed"
        reason = "cleanup-or-closure-error: " + type(error).__name__ + ": " + str(error)
        cleanup = {
            "authorized": False,
            "decision": "cleanup-failed-evidence-retained",
            "error": str(error),
            "retained_owned_directories": [str(entry.directory)
                                            for entry in context.owner.transfer()],
        }
    finalizer.finalize(verdict, reason, cleanup)
    return verdict, reason


def main(argv: list[str] | None = None) -> int:
    invocation = sys.argv[1:] if argv is None else argv
    args = _parse_args(invocation)
    finalizer = _new_finalizer(args, invocation)
    context, setup_failure, verdict, reason = _execute_scenario(args, finalizer)
    verdict, reason = _finalize_run(finalizer, context, setup_failure, verdict, reason)
    summary = {"verdict": verdict, "reason": reason, "receipt": str(args.output)}
    sys.stdout.write(json.dumps(summary, sort_keys=True) + "\n")
    if verdict == "passed":
        return 0
    if verdict == "BLOCKED":
        return 3
    return 1
