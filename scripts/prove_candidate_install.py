#!/usr/bin/env python3
"""Prove one unpublished candidate installs on this host through exactly one channel."""

from __future__ import annotations

import argparse
from pathlib import Path
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.release_verification.assets import GITHUB_AUTH_MODES
from scripts.release_verification.candidate_install_proof import CHANNELS
from scripts.release_verification.candidate_install_proof import ProofRequest
from scripts.release_verification.candidate_install_proof import prove_candidate_install
from scripts.acceptance.evidence.source import VerificationError


# The executing checkout is the one this entry point runs from; it is never an input.
CHECKOUT = Path(__file__).resolve().parent.parent


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifact-id", type=int, required=True)
    parser.add_argument("--channel", choices=tuple(CHANNELS), required=True)
    parser.add_argument("--output-dir", type=Path, required=True,
                        help="absolute path of a new directory this invocation creates and owns")
    parser.add_argument("--github-auth", choices=GITHUB_AUTH_MODES, default="environment")
    parser.add_argument("--bootstrap-parent", type=Path,
                        help="existing short directory for the bootstrap probe's private root (default: HOME)")
    parser.add_argument("--bootstrap-cpu-guard", choices=("true", "false"), default="true",
                        help="check CPU pressure for the installed bootstrap task (default: true)")
    return parser


def main(argv: list[str] | None = None) -> int:
    arguments = _parser().parse_args(argv)
    request = ProofRequest(
        artifact_id=arguments.artifact_id, channel=CHANNELS[arguments.channel], output_dir=arguments.output_dir,
        checkout=CHECKOUT, github_auth=arguments.github_auth, bootstrap_parent=arguments.bootstrap_parent,
        bootstrap_cpu_guard=arguments.bootstrap_cpu_guard == "true")
    try:
        return prove_candidate_install(request)
    except VerificationError as error:
        print(f"candidate install proof failed: {error.code} ({error.stage})", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
