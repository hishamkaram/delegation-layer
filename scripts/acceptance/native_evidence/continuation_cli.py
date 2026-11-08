#!/usr/bin/env python3
"""Verify actual public-continue and Pi session-miss behavior from retained native evidence."""

from __future__ import annotations

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[3])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]


import argparse
import json
import os
from typing import Mapping


from scripts.acceptance.evidence.bounded_files import BoundedReadError
from scripts.acceptance.native_evidence.contract import EvidenceError
from scripts.acceptance.native_evidence.supplemental import SupplementalSummary
from scripts.acceptance.native_evidence.supplemental import validate_supplemental
from scripts.acceptance.evidence.source import VerificationError


def verify_native_continuation(*, supplemental_root: Path, canonical_aggregate: Path,
                               source_revision: str | None, candidate_delegate: Path, pi_control: Path,
                               environment: Mapping[str, str], checkout: Path) -> SupplementalSummary:
    """Observe existing product evidence; no ledger, launch, write or cleanup."""
    return validate_supplemental(
        supplemental_root=supplemental_root, canonical_aggregate=canonical_aggregate,
        source_revision=source_revision, candidate_delegate=candidate_delegate, pi_control=pi_control,
        environment=environment, checkout=checkout)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog='acceptance-native-evidence', description=__doc__)
    for name in ("supplemental-root", "canonical-aggregate", "source-revision", "candidate-delegate", "pi-control"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args(argv)
    try:
        summary = verify_native_continuation(
            supplemental_root=Path(args.supplemental_root), canonical_aggregate=Path(args.canonical_aggregate),
            source_revision=args.source_revision, candidate_delegate=Path(args.candidate_delegate),
            pi_control=Path(args.pi_control), environment=os.environ,
            checkout=Path(__file__).resolve().parents[3])
    except (EvidenceError, VerificationError, BoundedReadError, OSError, ValueError) as error:
        print(f"native continuation verification failed: {error}", file=sys.stderr)
        return 1
    print(json.dumps(summary, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
