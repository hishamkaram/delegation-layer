#!/usr/bin/env python3
"""Prove one unpublished candidate installs on this host through exactly one channel."""

from __future__ import annotations

import argparse
from pathlib import Path
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.release_verification.assets import GITHUB_AUTH_MODES
from scripts.delegate_acceptance.release_verification.candidate_install_proof import (
    CHANNELS, ProofRequest, prove_candidate_install,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError


# The executing checkout is the one this entry point runs from; it is never an input.
CHECKOUT = Path(__file__).resolve().parent.parent


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--locator", type=Path, required=True)
    parser.add_argument("--channel", choices=tuple(CHANNELS), required=True)
    parser.add_argument("--output-dir", type=Path, required=True,
                        help="absolute path of a new directory this invocation creates and owns")
    parser.add_argument("--github-auth", choices=GITHUB_AUTH_MODES, default="environment")
    return parser


def main(argv: list[str] | None = None) -> int:
    arguments = _parser().parse_args(argv)
    request = ProofRequest(
        locator=arguments.locator, channel=CHANNELS[arguments.channel], output_dir=arguments.output_dir,
        checkout=CHECKOUT, github_auth=arguments.github_auth)
    try:
        return prove_candidate_install(request)
    except VerificationError as error:
        print(f"candidate install proof failed: {error.code} ({error.stage})", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
