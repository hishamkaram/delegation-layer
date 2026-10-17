"""Modern cross-binary contention using the existing directory/process/task owners.

Install the client fixture in BOTH modern bundles before dispatch/bootstrap.
run_modern_binary_contention consumes a fresh NativeTaskOps and a prepared finite
PI fixture. It never interprets the two bundles as the historical legacy mode.
The caller retains private-supervisor shutdown and owned-root finalization.
"""

from dataclasses import asdict, dataclass
import hashlib
from pathlib import Path
import shlex
import sys
import time
from typing import Callable
import uuid

from scripts.delegate_acceptance.directories.operations import DirectoryOwner, OwnedDirectory
from scripts.delegate_acceptance.evidence.files import digest, read_json, write_json
from scripts.delegate_acceptance.evidence.records import parse_json_output
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.processes.operations import Process, Processes
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps
from scripts.delegate_acceptance.tasks.publication import snapshot, verify_collected_outcome
from scripts.delegate_acceptance.private_cli import control_fixture
from scripts.delegate_acceptance.private_cli.capacity_ownership import prove_lifetime_owner_cohort
from scripts.delegate_acceptance.private_cli.capacity_records import capacity_lease_specifications
from scripts.delegate_acceptance.private_cli.contention_locks import probe_generation_lease
from scripts.delegate_acceptance.private_cli.contention_policy import (
    ModernInstallation,
    MODERN_CONTENTION_SCENARIO,
    MODERN_CONTENTION_TASK_COUNT,
    MODERN_CONTENTION_TASK_BUDGET,
    LOGICAL_IDENTITY_FIELDS,
    migration_expectation,
    require_boundary_identity,
    require_content_identity,
    require_control_response,
    require_migration_response,
    require_same_lock,
    require_boundary_order,
)
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli.fixture_records import FixtureInstallation
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor
from scripts.delegate_acceptance.private_cli.fingerprints import file_fingerprint
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort, LockedReleaseGate
from scripts.delegate_acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.delegate_acceptance.private_cli.supervisor import await_managed_lifetime_closure


ContentReader = Callable[[], dict[str, object]]


@dataclass(frozen=True)
class ModernFixture:
    controls: OwnedDirectory
    brief: Path
    workspace: Path
    gate: Path
    waiter_directory: Path
    lease_nonce: str


@dataclass(frozen=True)
class ControlGate:
    directory: Path
    nonce: str

    @property
    def release(self) -> Path:
        return self.directory / "release"

    @property
    def receipt(self) -> Path:
        return self.directory / "entered.json"


def control_gate(parent: OwnedDirectory, label: str) -> ControlGate:
    parent.assert_live()
    owner = DirectoryOwner()
    try:
        token = owner.create("modern control port " + label, parent.directory / label, parent.directory)
        directory = token.directory
        owner.transfer_child_to(parent)
    except BaseException:
        owner.rollback()
        raise
    return ControlGate(directory, uuid.uuid4().hex)


def install_contention_client(destination: Path, executable: Path, controls: OwnedDirectory) -> None:
    """Explicit setup port: call only before either bundle publishes its pair."""
    controls.assert_live()
    argv = [
        sys.executable,
        str(Path(control_fixture.__file__).resolve()),
        "client",
        str(executable),
        str(controls.directory),
    ]
    destination.write_text("#!/bin/sh\nset -eu\nexec " + shlex.join(argv) + ' "$@"\n')
    destination.chmod(0o700)


def start_control(
    processes: Processes, binary: Path, arguments: tuple[str, ...], state: Path, label: str, gate: ControlGate
) -> Process:
    write_json(
        gate.directory / "request.json",
        {"schema_version": 1, "nonce": gate.nonce, "config": str(state / ".supervisor" / "pueue.yml")},
    )
    process = processes.start(
        label,
        [sys.executable, control_fixture.__file__, "launch", binary, gate.directory, "--root", state, *arguments],
        state.parent,
    )
    (gate.directory / "launch").touch(mode=0o600)
    return process


def observe_boundary(process: Process, gate: ControlGate, lock_path: Path, mode: str) -> dict[str, object]:
    def entered():
        require(process.poll() is None, "modern control exited before reaching its client boundary")
        return gate.receipt.is_file()

    wait_until(entered, timeout=15, description="modern binary JSON status lease boundary")
    receipt = require_boundary_identity(
        read_json(gate.receipt), gate.nonce, process.pid, str(lock_path.parent / "pueue.yml")
    )
    return {"boundary": receipt, "lease": probe_generation_lease(lock_path, mode)}


def release_control(gate: ControlGate) -> int:
    released = time.monotonic_ns()
    gate.release.touch(mode=0o600, exist_ok=True)
    (gate.directory / "launch").touch(mode=0o600, exist_ok=True)
    return released


def finish_controls(processes: Processes, gates: tuple[ControlGate, ...]) -> None:
    for gate in gates:
        release_control(gate)
    pending = processes.drain(40)
    require(not pending and processes.finished_naturally(), "modern controls did not close naturally")


def optional_control_record(path: Path) -> dict[str, object] | None:
    require(not path.is_symlink(), "modern migration record is unsafe: " + str(path))
    if not path.exists():
        return None
    require(path.is_file(), "modern migration record is not a regular file: " + str(path))
    record = read_json(path)
    require(isinstance(record, dict), "modern migration record is not an object: " + str(path))
    return record


def active_supervisor_content(state: Path) -> dict[str, object]:
    supervisor = state / ".supervisor"
    names = ("pueue.yml", "supervisor.identity.json", "daemon.identity.json", "run/pueue.pid")
    for name in names:
        path = supervisor / name
        require(path.is_file() and not path.is_symlink(), "modern active supervisor record is unsafe: " + str(path))
    pid = (supervisor / "run" / "pueue.pid").read_text().strip()
    require(pid.isascii() and pid.isdecimal() and int(pid) > 0, "modern daemon PID record is invalid")
    return {"daemon_pid": int(pid), "records": {name: digest(supervisor / name) for name in names}}


def observe_migration(state: Path, writer: ModernInstallation) -> dict[str, object]:
    """Snapshot published facts and the packaged resolver's content-addressed target.

    No inventory pointer chooses the active generation and no record is changed.
    The caller holds its reader gate when taking active-round observations.
    """
    supervisor = state / ".supervisor"
    published = optional_control_record(supervisor / "supervisor.identity.json")
    require(published is not None and isinstance(published.get("binding"), dict), "modern published binding is missing")
    binding = published["binding"]
    members = {name: digest(writer.libexec / name) for name in ("pueue", "pueued")}
    pair_digest = hashlib.sha256((members["pueue"] + "\n" + members["pueued"]).encode()).hexdigest()
    target = supervisor / ("pueue-pair-" + pair_digest)
    for role in ("client", "daemon"):
        path = Path(binding[role + "_executable"])
        require(path.is_file() and not path.is_symlink(), "modern published executable is unsafe")
        require(digest(path) == binding[role + "_sha256"], "modern published executable bytes changed")
    alias_config = supervisor / "legacy-pueue.yml"
    require(not alias_config.is_symlink(), "modern recovery config is unsafe")
    alias = optional_control_record(supervisor / "legacy-supervisor.json")
    if alias is not None:
        require(alias.get("config_path") == str(alias_config), "modern recovery config escaped its owned supervisor")
    unsettled = ("bootstrap.pending", "identity.pending.json", "legacy-supervisor.pending.json", "legacy-pueue.pending")
    return {
        "published": binding,
        "daemon": optional_control_record(supervisor / "daemon.identity.json"),
        "writer": {"executable": str(target / "pueued"), "sha256": members["pueued"],
                   **{field: binding.get(field) for field in LOGICAL_IDENTITY_FIELDS}},
        "migration": optional_control_record(supervisor / "migration.json"),
        "alias": alias,
        "alias_config_sha256": digest(alias_config) if alias_config.exists() else None,
        "unsettled_records": [name for name in unsettled if (supervisor / name).exists() or (supervisor / name).is_symlink()],
    }


def prove_contention_round(
    *, processes: Processes,
    reader: Path,
    writer: ModernInstallation,
    state: Path,
    controls: OwnedDirectory,
    label: str,
    task_id: str,
    immutable_content: ContentReader,
    assert_live: Callable[[], None],
) -> dict[str, object]:
    shared_gate = control_gate(controls, label + "-shared")
    exclusive_gate = control_gate(controls, label + "-exclusive")
    lock_path = state / ".supervisor" / "bootstrap.lock"
    content = lambda: {"tasks": immutable_content(), "supervisor": active_supervisor_content(state)}
    before = content()
    root_id = read_json(state / "root.json")["root_id"]
    free_before = probe_generation_lease(lock_path, "unlocked")
    try:
        shared = start_control(processes, reader, ("status", task_id, "--json"), state, label + "-shared", shared_gate)
        shared_evidence = observe_boundary(shared, shared_gate, lock_path, "shared")
        assert_live()
        facts = observe_migration(state, writer)
        expected = migration_expectation(facts, active=True)
        exclusive = start_control(
            processes, writer.delegate, ("supervisor", "migrate", "--json"), state, label + "-exclusive", exclusive_gate
        )
        require(exclusive.poll() is None, "modern exclusive control exited before shared release")
        require(not exclusive_gate.receipt.exists(), "exclusive client entered while a shared lease was live")
        require_content_identity(before, content(), "shared lease")
        shared_release = release_control(shared_gate)
        # Do not wait for the whole reader CLI here. It may need another shared
        # observation while the writer already owns EX and awaits fixture release.
        exclusive_evidence = observe_boundary(exclusive, exclusive_gate, lock_path, "exclusive")
        assert_live()
        pair = assert_state_supervisor_pair(state, writer.libexec)
        require(str(pair / "pueued") == facts["writer"]["executable"], "modern writer resolved another daemon")
        require_content_identity(before, content(), "exclusive entry")
        exclusive_release = release_control(exclusive_gate)
        exclusive.wait(35, expected.exit_code)
        require_content_identity(before, content(), "control completion")
        shared.wait(35, 0)
        require_control_response(parse_json_output(shared, label + " status"), root_id, task_id=task_id)
        response = parse_json_output(exclusive, label + " migrate")
        require_migration_response(response, root_id, expected)
        require_content_identity(before, content(), expected.case)
        assert_live()
        free_after = probe_generation_lease(lock_path, "unlocked")
        require_same_lock(free_before, shared_evidence["lease"], exclusive_evidence["lease"], free_after)
        require_boundary_order(
            shared_evidence["boundary"], shared_release, exclusive_evidence["boundary"], exclusive_release
        )
        return {
            "reader": str(reader),
            "writer": str(writer.delegate),
            "shared": shared_evidence,
            "exclusive": exclusive_evidence,
            "root_id": root_id,
            "task_id": task_id,
            "shared_release_ns": shared_release,
            "exclusive_release_ns": exclusive_release,
            "initial_lock": free_before,
            "final_lock": free_after,
            "active_work_refused": expected.state == "blocked",
            "migration_facts_before": facts,
            "migration_expectation": asdict(expected),
            "migration_response": response,
            "immutable_content_before": before,
            "immutable_content_after": content(),
        }
    finally:
        finish_controls(processes, (shared_gate, exclusive_gate))


def prove_exclusive_handoff(
    tasks: NativeTaskOps, candidate: ModernInstallation, controls: OwnedDirectory, immutable_content: ContentReader
) -> dict[str, object]:
    gate = control_gate(controls, "candidate-final-handoff")
    before = immutable_content()
    root_id = tasks.root_id
    facts = observe_migration(tasks.state, candidate)
    expected = migration_expectation(facts, active=False)
    try:
        process = start_control(
            tasks.processes,
            candidate.delegate,
            ("supervisor", "migrate", "--json"),
            tasks.state,
            "candidate-final-handoff",
            gate,
        )
        boundary = observe_boundary(process, gate, tasks.state / ".supervisor" / "bootstrap.lock", "exclusive")
        require_content_identity(before, immutable_content(), "final handoff entry")
        released = release_control(gate)
        process.wait(35, expected.exit_code)
        response = parse_json_output(process, "candidate final handoff")
        require_migration_response(response, root_id, expected)
        pair = assert_state_supervisor_pair(tasks.state, candidate.libexec)
        require_content_identity(before, immutable_content(), "final handoff completion")
        unlocked = probe_generation_lease(tasks.state / ".supervisor" / "bootstrap.lock", "unlocked")
        return {
            "response": response,
            "migration_facts_before": facts,
            "migration_expectation": asdict(expected),
            "boundary": boundary,
            "released_ns": released,
            "final_lock": unlocked,
            "candidate_pair": str(pair),
            "immutable_content_before": before,
            "immutable_content_after": immutable_content(),
        }
    finally:
        finish_controls(tasks.processes, (gate,))


def prove_candidate_observation(
    tasks: NativeTaskOps, candidate: ModernInstallation, controls: OwnedDirectory,
    immutable_content: ContentReader, assert_live: Callable[[], None],
) -> dict[str, object]:
    """Exercise the candidate's read-only consumer after the live migration attempt."""
    gate = control_gate(controls, "candidate-after-active-migration")
    lock_path = tasks.state / ".supervisor" / "bootstrap.lock"
    content = lambda: {"tasks": immutable_content(), "supervisor": active_supervisor_content(tasks.state)}
    before = content()
    facts = observe_migration(tasks.state, candidate)
    migration_expectation(facts, active=True)
    task_id = tasks.tasks["modern-candidate"]
    try:
        process = start_control(tasks.processes, candidate.delegate, ("status", task_id, "--json"),
                                tasks.state, "candidate-after-active-migration", gate)
        boundary = observe_boundary(process, gate, lock_path, "shared")
        assert_live()
        require_content_identity(before, content(), "candidate observation entry")
        released = release_control(gate)
        process.wait(35, 0)
        response = parse_json_output(process, "candidate observation after active migration")
        require_control_response(response, tasks.root_id, task_id=task_id)
        require_content_identity(facts, observe_migration(tasks.state, candidate), "read-only migration facts")
        require_content_identity(before, content(), "candidate observation completion")
        assert_live()
        return {"boundary": boundary, "released_ns": released, "response": response,
                "migration_facts": facts, "final_lock": probe_generation_lease(lock_path, "unlocked"),
                "immutable_content_before": before, "immutable_content_after": content()}
    finally:
        finish_controls(tasks.processes, (gate,))


def stable_task_content(tasks: NativeTaskOps) -> dict[str, object]:
    """Read the fixed admission/launch records after the live owner proof."""
    result = {"root.json": digest(tasks.state / "root.json")}
    for task_id in sorted(tasks.tasks.values()):
        directory = tasks.state / "tasks" / task_id
        names = [
            "task.json",
            "meta.json",
            "brief.md",
            "supervisor.ref.json",
            "lifetime/intent.json",
            "lifetime/registration.json",
            "lifetime/worker.json",
        ]
        names.extend(
            str(path.relative_to(directory)) for path in sorted((directory / "lifetime").glob("*.launch.json"))
        )
        for name in names:
            path = directory / name
            require(path.is_file() and not path.is_symlink(), "modern task record is unsafe: " + str(path))
            result[task_id + "/" + name] = digest(path)
    return result


def select_installation(tasks: NativeTaskOps, installation: ModernInstallation) -> None:
    """Select executable inputs; NativeTaskOps keeps the one task/admission ledger."""
    tasks.delegate = installation.delegate
    tasks.runner = installation.runner
    config = tasks.state / ".supervisor" / "pueue.yml"
    tasks.bind_supervisor(config)
    identity = config.parent / "supervisor.identity.json"
    if identity.exists():
        tasks.pueue = Path(read_json(identity)["binding"]["client_executable"])


def dispatch_modern_task(
    tasks: NativeTaskOps, installation: ModernInstallation, fixture: ModernFixture, name: str, waiting_brief: Path
) -> dict[str, object]:
    select_installation(tasks, installation)
    task_id = uuid.uuid4().hex
    response = tasks.dispatch(
        name,
        task_id,
        [
            installation.delegate,
            "--root",
            tasks.state,
            "dispatch",
            "--provider",
            "pi:json",
            "--id",
            task_id,
            "--brief",
            waiting_brief,
            "--cwd",
            fixture.workspace,
            "--permission",
            "read-only",
            "--budget",
            MODERN_CONTENTION_TASK_BUDGET,
            "--json",
        ],
    )
    select_installation(tasks, installation)
    return response


def acquire_modern_cohort(
    tasks: NativeTaskOps, fixture: ModernFixture, responses: list[dict[str, object]]
) -> LiveFIFOLeaseCohort:
    def candidates():
        values = capacity_lease_specifications(tasks.state, responses, fixture.waiter_directory, fixture.lease_nonce)
        return (
            values
            if len(values) == MODERN_CONTENTION_TASK_COUNT and all(value["path"].exists() for value in values)
            else None
        )

    specifications = wait_until(candidates, timeout=30, description="both modern finite provider leases")
    return LiveFIFOLeaseCohort.acquire(specifications, fixture.lease_nonce)


def close_modern_cohort(
    tasks: NativeTaskOps, fixture: ModernFixture, release: LockedReleaseGate, cohort: LiveFIFOLeaseCohort | None
) -> dict[str, object]:
    release.release()
    try:
        closure = await_managed_lifetime_closure(tasks.state, tuple(sorted(tasks.dispatch_attempts)), timeout=45)
        if cohort is not None:
            cohort.remove_closed_fifos()
        return closure
    finally:
        if cohort is not None:
            cohort.close()


def collect_modern_tasks(tasks: NativeTaskOps, baseline: ModernInstallation, candidate: ModernInstallation) -> None:
    for name, installation in (("modern-baseline", baseline), ("modern-candidate", candidate)):
        select_installation(tasks, installation)
        # The current CLI consumes both tasks; queue validation still checks
        # each task's original immutable managed-runner fingerprint.
        tasks.delegate = candidate.delegate
        task_id = tasks.tasks[name]
        tasks.wait_task(name, task_id)
        response = tasks.collect(name + "-collect", task_id)
        verify_collected_outcome(response, tasks.state / "tasks" / task_id, "committed")


def replay_after_handoff(tasks: NativeTaskOps, candidate: ModernInstallation) -> None:
    for task_id in sorted(tasks.tasks.values()):
        for command in ("status", "collect"):
            process = tasks.direct(
                "modern-post-handoff-" + command + "-" + task_id,
                [candidate.delegate, "--root", tasks.state, command, task_id, "--json"],
                timeout=35,
            )
            response = parse_json_output(process, "modern candidate " + command)
            require_control_response(response, tasks.root_id, task_id=task_id)
            verify_collected_outcome(response, tasks.state / "tasks" / task_id, "committed")


def run_modern_binary_contention(
    tasks: NativeTaskOps, baseline: ModernInstallation, candidate: ModernInstallation, fixture: ModernFixture
) -> dict[str, object]:
    """Run two live modern tasks, bidirectional lock contention, then real handoff.

    The caller supplies the same canonical task/process owner for both bundles
    and installs gated clients before either dispatch. This operation does not
    stop the private daemon or delete caller-owned directories.
    """
    require(
        isinstance(tasks, NativeTaskOps) and not tasks.closed and not tasks.dispatch_attempts,
        "modern contention requires a fresh canonical task owner",
    )
    require(tasks.daemon is None, "modern contention requires the application-owned private daemon")
    require(baseline.delegate.resolve() != candidate.delegate.resolve(), "modern contention requires two CLI binaries")
    require(
        "TMPDIR" not in tasks.processes.environment,
        "modern task admission environment must omit TMPDIR so status retains its current control port",
    )
    fixture.controls.assert_live()
    waiting_brief = fixture.brief.with_name("modern-contention-waiting.md")
    with waiting_brief.open("x", encoding="utf-8") as stream:
        stream.write("DELEGATE_PRIVATE_UPGRADE_WAIT\n")
    release = LockedReleaseGate.acquire(fixture.gate)
    cohort = None
    failure = None
    try:
        responses = dispatch_modern_pair(tasks, baseline, candidate, fixture, waiting_brief)
        cohort = acquire_modern_cohort(tasks, fixture, responses)
        owner_proof, active_before, rounds = run_live_contention(tasks, baseline, candidate, fixture, responses, cohort)
    except BaseException as error:
        failure = error
        raise
    finally:
        try:
            closure = close_modern_cohort(tasks, fixture, release, cohort)
        except BaseException as cleanup_error:
            if failure is not None:
                raise RuntimeError("modern contention cleanup failed: " + str(cleanup_error)) from failure
            raise
    collect_modern_tasks(tasks, baseline, candidate)
    before = {task_id: snapshot(tasks.state / "tasks" / task_id) for task_id in sorted(tasks.tasks.values())}
    full_content = lambda: {
        "root": digest(tasks.state / "root.json"),
        "tasks": {task_id: snapshot(tasks.state / "tasks" / task_id) for task_id in sorted(tasks.tasks.values())},
    }
    handoff = prove_exclusive_handoff(tasks, candidate, fixture.controls, full_content)
    replay_after_handoff(tasks, candidate)
    after = {task_id: snapshot(tasks.state / "tasks" / task_id) for task_id in sorted(tasks.tasks.values())}
    require_content_identity(before, after, "candidate status/collection replay")
    fixture.controls.assert_live()
    return {
        "scenario": MODERN_CONTENTION_SCENARIO,
        "root_id": tasks.root_id,
        "installations": {
            name: {
                role: {"path": str(path), **file_fingerprint(path)}
                for role, path in (
                    ("delegate", value.delegate),
                    ("runner", value.runner),
                    ("pueue", value.libexec / "pueue"),
                    ("pueued", value.libexec / "pueued"),
                )
            }
            for name, value in (("baseline", baseline), ("candidate", candidate))
        },
        "source": {
            name: file_fingerprint(Path(__file__).with_name(name))
            for name in ("contention_controls.py", "contention_locks.py", "contention_policy.py", "control_fixture.py")
        },
        "task_ids": sorted(tasks.tasks.values()),
        "active_content": active_before,
        "live_owner_proof": owner_proof,
        "contention_rounds": rounds,
        "lifetime_closure": closure,
        "handoff": handoff,
        "task_content_before_handoff": before,
        "task_content_after_handoff": after,
    }


def run_live_contention(
    tasks: NativeTaskOps,
    baseline: ModernInstallation,
    candidate: ModernInstallation,
    fixture: ModernFixture,
    responses: list[dict[str, object]],
    cohort: LiveFIFOLeaseCohort,
) -> tuple[dict[str, object], dict[str, object], list[dict[str, object]]]:
    specifications = capacity_lease_specifications(
        tasks.state, responses, fixture.waiter_directory, fixture.lease_nonce
    )
    owner_proof = prove_lifetime_owner_cohort(specifications, cohort)
    immutable = lambda: {"tasks": stable_task_content(tasks), "supervisor": active_supervisor_content(tasks.state)}
    active_before = immutable()
    rounds = [
        prove_contention_round(
            processes=tasks.processes,
            reader=reader.delegate,
            writer=writer,
            state=tasks.state,
            controls=fixture.controls,
            label=label,
            task_id=tasks.tasks[name],
            immutable_content=immutable,
            assert_live=cohort.assert_live,
        )
        for reader, writer, label, name in (
            (candidate, baseline, "new-reader-old-writer", "modern-candidate"),
            (baseline, candidate, "old-reader-new-writer", "modern-baseline"),
        )
    ]
    # The old writer runs against healthy state. Only the candidate is claimed
    # to observe the prepared intent/alias left by a refused generation change.
    rounds[-1]["candidate_post_migration_observation"] = prove_candidate_observation(
        tasks, candidate, fixture.controls, immutable, cohort.assert_live
    )
    require_content_identity(active_before, immutable(), "both live contention rounds")
    return owner_proof, active_before, rounds


def dispatch_modern_pair(
    tasks: NativeTaskOps,
    baseline: ModernInstallation,
    candidate: ModernInstallation,
    fixture: ModernFixture,
    waiting_brief: Path,
) -> list[dict[str, object]]:
    old = dispatch_modern_task(tasks, baseline, fixture, "modern-baseline", waiting_brief)
    assert_state_supervisor_pair(tasks.state, baseline.libexec)
    identity = tasks.state / ".supervisor" / "daemon.identity.json"
    launch_before = digest(identity)
    pid = identity.parent / "run" / "pueue.pid"
    pid_before = digest(pid)
    binding = identity.with_name("supervisor.identity.json")
    binding_before = digest(binding)
    new = dispatch_modern_task(tasks, candidate, fixture, "modern-candidate", waiting_brief)
    # Installation stages candidate inventory; only migration may replace the
    # active generation or the daemon's launch provenance.
    assert_state_supervisor_pair(tasks.state, candidate.libexec)
    require_content_identity(launch_before, digest(identity), "modern warm candidate admission")
    require_content_identity(binding_before, digest(binding), "modern active generation")
    require_content_identity(pid_before, digest(pid), "modern warm candidate daemon PID")
    return [old, new]


def run_prepared_modern_contention(
    lifetime: FixtureLifetime, installed: FixtureInstallation, response_directory: Path, run_receipt: AcceptanceReceipt
) -> dict[str, object]:
    """Facade port for existing prepared old/upgraded flag inputs, both modern.

    FixtureLifetime owns the directory tokens and final shutdown/root cleanup.
    NativeTaskOps owns task attempts; the finalizer list is derived only when
    this operation returns or raises, including unanswered dispatch attempts.
    """
    require(
        isinstance(lifetime, FixtureLifetime) and isinstance(installed, FixtureInstallation),
        "modern facade requires canonical prepared fixture inputs",
    )
    run_receipt.phase("modern_two_binary_contention")
    controls = lifetime.ownership.create("modern control ports", lifetime.base / "modern-controls", lifetime.base)
    captures = lifetime.ownership.create("modern process captures", lifetime.base / "modern-processes", lifetime.base)
    processes = Processes(captures.directory, capture_root=captures, environment=lifetime.env)
    baseline = ModernInstallation(
        lifetime.old_install / "delegate", lifetime.old_install / "delegate-run", lifetime.old_libexec
    )
    candidate = ModernInstallation(
        installed.upgraded_install / "delegate", installed.upgraded_install / "delegate-run", lifetime.upgraded_libexec
    )
    install_contention_client(baseline.libexec / "pueue", installed.pueue_binary, controls)
    install_contention_client(candidate.libexec / "pueue", installed.pueue_binary, controls)
    tasks = NativeTaskOps(
        processes,
        baseline.delegate,
        baseline.runner,
        baseline.libexec / "pueue",
        lifetime.base,
        installed.state,
        response_directory,
        managed_runner=True,
    )
    fixture = ModernFixture(
        controls, installed.brief, installed.workspace, lifetime.gate, installed.waiter_directory, installed.lease_nonce
    )
    try:
        evidence = run_modern_binary_contention(tasks, baseline, candidate, fixture)
    finally:
        lifetime.attempted_task_ids[:] = sorted(tasks.dispatch_attempts)
        finish_controls(processes, ())
    select_installation(tasks, candidate)
    shutdown_private_supervisor(
        tasks.pueue,
        lifetime.private_config,
        lifetime.env,
        state=installed.state,
        task_ids=tuple(lifetime.attempted_task_ids),
        capture_root=lifetime.base_token,
    )
    run_receipt.add_details(modern_binary_contention=evidence)
    run_receipt.update_counts(
        dispatch_completed_count=len(tasks.tasks),
        admitted_task_count=len(tasks.tasks),
        running_task_count=0,
        live_lease_count=0,
        collected_task_count=len(tasks.tasks),
    )
    run_receipt.stage("modern-two-binary-contention-proved")
    return evidence
