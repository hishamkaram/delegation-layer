#!/usr/bin/env python3
"""Validate public native proofs using protected-main release policy."""

from __future__ import annotations

import argparse
from pathlib import Path
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.native_hosted import (
    GITHUB_AUTH_MODES, validate_promotion, validate_public_prerequisite, validate_submission,
)


PUBLIC_POLICY_CODE = "anonymous_installation_requires_public_repository"


def public_prerequisite(args: argparse.Namespace) -> int:
    try:
        return validate_public_prerequisite(args)
    except VerificationError as error:
        print(f"Public repository prerequisite failed: {error.code} ({error.stage})", file=sys.stderr)
        if error.code == PUBLIC_POLICY_CODE:
            print("Make the repository public, then rerun this release workflow.", file=sys.stderr)
        return 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    validate = commands.add_parser("validate")
    validate.add_argument("--tag", required=True)
    validate.add_argument("--asset-id", type=int, required=True)
    validate.add_argument("--proof-sha256", required=True)
    validate.add_argument("--invocation-id", required=True)
    validate.add_argument("--opencode-model", required=True)
    validate.add_argument("--ccr-model", default="")
    validate.add_argument("--report", required=True)
    promote = commands.add_parser("promotion")
    promote.add_argument("--tag", required=True)
    promote.add_argument("--repo", required=True)
    promote.add_argument("--source-revision", required=True)
    promote.add_argument("--native-validation-run-id", type=int, required=True)
    promote.add_argument("--work-dir", required=True)
    promote.add_argument("--github-auth", choices=GITHUB_AUTH_MODES, default="environment")
    public = commands.add_parser("public-prerequisite")
    public.add_argument("--repo", required=True)
    public.add_argument("--github-auth", choices=GITHUB_AUTH_MODES, default="environment")
    args = parser.parse_args(argv)
    if args.command == "public-prerequisite":
        return public_prerequisite(args)
    try:
        if args.command == "validate":
            return validate_submission(args)
        if args.command == "promotion":
            return validate_promotion(args)
    except VerificationError as error:
        print(f"Native proof validation failed: {error.code} ({error.stage})", file=sys.stderr)
        return 1
    parser.error(f"unsupported command: {args.command}")


if __name__ == "__main__":
    raise SystemExit(main())
