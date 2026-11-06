"""Private CLI arguments and receipt destination validation."""

import argparse
from pathlib import Path


from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.delegate_acceptance.private_cli.fingerprints import REPOSITORY
from scripts.delegate_acceptance.private_cli.configuration import CAPACITY_TARGET_TASKS
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_DISPATCH_WORKERS
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_TASK_COUNT
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_TASK_COUNT
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_NOEXEC_STATE_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_NOEXEC_STATE_TASK_COUNT
from scripts.delegate_acceptance.private_cli.configuration import SUPERVISOR_UPGRADE_RECOVERY_SCENARIO
from scripts.delegate_acceptance.private_cli.contention_policy import (
    MODERN_CONTENTION_SCENARIO, MODERN_CONTENTION_TASK_COUNT,
)


def parse_arguments():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source-root", type=Path, default=REPOSITORY,
                        help="checkout to build directly into the owned baseline installation (defaults to this checkout)")
    parser.add_argument("--upgraded-source-root", type=Path,
                        help="checkout for a distinct upgraded installation; defaults to --source-root")
    for retired in ("delegate", "runner", "upgraded-delegate", "upgraded-runner"):
        parser.add_argument("--" + retired, type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--pueue", type=Path, required=True)
    parser.add_argument("--pueued", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument(
        "--scenario",
        choices=(
            "capacity",
            DEFAULT_STOP_SCENARIO,
            EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
            EXTERNAL_NOEXEC_STATE_SCENARIO,
            SUPERVISOR_UPGRADE_RECOVERY_SCENARIO,
            MODERN_CONTENTION_SCENARIO,
            "nested-survival",
        ),
        default="capacity",
    )
    parser.add_argument("--parallel-tasks", type=int, default=CAPACITY_TARGET_TASKS)
    parser.add_argument("--nested-topology", choices=("shared-warm", "other-warm", "other-cold"))
    parser.add_argument("--expected-backend", choices=("cgroup-v2", "process-group"))
    parser.add_argument("--native-fixture", type=Path, help="native lifetime test binary for nested survival")
    parser.add_argument(
        "--dispatch-workers",
        type=int,
        help="concurrent dispatch callers (defaults to 16; capped at --parallel-tasks; this load-generator setting is not a product task limit)",
    )
    parser.add_argument(
        "--runtime-only", action="store_true", help="exercise a root without a legacy model-discovery group journal"
    )
    parser.add_argument(
        "--noexec-state-parent", type=Path, help="caller-owned noexec directory that holds the scenario state root"
    )
    args = parser.parse_args()
    if any(getattr(args, name) is not None for name in
           ("delegate", "runner", "upgraded_delegate", "upgraded_runner")):
        parser.error("installed binary inputs are retired; select --source-root and optionally "
                     "--upgraded-source-root to build independent owned installations")
    _validate_scenario_arguments(args, parser)
    if args.dispatch_workers is not None and args.dispatch_workers <= 0:
        parser.error("--dispatch-workers must be positive")
    dispatch_workers = min(args.parallel_tasks, args.dispatch_workers or DEFAULT_DISPATCH_WORKERS)
    return args, dispatch_workers, parser


def _validate_scenario_arguments(args, parser):
    if (args.noexec_state_parent is not None) != (args.scenario == EXTERNAL_NOEXEC_STATE_SCENARIO):
        parser.error(
            "--noexec-state-parent is required by, and only valid for, --scenario %s" % EXTERNAL_NOEXEC_STATE_SCENARIO
        )
    nested_options = (args.nested_topology, args.expected_backend, args.native_fixture)
    if args.scenario == "nested-survival":
        if (not all(nested_options) or args.parallel_tasks != 3 or args.dispatch_workers != 1
                or not args.runtime_only or args.upgraded_source_root):
            parser.error("nested-survival requires topology/backend/native-fixture, three tasks, one dispatch worker, "
                         "runtime-only and the current installation")
    elif any(option is not None for option in nested_options):
        parser.error("nested topology/backend/native fixture options require nested-survival")
    elif args.scenario == MODERN_CONTENTION_SCENARIO:
        if (args.parallel_tasks != MODERN_CONTENTION_TASK_COUNT
                or args.upgraded_source_root is None):
            parser.error("modern-two-binary-contention requires two tasks and --upgraded-source-root")
    elif args.scenario == DEFAULT_STOP_SCENARIO:
        if args.parallel_tasks != DEFAULT_STOP_TASK_COUNT:
            parser.error(
                "--parallel-tasks must be exactly %d for the %s scenario"
                % (DEFAULT_STOP_TASK_COUNT, DEFAULT_STOP_SCENARIO)
            )
        if not args.runtime_only:
            parser.error("--scenario %s requires --runtime-only" % DEFAULT_STOP_SCENARIO)
        if args.upgraded_source_root is not None:
            parser.error("--scenario %s does not accept upgraded installation arguments" % DEFAULT_STOP_SCENARIO)
    elif args.scenario == EXTERNAL_INSPECTION_HANDOFF_SCENARIO:
        if args.parallel_tasks != EXTERNAL_INSPECTION_HANDOFF_TASK_COUNT:
            parser.error(
                "--parallel-tasks must be exactly %d for the %s scenario"
                % (EXTERNAL_INSPECTION_HANDOFF_TASK_COUNT, EXTERNAL_INSPECTION_HANDOFF_SCENARIO)
            )
        if args.runtime_only:
            parser.error(
                "--scenario %s requires an inspection journal and cannot be runtime-only"
                % EXTERNAL_INSPECTION_HANDOFF_SCENARIO
            )
        if args.upgraded_source_root is not None:
            parser.error(
                "--scenario %s does not accept upgraded installation arguments" % EXTERNAL_INSPECTION_HANDOFF_SCENARIO
            )
    elif args.scenario == EXTERNAL_NOEXEC_STATE_SCENARIO:
        _validate_noexec_state_arguments(args, parser)
    elif args.scenario == SUPERVISOR_UPGRADE_RECOVERY_SCENARIO:
        if args.parallel_tasks != 2 or args.runtime_only or args.upgraded_source_root:
            parser.error("supervisor-upgrade-recovery requires two tasks and the current packaged installation")
    elif args.parallel_tasks != CAPACITY_TARGET_TASKS:
        parser.error("--parallel-tasks must be exactly %d for the capacity proof" % CAPACITY_TARGET_TASKS)


def _validate_noexec_state_arguments(args, parser):
    if args.parallel_tasks != EXTERNAL_NOEXEC_STATE_TASK_COUNT:
        parser.error(
            "--parallel-tasks must be exactly %d for the %s scenario"
            % (EXTERNAL_NOEXEC_STATE_TASK_COUNT, EXTERNAL_NOEXEC_STATE_SCENARIO)
        )
    if args.runtime_only or args.upgraded_source_root is not None:
        parser.error(
            "--scenario %s requires an inspection journal and the current installation only"
            % EXTERNAL_NOEXEC_STATE_SCENARIO
        )


def prepare_receipt(args, dispatch_workers, parser):
    output = args.output.resolve()
    response_directory = output.with_name(output.name + ".responses")
    if output.exists() or output.is_symlink():
        raise RuntimeError("private CLI acceptance output already exists: " + str(output))
    if response_directory.exists() or response_directory.is_symlink():
        raise RuntimeError("private CLI response destination already exists: " + str(response_directory))
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)

    legacy_mode = (args.scenario != MODERN_CONTENTION_SCENARIO
                   and args.upgraded_source_root is not None)
    if legacy_mode and args.runtime_only:
        parser.error("--runtime-only requires the current-installation acceptance scenario")
    queued_runner_check = (
        "the legacy CLI queued a task whose runner path pointed into its install"
        if legacy_mode
        else "the CLI queued a task using its selected installed runner"
    )
    inspection_runner_check = (
        "the upgraded CLI preserved an ambiguous legacy inspection runner and completed it before cleanup"
        if legacy_mode
        else "the upgraded CLI retained and completed an explicitly managed installed inspection runner"
    )
    binary_sources = {
        "pueue": args.pueue,
        "pueued": args.pueued,
    }
    if args.native_fixture is not None:
        binary_sources["native-fixture"] = args.native_fixture
    run_receipt = AcceptanceReceipt(output, args.parallel_tasks, dispatch_workers, binary_sources)
    run_receipt.add_details(scenario=args.scenario, candidate_sources={
        "baseline": str(args.source_root), "upgraded": str(args.upgraded_source_root or args.source_root),
    })
    return (legacy_mode, queued_runner_check, inspection_runner_check, response_directory, run_receipt)
