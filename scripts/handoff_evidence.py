#!/usr/bin/env python3
"""Write a bounded, deterministic evidence index for a repository handoff, or verify one read-only."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import sys
import tempfile

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.handoff_index import (
    MAX_INDEX_BYTES, MAX_PATH_BYTES, SCHEMA_VERSION, TOOL_VERSION, EvidenceError, EvidenceRequest,
    build_document, canonical_path, repository_root, run_git, safe_text,
)
from scripts.handoff_verify import verify_index

GENERATION_OPTIONS = (
    "baseline", "binary", "receipt", "require_source", "require_binary", "require_receipt",
    "transition_row", "unresolved",
)


def _validate_output(root: Path, raw_output: str) -> Path:
    safe_text(raw_output, MAX_PATH_BYTES)
    candidate = Path(raw_output)
    if not candidate.is_absolute():
        candidate = Path.cwd() / candidate
    try:
        if candidate.is_symlink():
            raise EvidenceError("invalid_output_path", "validate-output")
        output = canonical_path(candidate, strict=False)
        if output.exists() and not output.is_file():
            raise EvidenceError("invalid_output_path", "validate-output")
    except OSError:
        raise EvidenceError("invalid_output_path", "validate-output") from None
    try:
        relative = output.relative_to(root)
    except ValueError:
        return output
    code, _ = run_git(root, "ls-files", "--error-unmatch", "--", relative.as_posix(), stage="validate-output", allow_failure=True)
    if code == 0:
        raise EvidenceError("output_overlaps_input", "validate-output")
    if code != 1:
        raise EvidenceError("git_command_failed", "validate-output")
    code, _ = run_git(
        root, "check-ignore", "-q", "--no-index", "--", relative.as_posix(),
        stage="validate-output", allow_failure=True, literal_pathspecs=False)
    if code == 1:
        raise EvidenceError("output_inside_source_scope", "validate-output")
    if code != 0:
        raise EvidenceError("git_command_failed", "validate-output")
    return output


def _failure(root: Path, error: EvidenceError) -> dict:
    return {
        "schema_version": SCHEMA_VERSION,
        "tool_version": TOOL_VERSION,
        "verdict": "failed",
        "repository": {"root": os.fspath(root), "head": None, "baseline": None, "branch": None, "detached": None},
        "source_manifest": [], "binaries": [], "receipts": [],
        "transition_rows": [], "unresolved": [],
        "failure": {"code": error.code, "stage": error.stage},
    }


def _atomic_write(path: Path, document: dict) -> None:
    try:
        data = (json.dumps(document, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n").encode("ascii")
        if len(data) > MAX_INDEX_BYTES:
            raise EvidenceError("index_too_large", "write-index")
        path.parent.mkdir(parents=True, exist_ok=True)
        descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
        temporary = Path(temporary_name)
        try:
            with os.fdopen(descriptor, "wb") as stream:
                stream.write(data)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, path)
        finally:
            temporary.unlink(missing_ok=True)
    except EvidenceError:
        raise
    except (OSError, ValueError, TypeError):
        raise EvidenceError("output_write_failed", "write-index") from None


class _Parser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        del message
        raise EvidenceError("arguments_invalid", "parse-arguments")


def _parser() -> argparse.ArgumentParser:
    parser = _Parser(description=__doc__)
    parser.add_argument("--root", required=True)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--output")
    mode.add_argument("--verify", metavar="INDEX")
    parser.add_argument("--baseline")
    parser.add_argument("--binary", nargs=3, action="append", default=[], metavar=("ROLE", "PATH", "IDENTITY"))
    parser.add_argument("--receipt", nargs=2, action="append", default=[], metavar=("CLAIM", "PATH"))
    parser.add_argument("--require-source", action="append", default=[])
    parser.add_argument("--require-binary", action="append", default=[])
    parser.add_argument("--require-receipt", action="append", default=[])
    parser.add_argument("--transition-row", action="append", default=[])
    parser.add_argument("--unresolved", action="append", default=[])
    return parser


def _request(args: argparse.Namespace, output: Path) -> EvidenceRequest:
    return EvidenceRequest(
        base=Path.cwd().resolve(), output=output, baseline=args.baseline,
        binaries=tuple(tuple(values) for values in args.binary),
        receipts=tuple(tuple(values) for values in args.receipt),
        transition_rows=tuple(args.transition_row), unresolved=tuple(args.unresolved),
        require_source=tuple(args.require_source), require_binary=tuple(args.require_binary),
        require_receipt=tuple(args.require_receipt),
    )


def _generate(args: argparse.Namespace) -> int:
    root: Path | None = None
    output: Path | None = None
    safe_to_write = False
    try:
        root = repository_root(safe_text(args.root, MAX_PATH_BYTES))
        output = _validate_output(root, args.output)
        safe_to_write = True
        document, code = build_document(root, _request(args, output))
        _atomic_write(output, document)
        if code:
            print(f"handoff evidence: {document.get('failure', {}).get('code', 'evidence_incomplete')}", file=sys.stderr)
        return code
    except EvidenceError as error:
        if root is not None and output is not None and safe_to_write and error.safe_to_write:
            try:
                _atomic_write(output, _failure(root, error))
            except EvidenceError:
                pass
        print(f"handoff evidence: {error.code}", file=sys.stderr)
        return 1
    except Exception:
        error = EvidenceError("internal_failure", "process")
        if root is not None and output is not None and safe_to_write:
            try:
                _atomic_write(output, _failure(root, error))
            except EvidenceError:
                pass
        print(f"handoff evidence: {error.code}", file=sys.stderr)
        return 1


def _verification_failure(error: EvidenceError) -> dict:
    return {"tool_version": TOOL_VERSION, "verified": False, "failure": {"code": error.code, "stage": error.stage}}


def _verify(args: argparse.Namespace) -> int:
    try:
        if any(getattr(args, name) for name in GENERATION_OPTIONS):
            raise EvidenceError("arguments_invalid", "parse-arguments")
        root = repository_root(safe_text(args.root, MAX_PATH_BYTES))
        index = Path(safe_text(args.verify, MAX_PATH_BYTES))
        report, code = verify_index(root, index if index.is_absolute() else Path.cwd() / index)
    except EvidenceError as error:
        report, code = _verification_failure(error), 1
        print(f"handoff evidence: {error.code}", file=sys.stderr)
    except Exception:
        report, code = _verification_failure(EvidenceError("internal_failure", "process")), 1
        print("handoff evidence: internal_failure", file=sys.stderr)
    print(json.dumps(report, sort_keys=True, separators=(",", ":"), ensure_ascii=True))
    return code


def main(argv: list[str] | None = None) -> int:
    try:
        args = _parser().parse_args(argv)
    except EvidenceError as error:
        print(f"handoff evidence: {error.code}", file=sys.stderr)
        return 1
    return _verify(args) if args.verify is not None else _generate(args)


if __name__ == "__main__":
    raise SystemExit(main())
