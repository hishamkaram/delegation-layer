#!/usr/bin/env python3
"""Publish the exact CI artifact selected by the trusted main release workflow."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import re
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.release_verification.candidate_publication_delivery import HostedPublication
from scripts.release_verification.candidate_publication_delivery import hosted_run
from scripts.release_verification.candidate_publication_delivery import publish_candidate
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.published_ports import default_ports


CHECKOUT = Path(__file__).resolve().parent.parent


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, required=True)
    arguments = parser.parse_args(argv)
    environment = dict(os.environ)
    try:
        identity, resume = environment.get("ARTIFACT_ID", ""), environment.get("RESUME_PUBLISHED")
        if re.fullmatch(r"[1-9][0-9]{0,17}", identity) is None or resume not in ("true", "false"):
            raise VerificationError("publication_input_invalid", "publication-input")
        request = HostedPublication(artifact_id=int(identity), resume_published=resume == "true",
                                    run=hosted_run(environment), output_dir=arguments.output_dir, checkout=CHECKOUT)
        return publish_candidate(request, environment=environment, ports=default_ports())
    except VerificationError as error:
        print(f"distribution publication refused: {error.code} ({error.stage})", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
