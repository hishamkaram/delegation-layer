#!/usr/bin/env python3
"""Run the bounded live acceptance gate for the native provider matrix."""

from __future__ import annotations

import argparse
import json
import sys
import traceback
from pathlib import Path

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.native.acceptance import (
    PROFILES,
    TASK_BUDGET,
    BlockedFailure,
    NativeAcceptance,
)
from scripts.delegate_acceptance.native.acceptance_discovery import (
    require_discovery,
    resolve_executable,
)
from scripts.delegate_acceptance.native.output import choose_output as _choose_output
from scripts.delegate_acceptance.native.workspace import has_git_ancestor
from scripts.delegate_acceptance.paths.operations import reject_tmp
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps
from scripts.native_acceptance_evidence import (
    EvidenceError,
    validate_prepared_case_directory,
)


def choose_output(
        provider: str, explicit: str | None, permission: str | None = None,
        scenario: str | None = None) -> Path:
    return _choose_output(
        provider, explicit, permission, scenario,
        validate_prepared_case_directory, EvidenceError)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--provider", required=True, choices=sorted(PROFILES))
    parser.add_argument("--permission", "--mode", dest="mode",
                        choices=("read-only", "workspace-write"), default=None)
    parser.add_argument("--scenario", choices=(
        "read-only", "write", "timeout-resume", "interactive", "non-git"))
    parser.add_argument("--output")
    parser.add_argument("--delegate")
    parser.add_argument("--runner")
    parser.add_argument("--pueue")
    parser.add_argument("--pueued")
    parser.add_argument("--provider-executable")
    parser.add_argument("--model")
    parser.add_argument("--effort")
    parser.add_argument("--discover-models", action="store_true",
                        help="run advisory model discovery and stop without admitting a task")
    return parser


def _persist_failure_receipt(output: Path | None, value: dict[str, object]) -> bool:
    if output is None:
        return True
    try:
        write_json(output / "failure.json", value)
        return True
    except BaseException as error:
        print("FAIL acceptance-native: failure diagnostics could not be persisted: " + str(error),
              file=sys.stderr, flush=True)
        return False


def _finalize_run(run: NativeAcceptance | None, success: bool, *, blocked: bool = False) -> bool:
    if run is None:
        return True
    try:
        finalized = run.finalize(success, blocked=blocked)
    except BaseException as error:
        print("FAIL acceptance-native: test-owned runtime cleanup raised: " + str(error),
              file=sys.stderr, flush=True)
        return False
    if not finalized:
        print("FAIL acceptance-native: test-owned runtime cleanup was not completed",
              file=sys.stderr, flush=True)
    return finalized


def _sanitized_model_discovery_summary(run: NativeAcceptance | None) -> dict[str, object] | None:
    summary = getattr(run, "model_discovery_summary", None)
    sanitized: dict[str, object] = {}
    if isinstance(summary, dict):
        for key in ("status", "reason_code", "complete", "model_count", "effort_count"):
            value = summary.get(key)
            if key in {"status", "reason_code"} and isinstance(value, str):
                sanitized[key] = value
            elif key == "complete" and type(value) is bool:
                sanitized[key] = value
            elif key in {"model_count", "effort_count"} and (value is None or type(value) is int):
                sanitized[key] = value
    return sanitized or None


def _model_discovery_failure_receipt(
        args: argparse.Namespace, run: NativeAcceptance, code: int) -> dict[str, object]:
    return {
        "gate": "acceptance-native",
        "status": "BLOCKED" if code == 2 else "failed",
        "provider": args.provider,
        "mode": args.mode,
        "scenario": args.scenario,
        "task_budget": TASK_BUDGET,
        "requested_model": args.model,
        "requested_effort": args.effort,
        "discover_models": True,
        "discovery_exit_code": code,
        "model_discovery": _sanitized_model_discovery_summary(run),
        "no_retry": True,
        "credentials_in_receipt": False,
    }


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    run: NativeAcceptance | None = None
    output: Path | None = None
    try:
        output = choose_output(args.provider, args.output, args.mode, args.scenario)
        if args.provider == "ccr:print" and not args.model and not args.discover_models:
            raise BlockedFailure("CCR acceptance requires --model with a configured alias")
        if args.provider == "opencode:run" and not args.model and not args.discover_models:
            raise BlockedFailure("OpenCode acceptance requires --model with a tool-capable configured route")
        run = NativeAcceptance(argparse.Namespace(**{**vars(args), "output": str(output)}))
        code = run.run()
        if not _finalize_run(run, code == 0, blocked=(code == 2)):
            return 1
        if code != 0:
            if args.discover_models:
                if not _persist_failure_receipt(
                        output, _model_discovery_failure_receipt(args, run, code)):
                    return 1
            elif not _persist_failure_receipt(output, {
                    "gate": "acceptance-native", "status": "BLOCKED" if code == 2 else "failed",
                    "provider": args.provider, "mode": args.mode, "scenario": args.scenario,
                    "task_budget": TASK_BUDGET, "requested_model": args.model,
                    "requested_effort": args.effort,
                    "model_discovery": _sanitized_model_discovery_summary(run),
                    "no_retry": True, "credentials_in_receipt": False}):
                return 1
        else:
            run.publish_success()
        status = "passed" if code == 0 else "BLOCKED" if code == 2 else "failed"
        print(json.dumps({"gate": "acceptance-native", "status": status,
                          "provider": args.provider, "evidence": str(output)}, sort_keys=True))
        return code
    except BlockedFailure as error:
        receipt_ok = _persist_failure_receipt(output, {
            "gate": "acceptance-native", "status": "BLOCKED", "provider": args.provider,
            "mode": args.mode, "scenario": args.scenario, "task_budget": TASK_BUDGET,
            "requested_model": args.model, "requested_effort": args.effort,
            "discover_models": args.discover_models,
            "model_discovery": _sanitized_model_discovery_summary(run), "error": str(error),
            "no_retry": True, "credentials_in_receipt": False})
        cleanup_ok = _finalize_run(run, False, blocked=True)
        if not receipt_ok or not cleanup_ok:
            return 1
        print(f"BLOCKED acceptance-native: {error}", file=sys.stderr, flush=True)
        return 2
    except (AcceptanceFailure, OSError, ValueError, json.JSONDecodeError) as error:
        _persist_failure_receipt(output, {
            "gate": "acceptance-native", "status": "failed", "provider": args.provider,
            "mode": args.mode, "scenario": args.scenario, "task_budget": TASK_BUDGET,
            "requested_model": args.model, "requested_effort": args.effort,
            "discover_models": args.discover_models,
            "model_discovery": _sanitized_model_discovery_summary(run), "error": str(error),
            "no_retry": True, "credentials_in_receipt": False})
        _finalize_run(run, False)
        print(f"FAIL acceptance-native: {error}", file=sys.stderr, flush=True)
        return 1
    except BaseException as error:
        _persist_failure_receipt(output, {
            "gate": "acceptance-native", "status": "failed", "provider": args.provider,
            "mode": args.mode, "scenario": args.scenario, "task_budget": TASK_BUDGET,
            "requested_model": args.model, "requested_effort": args.effort,
            "discover_models": args.discover_models,
            "model_discovery": _sanitized_model_discovery_summary(run), "error": str(error),
            "traceback": traceback.format_exc(), "no_retry": True,
            "credentials_in_receipt": False})
        _finalize_run(run, False)
        print(f"FAIL acceptance-native: unexpected {error}", file=sys.stderr, flush=True)
        return 1


__all__ = [
    "AcceptanceFailure", "BlockedFailure", "EvidenceError", "NativeAcceptance",
    "NativeTaskOps", "Path", "Processes", "build_parser", "choose_output",
    "has_git_ancestor", "main", "reject_tmp", "require_discovery", "resolve_executable",
    "validate_prepared_case_directory",
]


if __name__ == "__main__":
    raise SystemExit(main())
