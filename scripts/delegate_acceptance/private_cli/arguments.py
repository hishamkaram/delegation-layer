"""Private CLI arguments and receipt destination validation."""

import argparse
from pathlib import Path


from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.delegate_acceptance.private_cli.configuration import CAPACITY_TARGET_TASKS
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_DISPATCH_WORKERS
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_TASK_COUNT
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_TASK_COUNT
from scripts.delegate_acceptance.private_cli.configuration import SUPERVISOR_UPGRADE_RECOVERY_SCENARIO
from scripts.delegate_acceptance.private_cli.contention_policy import (
    MODERN_CONTENTION_SCENARIO, MODERN_CONTENTION_TASK_COUNT,
)


def parse_arguments():
    parser = argparse.ArgumentParser()
    parser.add_argument("--delegate", type=Path, required=True)
    parser.add_argument("--runner", type=Path, required=True)
    parser.add_argument("--upgraded-delegate", type=Path)
    parser.add_argument("--upgraded-runner", type=Path)
    parser.add_argument("--pueue", type=Path, required=True)
    parser.add_argument("--pueued", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument(
        "--scenario",
        choices=(
            "capacity",
            DEFAULT_STOP_SCENARIO,
            EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
            SUPERVISOR_UPGRADE_RECOVERY_SCENARIO,
            MODERN_CONTENTION_SCENARIO,
        ),
        default="capacity",
    )
    parser.add_argument("--parallel-tasks", type=int, default=CAPACITY_TARGET_TASKS)
    parser.add_argument(
        "--dispatch-workers",
        type=int,
        help="concurrent dispatch callers (defaults to 16; capped at --parallel-tasks; this load-generator setting is not a product task limit)",
    )
    parser.add_argument(
        "--runtime-only", action="store_true", help="exercise a root without a legacy model-discovery group journal"
    )
    args = parser.parse_args()
    if args.scenario == MODERN_CONTENTION_SCENARIO:
        if (args.parallel_tasks != MODERN_CONTENTION_TASK_COUNT
                or args.upgraded_delegate is None or args.upgraded_runner is None):
            parser.error("modern-two-binary-contention requires two tasks and both candidate binaries")
    elif args.scenario == DEFAULT_STOP_SCENARIO:
        if args.parallel_tasks != DEFAULT_STOP_TASK_COUNT:
            parser.error(
                "--parallel-tasks must be exactly %d for the %s scenario"
                % (DEFAULT_STOP_TASK_COUNT, DEFAULT_STOP_SCENARIO)
            )
        if not args.runtime_only:
            parser.error("--scenario %s requires --runtime-only" % DEFAULT_STOP_SCENARIO)
        if args.upgraded_delegate is not None or args.upgraded_runner is not None:
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
        if args.upgraded_delegate is not None or args.upgraded_runner is not None:
            parser.error(
                "--scenario %s does not accept upgraded installation arguments" % EXTERNAL_INSPECTION_HANDOFF_SCENARIO
            )
    elif args.scenario == SUPERVISOR_UPGRADE_RECOVERY_SCENARIO:
        if args.parallel_tasks != 2 or args.runtime_only or args.upgraded_delegate or args.upgraded_runner:
            parser.error("supervisor-upgrade-recovery requires two tasks and the current packaged installation")
    elif args.parallel_tasks != CAPACITY_TARGET_TASKS:
        parser.error("--parallel-tasks must be exactly %d for the capacity proof" % CAPACITY_TARGET_TASKS)
    if args.dispatch_workers is not None and args.dispatch_workers <= 0:
        parser.error("--dispatch-workers must be positive")
    dispatch_workers = min(args.parallel_tasks, args.dispatch_workers or DEFAULT_DISPATCH_WORKERS)
    return args, dispatch_workers, parser


def prepare_receipt(args, dispatch_workers, parser):
    output = args.output.resolve()
    response_directory = output.with_name(output.name + ".responses")
    if output.exists() or output.is_symlink():
        raise RuntimeError("private CLI acceptance output already exists: " + str(output))
    if response_directory.exists() or response_directory.is_symlink():
        raise RuntimeError("private CLI response destination already exists: " + str(response_directory))
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)

    legacy_mode = (args.scenario != MODERN_CONTENTION_SCENARIO
                   and args.upgraded_delegate is not None and args.upgraded_runner is not None)
    if legacy_mode and args.runtime_only:
        parser.error("--runtime-only requires the current-installation acceptance scenario")
    queued_runner_check = (
        "the legacy CLI queued a task whose runner path pointed into its install"
        if legacy_mode
        else "the CLI queued a task whose runner path pointed to the durable state root"
    )
    inspection_runner_check = (
        "the upgraded CLI preserved an ambiguous legacy inspection runner and completed it before cleanup"
        if legacy_mode
        else "the upgraded CLI retained and completed an explicitly managed state-root inspection runner"
    )
    binary_sources = {
        "delegate": args.delegate,
        "delegate-run": args.runner,
        "pueue": args.pueue,
        "pueued": args.pueued,
    }
    if args.upgraded_delegate is not None:
        binary_sources["upgraded-delegate"] = args.upgraded_delegate
    if args.upgraded_runner is not None:
        binary_sources["upgraded-delegate-run"] = args.upgraded_runner
    run_receipt = AcceptanceReceipt(output, args.parallel_tasks, dispatch_workers, binary_sources)
    run_receipt.add_details(scenario=args.scenario)
    return (legacy_mode, queued_runner_check, inspection_runner_check, response_directory, run_receipt)
