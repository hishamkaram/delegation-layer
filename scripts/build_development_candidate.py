#!/usr/bin/env python3
"""Build distribution archives, npm tarball, formula and checksums once from committed source."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.release_verification.candidate_build import (
    BuildPaths, build_candidate,
)
from scripts.delegate_acceptance.release_verification.candidate_build_plan import BuildRequest
from scripts.delegate_acceptance.release_verification.contract import VerificationError


HARNESS_ROOT = Path(__file__).resolve().parent.parent


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    build = commands.add_parser("build", help="build the seven candidate files and the manifest")
    build.add_argument("--source-root", type=Path, default=HARNESS_ROOT)
    for name in ("version", "commit", "repository"):
        build.add_argument(f"--{name}", required=True)
    build.add_argument("--base-commit", help="defaults to the source commit")
    for name in ("work-dir", "output-dir", "receipt"):
        build.add_argument(f"--{name}", type=Path, required=True)
    build.add_argument("--goreleaser", type=Path, required=True)
    return parser


def _build(arguments: argparse.Namespace) -> None:
    # The harness is the source commit itself; a different root would build with foreign code.
    if arguments.source_root.resolve() != HARNESS_ROOT:
        raise VerificationError("source_root_not_harness", "build-candidate")
    request = BuildRequest(
        version=arguments.version, commit=arguments.commit, base_commit=arguments.base_commit or arguments.commit,
        repository=arguments.repository)
    paths = BuildPaths(
        source_root=arguments.source_root, work_dir=arguments.work_dir,
        output_dir=arguments.output_dir, receipt_path=arguments.receipt,
        goreleaser=arguments.goreleaser, python=sys.executable)
    build_candidate(request, paths, os.environ)


def main(argv: list[str] | None = None) -> int:
    arguments = _parser().parse_args(argv)
    try:
        _build(arguments)
    except VerificationError as error:
        print(f"candidate {arguments.command} failed: {error.code} ({error.stage})", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
