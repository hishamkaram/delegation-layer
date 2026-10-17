"""Private CLI argument boundary and finite acceptance orchestration."""

import uuid

from scripts.delegate_acceptance.directories.operations import DirectoryOwner


from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import SUPERVISOR_UPGRADE_RECOVERY_SCENARIO
from scripts.delegate_acceptance.private_cli.contention_controls import run_prepared_modern_contention
from scripts.delegate_acceptance.private_cli.contention_policy import MODERN_CONTENTION_SCENARIO


from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli.finalization import finalize_fixture
from scripts.delegate_acceptance.private_cli.arguments import parse_arguments
from scripts.delegate_acceptance.private_cli.fixture_setup import prepare_fixture
from scripts.delegate_acceptance.private_cli.initial import prepare_initial_observations
from scripts.delegate_acceptance.private_cli.arguments import prepare_receipt
from scripts.delegate_acceptance.private_cli.capacity_entry import run_capacity_scenario
from scripts.delegate_acceptance.private_cli.scenario_entry import run_handoff_scenarios
from scripts.delegate_acceptance.private_cli.legacy import run_legacy_scenario
from scripts.delegate_acceptance.private_cli.scenario_entry import run_recovery_scenario
from scripts.delegate_acceptance.private_cli.scenario_entry import run_stop_scenario


def main():
    args, dispatch_workers, parser = parse_arguments()
    legacy_mode, queued_runner_check, inspection_runner_check, response_directory, run_receipt = prepare_receipt(
        args, dispatch_workers, parser
    )
    lifetime = FixtureLifetime(DirectoryOwner(), DirectoryOwner())
    success = False
    failure = None
    lease_nonce = uuid.uuid4().hex
    fixture = None
    try:
        fixture = prepare_fixture(args, lease_nonce, legacy_mode, lifetime, response_directory, run_receipt)
        if args.scenario == MODERN_CONTENTION_SCENARIO:
            run_prepared_modern_contention(lifetime, fixture, response_directory, run_receipt)
            success = True
            return
        if args.scenario == SUPERVISOR_UPGRADE_RECOVERY_SCENARIO:
            run_recovery_scenario(lease_nonce, lifetime, response_directory, run_receipt, fixture)
            success = True
            return

        if args.scenario == DEFAULT_STOP_SCENARIO:
            run_stop_scenario(lease_nonce, lifetime, response_directory, run_receipt, fixture)
            success = True
            return

        if args.scenario == EXTERNAL_INSPECTION_HANDOFF_SCENARIO:
            run_handoff_scenarios(lease_nonce, lifetime, response_directory, run_receipt, fixture)
            success = True
            return

        initial = prepare_initial_observations(args, legacy_mode, lifetime, response_directory, run_receipt, fixture)
        if not legacy_mode:
            run_capacity_scenario(
                args, dispatch_workers, lease_nonce, lifetime, response_directory, run_receipt, fixture, initial
            )
            success = True
            return
        run_legacy_scenario(
            inspection_runner_check,
            legacy_mode,
            lifetime,
            queued_runner_check,
            response_directory,
            run_receipt,
            fixture,
            initial,
        )
        success = True
    except BaseException as error:
        failure = error
        run_receipt.add_details(failure_phase=run_receipt.current_phase)
        raise
    finally:
        finalize_fixture(args, failure, lifetime, run_receipt, success)
