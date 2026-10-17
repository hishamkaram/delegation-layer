#!/usr/bin/env python3
"""Run the three-turn Antigravity native acceptance gate.

The harness uses the shared acceptance process owner for direct observations.
It retains the caller's environment so the signed-in native CLI can be
exercised, while the production adapter sends only its bounded allowlist
through the queue. The harness records only environment names and never uses a
timeout wrapper or a signal. An observation timeout therefore leaves the
Popen handle and all evidence in place for review.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import secrets
import sys

if __package__ in (None, ""):
    repository_root = Path(__file__).resolve().parents[1]
    if str(repository_root) not in sys.path:
        sys.path.insert(0, str(repository_root))

from scripts.acceptance_provider_common import (
    AcceptanceFailure,
    clean_absolute,
    reject_tmp,
    require,
)
from scripts.acceptance_supervisor_common import write_json
from scripts.delegate_acceptance.provider_drivers.agy.runtime import DEFAULT_EVIDENCE_PARENT
from scripts.delegate_acceptance.provider_drivers.agy.prepared import (
    Prepared,
    fresh_prepared,
    load_prepared,
)
from scripts.delegate_acceptance.provider_drivers.agy.receipts import (
    best_effort_failure_receipt,
    failure_cleanup,
    remove_passed_receipt,
)
from scripts.delegate_acceptance.provider_drivers.agy.runner import NativeRun
from scripts.delegate_acceptance.provider_drivers.agy.runtime import BlockedFailure, utc_stamp


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prepared", default=None, help="prepared acceptance JSON receipt")
    parser.add_argument("--output", default=None, help="evidence directory")
    parser.add_argument("--delegate", default=None, help="absolute shipped delegate executable")
    parser.add_argument("--runner", default=None, help="absolute shipped delegate-run executable")
    parser.add_argument("--agy", default=None, help="absolute agy executable")
    parser.add_argument("--pueue", default=None, help="absolute pueue executable")
    parser.add_argument("--pueued", default=None, help="absolute pueued executable")
    return parser


def choose_output(prepared: Prepared | None, explicit: str | None) -> Path:
    value = explicit or os.environ.get("AGY_ACCEPTANCE_OUTPUT")
    if value:
        path = clean_absolute(value, "acceptance output")
        reject_tmp(path, "acceptance output")
        return path
    require(prepared is not None, "prepared inputs are required to derive an output path")
    return DEFAULT_EVIDENCE_PARENT / prepared.state.parent.name


def initial_output(explicit: str | None, run_name: str) -> Path:
    """Select a terminal receipt path before fresh preparation can fail."""
    if explicit:
        return choose_output(None, explicit)
    return DEFAULT_EVIDENCE_PARENT / run_name


def handle_failure(output: Path | None, error: BaseException, run: NativeRun | None,
                   blocked: bool, prepared: Prepared | None = None,
                   stage: str = "unknown") -> int:
    receipt_removal_error = remove_passed_receipt(output)
    cleanup_summary: dict[str, object] = {"status": "started", "errors": []}
    best_effort_failure_receipt(output, error, run, stage, cleanup_summary, prepared)
    standalone = () if prepared is None or run is not None else prepared.owned_directories
    cleanup_errors = failure_cleanup(run, output, standalone)
    if receipt_removal_error is not None:
        cleanup_errors.insert(0, receipt_removal_error)
    if cleanup_errors:
        print(f"FAIL acceptance-agy cleanup: {cleanup_errors[0]}", file=sys.stderr, flush=True)
    cleanup_summary = {
        "status": "completed" if not cleanup_errors else "failed",
        "errors": [str(item) for item in cleanup_errors],
    }
    best_effort_failure_receipt(output, error, run, stage, cleanup_summary, prepared)
    if cleanup_errors:
        return 1
    print(f"{'BLOCKED' if blocked else 'FAIL'} acceptance-agy: {error}",
          file=sys.stderr, flush=True)
    return 2 if blocked else 1


def main(argv: list[str]) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    prepared: Prepared | None = None
    output: Path | None = None
    run: NativeRun | None = None
    stage = "initialization"
    run_name = f"agy-{utc_stamp()}-{secrets.token_hex(4)}"
    try:
        prepared_path = args.prepared or os.environ.get("AGY_ACCEPTANCE_PREPARED")
        output = initial_output(args.output, run_name)
        if prepared_path:
            stage = "load-prepared"
            prepared = load_prepared(clean_absolute(prepared_path, "prepared receipt"))
            output = choose_output(prepared, args.output)
            require(not output.exists(), f"acceptance output already exists: {output}")
        else:
            # Fresh inputs are created in the controlled acceptance tree.  The
            # output directory is selected after the unique state parent exists.
            # Fresh inputs and their compact evidence are separate roots so the
            # former can be removed after verified supervisor shutdown.
            stage = "fresh-prepared"
            prepared = fresh_prepared(run_name)
            output = choose_output(prepared, args.output)
            require(not output.exists(), f"fresh acceptance output unexpectedly exists: {output}")
        require(prepared is not None and output is not None, "acceptance inputs were not initialized")
        stage = "setup"
        run = NativeRun(args, prepared, output)
        stage = "run"
        success_receipt = run.run()
        stage = "finalize"
        if not run.finalize(True):
            raise AcceptanceFailure("completed acceptance artifacts could not be cleaned")
        write_json(output / "success.json", success_receipt, replace=True)
        print(json.dumps({"gate": "acceptance-agy", "status": "passed", "evidence": str(output)}, sort_keys=True))
        return 0
    except BlockedFailure as error:
        return handle_failure(output, error, run, True, prepared, stage)
    except (AcceptanceFailure, OSError, ValueError, json.JSONDecodeError) as error:
        return handle_failure(output, error, run, False, prepared, stage)
    except BaseException as error:
        return handle_failure(output, error, run, False, prepared, stage)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
