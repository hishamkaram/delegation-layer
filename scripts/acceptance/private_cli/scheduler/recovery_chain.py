"""Native Linux/macOS A→B→C acceptance on one creator-owned temp root.

A is the pinned released 0.7.0 distribution. B and C are separately installed
builds of this checkout. No command targets the caller's default Delegate root.
"""

from dataclasses import replace
from functools import partial
import hashlib
import json
from pathlib import Path
import platform
import time
import uuid

from scripts.acceptance.directories.operations import DirectoryOwner, OwnedDirectory, rollback_owned_directories
from scripts.acceptance.evidence.validation import AcceptanceFailure, require
from scripts.acceptance.evidence.files import digest
from scripts.acceptance.lifetime.evidence import _validate_managed_ordinary_lifetime
from scripts.acceptance.processes.contracts import BoundedCommandResult
from scripts.acceptance.private_cli.configuration import SUPERVISOR_UPGRADE_RECOVERY_SCENARIO
from scripts.acceptance.private_cli.errors import AcceptanceBlocked
from scripts.acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.acceptance.private_cli.fixture_setup import prepare_fixture
from scripts.acceptance.private_cli.installation import install_package_layout, install_supervisor_pair
from scripts.acceptance.private_cli.leases import private_fixture_transition
from scripts.acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.acceptance.private_cli.scheduler.acceptance import finish_case, output_directory
from scripts.acceptance.private_cli.scheduler.cases import SchedulerCase
from scripts.acceptance.private_cli.scheduler.historical_state import seed, require_preserved
from scripts.acceptance.private_cli.scheduler.host import SchedulerHost
from scripts.acceptance.private_cli.scheduler.occurrences import json_object
from scripts.acceptance.private_cli.scheduler.releases import RECOVERY_RELEASE, ReleaseInstaller
from scripts.acceptance.private_cli.scheduler.upgrade import (
    UpgradeArguments, collect_schedule, create_definition, delete_definitions,
    entry_count, install_entry_counter, parse_arguments as upgrade_arguments,
)
from scripts.acceptance.private_cli.scheduler.upgrade_snapshot import snapshot, require_preserved as require_scheduler_preserved
from scripts.acceptance.private_cli.supervisor import (
    await_private_owner_absence, private_shutdown_evidence, selected_owner_subject,
    shutdown_observed_supervisor, register_terminal_private_root,
)
from scripts.acceptance.private_cli.wrappers import install_wrapper
from scripts.acceptance.processes.operations import run_bounded_command
from scripts.acceptance.processes.observation import inherited_environment, wait_until


B_VERSION = "0.7.1-upgrade-b"
C_VERSION = "0.7.1-upgrade-c"


def retain_fixture_process(evidence: Path, result: BoundedCommandResult) -> None:
    """Persist bounded output emitted only by the repository-owned fixture."""
    evidence.write_text(json.dumps({
        "exit_code": result.exit_code, "stdout_bytes": len(result.output), "stderr_bytes": len(result.stderr),
        "stdout_sha256": hashlib.sha256(result.output).hexdigest(),
        "stderr_sha256": hashlib.sha256(result.stderr).hexdigest(),
        "stdout": result.output.decode("utf-8", errors="replace"),
        "stderr": result.stderr.decode("utf-8", errors="replace"),
    }, indent=2) + "\n")


def observe_chain_task_closure(case: SchedulerCase, state: Path, task_ids: tuple[str, ...]) -> dict[str, object]:
    """Observe this chain's exact task, without authorizing root disposal."""
    require(state == case.fixture.state and len(task_ids) == 1 and task_ids[0] in case.lifetime.attempted_task_ids,
            "chain task closure observer received an unowned root or task")
    if case.lifetime.base is None:
        raise RuntimeError("task closure observer lost its owned fixture")
    task_id = task_ids[0]
    case.command_count += 1
    evidence = case.responses / f"{case.command_count:03d}-task-closure-{task_id}.json"
    result = run_bounded_command([
        str(case.lifetime.base / "native-upgrade-fixture"), "--root", str(state), "--observe-task-closure", task_id,
    ], case.lifetime.env, cwd=case.fixture.workspace, timeout=min(15, case.remaining()),
        capture=True, capture_stderr=True, max_output=64 << 10)
    retain_fixture_process(evidence, result)
    if result.exit_code != 0:
        raise RuntimeError("canonical task closure observer failed; inspect " + str(evidence))
    observed = json_object(json.loads(result.output))
    require(observed.get("root_id") == case.root_id and observed.get("task_id") == task_id,
            "canonical task closure observer changed its subject")
    if observed.get("closed") is not True:
        if observed.get("closed") is False and observed.get("pending_reason") in ("owner_busy", "closure_unproven"):
            raise AcceptanceFailure("exact task closure remains pending: " + str(observed["pending_reason"]))
        raise RuntimeError("canonical task closure observation is incomplete or faulted; inspect " + str(evidence))
    directory = state / "tasks" / task_id
    require(observed.get("kind") == "registered-native"
            and observed.get("reference_sha256") == digest(directory / "lifetime/worker-wait.json"),
            "future task lacks its canonical registered-native closure reference")
    closure = _validate_managed_ordinary_lifetime(
        directory, case.root_id, task_id, digest(directory / "task.json"), digest(directory / "meta.json"))
    return {"root_id": case.root_id, "tasks": {task_id: closure}}


def advance(case: SchedulerCase, name: str, operation: str | None = None, *, explicit: bool = False) -> str:
    """Repeat only a typed acknowledgement, retaining one operation identity."""
    selected: list[str] = [] if operation is None else [operation]
    attempts = 0

    def completed():
        nonlocal attempts
        attempts += 1
        arguments = ["supervisor", "ensure-owner"]
        if explicit:
            if operation is None:
                raise RuntimeError("explicit completion requires the prepared operation")
            arguments.extend(["--complete-upgrade", operation])
        response = case.command(name + "-" + str(attempts), arguments, (0, 3))
        require(not response.get("error"), "acknowledged upgrade returned an operational error")
        control = json_object(response.get("supervisor_control"))
        report = control.get("upgrade")
        if report is None:
            require(control.get("next_action") == "repeat_supervisor_ensure_owner",
                    "upgrade preflight did not acknowledge progress")
            return False
        upgrade = json_object(report)
        actual = upgrade.get("operation_id")
        if not isinstance(actual, str):
            raise RuntimeError("upgrade omitted a string operation identity")
        require(actual.startswith("upgrade-v1:") and len(actual) == 43,
                "upgrade omitted its durable operation identity")
        require(not selected or selected[0] == actual, "interrupted upgrade replaced its operation")
        selected[:] = [actual]
        if upgrade.get("completed") is True:
            require(control.get("state") == "migrated" and control.get("next_action") == "retry_dispatch",
                    "completed upgrade did not establish readiness")
            return True
        require(control.get("next_action") == "repeat_supervisor_ensure_owner",
                "upgrade progress did not request a repeat")
        return False

    wait_until(completed, timeout=min(150, case.remaining()), description=name + " acknowledged handover")
    if not selected:
        raise RuntimeError("completed upgrade lost its operation")
    return selected[0]


def native_seed(case: SchedulerCase, source: Path, owner: DirectoryOwner,
                tokens: list[OwnedDirectory]) -> tuple[str | None, str, dict[str, object]]:
    base = case.lifetime.base
    if base is None:
        raise RuntimeError("native seed lost fixture ownership")
    binary = base / "native-upgrade-fixture"
    result = run_bounded_command(
        ["go", "build", "-buildvcs=false", "-o", str(binary), "./internal/testutil/upgradefixture"],
        inherited_environment(), cwd=source, timeout=900, capture=True, capture_stderr=True, max_output=1 << 20,
    )
    require(result.exit_code == 0, "native cooperative fixture build failed")
    operation = uuid.uuid4().hex
    admission = uuid.uuid4().hex
    arguments = [str(binary), "--root", str(case.fixture.state), "--workspace", str(case.fixture.workspace),
                 "--id", operation, "--admission-id", admission]
    cooperative = platform.system() == "Darwin"
    if not cooperative:
        arguments.append("--admission-only")
    # Register before the possibly-sent mutation; normal cancellation and the
    # unchanged finalizer own this real admission record even after a fault.
    case.lifetime.attempted_task_ids.append(admission)
    result = run_bounded_command(
        arguments,
        case.lifetime.env, cwd=case.fixture.workspace, timeout=75,
        capture=True, capture_stderr=True, max_output=64 << 10,
    )
    evidence = case.responses / "native-upgrade-fixture.json"
    # Only this repository-owned fixture process can emit these bounded bytes.
    # Never retain inherited environments or unrestricted provider output here.
    retain_fixture_process(evidence, result)
    require(result.exit_code == 0, "native cooperative fixture process failed; inspect " + str(evidence))
    proof = json_object(json.loads(result.output))
    require(proof == {"operation_id": operation if cooperative else "", "admission_id": admission, "root_id": case.root_id,
                      "worker_observed_before_wait": cooperative, "worker_waited": cooperative, "native_scope_empty": cooperative},
            "native seeder did not prove its actual observed and Waited worker")
    if cooperative:
        path = case.fixture.state / "inspections" / operation
        tokens.append(owner.adopt_created("synthetic native inspection", path, path.parent))
    return operation if cooperative else None, admission, proof


def close_receiptless_task(case: SchedulerCase, task_id: str) -> None:
    response = case.command("receiptless-typed-settle", ["settle", task_id, "--prepare"], (1,))
    fact = json_object(response.get("control_fact"))
    require(response.get("error") and fact.get("resource_kind") == "task"
            and fact.get("resource_id") == task_id and fact.get("name") == "supervisor.ref.json"
            and fact.get("next_action") == "cancel_task", "receiptless CLI returned an untyped or misleading refusal")
    cancelled = case.command("receiptless-public-cancel", ["cancel", task_id])
    require(not cancelled.get("error"), "durable cancellation did not close the unlaunched admission")


def require_inspection_target_error(case: SchedulerCase, operation: str) -> None:
    response = case.command("default-task-target-mismatch", ["settle", operation, "--prepare"], (1,))
    fact = json_object(response.get("control_fact"))
    require(response.get("error") and fact == {
                "resource_kind": "inspection", "resource_id": operation,
                "fact_kind": "target_kind", "name": "inspection",
                "reason_code": "inspection_requires_kind", "next_action": "settle_with_kind_inspection",
            },
            "built CLI default-task mismatch did not return a named typed fact with exit1")


def settle_native(case: SchedulerCase, operation: str) -> bytes:
    require_inspection_target_error(case, operation)
    prepared = case.command("inspection-prepare", ["settle", "--kind", "inspection", operation, "--prepare"])
    settlement = json_object(prepared.get("settlement"))
    challenge = settlement.get("challenge")
    if not isinstance(challenge, str):
        raise RuntimeError("native prepare omitted a string challenge")
    require(len(challenge) == 64
            and settlement.get("target_kind") == "inspection"
            and settlement.get("administrative_state") == "prepared", "native prepare omitted a typed challenge")
    # This is an explicit test-human decision about the synthetic, Waited worker
    # above. Provider-driven routing fixtures never receive this authorization.
    confirmed = case.command("inspection-test-human-confirm", [
        "settle", "--kind", "inspection", operation, "--confirm", challenge,
    ])
    require(json_object(confirmed.get("settlement")).get("administrative_state") == "settled",
            "native confirmation did not persist maintenance-only permission")
    return (case.fixture.state / "inspections" / operation / "operator-disposition.json").read_bytes()


def close_actual_resources(case: SchedulerCase) -> None:
    """Close known native resources before retiring creator-owned synthetic data."""
    case.service.stop()
    case.reconcile_cleanup_tasks(case.discover(), deadline=time.monotonic() + 45)
    config = case.lifetime.private_config
    if config is None:
        raise RuntimeError("chain lost its selected supervisor config")
    deadline = time.monotonic() + 45
    pending = register_terminal_private_root(
        case.lifetime.supervisor_cleanups, case.delegate, case.supervisor_directory / "pueue",
        config, case.lifetime.env, state=case.fixture.state, task_ids=tuple(case.lifetime.attempted_task_ids),
    )
    pending["root_id"] = case.root_id
    # Only the finite seeded process was omitted from generic journal closure.
    # Its execution owner already Waited and proved its actual native scope empty.
    # All real dispatched tasks were collected by their normal lifecycle owner.
    with private_fixture_transition(case.fixture.state):
        daemon = private_shutdown_evidence(config)
        pending["daemon"] = daemon
    pending["owner"] = "unresolved"
    stopped = case.command("chain-stop-owner", ["supervisor", "stop-owner"], (0, 3), deadline=deadline)
    selected = selected_owner_subject(stopped, case.root_id, config)
    pending["owner"] = selected
    with private_fixture_transition(case.fixture.state):
        shutdown_observed_supervisor(case.supervisor_directory / "pueue", config, case.lifetime.env,
                                     daemon, deadline, capture_root=case.lifetime.capture_token)
    if selected is not None:
        await_private_owner_absence(selected["process"], deadline, capture_root=case.lifetime.capture_token)


def retire_synthetic(case: SchedulerCase, owner: DirectoryOwner, tokens: list[OwnedDirectory], expected: dict[str, str]) -> dict[str, str]:
    """Deletion is test teardown after both successful drains and native closure."""
    require_preserved(case.fixture.state, expected)
    inventory: dict[str, str] = {}
    # Every token was acquired at creation, before maintenance. No user history
    # can enter this ledger. Refuse nested directories, symlinks or unbounded data.
    for token in tokens:
        token.assert_live()
        files = tuple(token.directory.iterdir())
        require(len(files) <= 32, "synthetic journal cleanup inventory exceeded bound")
        for path in files:
            require(path.is_file() and not path.is_symlink() and path.stat().st_size <= 1 << 20,
                    "synthetic cleanup encountered an unknown filesystem entry")
            inventory[str(path.relative_to(case.fixture.state))] = hashlib.sha256(path.read_bytes()).hexdigest()
    close_actual_resources(case)
    require_preserved(case.fixture.state, expected)
    owner.rollback()
    return inventory


def select_second_candidate(case: SchedulerCase, source_root: Path) -> None:
    """Install C's native pair and the existing startup-marker instrumentation."""
    if case.lifetime.base is None:
        raise RuntimeError("chain lost installation ownership")
    base = case.lifetime.base
    owner = case.lifetime.ownership
    croot = owner.create("second candidate package", base / "second-candidate", base).directory
    cinstall, cbin, clibexec = install_package_layout(owner, source_root, croot, True, version=C_VERSION)
    native = owner.create("second candidate native supervisor", clibexec / "native", clibexec).directory
    # Ordinary archive installation into this generation; never copy or move
    # selected installed executable contents to add instrumentation.
    install_supervisor_pair(native)
    daemon_marker = base / "second-candidate-daemon-starts"
    for role, name, marker in (("client", "pueue", base / "second-candidate-client-starts"),
                               ("daemon", "pueued", daemon_marker)):
        install_wrapper(clibexec / name, native / name, marker, "second-candidate", role)
    case.fixture = replace(case.fixture, upgraded_install=cinstall, upgraded_delegate_binary=cbin / "delegate",
                           upgraded_runner_binary=cbin / "delegate-run", upgraded_daemon_marker=daemon_marker)
    case.lifetime.upgraded_libexec = clibexec
    case.lifetime.delegate = case.delegate


def exercise(case: SchedulerCase, args: UpgradeArguments, synthetic: DirectoryOwner) -> None:
    counter = install_entry_counter(case)
    paused = create_definition(case, "preserved-paused", paused=True)
    future = create_definition(case, "future", paused=True)
    case.command("initialize-released-owner", ["supervisor", "ensure-owner"])
    case.service.start()
    case.service.stop()
    before = snapshot(case.fixture.state, case.root_id)
    native, disposition, operation_b, admission = None, None, None, None
    if platform.system() == "Linux":
        prepared = case.command("a-prepare-installation", ["supervisor", "ensure-owner", "--prepare-upgrade"])
        report = json_object(json_object(prepared.get("supervisor_control")).get("upgrade"))
        operation_b = report.get("operation_id")
        if not isinstance(operation_b, str):
            raise RuntimeError("released preparation omitted its operation")
        require(report.get("phase") == "awaiting_installation", "released preparation did not preserve the install boundary")
        repeated = case.command("a-interrupted-preparation", ["supervisor", "ensure-owner"], (3,))
        require(json_object(json_object(repeated.get("supervisor_control")).get("upgrade")).get("operation_id") == operation_b,
                "separate invocation replaced prepared operation")
    tokens: list[OwnedDirectory] = []
    historical = seed(args.source_root, case.fixture.state, case.root_id, synthetic, tokens,
                      namespace_owner=case.lifetime.ownership)
    if platform.system() == "Darwin":
        native, admission, native_proof = native_seed(case, args.source_root, synthetic, tokens)
        case.receipt.add_details(native_cooperative_fixture=native_proof, cooperative_settlement_backend="native")
    case.previous_services.append(case.service)
    rollback_owned_directories(case.lifetime.old_layout_owned)
    case.lifetime.old_layout_owned = ()
    case.upgraded = True
    case.lifetime.delegate = case.delegate
    case.service = SchedulerHost(case.lifetime, case.fixture.state, case.delegate, case.deadline, generation=1)
    if admission is not None:
        close_receiptless_task(case, admission)
    if native is not None:
        blocked = case.command("b-interrupted-drain", ["supervisor", "ensure-owner"], (3,))
        control = json_object(blocked.get("supervisor_control"))
        report = json_object(control.get("upgrade"))
        operation_b = report.get("operation_id")
        if not isinstance(operation_b, str):
            raise RuntimeError("first B drain did not commit its operation")
        blocker = json_object(control.get("first_blocker"))
        require(control.get("state") == "blocked" and blocker.get("kind") == "inspection"
                and blocker.get("operation_id") == native
                and blocker.get("reason_code") == "inspection_closure_unproven"
                and blocker.get("next_action") == "settle_inspection", "B did not select supported native inspection settlement")
        observed = case.command("b-read-after-interruption", ["supervisor", "status"])
        require(json_object(json_object(observed.get("supervisor_control")).get("upgrade")).get("operation_id") == operation_b,
                "separate observation lost the interrupted B operation")
        disposition = settle_native(case, native)
    require(advance(case, "b-complete", operation_b, explicit=native is None) == operation_b,
            "B did not complete its original operation")
    require_preserved(case.fixture.state, historical)
    require_scheduler_preserved(before, snapshot(case.fixture.state, case.root_id))
    if platform.system() == "Linux":
        # A's prepared fence correctly forbids new consumed admission permits.
        # Exercise this CLI regression only after B completes that same fence.
        _native, admission, admission_proof = native_seed(case, args.source_root, synthetic, tokens)
        require(_native is None, "Linux selected an unverified cooperative fixture")
        case.receipt.add_details(native_cooperative_fixture=admission_proof,
                                cooperative_settlement_backend="not_applicable_linux_containment")
        close_receiptless_task(case, admission)
    # Build C independently; changing installed path/version exercises the next
    # upgrade, rather than restarting B and calling that a second generation.
    select_second_candidate(case, args.source_root)
    case.previous_services.append(case.service)
    case.service = SchedulerHost(case.lifetime, case.fixture.state, case.delegate, case.deadline, generation=2)
    operation_c = advance(case, "c-complete")
    require(operation_c != operation_b, "next installed generation reused a completed operation")
    if native is not None:
        require((case.fixture.state / "inspections" / native / "operator-disposition.json").read_bytes() == disposition,
                "C changed or failed to retain B's human maintenance permission")
    require_preserved(case.fixture.state, historical)
    require_scheduler_preserved(before, snapshot(case.fixture.state, case.root_id))
    case.service.start()
    require(entry_count(counter) == 0 and not case.discover(), "upgrade replayed history or resumed a paused schedule")
    resume_ns = time.time_ns()
    case.command("c-resume-future", ["schedule", "resume", future])
    rows = collect_schedule(case, future, 1)
    require(rows[0].due_ns > resume_ns and entry_count(counter) == 1,
            "C did not execute exactly one actual future tick")
    case.command("c-pause-future", ["schedule", "pause", future])
    case.service.stop()
    shown = json_object(case.command("c-preserved-pause", ["schedule", "show", paused]).get("schedule"))
    require(shown.get("state") == "paused", "C resumed the user's paused definition")
    delete_definitions(case)
    inventory = retire_synthetic(case, synthetic, tokens, historical)
    case.receipt.add_details(upgrade_chain={"generations": [RECOVERY_RELEASE.version, B_VERSION, C_VERSION],
                            "same_root_id": case.root_id, "operations": [operation_b, operation_c],
                            "historical_journals_preserved_through_both_drains": 7,
                            "human_permission_used_by_next_upgrade": native is not None,
                            "no_replay": True, "future_provider_entries": entry_count(counter),
                            "synthetic_teardown_inventory": inventory})
    case.receipt.stage("native-same-root-upgrade-chain-proved")


def main(argv: list[str] | None = None) -> int:
    # Reuse the installed-input and owned-output argument boundary; only the
    # baseline is fixed here, so a contributor cannot silently test another A.
    parsed = upgrade_arguments(argv)
    if platform.system() not in ("Linux", "Darwin"):
        raise AcceptanceBlocked("upgrade-maintenance-chain-requires-linux-or-macos")
    output_owner, synthetic = DirectoryOwner(), DirectoryOwner()
    output = output_directory(parsed.output, output_owner)
    distribution: dict[str, object] = {}
    args = replace(parsed, output=output / "receipt.json", scenario=SUPERVISOR_UPGRADE_RECOVERY_SCENARIO,
                   baseline_pair_installer=ReleaseInstaller(RECOVERY_RELEASE, distribution), candidate_version=B_VERSION)
    receipt = AcceptanceReceipt(args.output, 1, 1, {"pueue": args.pueue, "pueued": args.pueued})
    lifetime, case, failure = FixtureLifetime(), None, None
    try:
        fixture = prepare_fixture(args, uuid.uuid4().hex, False, lifetime, output / "responses", receipt)
        deadline = time.monotonic() + 480
        if lifetime.old_install is None:
            raise RuntimeError("chain lost released installation")
        host = SchedulerHost(lifetime, fixture.state, lifetime.old_install / "delegate", deadline)
        case = SchedulerCase(fixture, lifetime, output / "responses", receipt, host, deadline)
        case.task_closure_reader = partial(observe_chain_task_closure, case)
        receipt.add_details(historical_distribution=distribution, scenario="upgrade-recovery-chain")
        exercise(case, args, synthetic)
    except BaseException as error:
        failure = error
        receipt.add_details(failure_phase=receipt.current_phase)
    finally:
        # Unknown synthetic/native closure retains records; it never deletes
        # fixture evidence merely to make the ordinary finalizer pass.
        synthetic.release()
        try:
            finish_case(args=args, case=case, lifetime=lifetime, receipt=receipt, failure=failure)
        finally:
            output_owner.release()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
