#!/usr/bin/env python3
"""Publish one frozen development candidate's exact bytes from the trusted-main release workflow.

The nine dispatch inputs are read from the environment variables named by
their upper-cased input names. They and this run's identity are validated
before any file, process or network effect; a refusal writes no receipt.
"""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.native_release.contract import ReleaseError
from scripts.delegate_acceptance.native_release.publication_contract import INPUT_NAMES, parse_dispatch_inputs
from scripts.delegate_acceptance.release_verification.candidate_publication_delivery import (
    HostedPublication, hosted_run, publish_candidate,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.published_ports import default_ports


# The executing checkout is the one this entry point runs from; it is never an input.
CHECKOUT = Path(__file__).resolve().parent.parent


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, required=True,
                        help="absolute path of a new directory this invocation creates and owns")
    return parser


def dispatch_inputs(environment: dict[str, str]) -> dict[str, str]:
    """The present dispatch input variables; the shared parser refuses a missing or extra one."""
    return {name: environment[name.upper()] for name in INPUT_NAMES if name.upper() in environment}


def main(argv: list[str] | None = None) -> int:
    arguments = _parser().parse_args(argv)
    environment = dict(os.environ)
    try:
        dispatch = parse_dispatch_inputs(dispatch_inputs(environment))
    except ReleaseError as error:
        print(f"candidate publication refused: {error.diagnostic}", file=sys.stderr)
        return 1
    try:
        request = HostedPublication(dispatch=dispatch, run=hosted_run(environment, dispatch),
                                    output_dir=arguments.output_dir, checkout=CHECKOUT)
        code = publish_candidate(request, environment=environment, ports=default_ports())
    except VerificationError as error:
        print(f"candidate publication refused: {error.code} ({error.stage})", file=sys.stderr)
        return 1
    if code:
        print("candidate publication failed; the receipt records the sanitized failure", file=sys.stderr)
    return code


if __name__ == "__main__":
    raise SystemExit(main())
