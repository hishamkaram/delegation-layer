"""Scheduler acceptance composition; existing fixture owners retain cleanup authority.

Managed-host ``all`` includes successive, overlap-stop and fresh-default.
Service ``all`` retains successive and overlap-stop only.
"""

import argparse
from dataclasses import dataclass, replace
from pathlib import Path
import tempfile
import time
import uuid

from scripts.delegate_acceptance.directories.operations import DirectoryOwner, release_owned_directories
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.finalization import finalize_fixture
from scripts.delegate_acceptance.private_cli.fingerprints import REPOSITORY
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli.fixture_setup import prepare_fixture
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.delegate_acceptance.private_cli.scheduler_cases import (
    CASE_SECONDS, SUCCESSIVE_SECONDS, SchedulerCase, run_overlap_stop,
)
from scripts.delegate_acceptance.private_cli.scheduler_sequence import require_overlap_history, run_successive
from scripts.delegate_acceptance.private_cli.scheduler_host import SchedulerHost
from scripts.delegate_acceptance.private_cli.scheduler_default import (
    bind_default_fixture, delete_default_schedule, run_fresh_default,
)
from scripts.delegate_acceptance.private_cli.scheduler_service import SchedulerService
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor


@dataclass(frozen=True)
class SchedulerArguments:
    delegate: Path
    runner: Path
    pueue: Path
    pueued: Path
    output: Path | None
    scenario: str = "all"
    hosting: str = "managed-host"
    # prepare_fixture consumes the same installation options as the existing
    # private CLI cases. Scheduler acceptance never selects a legacy upgrade.
    upgraded_delegate: None = None
    upgraded_runner: None = None
    noexec_state_parent: None = None


def parse_arguments(argv: list[str] | None = None) -> SchedulerArguments:
    parser = argparse.ArgumentParser(description="Native scheduler acceptance with real minute ticks and a finite pi fixture.")
    parser.add_argument("--delegate", type=Path, default=REPOSITORY / "bin" / "delegate")
    parser.add_argument("--runner", type=Path, default=REPOSITORY / "bin" / "delegate-run")
    parser.add_argument("--pueue", type=Path, default=REPOSITORY / "bin" / "test-supervisor" / "pueue")
    parser.add_argument("--pueued", type=Path, default=REPOSITORY / "bin" / "test-supervisor" / "pueued")
    parser.add_argument("--output", type=Path, help="fresh private output directory; default: a new directory under HOME")
    parser.add_argument(
        "--scenario", choices=("all", "successive", "overlap-stop", "fresh-default"), default="all",
        help="all runs successive, overlap-stop and fresh-default with managed-host; service all runs the first two only",
    )
    parser.add_argument("--hosting", choices=("managed-host", "service"), default="managed-host")
    args = parser.parse_args(argv)
    if args.scenario == "fresh-default" and args.hosting != "managed-host":
        parser.error("fresh-default requires --hosting managed-host")
    return SchedulerArguments(args.delegate, args.runner, args.pueue, args.pueued, args.output,
                              scenario=args.scenario, hosting=args.hosting)


def output_directory(requested: Path | None, owner: DirectoryOwner) -> Path:
    if requested is None:
        parent = Path.home().resolve(strict=True)
        created = Path(tempfile.mkdtemp(prefix="delegate-scheduler-output-", dir=parent)).resolve(strict=True)
        return owner.adopt_created("scheduler acceptance output", created, parent).directory
    path = requested.absolute()
    parent = path.parent.resolve(strict=True)
    return owner.create("scheduler acceptance output", parent / path.name, parent).directory


def retain_unknown_service(lifetime: FixtureLifetime, receipt: AcceptanceReceipt, error: BaseException) -> None:
    """A service/occurrence uncertainty forbids daemon shutdown and directory deletion."""
    faults = [type(error).__name__]
    if lifetime.release_gate is not None:
        try:
            lifetime.release_gate.release()
        except BaseException as release_error:
            faults.append(type(release_error).__name__)
    receipt.add_details(cleanup_uncertain=True, cleanup_error_types=faults,
                        retained_fixture=str(lifetime.base) if lifetime.base else None)
    receipt.write("FAIL", {"status": "retained", "host_closure": "unknown", "service_closure": "unknown"}, error)
    # These operations only close identity tokens and keep all filesystem
    # content, including service payload and captures, available to the caller.
    try:
        lifetime.ownership.release()
    finally:
        release_owned_directories(lifetime.old_layout_owned)


def retire_case_services(case: SchedulerCase) -> None:
    """Every owned generation gets a retirement attempt, including on failure."""
    errors: list[BaseException] = []
    for service in (case.service, *case.previous_services):
        service.deadline = time.monotonic() + 30
        try:
            service.retire()
        except BaseException as error:
            errors.append(error)
    if errors:
        raise BaseExceptionGroup("scheduler service retirement failed", errors)


def stop_case_hosts(case: SchedulerCase) -> None:
    """Join every generation before discovery, supervisor cleanup or deletion."""
    deadline = time.monotonic() + 45
    errors: list[BaseException] = []
    for service in (case.service, *case.previous_services):
        service.deadline = deadline
        try:
            service.stop()
        except BaseException as error:
            errors.append(error)
    if errors:
        raise BaseExceptionGroup("scheduler hosting closure failed", errors)


def finish_case(*, args: SchedulerArguments, case: SchedulerCase | None, lifetime: FixtureLifetime,
                receipt: AcceptanceReceipt, failure: BaseException | None) -> None:
    if case is not None and case.default_root:
        case.deadline = time.monotonic() + 45
        try:
            delete_default_schedule(case)
        except BaseException as cleanup_error:
            failure = cleanup_error if failure is None else BaseExceptionGroup(
                "scheduler case and definition cleanup failed", [failure, cleanup_error]
            )
    try:
        if case is not None:
            stop_case_hosts(case)
            case.discover()
    except BaseException as cleanup_error:
        if failure is not None:
            cleanup_error = BaseExceptionGroup("scheduler case and closure failed", [failure, cleanup_error])
        retain_unknown_service(lifetime, receipt, cleanup_error)
        raise RuntimeError("scheduler hosting or occurrence discovery remains uncertain; fixture retained") from cleanup_error
    if failure is None and case is not None:
        try:
            if lifetime.private_config is None or lifetime.old_libexec is None:
                raise RuntimeError("scheduler fixture lost its private supervisor cleanup binding")
            receipt.phase("canonical_cleanup")
            shutdown_private_supervisor(
                lifetime.old_libexec / "pueue", lifetime.private_config, lifetime.env,
                state=case.fixture.state, task_ids=tuple(lifetime.attempted_task_ids), capture_root=lifetime.capture_token,
            )
            retire_case_services(case)
        except BaseException as cleanup_error:
            failure = cleanup_error
    finalize_fixture(args, failure, lifetime, receipt, failure is None)
    if failure is not None:
        raise failure


def run_case(args: SchedulerArguments, output: Path, name: str) -> None:
    if name == "fresh-default" and args.hosting != "managed-host":
        raise AcceptanceBlocked("fresh-default-requires-managed-host")
    selected = replace(args, output=output / (name + ".json"), scenario=name)
    receipt = AcceptanceReceipt(selected.output, 3 if name == "successive" else 1, 1,
                                {"delegate": args.delegate, "delegate-run": args.runner,
                                 "pueue": args.pueue, "pueued": args.pueued})
    receipt.add_details(scenario="scheduler-" + name, acceptance_scope="native fixture; no AI account", hosting=args.hosting)
    lifetime = FixtureLifetime()
    case = None
    failure = None
    try:
        responses = output / (name + ".responses")
        fixture = prepare_fixture(selected, uuid.uuid4().hex, False, lifetime, responses, receipt)
        if name == "fresh-default":
            fixture = bind_default_fixture(fixture, lifetime)
        if lifetime.old_install is None:
            raise RuntimeError("scheduler fixture lost its installed executable")
        deadline = time.monotonic() + (SUCCESSIVE_SECONDS if name == "successive" else CASE_SECONDS)
        hosting_type = SchedulerHost if args.hosting == "managed-host" else SchedulerService
        if name == "fresh-default":
            service = SchedulerHost(lifetime, fixture.state, lifetime.old_install / "delegate", deadline, default_root=True)
        else:
            service = hosting_type(lifetime, fixture.state, lifetime.old_install / "delegate", deadline)
        case = SchedulerCase(fixture, lifetime, responses, receipt, service, deadline, default_root=name == "fresh-default")
        if isinstance(service, SchedulerService):
            receipt.add_details(service={"host": service.system, "unit": service.unit, "target": service.target,
                                         "files": str(service.directory)})
        if name == "fresh-default":
            run_fresh_default(case)
        elif name == "successive":
            run_successive(case)
        else:
            run_overlap_stop(case)
            require_overlap_history(case)
        case.discover()
        receipt.update_counts(admitted_task_count=len(lifetime.attempted_task_ids))
        receipt.stage("scheduler-native-case-proved")
    except BaseException as error:
        failure = error
        receipt.add_details(failure_phase=receipt.current_phase)
    finally:
        finish_case(args=selected, case=case, lifetime=lifetime, receipt=receipt, failure=failure)


def main(argv: list[str] | None = None) -> int:
    args = parse_arguments(argv)
    owner = DirectoryOwner()
    output = output_directory(args.output, owner)
    print("Scheduler acceptance output: " + str(output), flush=True)
    try:
        scenarios = ("successive", "overlap-stop")
        if args.scenario != "all":
            scenarios = (args.scenario,)
        elif args.hosting == "managed-host":
            scenarios += ("fresh-default",)
        for name in scenarios:
            run_case(args, output, name)
    except AcceptanceBlocked as error:
        print("BLOCKED scheduler acceptance: " + error.code, flush=True)
        return 2
    except BaseException as error:
        print("FAIL scheduler acceptance: " + type(error).__name__ + "; inspect private output", flush=True)
        return 1
    finally:
        owner.release()
    return 0
