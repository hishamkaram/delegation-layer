"""Finite, directly owned native process for the driver signal regression."""

from contextlib import ExitStack
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
from types import SimpleNamespace
from unittest.mock import patch

from scripts.acceptance.private_cli import application
from scripts.acceptance.private_cli import finalization
from scripts.acceptance.private_cli.contention.policy import MODERN_CONTENTION_SCENARIO
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.private_cli.nested import refusal
from scripts.acceptance.private_cli.receipt import AcceptanceReceipt


def main():
    if sys.argv[1:] == ["cleanup-child"]:
        signal.alarm(20)
        print("ready", flush=True)
        signal.pause()
        return
    if len(sys.argv) not in (3, 4) or sys.argv[2] not in ("ordinary", "refused"):
        raise SystemExit("expected output path, ordinary/refused scenario and optional cleanup phase")
    output = Path(sys.argv[1])
    refused = sys.argv[2] == "refused"
    phase = sys.argv[3] if len(sys.argv) == 4 else "body"
    if phase not in ("body", "success-cleanup", "error-cleanup", "outer-cleanup"):
        raise SystemExit("unknown signal fixture phase")
    args = SimpleNamespace(output=output, scenario="nested-refused-leader" if refused else MODERN_CONTENTION_SCENARIO,
                           pueue=Path(sys.executable), pueued=Path(sys.executable), native_fixture=Path(sys.executable),
                           expected_backend="process-group")
    receipt = AcceptanceReceipt(output, 3, 1, {})

    def waiting(*_):
        print("ready", flush=True)
        signal.pause()

    def close_child():
        counter = output.with_suffix(".count")
        count = int(counter.read_text()) + 1 if counter.exists() else 1
        counter.write_text(str(count))
        # A caught disposition resets on exec; SIG_IGN would strand this cleanup child.
        child = subprocess.Popen([sys.executable, "-u", "-m",
                                  "scripts.acceptance.private_cli.tests.signal_fixture", "cleanup-child"],
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            if child.stdout is None or child.stderr is None:
                raise RuntimeError("owned cleanup child pipes unavailable")
            if not select.select([child.stdout], [], [], 15)[0] or child.stdout.readline().strip() != "ready":
                raise RuntimeError("owned cleanup child did not become ready")
            if phase != "body":
                print("ready", flush=True)
                # A caught/deferred signal may arrive before the blocking call.
                # Pipe input remains buffered, so the parent's release cannot be lost.
                if sys.stdin.readline().strip() != "release":
                    raise RuntimeError("owned cleanup release was not received")
            # Later signals cannot interrupt unwinding or finalization.
            for number in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
                os.kill(os.getpid(), number)
            child.send_signal(signal.SIGTERM)
            code = child.wait(timeout=15)
            if code != -signal.SIGTERM:
                raise RuntimeError("cleanup child inherited an ignored signal")
        finally:
            if child.poll() is None:
                child.kill()
            child.wait(timeout=15)
            if child.stdout is not None:
                child.stdout.close()
            if child.stderr is not None:
                child.stderr.close()
        return count, code

    def finalize(_args, failure, _lifetime, current, success, *, current_failure=None):
        count, code = close_child()
        current.add_details(initiating_type=type(failure).__name__, cleanup_count=count,
                            cleanup_child_exit=code, success=success)
        current.write("FAIL", {"status": "not-required"}, failure)

    def cleanup_phase(name):
        receipt_phase(name)
        if name == "cleanup" and phase != "outer-cleanup":
            count, code = close_child()
            cleanup_receipt.add_details(cleanup_count=count, cleanup_child_exit=code)

    def fail_body(*_):
        raise RuntimeError("fixture-primary-error")

    with ExitStack() as patches:
        patches.enter_context(patch.multiple(
            application, parse_arguments=lambda: (args, 1, None),
            prepare_receipt=lambda *_: (False, False, False, output.parent, receipt)))
        if phase != "body":
            # Exercise the real finalizer, including its receipt decision, after
            # synchronizing the first signal at phase("cleanup") before its try.
            cleanup_receipt = receipt
            if refused:
                cleanup_receipt = AcceptanceReceipt(output.with_suffix(".inner.json"), 3, 1, {})
                patches.enter_context(patch.object(refusal, "AcceptanceReceipt", return_value=cleanup_receipt))
            receipt_phase = cleanup_receipt.phase
            patches.enter_context(patch.object(cleanup_receipt, "phase", side_effect=cleanup_phase))
            patches.enter_context(patch.object(finalization, "release_fixture_workers", return_value=(None, False, 0, True)))
            patches.enter_context(patch.object(finalization, "finish_root_cleanup",
                                              return_value=(True, {"status": "not-created"})))
            if refused and phase in ("success-cleanup", "outer-cleanup"):
                state = output.parent / "state"
                intent = state / "tasks" / ("a" * 32) / "lifetime" / "intent.json"
                intent.parent.mkdir(parents=True)
                intent.write_text("{}")
                case = SimpleNamespace(a=state, responses={"warm-a": {"task_id": "a" * 32}},
                                       evidence={}, commands=[], configuration={"leader": "b" * 32, "leader_brief": "brief"})

                def expected_refusal(*_):
                    raise AcceptanceFailure("fixture-expected-refusal")

                case.dispatch = expected_refusal

                def prepared(_args, _nonce, _legacy, lifetime, *_rest):
                    lifetime.closed_roots[state] = None
                    return object()

                def dispose(*_):
                    if phase == "outer-cleanup":
                        count, code = close_child()
                        receipt.add_details(cleanup_count=count, cleanup_child_exit=code)
                    output.with_suffix(".disposed").write_text("closed")
                    return {"status": "passed"}

                patches.enter_context(patch.multiple(refusal, prepare_fixture=prepared,
                    RefusedLeaderCase=lambda *_: case, bootstrap=lambda *_: None,
                    observe_live_supervisor=lambda *_: {}, require_backend=lambda *_: None,
                    require_injected_refusal=lambda *_: None, require_inner_failure=lambda *_: None,
                    dispose_retained_root=dispose))
            elif refused:
                patches.enter_context(patch.object(refusal, "prepare_fixture", side_effect=fail_body))
            else:
                patches.enter_context(patch.multiple(application, prepare_fixture=lambda *_: object(),
                    run_prepared_modern_contention=(fail_body if phase == "error-cleanup" else lambda *_: None)))
        elif refused:
            patches.enter_context(patch.multiple(refusal, prepare_fixture=waiting, finalize_fixture=finalize))

            def double_finalize(*_):
                raise AssertionError("outer owner finalized refused-leader twice")

            patches.enter_context(patch.object(application, "finalize_fixture", double_finalize))
        else:
            patches.enter_context(patch.multiple(application, prepare_fixture=lambda *_: object(),
                run_prepared_modern_contention=waiting, finalize_fixture=finalize))
        application.main()


if __name__ == "__main__":
    main()
