"""Compose the common finite successful turn before specialized checks."""

from scripts.delegate_acceptance.hermetic.evidence import require_provider_evidence
from scripts.delegate_acceptance.hermetic.evidence import require_terminal_evidence


def complete_success_case(case, provider_expected=0):
    case.dispatch_dynamic(status="queued")
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    response = case.finish_runner(runner, expected=(0,))
    case.set_status("ended")
    require_provider_evidence(case, provider_expected)
    require_terminal_evidence(case, response, provider_expected)
    return response
