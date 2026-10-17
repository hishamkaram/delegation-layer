"""Build, validate, and retain bounded native acceptance evidence."""

from __future__ import annotations

from pathlib import Path
import argparse
import json
import os
import sys
from .aggregate import (
    build_aggregate,
)
from .contract import (
    AGGREGATE_RECEIPT_NAME,
    CASE_MATRIX_NAME,
    EXPECTED_CASES,
    EvidenceError,
    _parse_optional_revision,
)
from .directories import (
    prepare_case_directory,
)
from .staging import (
    stage_evidence,
)
from .validation import (
    validate_aggregate_receipt,
)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("list-cases")
    prepare = commands.add_parser("prepare-case")
    prepare.add_argument("--aggregate-root", required=True)
    prepare.add_argument("--case-id", required=True)
    aggregate = commands.add_parser("aggregate")
    aggregate.add_argument("--aggregate-root", required=True)
    aggregate.add_argument("--matrix")
    aggregate.add_argument("--receipt")
    aggregate.add_argument("--tag")
    aggregate.add_argument("--source-revision")
    aggregate.add_argument("--retention-count", type=int)
    aggregate.add_argument("--require-pass", action="store_true")
    validate = commands.add_parser("validate")
    validate.add_argument("--receipt", required=True)
    validate.add_argument("--tag")
    validate.add_argument("--source-revision")
    stage = commands.add_parser("stage")
    stage.add_argument("--aggregate-receipt", required=True)
    stage.add_argument("--output", required=True)
    stage.add_argument("--archive-receipt")
    stage.add_argument("--native-receipt")
    stage.add_argument("--tag")
    stage.add_argument("--source-revision")
    return parser


def _command(argv: list[str]) -> int:
    args = _parser().parse_args(argv)
    if args.command == "list-cases":
        for case in EXPECTED_CASES:
            print("\t".join((case.case_id, case.provider, case.permission, case.scenario)))
        return 0
    if args.command == "prepare-case":
        directory = prepare_case_directory(Path(args.aggregate_root), args.case_id)
        print(json.dumps({"case_id": args.case_id, "case_directory": str(directory)}, sort_keys=True))
        return 0
    if args.command == "validate":
        source_revision = _parse_optional_revision(
            args.source_revision if args.source_revision is not None
            else os.environ.get("DELEGATE_ACCEPTANCE_SOURCE_REVISION"),
        )
        value, digest = validate_aggregate_receipt(
            Path(args.receipt).absolute(),
            expected_tag=args.tag or os.environ.get("DELEGATE_ACCEPTANCE_TAG") or None,
            expected_source_revision=source_revision,
        )
        print(json.dumps({
            "aggregate_receipt": str(Path(args.receipt).absolute()),
            "status": value["status"], "case_count": value["case_count"],
            "sha256": digest,
        }, sort_keys=True))
        return 0
    if args.command == "stage":
        source_revision = _parse_optional_revision(
            args.source_revision if args.source_revision is not None
            else os.environ.get("DELEGATE_ACCEPTANCE_SOURCE_REVISION"),
        )
        manifest = stage_evidence(
            Path(args.aggregate_receipt), Path(args.output),
            archive_receipt=Path(args.archive_receipt) if args.archive_receipt else None,
            native_receipt=Path(args.native_receipt) if args.native_receipt else None,
            expected_tag=args.tag or os.environ.get("DELEGATE_ACCEPTANCE_TAG") or None,
            expected_source_revision=source_revision,
        )
        print(json.dumps({"output": str(Path(args.output).absolute()), **manifest}, sort_keys=True))
        return 0
    root = Path(args.aggregate_root).absolute()
    matrix = Path(args.matrix).absolute() if args.matrix else root / CASE_MATRIX_NAME
    receipt = Path(args.receipt).absolute() if args.receipt else root / AGGREGATE_RECEIPT_NAME
    source_revision = _parse_optional_revision(
        args.source_revision if args.source_revision is not None
        else os.environ.get("DELEGATE_ACCEPTANCE_SOURCE_REVISION"),
    )
    value, exit_code = build_aggregate(
        root, matrix, receipt,
        tag=args.tag or os.environ.get("DELEGATE_ACCEPTANCE_TAG") or None,
        source_revision=source_revision,
        require_pass=args.require_pass, retention_count=args.retention_count,
    )
    print(json.dumps({
        "aggregate_receipt": str(receipt), "status": value["status"], "exit_code": exit_code,
        "case_count": value["case_count"], "unresolved_case_count": value["unresolved_case_count"],
    }, sort_keys=True))
    return exit_code


def main(argv: list[str] | None = None) -> int:
    try:
        return _command(argv if argv is not None else sys.argv[1:])
    except (EvidenceError, OSError, ValueError) as error:
        print(f"native acceptance evidence failed: {error}", file=sys.stderr)
        return 1
