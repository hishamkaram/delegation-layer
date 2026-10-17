"""Cli for exact release verification."""

from __future__ import annotations

import argparse
from .verification import (finalize_native, verify)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    verify_parser = commands.add_parser("verify")
    verify_parser.add_argument("--mode", choices=("archive-smoke", "native-acceptance"), required=True)
    verify_parser.add_argument("--assets-dir", required=True)
    verify_parser.add_argument("--extract-dir", required=True)
    verify_parser.add_argument("--receipt", required=True)
    verify_parser.add_argument("--tag", required=True)
    verify_parser.add_argument("--repo", required=True)
    verify_parser.add_argument("--source-revision", required=True)
    final_parser = commands.add_parser("finalize-native")
    final_parser.add_argument("--archive-receipt", required=True)
    final_parser.add_argument("--aggregate-receipt", required=True)
    final_parser.add_argument("--assets-dir", required=True)
    final_parser.add_argument("--extract-dir", required=True)
    final_parser.add_argument("--receipt", required=True)
    final_parser.add_argument("--exit-code-file")
    final_parser.add_argument("--acceptance-outcome", choices=("success", "failure", "cancelled", "skipped"), required=True)
    final_parser.add_argument("--tag", required=True)
    final_parser.add_argument("--repo")
    final_parser.add_argument("--source-revision", required=True)
    args = parser.parse_args(argv)
    return verify(args) if args.command == "verify" else finalize_native(args)
