#!/usr/bin/env python3
"""Run the bounded live acceptance gate for the native provider matrix."""

from __future__ import annotations

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[3])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]


import argparse
import json
import traceback


from scripts.acceptance.evidence.files import write_json
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.native.workflow import PROFILES
from scripts.acceptance.native.workflow import BlockedFailure
from scripts.acceptance.native.workflow import BlockReason
from scripts.acceptance.native.workflow import NativeAcceptance
from scripts.acceptance.native.discovery import model_discovery_block_reason
from scripts.acceptance.native.discovery import require_discovery
from scripts.acceptance.native.discovery import resolve_executable
from scripts.acceptance.native.constants import MODEL_DISCOVERY_OPERATION_BUDGET
from scripts.acceptance.native.constants import PI_SESSION_MISS_GATE
from scripts.acceptance.native.constants import PI_SESSION_MISS_SCENARIO
from scripts.acceptance.native.constants import case_task_budget
from scripts.acceptance.native.continuation_scope import CONTINUATION_COMMANDS
from scripts.acceptance.native.output import choose_output as _choose_output
from scripts.acceptance.native.workspace import has_git_ancestor
from scripts.acceptance.paths.operations import reject_tmp
from scripts.acceptance.processes.operations import Processes
from scripts.acceptance.tasks.operations import NativeTaskOps
from scripts.acceptance.native_evidence.contract import EvidenceError
from scripts.acceptance.native_evidence.directories import validate_prepared_case_directory


def choose_output(
        provider: str, explicit: str | None, permission: str | None = None,
        scenario: str | None = None) -> Path:
    return _choose_output(
        provider, explicit, permission, scenario,
        validate_prepared_case_directory, EvidenceError)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog='acceptance-native', description=__doc__)
    parser.add_argument("--provider", required=True, choices=sorted(PROFILES))
    parser.add_argument("--permission", "--mode", dest="mode",
                        choices=("read-only", "workspace-write"), default=None)
    parser.add_argument("--scenario", choices=(
        "read-only", "write", "timeout-resume", "interactive", "non-git", PI_SESSION_MISS_SCENARIO))
    parser.add_argument("--continuation-command", choices=CONTINUATION_COMMANDS,
                        help="contributor-only: admit the resumable successor through this public command "
                             "(default dispatch --resume-task)")
    parser.add_argument("--canonical-aggregate",
                        help="contributor-only: the passed canonical aggregate receipt a public-continue "
                             "or Pi session-miss run must validate before any task work")
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


def _budget_fields(args: argparse.Namespace) -> dict[str, object]:
    """Record the operation budget: its requested label (None when omitted) and persisted value.

    A discovery-only run admits no task; it records its one bounded inspection.
    """
    budget = (MODEL_DISCOVERY_OPERATION_BUDGET if args.discover_models
              else case_task_budget(args.provider))
    return {"task_budget": budget.requested, "canonical_task_budget": budget.canonical}


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


def _blocked_exit_reason(args: argparse.Namespace, run: NativeAcceptance | None) -> BlockReason:
    """Reason for a blocked exit code that raised nothing; only model discovery returns one."""
    if args.discover_models:
        return model_discovery_block_reason(getattr(run, "model_discovery_summary", None))
    return BlockReason.UNCLASSIFIED


def _exit_reason_field(
        code: int, args: argparse.Namespace, run: NativeAcceptance | None) -> dict[str, str]:
    return {"block_reason": _blocked_exit_reason(args, run).value} if code == 2 else {}


def _model_discovery_failure_receipt(
        args: argparse.Namespace, run: NativeAcceptance, code: int) -> dict[str, object]:
    observed = run.model_discovery_exit_code
    return {
        "gate": "acceptance-native",
        "status": "BLOCKED" if code == 2 else "failed",
        "provider": args.provider,
        "mode": args.mode,
        "scenario": args.scenario,
        **_budget_fields(args),
        "requested_model": args.model,
        "requested_effort": args.effort,
        "discover_models": True,
        "discovery_exit_code": observed if type(observed) is int else code,
        "model_discovery": _sanitized_model_discovery_summary(run),
        "no_retry": True,
        "credentials_in_receipt": False,
        **_exit_reason_field(code, args, run),
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
        # The Pi control publishes only its own named gate, outside the positive matrix.
        gate = PI_SESSION_MISS_GATE if args.scenario == PI_SESSION_MISS_SCENARIO else "acceptance-native"
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
                    **_budget_fields(args), "requested_model": args.model,
                    "requested_effort": args.effort,
                    "model_discovery": _sanitized_model_discovery_summary(run),
                    "no_retry": True, "credentials_in_receipt": False,
                    **_exit_reason_field(code, args, run)}):
                return 1
        else:
            run.publish_success(expected_gate=gate)
        status = "passed" if code == 0 else "BLOCKED" if code == 2 else "failed"
        print(json.dumps({"gate": gate, "status": status,
                          "provider": args.provider, "evidence": str(output)}, sort_keys=True))
        return code
    except BlockedFailure as error:
        receipt_ok = _persist_failure_receipt(output, {
            "gate": "acceptance-native", "status": "BLOCKED", "provider": args.provider,
            "mode": args.mode, "scenario": args.scenario, **_budget_fields(args),
            "requested_model": args.model, "requested_effort": args.effort,
            "discover_models": args.discover_models,
            "model_discovery": _sanitized_model_discovery_summary(run), "error": str(error),
            "block_reason": error.reason.value,
            "no_retry": True, "credentials_in_receipt": False})
        cleanup_ok = _finalize_run(run, False, blocked=True)
        if not receipt_ok or not cleanup_ok:
            return 1
        print(f"BLOCKED acceptance-native: {error}", file=sys.stderr, flush=True)
        return 2
    except (AcceptanceFailure, OSError, ValueError, json.JSONDecodeError) as error:
        _persist_failure_receipt(output, {
            "gate": "acceptance-native", "status": "failed", "provider": args.provider,
            "mode": args.mode, "scenario": args.scenario, **_budget_fields(args),
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
            "mode": args.mode, "scenario": args.scenario, **_budget_fields(args),
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
