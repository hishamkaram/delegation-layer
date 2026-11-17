"""Private CLI argument boundary and finite acceptance orchestration."""

from contextlib import contextmanager
import signal
import uuid

from scripts.acceptance.directories.operations import DirectoryOwner


from scripts.acceptance.private_cli.configuration import DEFAULT_STOP_SCENARIO
from scripts.acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_SCENARIO
from scripts.acceptance.private_cli.configuration import EXTERNAL_NOEXEC_STATE_SCENARIO
from scripts.acceptance.private_cli.configuration import SUPERVISOR_UPGRADE_RECOVERY_SCENARIO
from scripts.acceptance.private_cli.contention.controls import run_prepared_modern_contention
from scripts.acceptance.private_cli.contention.policy import MODERN_CONTENTION_SCENARIO


from scripts.acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.acceptance.private_cli.finalization import finalize_fixture
from scripts.acceptance.private_cli.arguments import parse_arguments
from scripts.acceptance.private_cli.fixture_setup import prepare_fixture
from scripts.acceptance.private_cli.initial import prepare_initial_observations
from scripts.acceptance.private_cli.arguments import prepare_receipt
from scripts.acceptance.private_cli.capacity.entry import run_capacity_scenario
from scripts.acceptance.private_cli.scenario_entry import run_handoff_scenarios
from scripts.acceptance.private_cli.scenario_entry import run_noexec_state_scenario
from scripts.acceptance.private_cli.legacy import run_legacy_scenario
from scripts.acceptance.private_cli.scenario_entry import run_recovery_scenario
from scripts.acceptance.private_cli.scenario_entry import run_stop_scenario
from scripts.acceptance.testing.fixture_cleanup import closing_preserving_primary


class _ShutdownState:
    """One invocation owns signal routing; cleanup and receipts only read its first interruption."""

    def __init__(self):
        self.interruption: KeyboardInterrupt | SystemExit | None = None
        self.cleaning = False
        self.cleanup_primary: BaseException | None = None

    def begin_cleanup(self, primary: BaseException | None):
        self.cleaning = True
        self.cleanup_primary = primary

    def current_failure(self, primary: BaseException | None) -> BaseException | None:
        return primary if primary is not None else self.interruption


@contextmanager
def _shutdown_signals():
    """Raise one interruption before cleanup, or defer it until once-only cleanup finishes."""
    shutdown = _ShutdownState()
    previous = []

    def interrupt(number, _frame):
        if shutdown.interruption is not None:
            return
        shutdown.interruption = KeyboardInterrupt() if number == signal.SIGINT else SystemExit(128 + number)
        if not shutdown.cleaning:
            raise shutdown.interruption

    try:
        for number in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            previous.append((number, signal.signal(number, interrupt)))
        try:
            yield shutdown
        except BaseException as error:
            if (shutdown.cleaning and shutdown.cleanup_primary is None
                    and shutdown.interruption is not None and error is not shutdown.interruption):
                raise shutdown.interruption from error
            raise
        else:
            if shutdown.interruption is not None:
                raise shutdown.interruption
    finally:
        for number, handler in reversed(previous):
            signal.signal(number, handler)


def main():
    with _shutdown_signals() as shutdown:
        _run_fixture(shutdown)


def _run_fixture(shutdown: _ShutdownState):
    args, dispatch_workers, parser = parse_arguments()
    legacy_mode, queued_runner_check, inspection_runner_check, response_directory, run_receipt = prepare_receipt(
        args, dispatch_workers, parser
    )
    if args.scenario == "nested-refused-leader":
        from scripts.acceptance.private_cli.nested.refusal import run_refused_leader
        run_refused_leader(args, response_directory, run_receipt,
                          begin_cleanup=shutdown.begin_cleanup, current_failure=shutdown.current_failure)
        return
    lifetime = FixtureLifetime(DirectoryOwner(), DirectoryOwner())
    success = False
    failure = None
    lease_nonce = uuid.uuid4().hex
    fixture = None
    try:
        fixture = prepare_fixture(args, lease_nonce, legacy_mode, lifetime, response_directory, run_receipt)
        if args.scenario == "teams":
            from scripts.acceptance.private_cli.teams.scenario import run_teams_scenario
            run_teams_scenario(lifetime, fixture, response_directory, run_receipt)
            success = True
            return
        if args.scenario == "nested-survival":
            from scripts.acceptance.private_cli.nested.workflow import run_nested_survival
            run_nested_survival(args, lifetime, fixture, response_directory, run_receipt)
            success = True
            return
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

        if args.scenario == EXTERNAL_NOEXEC_STATE_SCENARIO:
            run_noexec_state_scenario(lease_nonce, lifetime, response_directory, run_receipt, fixture)
            success = True
            return

        initial = prepare_initial_observations(args, legacy_mode, lifetime, response_directory, run_receipt, fixture)
        if not legacy_mode:
            run_capacity_scenario(
                args=args, dispatch_workers=dispatch_workers, lease_nonce=lease_nonce, lifetime=lifetime,
                response_directory=response_directory, run_receipt=run_receipt, fixture=fixture, initial=initial
            )
            success = True
            return
        run_legacy_scenario(
            inspection_runner_check=inspection_runner_check,
            legacy_mode=legacy_mode,
            lifetime=lifetime,
            queued_runner_check=queued_runner_check,
            response_directory=response_directory,
            run_receipt=run_receipt,
            fixture=fixture,
            initial=initial,
        )
        success = True
    except BaseException as error:
        failure = error
        run_receipt.add_details(failure_phase=run_receipt.current_phase)
        raise
    finally:
        shutdown.begin_cleanup(failure)
        with closing_preserving_primary(lambda: finalize_fixture(args, failure, lifetime, run_receipt, success,
                                                                current_failure=shutdown.current_failure)):
            if failure is not None:
                raise failure
